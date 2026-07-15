package router

import (
	"strings"
	"testing"
)

// FuzzParsePathPattern 对路径模式解析做 fuzz 测试。
// 确保任意输入不会导致 panic，合法模式能正确解析，非法模式返回错误。
func FuzzParsePathPattern(f *testing.F) {
	// 添加种子用例
	f.Add("/users")
	f.Add("/users/:id")
	f.Add("/files/*path")
	f.Add("/api/v1/users")
	f.Add("")
	f.Add("/a//b")
	f.Add("/:id/:id")

	f.Fuzz(func(t *testing.T, pattern string) {
		segs, err := parsePathPattern(pattern)
		if err != nil {
			// 错误是合法的，只要不 panic
			return
		}
		// 如果解析成功，验证段不为空
		for _, seg := range segs {
			if seg.value == "" {
				t.Errorf("解析成功但段值为空: pattern=%q", pattern)
			}
		}
	})
}

// FuzzCompileAndMatch 对编译和匹配做 fuzz 测试。
// 确保任意路由配置和请求不会导致 panic。
func FuzzConflictDetection(f *testing.F) {
	f.Add("/users/:id", "/users/:name", 0, 0)
	f.Add("/users/profile", "/users/:id", 0, 0)
	f.Add("/files/*path", "/files/:id", 1, 1)

	f.Fuzz(func(t *testing.T, pathA, pathB string, priorityA, priorityB int) {
		if len(pathA) > 100 || len(pathB) > 100 {
			return
		}
		a := CompileInput{RouteID: "a", Host: "api.example.com", Method: "GET", Path: pathA, Upstream: "mock", Priority: priorityA}
		b := CompileInput{RouteID: "b", Host: "api.example.com", Method: "GET", Path: pathB, Upstream: "mock", Priority: priorityB}

		// 冲突判定必须对输入顺序对称；任意路径字符串都不得造成 panic。
		_, errAB := Compile([]CompileInput{a, b})
		_, errBA := Compile([]CompileInput{b, a})
		if (errAB == nil) != (errBA == nil) {
			t.Fatalf("交换路由顺序改变编译结果: AB=%v BA=%v", errAB, errBA)
		}
	})
}

func FuzzCompileAndMatch(f *testing.F) {
	// 添加种子用例
	f.Add("GET", "/users", "GET", "users")
	f.Add("GET", "/users/:id", "GET", "users")
	f.Add("GET", "/files/*path", "GET", "files")
	f.Add("POST", "/api/v1", "GET", "api")

	f.Fuzz(func(t *testing.T, routeMethod, routePath, reqMethod string, reqPath string) {
		// 限制输入长度，避免超长输入
		if len(routePath) > 100 || len(reqPath) > 100 {
			return
		}

		routes := []CompileInput{
			{RouteID: "r1", Method: routeMethod, Path: routePath, Upstream: "mock"},
		}

		r, err := Compile(routes)
		if err != nil {
			return // 冲突或非法配置是合法的
		}

		// 解析请求路径为段
		segs := strings.Split(reqPath, "/")
		// 去除空首段
		if len(segs) > 0 && segs[0] == "" {
			segs = segs[1:]
		}

		// 匹配不应 panic
		_, _ = r.Match("", reqMethod, segs)
	})
}
