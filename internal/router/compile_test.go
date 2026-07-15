package router

import (
	"strings"
	"testing"
)

func TestCompile_BasicRoutes(t *testing.T) {
	routes := []CompileInput{
		{RouteID: "r1", Method: "GET", Path: "/users", Upstream: "mock"},
		{RouteID: "r2", Method: "GET", Path: "/users/:id", Upstream: "mock"},
		{RouteID: "r3", Method: "POST", Path: "/users", Upstream: "mock"},
		{RouteID: "r4", Method: "GET", Path: "/files/*path", Upstream: "mock"},
	}
	r, err := Compile(routes)
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}

	tests := []struct {
		method     string
		segs       []string
		wantID     string
		wantParams map[string]string
	}{
		{method: "GET", segs: []string{"users"}, wantID: "r1"},
		{method: "GET", segs: []string{"users", "123"}, wantID: "r2", wantParams: map[string]string{"id": "123"}},
		{method: "POST", segs: []string{"users"}, wantID: "r3"},
		{method: "GET", segs: []string{"files", "a", "b", "c"}, wantID: "r4", wantParams: map[string]string{"path": "a/b/c"}},
		{method: "GET", segs: []string{"files"}, wantID: "r4", wantParams: map[string]string{"path": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.method+"_"+strings.Join(tt.segs, "/"), func(t *testing.T) {
			result, err := r.Match("", tt.method, tt.segs)
			if err != nil {
				t.Fatalf("Match 错误: %v", err)
			}
			if result.RouteID != tt.wantID {
				t.Errorf("RouteID = %q, want %q", result.RouteID, tt.wantID)
			}
			if tt.wantParams != nil {
				for k, v := range tt.wantParams {
					if result.Params[k] != v {
						t.Errorf("Params[%q] = %q, want %q", k, result.Params[k], v)
					}
				}
			}
		})
	}
}

func TestCompile_ConflictsDetected(t *testing.T) {
	tests := []struct {
		name    string
		routes  []CompileInput
		wantErr string
	}{
		{
			name: "完全相同路由冲突",
			routes: []CompileInput{
				{RouteID: "r1", Method: "GET", Path: "/users", Upstream: "mock"},
				{RouteID: "r2", Method: "GET", Path: "/users", Upstream: "mock"},
			},
			wantErr: "路由冲突",
		},
		{
			name: "参数名不同但模式相同冲突",
			routes: []CompileInput{
				{RouteID: "r1", Method: "GET", Path: "/users/:id", Upstream: "mock"},
				{RouteID: "r2", Method: "GET", Path: "/users/:name", Upstream: "mock"},
			},
			wantErr: "路由冲突",
		},
		{
			name: "不同 priority 不冲突",
			routes: []CompileInput{
				{RouteID: "r1", Method: "GET", Path: "/users", Upstream: "mock", Priority: 1},
				{RouteID: "r2", Method: "GET", Path: "/users", Upstream: "mock", Priority: 2},
			},
		},
		{
			name: "static 和 param 不冲突",
			routes: []CompileInput{
				{RouteID: "r1", Method: "GET", Path: "/users/profile", Upstream: "mock"},
				{RouteID: "r2", Method: "GET", Path: "/users/:id", Upstream: "mock"},
			},
		},
		{
			name: "不同方法不冲突",
			routes: []CompileInput{
				{RouteID: "r1", Method: "GET", Path: "/users", Upstream: "mock"},
				{RouteID: "r2", Method: "POST", Path: "/users", Upstream: "mock"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile(tt.routes)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("期望错误包含 %q, got %v", tt.wantErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("意外错误: %v", err)
				}
			}
		})
	}
}

func TestCompile_RejectsInvalidRouteSyntaxAndDuplicateID(t *testing.T) {
	tests := []struct {
		name    string
		routes  []CompileInput
		wantErr string
	}{
		{
			name:    "路径必须使用绝对形式",
			routes:  []CompileInput{{RouteID: "r1", Path: "users/:id", Upstream: "mock"}},
			wantErr: "必须以 / 开头",
		},
		{
			name:    "非法 Method token",
			routes:  []CompileInput{{RouteID: "r1", Method: "BAD METHOD", Path: "/users", Upstream: "mock"}},
			wantErr: "非法 HTTP token",
		},
		{
			name: "RouteID 全局重复",
			routes: []CompileInput{
				{RouteID: "same", Method: "GET", Path: "/users", Upstream: "mock"},
				{RouteID: "same", Method: "POST", Path: "/users", Upstream: "mock"},
			},
			wantErr: "ID \"same\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile(tt.routes)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Compile 错误 = %v, want 包含 %q", err, tt.wantErr)
			}
		})
	}
}

func TestMatch_NoRoute(t *testing.T) {
	routes := []CompileInput{
		{RouteID: "r1", Method: "GET", Path: "/users", Upstream: "mock"},
	}
	r, err := Compile(routes)
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}

	result, matchErr := r.Match("", "GET", []string{"nonexistent"})
	if result != nil {
		t.Errorf("期望 nil result, got %+v", result)
	}
	if matchErr == nil || matchErr.Code != MatchErrNoRoute {
		t.Errorf("期望 MatchErrNoRoute, got %v", matchErr)
	}
}

func TestMatch_StaticWinsOverParam(t *testing.T) {
	routes := []CompileInput{
		{RouteID: "param", Method: "GET", Path: "/users/:id", Upstream: "mock"},
		{RouteID: "static", Method: "GET", Path: "/users/profile", Upstream: "mock"},
	}
	r, err := Compile(routes)
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}

	result, matchErr := r.Match("", "GET", []string{"users", "profile"})
	if matchErr != nil {
		t.Fatalf("Match 错误: %v", matchErr)
	}
	if result.RouteID != "static" {
		t.Errorf("期望 static 路由胜出, got %q", result.RouteID)
	}
}

func TestMatch_InsertionOrderIndependence(t *testing.T) {
	routesA := []CompileInput{
		{RouteID: "r1", Method: "GET", Path: "/users/:id", Upstream: "mock"},
		{RouteID: "r2", Method: "GET", Path: "/users/profile", Upstream: "mock"},
	}
	routesB := []CompileInput{
		{RouteID: "r2", Method: "GET", Path: "/users/profile", Upstream: "mock"},
		{RouteID: "r1", Method: "GET", Path: "/users/:id", Upstream: "mock"},
	}

	rA, err := Compile(routesA)
	if err != nil {
		t.Fatalf("Compile A 错误: %v", err)
	}
	rB, err := Compile(routesB)
	if err != nil {
		t.Fatalf("Compile B 错误: %v", err)
	}

	segs := []string{"users", "profile"}
	resA, _ := rA.Match("", "GET", segs)
	resB, _ := rB.Match("", "GET", segs)

	if resA == nil || resB == nil {
		t.Fatalf("匹配返回 nil: A=%v B=%v", resA, resB)
	}
	if resA.RouteID != resB.RouteID {
		t.Errorf("插入顺序影响结果: A=%q, B=%q", resA.RouteID, resB.RouteID)
	}
}

func TestMatch_HostMatching(t *testing.T) {
	routes := []CompileInput{
		{RouteID: "r1", Host: "api.example.com", Method: "GET", Path: "/data", Upstream: "mock"},
		{RouteID: "r2", Host: "*.example.com", Method: "GET", Path: "/data", Upstream: "mock2"},
		{RouteID: "r3", Method: "GET", Path: "/data", Upstream: "mock3"},
	}
	r, err := Compile(routes)
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}

	tests := []struct {
		host   string
		wantID string
	}{
		{host: "api.example.com", wantID: "r1"},
		{host: "other.example.com", wantID: "r2"},
		{host: "different.com", wantID: "r3"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			result, matchErr := r.Match(tt.host, "GET", []string{"data"})
			if matchErr != nil {
				t.Fatalf("Match 错误: %v", matchErr)
			}
			if result.RouteID != tt.wantID {
				t.Errorf("RouteID = %q, want %q", result.RouteID, tt.wantID)
			}
		})
	}
}

func TestMatch_HeadFallback(t *testing.T) {
	routes := []CompileInput{
		{RouteID: "get", Method: "GET", Path: "/users", Upstream: "mock"},
	}
	r, err := Compile(routes)
	if err != nil {
		t.Fatalf("Compile 错误: %v", err)
	}

	// HEAD 请求应回退到 GET 路由
	result, matchErr := r.Match("", "HEAD", []string{"users"})
	if matchErr != nil {
		t.Fatalf("Match 错误: %v", matchErr)
	}
	if result.RouteID != "get" {
		t.Errorf("HEAD 回退 GET 失败: RouteID = %q, want %q", result.RouteID, "get")
	}
}
