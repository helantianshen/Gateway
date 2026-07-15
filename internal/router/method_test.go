package router

import "testing"

func TestMatchMethod(t *testing.T) {
	tests := []struct {
		name          string
		routeMethod   string
		requestMethod string
		want          bool
	}{
		{name: "精确匹配", routeMethod: "GET", requestMethod: "GET", want: true},
		{name: "精确不匹配", routeMethod: "GET", requestMethod: "POST", want: false},
		{name: "任意方法匹配 GET", routeMethod: "", requestMethod: "GET", want: true},
		{name: "任意方法匹配 POST", routeMethod: "", requestMethod: "POST", want: true},
		{name: "任意方法匹配 HEAD", routeMethod: "", requestMethod: "HEAD", want: true},
		{name: "大小写敏感", routeMethod: "GET", requestMethod: "get", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchMethod(tt.routeMethod, tt.requestMethod); got != tt.want {
				t.Errorf("MatchMethod(%q, %q) = %v, want %v", tt.routeMethod, tt.requestMethod, got, tt.want)
			}
		})
	}
}

func TestNormalizeMethod(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "任意方法", raw: "", want: ""},
		{name: "标准方法转大写", raw: "get", want: "GET"},
		{name: "自定义 token", raw: "m-search", want: "M-SEARCH"},
		{name: "允许外围空白", raw: "  post  ", want: "POST"},
		{name: "拒绝内部空格", raw: "BAD METHOD", wantErr: true},
		{name: "拒绝斜杠", raw: "GET/POST", wantErr: true},
		{name: "拒绝非 ASCII", raw: "读取", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeMethod(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeMethod(%q) error = %v, wantErr=%v", tt.raw, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NormalizeMethod(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestResolveMethod(t *testing.T) {
	tests := []struct {
		name          string
		requestMethod string
		want          []string
	}{
		{name: "HEAD 固定三级查找", requestMethod: "HEAD", want: []string{"HEAD", "GET", ""}},
		{name: "GET 直接查找", requestMethod: "GET", want: []string{"GET", ""}},
		{name: "POST 直接查找", requestMethod: "POST", want: []string{"POST", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveMethod(tt.requestMethod)
			if len(got) != len(tt.want) {
				t.Fatalf("ResolveMethod 返回 %d 个方法, want %d, got=%v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				if got[i] != want {
					t.Errorf("方法 %d = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}
