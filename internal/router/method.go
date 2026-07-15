package router

import (
	"fmt"
	"strings"
)

// MatchMethod 检查请求方法是否直接匹配路由的方法约束。
// 空路由方法表示任意 Method；HEAD 到 GET 的回退由 ResolveMethod 统一决定。
func MatchMethod(routeMethod, requestMethod string) bool {
	return routeMethod == "" || routeMethod == requestMethod
}

// NormalizeMethod 将配置中的 Method 规范化为大写并验证 RFC HTTP token 语法。
// 空字符串是合法值，表示任意 Method。自定义 Method 也允许，但必须完全由 ASCII
// token 字符构成；这样可以在启动阶段拒绝空白、控制字符和分隔符，避免生成
// net/http 无法发送的请求方法。
func NormalizeMethod(raw string) (string, error) {
	method := strings.TrimSpace(raw)
	if method == "" {
		return "", nil
	}
	for i := 0; i < len(method); i++ {
		if !isHTTPTokenByte(method[i]) {
			return "", fmt.Errorf("method %q 包含非法 HTTP token 字符", raw)
		}
	}
	return strings.ToUpper(method), nil
}

// ResolveMethod 返回 Router 对一个请求需要依次查询的方法树。
//
// HEAD 必须固定尝试显式 HEAD、GET fallback、任意 Method。即使配置中存在 HEAD
// 路由，它也可能只覆盖另一条路径，因此不能仅凭“存在 HEAD 方法树”跳过 GET。
// 回退只改变路由选择，不改写原始请求方法，upstream 仍收到 HEAD。
func ResolveMethod(requestMethod string) []string {
	if requestMethod == "HEAD" {
		return []string{"HEAD", "GET", ""}
	}
	return []string{requestMethod, ""}
}

// MethodSpecificity 返回方法模式的静态 specificity。
// 显式 Method 高于任意 Method；HEAD 与 GET fallback 的请求态顺序由 ResolveMethod
// 中的查询次序表达，不混入配置态权重。
func MethodSpecificity(method string) int {
	if method == "" {
		return 0
	}
	return 1
}

func isHTTPTokenByte(c byte) bool {
	if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}
