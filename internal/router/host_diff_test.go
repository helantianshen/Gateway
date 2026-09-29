package router_test

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/helantianshen/gateway/internal/router"
)

// TestDiff_HostSelector 比较多 Host、跨层回退和参数回溯，参考实现仍逐条扫描声明
func TestDiff_HostSelector(t *testing.T) {
	rng := rand.New(rand.NewSource(20260929))
	routes := []router.CompileInput{{RouteID: "any", Method: "GET", Path: "/fallback/:id", Upstream: "fallback"}}
	for i := range 100 {
		for j, host := range []string{fmt.Sprintf("api.h%d.test", i), fmt.Sprintf("*.h%d.test", i)} {
			for k, path := range []string{"/users/:id", "/files/*path", "/a/:first/end/:last", "/a/:other/*rest"} {
				routes = append(routes, router.CompileInput{RouteID: fmt.Sprintf("%d-%d-%d", i, j, k), Host: host, Method: []string{"GET", "POST", "HEAD", ""}[rng.Intn(4)], Path: path, Upstream: fmt.Sprintf("up%d", i), PreserveHost: i%2 == 0})
			}
		}
	}
	for _, config := range [][]router.CompileInput{routes, shuffleRoutes(rng, routes)} {
		radix, err := router.Compile(config)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := newReferenceMatcher(config)
		if err != nil {
			t.Fatal(err)
		}
		for range 3000 {
			host := fmt.Sprintf("%s.h%d.test", []string{"api", "foo", "a.b"}[rng.Intn(3)], rng.Intn(110))
			paths := [][]string{{"users", "42"}, {"files"}, {"files", ""}, {"files", "a", "b"}, {"a", "1", "end", "2"}, {"a", "1", "end", "2", "extra"}, {"fallback", "7"}, {"missing"}}
			path := paths[rng.Intn(len(paths))]
			method := []string{"GET", "HEAD", "POST", "OPTIONS", "PATCH"}[rng.Intn(5)]
			got, gotErr := radix.Match(host, method, path)
			want, err := ref.Match(host, method, path)
			assertReferenceError(t, gotErr, err)
			assertReferenceResult(t, got, want, host, method, path)
		}
	}
}

func assertReferenceResult(t *testing.T, got *router.MatchResult, want *referenceResult, host, method string, path []string) {
	t.Helper()
	if got == nil || want == nil {
		if (got == nil) != (want == nil) {
			t.Fatalf("host=%q method=%q path=%v: got=%+v want=%+v", host, method, path, got, want)
		}
		return
	}
	if got.RouteID != want.RouteID || got.PathTemplate != want.PathTemplate || got.UpstreamID != want.UpstreamID || got.PreserveHost != want.PreserveHost || len(got.Params) != len(want.Params) || (len(got.Params) > 0 && !reflect.DeepEqual(got.Params, want.Params)) {
		t.Fatalf("host=%q method=%q path=%v: got=%+v want=%+v", host, method, path, got, want)
	}
}

func TestDiff_IllegalHostCannotReachAnyRoute(t *testing.T) {
	routes := []router.CompileInput{{RouteID: "any", Path: "/*path", Upstream: "mock"}}
	radix, err := router.Compile(routes)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := newReferenceMatcher(routes)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"foo_bar.example.com", "api.example.com:bad", "api.example.com..", "-bad.test", "bad-.test", "a..test", "[::1", "example.com:0", "example.com:65536", "user@host", "a b", "例子.test", "\x00", ".", "[no-ip]"} {
		t.Run(host, func(t *testing.T) {
			got, gotErr := radix.Match(host, "GET", []string{"users"})
			want, wantErr := ref.Match(host, "GET", []string{"users"})
			if got != nil || want != nil || gotErr == nil || gotErr.Code != router.MatchErrIllegalHost || wantErr == nil {
				t.Fatalf("非法 Host: got=%+v/%v ref=%+v/%v", got, gotErr, want, wantErr)
			}
		})
	}
}

func assertReferenceError(t *testing.T, got *router.MatchError, want error) {
	t.Helper()
	if got == nil || want == nil {
		if (got == nil) != (want == nil) {
			t.Fatalf("错误不同: got=%v want=%v", got, want)
		}
		return
	}
	ref, ok := want.(*referenceError)
	if !ok {
		t.Fatal(want)
	}
	code := map[string]router.MatchErrorCode{"host": router.MatchErrIllegalHost, "method": router.MatchErrMethodNotAllowed, "no_route": router.MatchErrNoRoute}[ref.kind]
	if got.Code != code || !reflect.DeepEqual(got.AllowedMethods, ref.allowed) {
		t.Fatalf("错误不同: got=%+v want=%+v", got, ref)
	}
}

func TestDiffHostIndexIPAndFallback(t *testing.T) {
	routes := []router.CompileInput{
		{RouteID: "ipv4", Host: "127.0.0.1", Path: "/ip", Upstream: "ip"},
		{RouteID: "ipv6", Host: "[2001:db8::1]", Path: "/ip", Upstream: "ip"},
		{RouteID: "exact", Host: "api.example.com", Method: "POST", Path: "/other", Upstream: "exact"},
		{RouteID: "wild", Host: "*.example.com", Method: "GET", Path: "/users/:id", Upstream: "wild"},
		{RouteID: "any", Path: "/fallback", Upstream: "any"},
	}
	radix, err := router.Compile(routes)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := newReferenceMatcher(routes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ host, path, id string }{
		{"127.0.0.1:8080", "ip", "ipv4"}, {"[2001:DB8::1]:443", "ip", "ipv6"}, {"2001:DB8::1", "ip", "ipv6"},
		{"API.Example.COM.:80", "users/42", "wild"}, {"a.b.example.com", "users/42", ""},
		{"api.example.com", "fallback", "any"}, {"", "fallback", "any"},
	} {
		path := strings.Split(tc.path, "/")
		got, gotErr := radix.Match(tc.host, "GET", path)
		want, wantErr := ref.Match(tc.host, "GET", path)
		assertReferenceResult(t, got, want, tc.host, "GET", path)
		assertReferenceError(t, gotErr, wantErr)
		if tc.id != "" && (got == nil || got.RouteID != tc.id) {
			t.Fatalf("%+v: %+v/%v", tc, got, gotErr)
		}
		if tc.id == "" && got != nil {
			t.Fatal("多层子域被单层 wildcard 接受")
		}
	}
}
