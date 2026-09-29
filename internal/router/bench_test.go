package router

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkMatch 分别测试不同路由规模下的命中和未命中性能
// 计时前完成编译；hit 选择最后生成的 static 分支并提取 :id，miss 验证失败路径
func BenchmarkMatch(b *testing.B) {
	for _, size := range []int{10, 1000, 10000} {
		routes := generateBenchmarkRoutes(size)
		r, err := Compile(routes)
		if err != nil {
			b.Fatalf("Compile 失败: %v", err)
		}
		cases := []struct {
			name string
			segs []string
		}{
			{name: "hit", segs: []string{fmt.Sprintf("svc%d", size-1), "123"}},
			{name: "miss", segs: []string{"missing", "123"}},
		}
		for _, tc := range cases {
			b.Run(fmt.Sprintf("routes_%d/%s", size, tc.name), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _ = r.Match("", "GET", tc.segs)
				}
			})
		}
	}
}

// BenchmarkMatchParallel 验证冻结 Router 在并发读取下的吞吐和分配特征
func BenchmarkMatchParallel(b *testing.B) {
	r, err := Compile(generateBenchmarkRoutes(1000))
	if err != nil {
		b.Fatalf("Compile 失败: %v", err)
	}
	segs := []string{"svc999", "123"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = r.Match("", "GET", segs)
		}
	})
}

var benchmarkRouter *Router

// BenchmarkCompile 测试完整 builder → 冲突检测 → 压缩 → freeze 的启动期开销
func BenchmarkCompile(b *testing.B) {
	for _, size := range []int{10, 1000, 10000} {
		b.Run(fmt.Sprintf("routes_%d", size), func(b *testing.B) {
			routes := generateBenchmarkRoutes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				compiled, err := Compile(routes)
				if err != nil {
					b.Fatalf("Compile 失败: %v", err)
				}
				benchmarkRouter = compiled
			}
		})
	}
}

// BenchmarkMatchCatchAll 测试 catch-all 路由的匹配性能
func BenchmarkMatchCatchAll(b *testing.B) {
	routes := []CompileInput{
		{RouteID: "r1", Method: "GET", Path: "/files/*path", Upstream: "mock"},
	}
	r, err := Compile(routes)
	if err != nil {
		b.Fatalf("Compile 失败: %v", err)
	}

	segs := []string{"files", "a", "b", "c", "d", "e"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Match("", "GET", segs)
	}
}

// BenchmarkMatchHighFanOut 测试高 fan-out 场景（根节点大量子节点）
func BenchmarkMatchHighFanOut(b *testing.B) {
	routes := make([]CompileInput, 100)
	for i := 0; i < 100; i++ {
		routes[i] = CompileInput{
			RouteID:  fmt.Sprintf("r%d", i),
			Method:   "GET",
			Path:     fmt.Sprintf("/svc%d/:id", i),
			Upstream: "mock",
		}
	}
	r, err := Compile(routes)
	if err != nil {
		b.Fatalf("Compile 失败: %v", err)
	}

	segs := []string{"svc99", "123"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Match("", "GET", segs)
	}
}

// generateBenchmarkRoutes 生成指定数量的不冲突路由
func generateBenchmarkRoutes(count int) []CompileInput {
	routes := make([]CompileInput, count)
	for i := 0; i < count; i++ {
		routes[i] = CompileInput{
			RouteID:  fmt.Sprintf("r%d", i),
			Method:   "GET",
			Path:     fmt.Sprintf("/svc%d/:id", i),
			Upstream: "mock",
		}
	}
	return routes
}

// BenchmarkHostSelector 将 Host 数量与 Path 数量分开，最末 exact 可暴露线性扫描成本
// wildcard 使用不同后缀；fallback 有 any 路由，miss 没有 any 路由
func BenchmarkHostSelector(b *testing.B) {
	for _, size := range []int{10, 100, 1000, 10000} {
		for _, scenario := range []string{"exact", "wildcard", "fallback", "miss"} {
			b.Run(fmt.Sprintf("hosts_%d/%s", size, scenario), func(b *testing.B) {
				routes := make([]CompileInput, 0, size+1)
				for i := range size {
					host := fmt.Sprintf("h%05d.example.com", i)
					if scenario == "wildcard" {
						host = fmt.Sprintf("*.h%05d.example.com", i)
					}
					routes = append(routes, CompileInput{RouteID: host, Host: host, Method: "GET", Path: "/users/:id", Upstream: "mock"})
				}
				host := fmt.Sprintf("h%05d.example.com", size-1)
				if scenario == "wildcard" {
					host = "api." + host
				}
				if scenario == "fallback" || scenario == "miss" {
					host = "unmatched.example.org"
				}
				if scenario == "fallback" {
					routes = append(routes, CompileInput{RouteID: "any", Method: "GET", Path: "/users/:id", Upstream: "mock"})
				}
				benchmarkMatchRequest(b, routes, host, "GET", []string{"users", "42"}, scenario != "miss")
			})
		}
	}
}

// BenchmarkRouteMatrix 覆盖同一规模下的静态、动态、深路径与 Method 分支
func BenchmarkRouteMatrix(b *testing.B) {
	for _, size := range []int{10, 1000, 10000} {
		for _, test := range []struct {
			name, pattern, path, routeMethod, requestMethod string
			hit                                             bool
		}{
			{"static", "/api/v1/users/profile", "/api/v1/users/profile", "GET", "GET", true},
			{"param", "/api/:tenant/users/:user", "/api/acme/users/42", "GET", "GET", true},
			{"multi_param", "/api/:tenant/users/:user/orders/:order", "/api/acme/users/42/orders/7", "GET", "GET", true},
			{"catch_all", "/static/*path", "/static/a/b/c", "GET", "GET", true},
			{"fan_out", "/target", "/target", "GET", "GET", true},
			{"depth_5", "/a/b/c/d/e", "/a/b/c/d/e", "GET", "GET", true},
			{"depth_10", "/a/b/c/d/e/f/g/h/i/j", "/a/b/c/d/e/f/g/h/i/j", "GET", "GET", true},
			{"depth_20", "/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t", "/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t", "GET", "GET", true},
			{"post", "/users/:id", "/users/42", "POST", "POST", true},
			{"head_fallback", "/users/:id", "/users/42", "GET", "HEAD", true},
			{"any_method", "/users/:id", "/users/42", "", "PATCH", true},
			{"method_miss", "/users/:id", "/users/42", "GET", "POST", false},
		} {
			b.Run(fmt.Sprintf("routes_%d/%s", size, test.name), func(b *testing.B) {
				routes := make([]CompileInput, 0, size)
				for i := 0; i < size-1; i++ {
					routes = append(routes, CompileInput{RouteID: fmt.Sprint(i), Method: test.routeMethod, Path: fmt.Sprintf("/svc%d", i), Upstream: "mock"})
				}
				routes = append(routes, CompileInput{RouteID: "target", Method: test.routeMethod, Path: test.pattern, Upstream: "mock"})
				benchmarkMatchRequest(b, routes, "api.example.com", test.requestMethod, strings.Split(test.path[1:], "/"), test.hit)
			})
		}
	}
}

func benchmarkMatchRequest(b *testing.B, routes []CompileInput, host, method string, segments []string, hit bool) {
	b.Helper()
	r, err := Compile(routes)
	if err != nil {
		b.Fatal(err)
	}
	if result, _ := r.Match(host, method, segments); (result != nil) != hit {
		b.Fatal("基准场景未得到预期匹配结果")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Match(host, method, segments)
	}
}
