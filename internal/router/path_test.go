package router

import (
	"net/http"
	"net/url"
	"testing"
)

// newRequest 构造一个 HTTP 请求，正确设置 Path 和 RawPath
// 使用 url.ParseRequestURI 确保 EscapedPath() 返回正确的编码路径
func newRequest(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	u, err := url.ParseRequestURI(rawurl)
	if err != nil {
		t.Fatalf("解析 URL %q 失败: %v", rawurl, err)
	}
	return &http.Request{URL: u}
}

func TestParsePath_ValidPaths(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantSeg []string
	}{
		{name: "根路径", url: "http://example.com/", wantSeg: nil},
		{name: "单段", url: "http://example.com/users", wantSeg: []string{"users"}},
		{name: "多段", url: "http://example.com/api/v1/users", wantSeg: []string{"api", "v1", "users"}},
		{name: "带中文", url: "http://example.com/%E7%94%A8%E6%88%B7", wantSeg: []string{"用户"}},
		{name: "带空格编码", url: "http://example.com/hello%20world", wantSeg: []string{"hello world"}},
		{name: "尾斜杠", url: "http://example.com/users/", wantSeg: []string{"users", ""}},
		{name: "重复斜杠不合并", url: "http://example.com/a//b", wantSeg: []string{"a", "", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(t, tt.url)
			segs, err := ParsePath(req)
			if err != nil {
				t.Fatalf("ParsePath 错误: %v", err)
			}
			if len(segs) != len(tt.wantSeg) {
				t.Fatalf("段数 = %d, want %d, segs=%v", len(segs), len(tt.wantSeg), segs)
			}
			for i, want := range tt.wantSeg {
				if segs[i] != want {
					t.Errorf("段 %d = %q, want %q", i, segs[i], want)
				}
			}
		})
	}
}

func TestParsePath_IllegalPaths(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "编码斜杠 %2F", url: "http://example.com/api%2Fv1"},
		{name: "编码斜杠小写 %2f", url: "http://example.com/api%2fv1"},
		{name: "编码反斜杠 %5C", url: "http://example.com/api%5Cv1"},
		{name: "编码反斜杠小写 %5c", url: "http://example.com/api%5cv1"},
		{name: "dot segment", url: "http://example.com/api/./users"},
		{name: "double dot segment", url: "http://example.com/api/../users"},
		{name: "编码 dot segment %2e", url: "http://example.com/api/%2e/users"},
		{name: "编码 double dot %2e%2e", url: "http://example.com/api/%2e%2e/users"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(t, tt.url)
			_, err := ParsePath(req)
			if err == nil {
				t.Fatal("期望非法路径错误，但返回 nil")
			}
			if err.Code != MatchErrIllegalPath {
				t.Errorf("错误码 = %d, want %d", err.Code, MatchErrIllegalPath)
			}
		})
	}
}
