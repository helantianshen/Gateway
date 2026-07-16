package router

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CompileInput 是编译路由器的输入。
type CompileInput struct {
	// RouteID 是路由的唯一标识符。
	RouteID string
	// Host 是路由的 Host 模式（空表示任意 Host）。
	Host string
	// Method 是路由的 HTTP 方法（空表示任意 Method）。
	Method string
	// Path 是路由的路径模式（支持 static/:param/*catchAll）。
	Path string
	// Upstream 是路由引用的 upstream ID。
	Upstream string
	// Priority 是路由的优先级（仅在同 specificity 时参与比较）。
	Priority int
	// PreserveHost 为 true 时保留客户端原始 Host 头。
	PreserveHost bool
}

// Router 是编译后的不可变路由器。
//
// 编译后的树完全不可变，可被并发安全读取。
type Router struct {
	// hostGroups 已按 exact > wildcard > any 和模式值稳定排序。
	hostGroups []*hostGroup
}

// hostGroup 将共享相同 host 模式的路由组织在一起。
type hostGroup struct {
	pattern hostPattern
	// methodTrees 是编译完成后不再修改的查询表，支持显式方法、HEAD 回退和 any。
	methodTrees map[string]*methodTree
}

// hostGroupBuilder 只在 Compile 内聚合同一 Host 模式的解析路由。
type hostGroupBuilder struct {
	pattern hostPattern
	routes  []*parsedRoute
}

// methodTree 包含特定方法的 Radix Tree。
// 空方法的树用于 "任意 Method" 匹配。
type methodTree struct {
	root *node
}

// Compile 将路由列表编译为不可变 Radix Tree。
//
// 编译过程：
//  1. 解析每条路由的 host/method/path 模式
//  2. 按 host 分组
//  3. 在每组内按 method 分组
//  4. 在每个 method 组内构建 Radix Tree（含静态边压缩）
//  5. 检测冲突（完整 specificity tuple 比较）
//  6. 冻结节点
//
// 冲突条件：两条路由在同一 host+method 组内，path 匹配集合重叠，
// 且完整 specificity tuple 相同，且 priority 相同。
func Compile(routes []CompileInput) (*Router, error) {
	if len(routes) == 0 {
		return &Router{}, nil
	}

	// 解析所有路由，并在 Router 的独立入口保证 RouteID 全局唯一。
	parsed := make([]*parsedRoute, 0, len(routes))
	routeIDs := make(map[string]int, len(routes))
	for i, r := range routes {
		if previous, exists := routeIDs[r.RouteID]; exists {
			return nil, fmt.Errorf("路由 %d: ID %q 与路由 %d 重复", i, r.RouteID, previous)
		}
		routeIDs[r.RouteID] = i
		pr, err := parseRoute(r, i)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, pr)
	}

	// builder 负责聚合解析结果；最终 hostGroup 仅保留冻结树，不保留 parsedRoute。
	builders := groupByHost(parsed)
	hostGroups := make([]*hostGroup, 0, len(builders))
	for _, builder := range builders {
		methods := make(map[string][]*parsedRoute)
		for _, pr := range builder.routes {
			methods[pr.route.method] = append(methods[pr.route.method], pr)
		}

		group := &hostGroup{
			pattern:     builder.pattern,
			methodTrees: make(map[string]*methodTree, len(methods)),
		}
		for method, methodRoutes := range methods {
			tree, err := buildMethodTree(methodRoutes)
			if err != nil {
				return nil, err
			}
			group.methodTrees[method] = &methodTree{root: tree}
		}
		hostGroups = append(hostGroups, group)
	}

	// Host specificity 是完整比较元组的第一层。相同 specificity 的模式集合互斥，
	// 仍按 kind/value 排序以提供完全确定的冻结布局和可复现 benchmark。
	sort.Slice(hostGroups, func(i, j int) bool {
		a, b := hostGroups[i].pattern, hostGroups[j].pattern
		if a.Specificity() != b.Specificity() {
			return a.Specificity() > b.Specificity()
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.value < b.value
	})

	return &Router{hostGroups: hostGroups}, nil
}

// parsedRoute 是解析后的路由。
type parsedRoute struct {
	route    *compiledRoute
	segments []pathSegment
}

func parseRoute(r CompileInput, index int) (*parsedRoute, error) {
	if strings.TrimSpace(r.RouteID) == "" {
		return nil, fmt.Errorf("路由 %d: ID 不能为空", index)
	}
	if strings.TrimSpace(r.Upstream) == "" {
		return nil, fmt.Errorf("路由 %q: upstream 不能为空", r.RouteID)
	}
	if strings.TrimSpace(r.Path) == "" {
		return nil, fmt.Errorf("路由 %q: path 不能为空", r.RouteID)
	}

	hostPat, err := ParseHostPattern(r.Host)
	if err != nil {
		return nil, fmt.Errorf("路由 %q: %v", r.RouteID, err)
	}

	segments, err := parsePathPattern(r.Path)
	if err != nil {
		return nil, fmt.Errorf("路由 %q: %v", r.RouteID, err)
	}

	method, err := NormalizeMethod(r.Method)
	if err != nil {
		return nil, fmt.Errorf("路由 %q: %v", r.RouteID, err)
	}

	// 收集参数名
	var paramNames []string
	for _, seg := range segments {
		if seg.segType == segParam || seg.segType == segCatchAll {
			paramNames = append(paramNames, seg.value)
		}
	}

	return &parsedRoute{
		route: &compiledRoute{
			routeID:      r.RouteID,
			pathTemplate: r.Path,
			upstreamID:   r.Upstream,
			preserveHost: r.PreserveHost,
			priority:     r.Priority,
			hostPattern:  hostPat,
			method:       method,
			paramNames:   paramNames,
		},
		segments: segments,
	}, nil
}

// groupByHost 将路由按 host 模式分组。
// 相同 host 模式的路由放在同一组。
func groupByHost(routes []*parsedRoute) []*hostGroupBuilder {
	groups := make(map[string]*hostGroupBuilder)
	var groupKeys []string

	for _, pr := range routes {
		key := hostGroupKey(pr.route.hostPattern)
		if g, exists := groups[key]; exists {
			g.routes = append(g.routes, pr)
		} else {
			groups[key] = &hostGroupBuilder{
				pattern: pr.route.hostPattern,
				routes:  []*parsedRoute{pr},
			}
			groupKeys = append(groupKeys, key)
		}
	}

	// 按固定顺序返回，确保不依赖插入顺序
	result := make([]*hostGroupBuilder, 0, len(groupKeys))
	for _, key := range groupKeys {
		result = append(result, groups[key])
	}
	return result
}

func hostGroupKey(p hostPattern) string {
	switch p.kind {
	case hostAny:
		return "any"
	case hostExact:
		return "exact:" + p.value
	case hostWildcard:
		return "wild:" + p.value
	default:
		return "any"
	}
}

// buildMethodTree 在同一 host+method 组内构建 Radix Tree。
func buildMethodTree(routes []*parsedRoute) (*node, error) {
	root := &buildNode{}

	// 先检测冲突
	if err := detectConflicts(routes); err != nil {
		return nil, err
	}

	// 逐条插入
	for _, pr := range routes {
		insertRoute(root, pr)
	}

	// 排序所有层级的子节点
	sortAllChildren(root)

	// 压缩静态边
	compressStaticEdges(root)

	// 压缩后重新排序（压缩可能改变子节点结构）
	sortAllChildren(root)

	return freezeNode(root), nil
}

// detectConflicts 检测同一 host+method 组内的路由冲突。
//
// 组内 Host 和 Method specificity 已相同；两个路径只有在结构模式等价、priority
// 相同时才无法确定唯一胜者。使用规范化 key 做 O(n) 检测，避免 10k 路由启动时的
// O(n²) 全量两两比较。param/catch-all 名称不进入 key，因此 /:id 与 /:name 冲突。
func detectConflicts(routes []*parsedRoute) error {
	seen := make(map[string]*parsedRoute, len(routes))
	for _, route := range routes {
		key := conflictKey(route)
		if previous, exists := seen[key]; exists {
			return fmt.Errorf("路由冲突: %q 与 %q 在相同 host+method+path 模式上重叠且优先级相同",
				previous.route.routeID, route.route.routeID)
		}
		seen[key] = route
	}
	return nil
}

// conflictKey 编码完整的路径结构和 priority。static 值使用长度前缀避免分隔符碰撞；
// param/catch-all 只编码类型，使参数名不影响匹配等价性。
func conflictKey(route *parsedRoute) string {
	var key strings.Builder
	key.Grow(len(route.segments)*4 + 16)
	key.WriteString(strconv.Itoa(route.route.priority))
	key.WriteByte('|')
	for _, segment := range route.segments {
		key.WriteByte(byte('0' + segment.segType))
		if segment.segType == segStatic {
			key.WriteString(strconv.Itoa(len(segment.value)))
			key.WriteByte(':')
			key.WriteString(segment.value)
		}
		key.WriteByte('|')
	}
	return key.String()
}

// insertRoute 将一条路由插入 Radix Tree。
func insertRoute(root *buildNode, pr *parsedRoute) {
	current := root
	for _, seg := range pr.segments {
		child := findOrCreateChild(current, seg)
		current = child
	}
	// 叶子节点添加路由，按 priority 降序插入以保持排序
	current.routes = append(current.routes, pr.route)
	sort.SliceStable(current.routes, func(i, j int) bool {
		return current.routes[i].priority > current.routes[j].priority
	})
}

// findOrCreateChild 查找或创建匹配该段的子节点。
func findOrCreateChild(parent *buildNode, seg pathSegment) *buildNode {
	for _, child := range parent.children {
		if child.segType != seg.segType {
			continue
		}
		if seg.segType == segStatic && child.prefix == seg.value {
			return child
		}
		// 参数名不参与路径结构：/:id 与 /:name 必须共享同一个 param 分支，
		// catch-all 同理。参数名保存在叶子的 compiledRoute 中，匹配成功后再绑定。
		if seg.segType == segParam || seg.segType == segCatchAll {
			return child
		}
	}
	// 创建新节点
	child := &buildNode{segType: seg.segType}
	if seg.segType == segStatic {
		child.prefix = seg.value
	}
	parent.children = append(parent.children, child)
	return child
}

// compressStaticEdges 自底向上压缩连续的 static-only 子链。
// 当前节点不是根或路由终点、且只有一个 static 子节点时，可以吸收该子节点的
// prefix、routes 和 children；子节点可以是叶子，从而把完整静态链压成一条边。
// 压缩绝不跨越 param/catch-all 或已有路由终点。
func compressStaticEdges(n *buildNode) {
	for _, child := range n.children {
		compressStaticEdges(child)
	}

	// param/catch-all 节点不能吸收 static 子边。
	if n.segType != segStatic {
		return
	}

	// 只在当前节点有一个 static 子节点且没有其他类型子节点时压缩
	// 根节点只承担容器职责，不能吸收第一条边；已有路由终点也不能跨越压缩，
	// 否则 `/a` 会被错误改写成 `/a/b` 的终点。
	if n.prefix == "" || len(n.routes) > 0 {
		return
	}
	if len(n.children) != 1 {
		return
	}
	child := n.children[0]
	if child.segType != segStatic {
		return
	}

	// n 自身不是路由终点，因此可以安全吸收唯一 static 子节点，包括子节点的
	// routes 和 grandchildren，从而把完整 static-only 链压缩为一条边。
	n.prefix = n.prefix + "/" + child.prefix
	n.routes = child.routes
	n.children = child.children
}
