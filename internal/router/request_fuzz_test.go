package router_test

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/helantianshen/gateway/internal/router"
)

// FuzzRequestRoutingDifferential 同时覆盖原始编码路径、Host 错误与方法回退
// 参考路径解析按解码后的单段检查分隔符，不调用生产侧任何解析或匹配函数
func FuzzRequestRoutingDifferential(f *testing.F) {
	routes := []router.CompileInput{
		{RouteID: "exact", Host: "api.example.com", Method: "GET", Path: "/users/:id", Upstream: "exact", PreserveHost: true},
		{RouteID: "wild", Host: "*.example.com", Method: "POST", Path: "/users/:name", Upstream: "wild"},
		{RouteID: "any", Method: "PUT", Path: "/users/:user", Upstream: "any"},
		{RouteID: "unicode", Path: "/用户/:id", Upstream: "unicode"},
		{RouteID: "catch", Path: "/files/*path", Upstream: "files"},
		{RouteID: "root", Method: "GET", Path: "/", Upstream: "root"},
	}
	radix, err := router.Compile(routes)
	if err != nil {
		f.Fatal(err)
	}
	ref, err := newReferenceMatcher(routes)
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][3]string{
		{"API.example.com.:443", "HEAD", "/users/42"},
		{"a.example.com", "DELETE", "/users/42"},
		{"a.b.example.com", "GET", "/users/42"},
		{"foo_bar.example.com", "GET", "/files/x"},
		{"[::1]:8080", "GET", "/files/a//b/"},
		{"", "GET", "/"}, {"api.test", "OPTIONS", "/files"},
		{"api.test", "GET", "/%E7%94%A8%E6%88%B7/42"},
		{"api.test", "GET", "/files/%2F"}, {"api.test", "GET", "/files/%252F"},
		{"api.test", "GET", "/files/%00"}, {"api.test", "GET", "/files/%FF"},
		{"api.test", "GET", "/files/%2e%2e"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, host, method, rawPath string) {
		if len(host) > 512 || len(method) > 64 || len(rawPath) > 512 || !strings.HasPrefix(rawPath, "/") {
			return
		}
		u, err := url.ParseRequestURI(rawPath)
		if err != nil {
			return
		}
		segments, pathErr := router.ParsePath(&http.Request{URL: u})
		expected, refErr := refRequestPath(u)
		if (pathErr != nil) != (refErr != nil) || (pathErr == nil && !reflect.DeepEqual(segments, expected)) {
			t.Fatalf("路径解析不一致: %q got=%v/%v ref=%v/%v", rawPath, segments, pathErr, expected, refErr)
		}
		if pathErr != nil {
			return
		}
		got, gotErr := radix.Match(host, method, segments)
		want, wantErr := ref.Match(host, method, expected)
		assertReferenceResult(t, got, want, host, method, segments)
		assertReferenceError(t, gotErr, wantErr)
	})
}

func refRequestPath(u *url.URL) ([]string, error) {
	escaped := strings.TrimPrefix(u.EscapedPath(), "/")
	parts := []string{}
	if escaped == "" {
		return parts, nil
	}
	for _, raw := range strings.Split(escaped, "/") {
		value, err := url.PathUnescape(raw)
		if err != nil || !utf8.ValidString(value) || strings.ContainsAny(value, "/\\") || value == "." || value == ".." {
			return nil, &referenceError{kind: "path"}
		}
		for _, char := range value {
			if unicode.IsControl(char) {
				return nil, &referenceError{kind: "path"}
			}
		}
		parts = append(parts, value)
	}
	return parts, nil
}
