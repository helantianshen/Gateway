package config

import (
	"strings"
	"testing"
	"time"
)

func validSpec() *ConfigSpec {
	return &ConfigSpec{
		APIVersion: "v1",
		Upstreams: []UpstreamSpec{{
			ID: "mock-service",
			Endpoints: []EndpointSpec{{
				ID:     "mock-1",
				URL:    "http://127.0.0.1:18080",
				Weight: 100,
			}},
		}},
		Routes: []RouteSpec{{
			ID:       "default",
			Path:     "/",
			Upstream: "mock-service",
		}},
		Policies: PolicySpec{
			RequestTimeout: "3s",
			Rate:           0,
			Burst:          0,
		},
	}
}

func TestValidate_StructuralAndSemanticRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ConfigSpec)
		want   string
	}{
		{name: "api version", mutate: func(s *ConfigSpec) { s.APIVersion = "v2" }, want: "api_version: 必须为 \"v1\""},
		{name: "no upstreams", mutate: func(s *ConfigSpec) { s.Upstreams = nil }, want: "upstreams: 至少需要配置一个 upstream"},
		{name: "no routes", mutate: func(s *ConfigSpec) { s.Routes = nil }, want: "routes: 至少需要配置一条 route"},
		{name: "empty upstream id", mutate: func(s *ConfigSpec) { s.Upstreams[0].ID = "" }, want: "upstreams[0].id: 不能为空"},
		{name: "duplicate upstream id", mutate: func(s *ConfigSpec) { s.Upstreams = append(s.Upstreams, s.Upstreams[0]) }, want: "upstreams[1].id: 与 upstreams[0].id 重复"},
		{name: "empty endpoint id", mutate: func(s *ConfigSpec) { s.Upstreams[0].Endpoints[0].ID = "" }, want: "upstreams[0].endpoints[0].id: 不能为空"},
		{name: "duplicate endpoint id", mutate: func(s *ConfigSpec) {
			s.Upstreams[0].Endpoints = append(s.Upstreams[0].Endpoints, s.Upstreams[0].Endpoints[0])
		}, want: "endpoints[1].id: 与 upstreams[0].endpoints[0].id 重复"},
		{name: "empty URL", mutate: func(s *ConfigSpec) { s.Upstreams[0].Endpoints[0].URL = "" }, want: "endpoints[0].url: 不能为空"},
		{name: "relative URL", mutate: func(s *ConfigSpec) { s.Upstreams[0].Endpoints[0].URL = "/service" }, want: "必须是包含 host 的绝对 URL"},
		{name: "missing host", mutate: func(s *ConfigSpec) { s.Upstreams[0].Endpoints[0].URL = "http:///service" }, want: "必须是包含 host 的绝对 URL"},
		{name: "unsupported URL scheme", mutate: func(s *ConfigSpec) { s.Upstreams[0].Endpoints[0].URL = "ftp://service.local" }, want: "URL 协议必须为 http 或 https"},
		{name: "zero weight", mutate: func(s *ConfigSpec) { s.Upstreams[0].Endpoints[0].Weight = 0 }, want: ".weight: 必须为正整数"},
		{name: "negative weight", mutate: func(s *ConfigSpec) { s.Upstreams[0].Endpoints[0].Weight = -1 }, want: ".weight: 必须为正整数"},
		{name: "empty route id", mutate: func(s *ConfigSpec) { s.Routes[0].ID = "" }, want: "routes[0].id: 不能为空"},
		{name: "duplicate route id", mutate: func(s *ConfigSpec) { s.Routes = append(s.Routes, s.Routes[0]) }, want: "routes[1].id: 与 routes[0].id 重复"},
		{name: "empty route path", mutate: func(s *ConfigSpec) { s.Routes[0].Path = "" }, want: "routes[0].path: 不能为空"},
		{name: "empty route upstream", mutate: func(s *ConfigSpec) { s.Routes[0].Upstream = "" }, want: "routes[0].upstream: 不能为空"},
		{name: "unknown route upstream", mutate: func(s *ConfigSpec) { s.Routes[0].Upstream = "missing" }, want: "引用的 upstream \"missing\" 不存在"},
		{name: "empty request timeout", mutate: func(s *ConfigSpec) { s.Policies.RequestTimeout = "" }, want: "policies.request_timeout: 不能为空"},
		{name: "invalid request timeout", mutate: func(s *ConfigSpec) { s.Policies.RequestTimeout = "later" }, want: "必须是合法的 Go duration"},
		{name: "zero request timeout", mutate: func(s *ConfigSpec) { s.Policies.RequestTimeout = "0s" }, want: "必须为正数 duration"},
		{name: "negative request timeout", mutate: func(s *ConfigSpec) { s.Policies.RequestTimeout = "-1s" }, want: "必须为正数 duration"},
		{name: "negative rate", mutate: func(s *ConfigSpec) { s.Policies.Rate = -1 }, want: "policies.rate: 必须为非负整数"},
		{name: "negative burst", mutate: func(s *ConfigSpec) { s.Policies.Burst = -1 }, want: "policies.burst: 必须为非负整数"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.mutate(spec)
			err := Validate(spec, "test.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate 错误 = %v, want 包含 %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "test.yaml") {
				t.Errorf("语义错误没有文件路径: %v", err)
			}
		})
	}
}

func TestValidate_RejectsUserinfoWithoutLeakingURL(t *testing.T) {
	spec := validSpec()
	secretURL := "https://admin:super-secret@service.example/private?token=hidden"
	spec.Upstreams[0].Endpoints[0].URL = secretURL

	err := Validate(spec, "secret.yaml")
	if err == nil || !strings.Contains(err.Error(), "URL 不允许包含 userinfo") {
		t.Fatalf("userinfo 错误 = %v", err)
	}
	for _, secret := range []string{secretURL, "super-secret", "token=hidden"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("URL 校验错误泄露敏感内容 %q: %v", secret, err)
		}
	}
}

func TestValidate_RouterSyntaxAndConflicts(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ConfigSpec)
		wantErr string
	}{
		{
			name:    "路径缺少前导斜杠",
			mutate:  func(s *ConfigSpec) { s.Routes[0].Path = "users/:id" },
			wantErr: "必须以 / 开头",
		},
		{
			name:    "Host 非 ASCII",
			mutate:  func(s *ConfigSpec) { s.Routes[0].Host = "例子.example.com" },
			wantErr: "ASCII",
		},
		{
			name:    "Method 含非法字符",
			mutate:  func(s *ConfigSpec) { s.Routes[0].Method = "BAD METHOD" },
			wantErr: "非法 HTTP token",
		},
		{
			name: "相同模式冲突",
			mutate: func(s *ConfigSpec) {
				s.Routes = append(s.Routes, RouteSpec{ID: "duplicate", Path: "/", Upstream: "mock-service"})
			},
			wantErr: "路由冲突",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.mutate(spec)
			err := Validate(spec, "router.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate 错误 = %v, want 包含 %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_AcceptsMultipleRoutes(t *testing.T) {
	spec := validSpec()
	spec.Routes = append(spec.Routes,
		RouteSpec{ID: "users", Host: "api.example.com", Method: "GET", Path: "/users/:id", Upstream: "mock-service", Priority: 10, PreserveHost: true},
		RouteSpec{ID: "files", Host: "*.example.com", Path: "/files/*path", Upstream: "mock-service"},
	)
	if err := Validate(spec, "routes.yaml"); err != nil {
		t.Fatalf("合法多路由配置返回错误: %v", err)
	}
}

func TestValidate_AggregatesAllProblems(t *testing.T) {
	spec := validSpec()
	spec.APIVersion = "v9"
	spec.Upstreams[0].Endpoints[0].Weight = 0
	spec.Routes[0].Upstream = "missing"
	spec.Policies.Rate = -1
	spec.Policies.Burst = -2

	err := Validate(spec, "aggregate.yaml")
	if err == nil {
		t.Fatal("非法配置未返回错误")
	}
	for _, want := range []string{"api_version", ".weight", "upstream \"missing\" 不存在", "policies.rate", "policies.burst"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("聚合错误未包含 %q: %v", want, err)
		}
	}
}

func TestCompile_ProducesStronglyTypedConfigAndRetainsSpec(t *testing.T) {
	spec := validSpec()
	spec.Upstreams[0].Endpoints = append(spec.Upstreams[0].Endpoints,
		EndpointSpec{ID: "mock-2", URL: "http://127.0.0.1:18081", Weight: 50},
	)
	bootstrap := BootstrapConfig{
		ConfigFile:      "compile.yaml",
		PublicAddr:      ":8081",
		AdminAddr:       ":9091",
		ShutdownTimeout: 12 * time.Second,
	}

	cfg, err := Compile(spec, bootstrap)
	if err != nil {
		t.Fatalf("Compile 返回意外错误: %v", err)
	}
	upstream := cfg.Upstreams["mock-service"]
	if len(upstream.Endpoints) != 2 {
		t.Fatalf("编译 endpoint 数量 = %d, want 2", len(upstream.Endpoints))
	}
	if upstream.Endpoints[0].URL.Scheme != "http" || upstream.Endpoints[0].URL.Host != "127.0.0.1:18080" {
		t.Errorf("首个强类型 EndpointTarget 错误: %#v", upstream.Endpoints[0])
	}
	if upstream.Endpoints[1].ID != "mock-2" || upstream.Endpoints[1].Weight != 50 {
		t.Errorf("第二个 EndpointTarget 错误: %#v", upstream.Endpoints[1])
	}
	// Config target 必须与 YAML slice 解耦；修改原始 EndpointSpec 不影响已编译值
	spec.Upstreams[0].Endpoints[0].URL = "http://mutated.invalid"
	if upstream.Endpoints[0].URL.Host != "127.0.0.1:18080" {
		t.Errorf("编译 target 引用了可变 ConfigSpec: %#v", upstream.Endpoints[0])
	}
	if cfg.RequestTimeout != 3*time.Second {
		t.Errorf("RequestTimeout = %v, want 3s", cfg.RequestTimeout)
	}
	if cfg.Spec != spec {
		t.Error("Compile 未保留已校验 ConfigSpec 引用")
	}
}

func TestCompile_CannotBypassValidation(t *testing.T) {
	spec := validSpec()
	spec.Upstreams[0].Endpoints = nil
	_, err := Compile(spec, BootstrapConfig{
		ConfigFile:      "compile.yaml",
		PublicAddr:      ":8080",
		AdminAddr:       ":9090",
		ShutdownTimeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "至少需要配置一个 endpoint") {
		t.Fatalf("Compile 绕过了 endpoint 校验: %v", err)
	}
}

func TestValidateDecodedPathTemplates(t *testing.T) {
	for _, test := range []struct {
		path  string
		valid bool
	}{
		{"/用户/:id", true}, {"/100%", true}, {"/a?b#c", true},
		{"/a/../b", false}, {"/a\\b", false}, {"/a\x00b", false}, {"/\xff", false},
	} {
		spec := validSpec()
		spec.Routes[0].Path = test.path
		err := Validate(spec, "routes.yaml")
		if (err == nil) != test.valid {
			t.Errorf("Validate(%q)=%v", test.path, err)
		}
	}
}
