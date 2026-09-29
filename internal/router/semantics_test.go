package router

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestPathBoundaries(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		hit           bool
		params        map[string]string
	}{
		{"/files/*path", "/files", true, map[string]string{"path": ""}},
		{"/files/*path", "/files/", true, map[string]string{"path": ""}},
		{"/files/*path", "/files/a", true, map[string]string{"path": "a"}},
		{"/files/*path", "/files/a/b", true, map[string]string{"path": "a/b"}},
		{"/files/*path", "/files//a/", true, map[string]string{"path": "/a/"}},
		{"/users", "/users", true, nil}, {"/users", "/users/", false, nil},
		{"/a/b", "/a//b", false, nil}, {"/a/:id", "/a/", false, nil},
		{"/用户/:id", "/%E7%94%A8%E6%88%B7/42", true, map[string]string{"id": "42"}},
		{"/100%", "/100%25", true, nil}, {"/a?b#c", "/a%3Fb%23c", true, nil},
		{"/literal%2F", "/literal%252F", true, nil},
		{"/%E7%94%A8%E6%88%B7", "/%E7%94%A8%E6%88%B7", false, nil},
		{"/", "/", true, nil}, {"/", "//", false, nil},
	} {
		t.Run(tc.pattern+" "+tc.path, func(t *testing.T) {
			r, err := Compile([]CompileInput{{RouteID: "r", Path: tc.pattern, Upstream: "mock"}})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.ParseRequestURI(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			segments, pathErr := ParsePath(&http.Request{URL: parsed})
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			got, matchErr := r.Match("api.test", "GET", segments)
			if !tc.hit {
				if got != nil || matchErr == nil || matchErr.Code != MatchErrNoRoute {
					t.Fatalf("不应命中: %+v/%v", got, matchErr)
				}
				return
			}
			if got == nil || matchErr != nil || !reflect.DeepEqual(got.Params, tc.params) {
				t.Fatalf("匹配=%+v/%v, want %v", got, matchErr, tc.params)
			}
		})
	}
}

func TestPathPatternRejectsUnreachableAndControlSegments(t *testing.T) {
	for _, pattern := range []string{"/.", "/a/../b", "/a\\b", "/\xff", "/a\x00b", "/a\nb", "/a\u0085b", "/a//b", "/users/"} {
		if _, err := Compile([]CompileInput{{RouteID: "r", Path: pattern, Upstream: "mock"}}); err == nil {
			t.Errorf("Compile 接受非法模板 %q", pattern)
		}
	}
	for _, path := range []string{"/a%00b", "/a%0Ab", "/a%C2%85b", "/a%2Fb", "/a%5Cb", "/%2e%2e", "/%FF"} {
		parsed, err := url.ParseRequestURI(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParsePath(&http.Request{URL: parsed}); err == nil || err.Code != MatchErrIllegalPath {
			t.Errorf("ParsePath 接受非法路径 %q", path)
		}
	}
}

// TestParamBacktrackingAndResultOwnership 验证失败分支不会污染后继捕获，结果也不共享可变 map
func TestParamBacktrackingAndResultOwnership(t *testing.T) {
	r, err := Compile([]CompileInput{
		{RouteID: "deep", Path: "/a/:x/b/:y/end", Upstream: "deep"},
		{RouteID: "fallback", Path: "/a/:name/*rest", Upstream: "fallback"},
		{RouteID: "exact", Path: "/files", Upstream: "exact"},
		{RouteID: "catch", Path: "/files/*path", Upstream: "catch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, id string
		params   map[string]string
	}{
		{"/a/1/b/2/end", "deep", map[string]string{"x": "1", "y": "2"}},
		{"/a/1/b/2/missing", "fallback", map[string]string{"name": "1", "rest": "b/2/missing"}},
		{"/files", "exact", nil}, {"/files/", "catch", map[string]string{"path": ""}},
	} {
		got, err := r.Match("", "GET", splitPath(tc.path))
		if err != nil || got == nil || got.RouteID != tc.id || !reflect.DeepEqual(got.Params, tc.params) {
			t.Fatalf("%s=%+v/%v", tc.path, got, err)
		}
		if got.Params != nil {
			got.Params["mutated"] = "yes"
		}
		again, err := r.Match("", "GET", splitPath(tc.path))
		if err != nil || !reflect.DeepEqual(again.Params, tc.params) {
			t.Fatal("匹配结果共享可变参数")
		}
	}
}

func TestManyParamsAndBacktracking(t *testing.T) {
	var pattern, path []string
	want := map[string]string{}
	for i := range 20 {
		key, value := fmt.Sprintf("p%d", i), fmt.Sprintf("v%d", i)
		pattern = append(pattern, ":"+key)
		path = append(path, value)
		want[key] = value
	}
	base := "/" + strings.Join(pattern, "/")
	r, err := Compile([]CompileInput{
		{RouteID: "deep", Path: base + "/end", Upstream: "deep"},
		{RouteID: "catch", Path: base + "/*rest", Upstream: "catch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, matchErr := r.Match("", "GET", append(path, "end"))
	if matchErr != nil || result.RouteID != "deep" || !reflect.DeepEqual(result.Params, want) {
		t.Fatalf("深参数: %+v/%v", result, matchErr)
	}
	want["rest"] = "missing/tail"
	result, matchErr = r.Match("", "GET", append(path, "missing", "tail"))
	if matchErr != nil || result.RouteID != "catch" || !reflect.DeepEqual(result.Params, want) {
		t.Fatalf("深参数回溯: %+v/%v", result, matchErr)
	}
}
