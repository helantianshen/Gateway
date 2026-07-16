package router

import "testing"

func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "空", input: "", want: ""},
		{name: "简单", input: "api.example.com", want: "api.example.com"},
		{name: "大写转小写", input: "API.Example.COM", want: "api.example.com"},
		{name: "带端口", input: "api.example.com:8080", want: "api.example.com"},
		{name: "尾点", input: "api.example.com.", want: "api.example.com"},
		{name: "大写带端口尾点", input: "API.Example.COM.:9090", want: "api.example.com"},
		{name: "IPv6 无端口带括号", input: "[2001:DB8::1]", want: "2001:db8::1"},
		{name: "IPv6 带端口", input: "[2001:DB8::1]:8443", want: "2001:db8::1"},
		{name: "IPv6 无括号无端口", input: "2001:DB8::1", want: "2001:db8::1"},
		{name: "非法端口归一化为空", input: "api.example.com:bad", want: ""},
		{name: "多个尾点归一化为空", input: "api.example.com..", want: ""},
		{name: "非法 DNS 字符归一化为空", input: "foo_bar.example.com", want: ""},
		{name: "label 前连字符归一化为空", input: "-bad.example.com", want: ""},
		{name: "label 后连字符归一化为空", input: "bad-.example.com", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeHost(tt.input); got != tt.want {
				t.Errorf("NormalizeHost(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseHostPattern(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		kind    hostKind
		value   string
		wantErr bool
	}{
		{name: "空 host", input: "", kind: hostAny},
		{name: "精确 host", input: "api.example.com", kind: hostExact, value: "api.example.com"},
		{name: "精确 host 带合法端口", input: "api.example.com:443", kind: hostExact, value: "api.example.com"},
		{name: "通配 host", input: "*.example.com", kind: hostWildcard, value: "example.com"},
		{name: "通配 host 带端口", input: "*.example.com:8080", kind: hostWildcard, value: "example.com"},
		{name: "通配 host 大写", input: "*.Example.COM", kind: hostWildcard, value: "example.com"},
		{name: "IPv4", input: "127.0.0.1", kind: hostExact, value: "127.0.0.1"},
		{name: "IPv6 括号", input: "[2001:DB8::1]", kind: hostExact, value: "2001:db8::1"},
		{name: "IPv6 括号端口", input: "[2001:DB8::1]:443", kind: hostExact, value: "2001:db8::1"},
		{name: "非法后缀通配", input: "example.*", wantErr: true},
		{name: "非法通配后缀为空", input: "*.", wantErr: true},
		{name: "非法非 ASCII", input: "例子.example.com", wantErr: true},
		{name: "非法多个尾点", input: "api.example.com..", wantErr: true},
		{name: "非法空 label", input: "api..example.com", wantErr: true},
		{name: "非法字符", input: "api_example.com", wantErr: true},
		{name: "非法端口为空", input: "api.example.com:", wantErr: true},
		{name: "非法端口文本", input: "api.example.com:https", wantErr: true},
		{name: "非法端口零", input: "api.example.com:0", wantErr: true},
		{name: "非法端口越界", input: "api.example.com:65536", wantErr: true},
		{name: "非法 IPv6 括号", input: "[2001:db8::1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParseHostPattern(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("期望错误，但返回 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if p.kind != tt.kind {
				t.Errorf("kind = %d, want %d", p.kind, tt.kind)
			}
			if p.value != tt.value {
				t.Errorf("value = %q, want %q", p.value, tt.value)
			}
		})
	}
}

func TestMatchHost(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		request string
		want    bool
	}{
		{name: "any 匹配所有", pattern: "", request: "anything.com", want: true},
		{name: "精确匹配", pattern: "api.example.com", request: "api.example.com", want: true},
		{name: "精确不匹配", pattern: "api.example.com", request: "other.com", want: false},
		{name: "通配匹配单层", pattern: "*.example.com", request: "a.example.com", want: true},
		{name: "通配不匹配无前缀", pattern: "*.example.com", request: "example.com", want: false},
		{name: "通配不匹配多层", pattern: "*.example.com", request: "a.b.example.com", want: false},
		{name: "通配不匹配其他域名", pattern: "*.example.com", request: "a.other.com", want: false},
		{name: "通配不匹配非法字符", pattern: "*.example.com", request: "foo_bar.example.com", want: false},
		{name: "通配不匹配 label 前连字符", pattern: "*.example.com", request: "-bad.example.com", want: false},
		{name: "通配不匹配 label 后连字符", pattern: "*.example.com", request: "bad-.example.com", want: false},
		{name: "any 仍接收非法 host", pattern: "", request: "foo_bar.example.com", want: true},
		{name: "大小写归一化后匹配", pattern: "api.example.com", request: "API.Example.com", want: true},
		{name: "端口剥离后匹配", pattern: "api.example.com", request: "api.example.com:8080", want: true},
		{name: "尾点剥离后匹配", pattern: "api.example.com", request: "api.example.com.", want: true},
		{name: "IPv6 端口归一化", pattern: "[2001:db8::1]", request: "[2001:DB8::1]:8080", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParseHostPattern(tt.pattern)
			if err != nil {
				t.Fatalf("ParseHostPattern 错误: %v", err)
			}
			if got := p.MatchHost(NormalizeHost(tt.request)); got != tt.want {
				t.Errorf("MatchHost(%q, %q) = %v, want %v", tt.pattern, tt.request, got, tt.want)
			}
		})
	}
}
