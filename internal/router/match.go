package router

import (
	"sort"
	"strings"
)

// Match 按 Host、Method 和路径优先级匹配已解码的路径段
// 路径安全由调用方先通过 ParsePath 检查；Host 必须先验证才能进入任意 Host 路由
func (r *Router) Match(host, method string, pathSegments []string) (*MatchResult, *MatchError) {
	normalizedHost, hostErr := NormalizeHost(host)
	if hostErr != nil {
		return nil, hostErr
	}
	upperMethod := strings.ToUpper(method)

	// 高优先级 Host 没有匹配 Method/Path 时，仍继续尝试低优先级候选
	candidates := r.hostCandidates(normalizedHost)
	for _, hg := range candidates {
		if hg == nil {
			continue
		}

		// Method tree 的查询顺序直接写在热路径中，避免 ResolveMethod 返回切片产生分配
		// HEAD 固定尝试显式 HEAD → GET fallback → any；其他方法尝试显式 → any
		if upperMethod == "HEAD" {
			if result := matchMethodTree(hg, "HEAD", pathSegments); result != nil {
				return result, nil
			}
			if result := matchMethodTree(hg, "GET", pathSegments); result != nil {
				return result, nil
			}
		} else if result := matchMethodTree(hg, upperMethod, pathSegments); result != nil {
			return result, nil
		}
		if result := matchMethodTree(hg, "", pathSegments); result != nil {
			return result, nil
		}
	}

	// 只有正常匹配全部失败才探测其他方法，避免影响成功请求的路径
	methods, _ := collectAllowedMethods(candidates, pathSegments, upperMethod)
	if len(methods) > 0 {
		return nil, &MatchError{Code: MatchErrMethodNotAllowed, Message: "该路径不支持请求方法", AllowedMethods: methods}
	}
	return nil, &MatchError{Code: MatchErrNoRoute, Message: "没有匹配的路由"}
}

// AllowedMethods 查询已解码路径在所有匹配 Host 层级中可接受的方法
// methods 按字典序去重，GET 包含 HEAD；any 表示任意方法，此时 methods 为空
// 不存在路径时返回 MatchErrNoRoute，非法 Host 返回 MatchErrIllegalHost；不自动添加 OPTIONS
func (r *Router) AllowedMethods(host string, pathSegments []string) (methods []string, any bool, err *MatchError) {
	normalized, hostErr := NormalizeHost(host)
	if hostErr != nil {
		return nil, false, hostErr
	}
	methods, any = collectAllowedMethods(r.hostCandidates(normalized), pathSegments, "")
	if len(methods) == 0 && !any {
		return nil, false, &MatchError{Code: MatchErrNoRoute, Message: "没有匹配的路由"}
	}
	return methods, any, nil
}

// collectAllowedMethods 只读扫描候选中的 Method Tree，不跨到其他 Host
// attemptedMethod 非空时，跳过 Match 已经尝试失败的显式、HEAD 回退与 any 树
// 不限制方法的路径代表任意 token，无法用一个有限的 Allow 列表表达
func collectAllowedMethods(candidates [3]*hostGroup, path []string, attemptedMethod string) ([]string, bool) {
	var methods []string
	for _, group := range candidates {
		if group == nil {
			continue
		}
		for method, tree := range group.methodTrees {
			if attemptedMethod != "" && (method == attemptedMethod || method == "" || attemptedMethod == "HEAD" && method == "GET") {
				continue
			}
			if matchTree(tree, path) == nil {
				continue
			}
			if method == "" {
				return nil, true
			}
			methods = append(methods, method)
			if method == "GET" {
				methods = append(methods, "HEAD")
			}
		}
	}
	sort.Strings(methods)
	unique := methods[:0]
	for _, method := range methods {
		if len(unique) == 0 || unique[len(unique)-1] != method {
			unique = append(unique, method)
		}
	}
	return unique, false
}

// hostCandidates 按固定优先级返回至多三个 Host 组，不分配候选切片
// 只去除第一个 label，因而 *.example.com 不会匹配 a.b.example.com
func (r *Router) hostCandidates(host string) [3]*hostGroup {
	candidates := [3]*hostGroup{r.exactHosts[host], nil, r.anyHost}
	if dot := strings.IndexByte(host, '.'); dot > 0 {
		candidates[1] = r.wildcardHosts[host[dot+1:]]
	}
	return candidates
}

// matchMethodTree 在指定 Host 组中查找一个 Method tree；不存在时直接返回 nil
func matchMethodTree(group *hostGroup, method string, pathSegments []string) *MatchResult {
	tree, exists := group.methodTrees[method]
	if !exists {
		return nil
	}
	return matchTree(tree, pathSegments)
}

// matchTree 为常见路径提供八个栈上参数槽位；更深的参数路径由 append 正常扩容
// 捕获只在本次递归中使用，返回结果将值复制进独立 map，不持有缓冲区切片
func matchTree(tree *methodTree, segments []string) *MatchResult {
	var values [8]string
	return matchPath(tree.root, segments, values[:0])
}

// matchPath 在 Radix Tree 中递归匹配路径段
//
// 子节点已按优先级排序（static > param > catchAll），因此返回第一个匹配结果
// paramValues 的长度是当前分支深度；失败返回后，兄弟分支从父切片长度继续追加
// 只有成功叶子会构建独立结果，因此回溯覆盖缓冲区不会污染已返回的参数
// paramValues 只记录参数值，不绑定节点上的参数名；同一结构分支可能服务于
// `/:id` 与 `/:name`，最终必须按命中路由自己的 paramNames 构造 Params
func matchPath(n *node, segments []string, paramValues []string) *MatchResult {
	if n == nil {
		return nil
	}

	// static prefix 直接在压缩字符串上逐段比较，避免 strings.Split 在每次候选
	// 探测时创建切片。返回值 consumed 表示成功消费的请求路径段数
	if n.segType == segStatic && n.prefix != "" {
		consumed, ok := matchStaticPrefix(n.prefix, segments)
		if !ok {
			return nil
		}
		segments = segments[consumed:]
	}

	// 路径已完全消费
	if len(segments) == 0 {
		if len(n.routes) > 0 {
			return buildMatchResult(&n.routes[0], paramValues)
		}
		// catch-all 可匹配空剩余；精确终点已在上方优先返回
		for _, child := range n.children {
			if child.segType == segCatchAll && len(child.routes) > 0 {
				return buildMatchResult(&child.routes[0], append(paramValues, ""))
			}
		}
		return nil
	}

	// 同一父节点下每个 static 子边的首段唯一，并已按 prefix 字典序排序
	// 使用二分查找只探测可能命中的 static 边，避免高 fan-out 时扫描全部路由
	if child := findStaticChild(n.children, segments[0]); child != nil {
		if result := matchPath(child, segments, paramValues); result != nil {
			return result
		}
	}

	// 每个父节点至多各有一个结构化 param 和 catch-all 子边
	// children 已把所有 static 放在前面，因此二分定位第一个动态边后只检查最多两个节点
	dynamicStart := sort.Search(len(n.children), func(i int) bool {
		return n.children[i].segType != segStatic
	})
	for _, child := range n.children[dynamicStart:] {
		switch child.segType {
		case segParam:
			if segments[0] != "" {
				values := append(paramValues, segments[0])
				if result := matchPath(child, segments[1:], values); result != nil {
					return result
				}
			}
		case segCatchAll:
			if len(child.routes) > 0 {
				values := append(paramValues, strings.Join(segments, "/"))
				return buildMatchResult(&child.routes[0], values)
			}
		}
	}

	return nil
}

// findStaticChild 在已排序的 children 中按 static 首段做二分查找
func findStaticChild(children []*node, segment string) *node {
	index := sort.Search(len(children), func(i int) bool {
		child := children[i]
		if child.segType != segStatic {
			return true
		}
		return firstPrefixSegment(child.prefix) >= segment
	})
	if index >= len(children) || children[index].segType != segStatic {
		return nil
	}
	if firstPrefixSegment(children[index].prefix) != segment {
		return nil
	}
	return children[index]
}

func firstPrefixSegment(prefix string) string {
	if slash := strings.IndexByte(prefix, '/'); slash >= 0 {
		return prefix[:slash]
	}
	return prefix
}

// matchStaticPrefix 直接比较压缩 prefix 中的各段，不创建临时切片
func matchStaticPrefix(prefix string, segments []string) (int, bool) {
	consumed := 0
	for len(prefix) > 0 {
		if consumed >= len(segments) {
			return 0, false
		}
		part := prefix
		if slash := strings.IndexByte(prefix, '/'); slash >= 0 {
			part = prefix[:slash]
			prefix = prefix[slash+1:]
		} else {
			prefix = ""
		}
		if segments[consumed] != part {
			return 0, false
		}
		consumed++
	}
	return consumed, true
}

// buildMatchResult 从编译路由和按出现顺序收集的参数值构建 MatchResult
func buildMatchResult(route *compiledRoute, paramValues []string) *MatchResult {
	var params map[string]string
	if len(route.paramNames) > 0 {
		params = make(map[string]string, len(route.paramNames))
	}
	for i, name := range route.paramNames {
		// 参数数量由编译后的路径结构保证一致；边界检查防止内部状态损坏时 panic
		if i >= len(paramValues) {
			break
		}
		params[name] = paramValues[i]
	}
	return &MatchResult{
		RouteID:      route.routeID,
		PathTemplate: route.pathTemplate,
		UpstreamID:   route.upstreamID,
		Params:       params,
		PreserveHost: route.preserveHost,
	}
}
