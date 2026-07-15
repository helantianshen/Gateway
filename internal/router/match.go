package router

import (
	"sort"
	"strings"
)

// Match 在 Radix Tree 中查找匹配的路由。
//
// 参数：
//   - host: 请求的 Host 头（未归一化，函数内部归一化）
//   - method: 请求的 HTTP 方法
//   - pathSegments: 经 ParsePath 解析和解码后的路径段
//
// 返回：
//   - 成功：*MatchResult 和 nil
//   - 无匹配：nil 和 *MatchError（Code=MatchErrNoRoute）
//   - 不会返回 MatchErrIllegalPath（路径安全检查在 ParsePath 中完成）
func (r *Router) Match(host, method string, pathSegments []string) (*MatchResult, *MatchError) {
	normalizedHost := NormalizeHost(host)
	upperMethod := strings.ToUpper(method)

	// hostGroups 已在 Compile/freeze 阶段按 exact > wildcard > any 排序；
	// 请求热路径只读遍历，不复制或排序任何配置结构。
	for _, hg := range r.hostGroups {
		if !hg.pattern.MatchHost(normalizedHost) {
			continue
		}

		// Method tree 的查询顺序直接写在热路径中，避免 ResolveMethod 返回切片产生分配。
		// HEAD 固定尝试显式 HEAD → GET fallback → any；其他方法尝试显式 → any。
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

	return nil, &MatchError{Code: MatchErrNoRoute, Message: "没有路由匹配 " + method + " " + strings.Join(pathSegments, "/")}
}

// matchMethodTree 在指定 Host 组中查找一个 Method tree；不存在时直接返回 nil。
func matchMethodTree(group *hostGroup, method string, pathSegments []string) *MatchResult {
	tree, exists := group.methodTrees[method]
	if !exists {
		return nil
	}
	return matchPath(tree.root, pathSegments, nil)
}

// matchPath 在 Radix Tree 中递归匹配路径段。
//
// 子节点已按优先级排序（static > param > catchAll），因此返回第一个匹配结果。
// paramValues 只记录参数值，不绑定节点上的参数名；同一结构分支可能服务于
// `/:id` 与 `/:name`，最终必须按命中路由自己的 paramNames 构造 Params。
func matchPath(n *node, segments []string, paramValues []string) *MatchResult {
	if n == nil {
		return nil
	}

	// static prefix 直接在压缩字符串上逐段比较，避免 strings.Split 在每次候选
	// 探测时创建切片。返回值 consumed 表示成功消费的请求路径段数。
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
		// catch-all 可匹配空剩余；精确终点已在上方优先返回。
		for _, child := range n.children {
			if child.segType == segCatchAll && len(child.routes) > 0 {
				return buildMatchResult(&child.routes[0], appendParamValue(paramValues, ""))
			}
		}
		return nil
	}

	// 同一父节点下每个 static 子边的首段唯一，并已按 prefix 字典序排序。
	// 使用二分查找只探测可能命中的 static 边，避免高 fan-out 时扫描全部路由。
	if child := findStaticChild(n.children, segments[0]); child != nil {
		if result := matchPath(child, segments, paramValues); result != nil {
			return result
		}
	}

	// 每个父节点至多各有一个结构化 param 和 catch-all 子边。children 已把所有
	// static 放在前面，因此二分定位第一个动态边后只检查最多两个节点。
	dynamicStart := sort.Search(len(n.children), func(i int) bool {
		return n.children[i].segType != segStatic
	})
	for _, child := range n.children[dynamicStart:] {
		switch child.segType {
		case segParam:
			if segments[0] != "" {
				values := appendParamValue(paramValues, segments[0])
				if result := matchPath(child, segments[1:], values); result != nil {
					return result
				}
			}
		case segCatchAll:
			if len(child.routes) > 0 {
				values := appendParamValue(paramValues, strings.Join(segments, "/"))
				return buildMatchResult(&child.routes[0], values)
			}
		}
	}

	return nil
}

// findStaticChild 在已排序的 children 中按 static 首段做二分查找。
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

// matchStaticPrefix 直接比较压缩 prefix 中的各段，不创建临时切片。
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

// buildMatchResult 从编译路由和按出现顺序收集的参数值构建 MatchResult。
func buildMatchResult(route *compiledRoute, paramValues []string) *MatchResult {
	var params map[string]string
	if len(route.paramNames) > 0 {
		params = make(map[string]string, len(route.paramNames))
	}
	for i, name := range route.paramNames {
		// 参数数量由编译后的路径结构保证一致；边界检查防止内部状态损坏时 panic。
		if i >= len(paramValues) {
			break
		}
		params[name] = paramValues[i]
	}
	return &MatchResult{
		RouteID:      route.routeID,
		UpstreamID:   route.upstreamID,
		Params:       params,
		PreserveHost: route.preserveHost,
	}
}

// appendParamValue 复制并追加一个参数值，避免递归分支共享底层数组后相互覆盖。
func appendParamValue(values []string, value string) []string {
	result := make([]string, len(values), len(values)+1)
	copy(result, values)
	return append(result, value)
}
