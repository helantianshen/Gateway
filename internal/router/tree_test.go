package router

import (
	"sync"
	"testing"
)

func TestStaticEdgesAreFullyCompressed(t *testing.T) {
	r, err := Compile([]CompileInput{
		{RouteID: "deep", Method: "GET", Path: "/api/v1/users/profile", Upstream: "mock"},
	})
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}
	root := findMethodRoot(t, r, hostPattern{kind: hostAny}, "GET")
	if len(root.children) != 1 {
		t.Fatalf("根节点 children = %d, want 1", len(root.children))
	}
	edge := root.children[0]
	if edge.prefix != "api/v1/users/profile" {
		t.Fatalf("静态链未完整压缩: prefix=%q", edge.prefix)
	}
	if len(edge.routes) != 1 || edge.routes[0].routeID != "deep" {
		t.Fatalf("压缩边终点路由错误: %+v", edge.routes)
	}
	if len(edge.children) != 0 {
		t.Fatalf("完整静态链不应保留子节点: %d", len(edge.children))
	}
}

func TestCompressionDoesNotCrossRouteTerminal(t *testing.T) {
	r, err := Compile([]CompileInput{
		{RouteID: "short", Method: "GET", Path: "/api", Upstream: "mock"},
		{RouteID: "deep", Method: "GET", Path: "/api/v1/users", Upstream: "mock"},
	})
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}
	root := findMethodRoot(t, r, hostPattern{kind: hostAny}, "GET")
	api := root.children[0]
	if api.prefix != "api" {
		t.Fatalf("路由终点被跨越压缩: prefix=%q", api.prefix)
	}
	if len(api.routes) != 1 || api.routes[0].routeID != "short" {
		t.Fatalf("短路由终点丢失: %+v", api.routes)
	}
	if len(api.children) != 1 || api.children[0].prefix != "v1/users" {
		t.Fatalf("终点后的静态链压缩错误: %+v", api.children)
	}

	for _, tc := range []struct {
		segments []string
		wantID   string
	}{
		{segments: []string{"api"}, wantID: "short"},
		{segments: []string{"api", "v1", "users"}, wantID: "deep"},
	} {
		result, matchErr := r.Match("", "GET", tc.segments)
		if matchErr != nil || result.RouteID != tc.wantID {
			t.Fatalf("Match(%v) = (%v, %v), want %q", tc.segments, result, matchErr, tc.wantID)
		}
	}
}

func TestFreezeNodeDeepCopiesBuilderState(t *testing.T) {
	paramNames := []string{"id"}
	route := &compiledRoute{routeID: "before", upstreamID: "mock", paramNames: paramNames}
	builder := &buildNode{
		children: []*buildNode{{
			prefix:  "users",
			segType: segStatic,
			routes:  []*compiledRoute{route},
		}},
	}
	frozen := freezeNode(builder)

	builder.children[0].prefix = "mutated"
	builder.children[0].routes[0].routeID = "after"
	paramNames[0] = "changed"
	builder.children = nil

	if len(frozen.children) != 1 {
		t.Fatalf("冻结树受 builder children 修改影响: %+v", frozen.children)
	}
	got := frozen.children[0]
	if got.prefix != "users" || got.routes[0].routeID != "before" {
		t.Fatalf("冻结树引用了可变 builder: prefix=%q route=%q", got.prefix, got.routes[0].routeID)
	}
	if got.routes[0].paramNames[0] != "id" {
		t.Fatalf("冻结路由未深拷贝 paramNames: %v", got.routes[0].paramNames)
	}
}

func TestRouterConcurrentRead(t *testing.T) {
	r, err := Compile([]CompileInput{
		{RouteID: "static", Host: "api.example.com", Method: "GET", Path: "/users/profile", Upstream: "mock"},
		{RouteID: "param", Host: "*.example.com", Method: "GET", Path: "/users/:id", Upstream: "mock"},
		{RouteID: "catch", Method: "", Path: "/files/*path", Upstream: "mock"},
	})
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}

	const workers = 32
	const iterations = 200
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				result, matchErr := r.Match("API.EXAMPLE.COM:443", "HEAD", []string{"users", "profile"})
				if matchErr != nil || result == nil || result.RouteID != "static" {
					errs <- "并发匹配结果错误"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for message := range errs {
		t.Fatal(message)
	}
}

func findMethodRoot(t *testing.T, r *Router, pattern hostPattern, method string) *node {
	t.Helper()
	for _, group := range r.hostGroups {
		if group.pattern == pattern {
			tree := group.methodTrees[method]
			if tree == nil || tree.root == nil {
				t.Fatalf("未找到 method tree: host=%+v method=%q", pattern, method)
			}
			return tree.root
		}
	}
	t.Fatalf("未找到 host group: %+v", pattern)
	return nil
}
