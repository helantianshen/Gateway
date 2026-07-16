package policy

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type middlewareFunc func(http.Handler) http.Handler

func (f middlewareFunc) Wrap(next http.Handler) http.Handler { return f(next) }

func TestCompilePreservesDeclaredOrder(t *testing.T) {
	var calls []string
	makeMiddleware := func(name string) Middleware {
		return middlewareFunc(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, name+":before")
				next.ServeHTTP(w, r)
				calls = append(calls, name+":after")
			})
		})
	}
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls = append(calls, "terminal")
		w.WriteHeader(http.StatusNoContent)
	})

	chain, err := Compile(terminal, []Middleware{makeMiddleware("first"), makeMiddleware("second")})
	if err != nil {
		t.Fatalf("Compile 返回错误: %v", err)
	}
	recorder := httptest.NewRecorder()
	chain.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))

	want := []string{"first:before", "second:before", "terminal", "second:after", "first:after"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("执行顺序 = %v, want %v", calls, want)
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d, want 204", recorder.Code)
	}
}

func TestCompileRejectsInvalidInputs(t *testing.T) {
	valid := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if chain, err := Compile(nil, nil); err == nil || chain != nil {
		t.Fatalf("nil terminal = (%v, %v), want error", chain, err)
	}
	if chain, err := Compile(valid, []Middleware{nil}); err == nil || chain != nil {
		t.Fatalf("nil middleware = (%v, %v), want error", chain, err)
	}
	returnsNil := middlewareFunc(func(http.Handler) http.Handler { return nil })
	if chain, err := Compile(valid, []Middleware{returnsNil}); err == nil || chain != nil {
		t.Fatalf("nil wrapped handler = (%v, %v), want error", chain, err)
	}
}

func TestNilCompiledChainReturnsStandardError(t *testing.T) {
	var chain *CompiledChain
	recorder := httptest.NewRecorder()
	chain.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", recorder.Code)
	}
}
