package router

import (
	"reflect"
	"testing"
)

func TestMatchMethodAndAllow(t *testing.T) {
	routes := []CompileInput{
		{RouteID: "get", Host: "api.example.com", Method: "GET", Path: "/users/:id", Upstream: "get"},
		{RouteID: "head", Host: "api.example.com", Method: "HEAD", Path: "/users/:id", Upstream: "head"},
		{RouteID: "wild-post", Host: "*.example.com", Method: "POST", Path: "/users/:id", Upstream: "post"},
		{RouteID: "any-put", Method: "PUT", Path: "/users/:id", Upstream: "put"},
		{RouteID: "other-host", Host: "unrelated.test", Method: "DELETE", Path: "/users/:id", Upstream: "other"},
		{RouteID: "get-only", Method: "GET", Path: "/read", Upstream: "get"},
		{RouteID: "head-only", Method: "HEAD", Path: "/head", Upstream: "head"},
		{RouteID: "options", Method: "OPTIONS", Path: "/options", Upstream: "options"},
		{RouteID: "any-method", Path: "/any", Upstream: "any"},
		{RouteID: "custom", Method: "PURGE", Path: "/custom", Upstream: "custom"},
		{RouteID: "mixed-get", Method: "GET", Path: "/mixed", Upstream: "get"},
		{RouteID: "mixed-head", Method: "HEAD", Path: "/mixed", Upstream: "head"},
		{RouteID: "mixed-any", Path: "/mixed", Upstream: "any"},
		{RouteID: "layer-exact", Host: "api.example.com", Path: "/layered", Upstream: "exact"},
		{RouteID: "layer-any", Method: "GET", Path: "/layered", Upstream: "any"},
	}
	r, err := Compile(routes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, id string
		code             MatchErrorCode
		allow            []string
	}{
		{method: "GET", path: "users/42", id: "get"},
		{method: "HEAD", path: "users/42", id: "head"},
		{method: "POST", path: "users/42", id: "wild-post"},
		{method: "PUT", path: "users/42", id: "any-put"},
		{method: "DELETE", path: "users/42", code: MatchErrMethodNotAllowed, allow: []string{"GET", "HEAD", "POST", "PUT"}},
		{method: "OPTIONS", path: "users/42", code: MatchErrMethodNotAllowed, allow: []string{"GET", "HEAD", "POST", "PUT"}},
		{method: "HEAD", path: "read", id: "get-only"},
		{method: "GET", path: "head", code: MatchErrMethodNotAllowed, allow: []string{"HEAD"}},
		{method: "OPTIONS", path: "options", id: "options"},
		{method: "GET", path: "options", code: MatchErrMethodNotAllowed, allow: []string{"OPTIONS"}},
		{method: "OPTIONS", path: "any", id: "any-method"},
		{method: "UNKNOWN", path: "any", id: "any-method"},
		{method: "GET", path: "custom", code: MatchErrMethodNotAllowed, allow: []string{"PURGE"}},
		{method: "GET", path: "missing", code: MatchErrNoRoute},
		{method: "GET", path: "mixed", id: "mixed-get"},
		{method: "HEAD", path: "mixed", id: "mixed-head"},
		{method: "POST", path: "mixed", id: "mixed-any"},
		{method: "OPTIONS", path: "mixed", id: "mixed-any"},
		{method: "HEAD", path: "layered", id: "layer-exact"},
	} {
		t.Run(tc.method+"/"+tc.path, func(t *testing.T) {
			got, err := r.Match("api.example.com", tc.method, splitPath(tc.path))
			if tc.id != "" {
				if err != nil || got == nil || got.RouteID != tc.id {
					t.Fatalf("匹配=%+v/%v, want %s", got, err, tc.id)
				}
				return
			}
			if got != nil || err == nil || err.Code != tc.code || !reflect.DeepEqual(err.AllowedMethods, tc.allow) {
				t.Fatalf("匹配=%+v/%+v, want code=%v allow=%v", got, err, tc.code, tc.allow)
			}
		})
	}
	for _, tc := range []struct {
		path    string
		methods []string
		any     bool
		fail    bool
	}{
		{path: "users/42", methods: []string{"GET", "HEAD", "POST", "PUT"}},
		{path: "any", any: true},
		{path: "mixed", any: true}, {path: "missing", fail: true},
	} {
		methods, any, err := r.AllowedMethods("api.example.com", splitPath(tc.path))
		if !reflect.DeepEqual(methods, tc.methods) || any != tc.any || (err != nil) != tc.fail {
			t.Fatalf("AllowedMethods(%s)=%v/%v/%v", tc.path, methods, any, err)
		}
	}
	if _, _, err := r.AllowedMethods("bad_host", nil); err == nil || err.Code != MatchErrIllegalHost {
		t.Fatal("允许方法查询未拒绝非法 Host")
	}
}
