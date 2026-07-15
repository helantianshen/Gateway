package router

import (
	"fmt"
	"sort"
	"strings"
)

// buildNode 是编译阶段唯一可变的树节点。
// 插入、排序和静态边压缩只允许操作该类型；freezeNode 完成后 builder 会被丢弃。
type buildNode struct {
	prefix   string
	segType  segmentType
	children []*buildNode
	routes   []*compiledRoute
}

// node 是请求热路径只读访问的冻结 Radix 节点。
//
// 它与 buildNode 使用不同类型，编译器因此能阻止匹配代码意外调用构建期修改函数。
// freezeNode 会递归复制 children、compiledRoute 和 paramNames，最终树不引用 builder、
// CompileInput 或任何由调用方持有的可变 map/slice。
type node struct {
	// prefix 对 static 边是压缩后的多个路径段（如 "users/profile"）；
	// 对 param/catchAll 边为空。参数名属于具体路由，不属于共享结构节点。
	prefix  string
	segType segmentType

	// children 已按 static > param > catchAll、同类字典序冻结。
	children []*node

	// routes 按 priority 降序冻结为值切片；同模式不同 priority 可共存。
	routes []compiledRoute
}

// pathSegment 表示解析后的路径模式段。
type pathSegment struct {
	value   string // static 段的值或 param/catchAll 的参数名
	segType segmentType
}

// parsePathPattern 解析路由配置中的 path 模式字符串。
//
// 模式按 / 分段，每段可能是：
//   - static：普通字符串，如 users
//   - param：以 : 开头，如 :id
//   - catchAll：以 * 开头，如 *path
//
// 参数名约束：
//   - 参数名必须非空、只含 [a-zA-Z][a-zA-Z0-9_]*
//   - 同一路径模式中参数名不可重复
//   - catch-all 只能出现在最后一段
//   - catch-all 之后不能有更多段
func parsePathPattern(pattern string) ([]pathSegment, error) {
	if !strings.HasPrefix(pattern, "/") {
		return nil, fmt.Errorf("路径模式必须以 / 开头")
	}
	// 路由模式统一使用绝对路径；移除唯一的前导 / 后再按段解析。
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return nil, nil // 根路径
	}

	rawSegments := strings.Split(pattern, "/")
	segments := make([]pathSegment, 0, len(rawSegments))
	paramNames := make(map[string]bool)

	for i, raw := range rawSegments {
		if raw == "" {
			return nil, fmt.Errorf("路径模式段 %d 为空（重复斜杠）", i)
		}

		switch {
		case strings.HasPrefix(raw, ":"):
			name := raw[1:]
			if name == "" {
				return nil, fmt.Errorf("路径模式段 %d: 参数名不能为空", i)
			}
			if !isValidParamName(name) {
				return nil, fmt.Errorf("路径模式段 %d: 参数名 %q 不合法（必须以字母开头，只含字母数字下划线）", i, name)
			}
			if paramNames[name] {
				return nil, fmt.Errorf("路径模式段 %d: 参数名 %q 重复", i, name)
			}
			paramNames[name] = true
			segments = append(segments, pathSegment{value: name, segType: segParam})

		case strings.HasPrefix(raw, "*"):
			name := raw[1:]
			if name == "" {
				return nil, fmt.Errorf("路径模式段 %d: catch-all 参数名不能为空", i)
			}
			if !isValidParamName(name) {
				return nil, fmt.Errorf("路径模式段 %d: catch-all 参数名 %q 不合法", i, name)
			}
			if i != len(rawSegments)-1 {
				return nil, fmt.Errorf("catch-all 段 %q 必须是路径的最后一段", name)
			}
			if paramNames[name] {
				return nil, fmt.Errorf("catch-all 参数名 %q 重复", name)
			}
			paramNames[name] = true
			segments = append(segments, pathSegment{value: name, segType: segCatchAll})

		default:
			segments = append(segments, pathSegment{value: raw, segType: segStatic})
		}
	}

	return segments, nil
}

func isValidParamName(name string) bool {
	if len(name) == 0 {
		return false
	}
	if !isAlpha(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isAlphaNumUnderscore(name[i]) {
			return false
		}
	}
	return true
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isAlphaNumUnderscore(c byte) bool {
	return isAlpha(c) || (c >= '0' && c <= '9') || c == '_'
}

// sortAllChildren 递归排序 builder 的所有层级，确保匹配结果不依赖插入顺序。
func sortAllChildren(n *buildNode) {
	if n == nil {
		return
	}
	sortChildren(n.children)
	for _, child := range n.children {
		sortAllChildren(child)
	}
}

// sortChildren 固定为 static > param > catchAll，同类型按 prefix 字典序。
func sortChildren(children []*buildNode) {
	sort.SliceStable(children, func(i, j int) bool {
		a, b := children[i], children[j]
		if a.segType != b.segType {
			return a.segType < b.segType // static(0) < param(1) < catchAll(2)
		}
		return a.prefix < b.prefix
	})
}

// freezeNode 把完成压缩的 builder 深拷贝为请求侧只读节点。
func freezeNode(source *buildNode) *node {
	if source == nil {
		return nil
	}
	frozen := &node{
		prefix:   source.prefix,
		segType:  source.segType,
		children: make([]*node, len(source.children)),
		routes:   make([]compiledRoute, len(source.routes)),
	}
	for i, child := range source.children {
		frozen.children[i] = freezeNode(child)
	}
	for i, route := range source.routes {
		frozen.routes[i] = *route
		frozen.routes[i].paramNames = append([]string(nil), route.paramNames...)
	}
	return frozen
}
