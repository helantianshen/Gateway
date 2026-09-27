package router

import (
	"fmt"
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
