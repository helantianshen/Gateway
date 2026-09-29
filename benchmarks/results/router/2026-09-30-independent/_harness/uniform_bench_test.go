package router

import (
	"fmt"
	"testing"
)

func BenchmarkUniformHost(b *testing.B) {
	for _, n := range []int{10, 1000, 10000} {
		for _, kind := range []string{"exact", "wildcard"} {
			b.Run(fmt.Sprintf("hosts_%d/%s", n, kind), func(b *testing.B) {
				routes := make([]CompileInput, n)
				hosts := make([]string, n)
				for i := range n {
					h := fmt.Sprintf("h%05d.example.com", i)
					hosts[i] = h
					pattern := h
					if kind == "wildcard" {
						pattern = "*." + h
						hosts[i] = "api." + h
					}
					routes[i] = CompileInput{RouteID: fmt.Sprint(i), Host: pattern, Method: "GET", Path: "/users/:id", Upstream: "mock"}
				}
				r, e := Compile(routes)
				if e != nil {
					b.Fatal(e)
				}
				for i, h := range hosts {
					m, e := r.Match(h, "GET", []string{"users", "42"})
					if e != nil || m == nil || m.RouteID != fmt.Sprint(i) || m.Params["id"] != "42" {
						b.Fatal("匹配结果错误", i)
					}
				}
				segs := []string{"users", "42"}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r.Match(hosts[i%n], "GET", segs)
				}
			})
		}
	}
}
