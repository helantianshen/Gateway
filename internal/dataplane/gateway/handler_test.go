package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/response"
	"github.com/helantianshen/gateway/internal/router"
)

type capturedRequest struct {
	method  string
	host    string
	urlHost string
	urlPath string
	match   *router.MatchResult
}

type captureRoundTripper struct {
	requests chan capturedRequest
}

func (rt *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	match, _ := MatchResultFromContext(req.Context())
	rt.requests <- capturedRequest{
		method:  req.Method,
		host:    req.Host,
		urlHost: req.URL.Host,
		urlPath: req.URL.Path,
		match:   match,
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    req,
	}, nil
}

func TestGatewayHandlerRoutesToFixedProxyAndPropagatesMatch(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{
		{
			RouteID:      "user-detail",
			Host:         "api.example.com",
			Method:       http.MethodGet,
			Path:         "/users/:id",
			Upstream:     "api",
			PreserveHost: true,
		},
		{RouteID: "fallback", Path: "/*path", Upstream: "default"},
	})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 1)}
	handler := NewGatewayHandler(r, map[string]*url.URL{
		"api":     mustParseURL(t, "http://api.internal/base"),
		"default": mustParseURL(t, "http://default.internal"),
	}, rt, time.Second)

	// HEAD 没有显式路由，必须回退到 GET 路由；转发给 upstream 时仍保持 HEAD。
	req := httptest.NewRequest(http.MethodHead, "http://gateway.local/users/42", nil)
	req.Host = "api.example.com:8443"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	captured := <-rt.requests
	if captured.method != http.MethodHead {
		t.Errorf("upstream Method = %q, want HEAD", captured.method)
	}
	if captured.urlHost != "api.internal" || captured.urlPath != "/base/users/42" {
		t.Errorf("固定目标 = %s%s, want api.internal/base/users/42", captured.urlHost, captured.urlPath)
	}
	if captured.host != "api.example.com:8443" {
		t.Errorf("preserveHost 后 Host = %q, want %q", captured.host, "api.example.com:8443")
	}
	if captured.match == nil {
		t.Fatal("请求 Context 中没有路由匹配结果")
	}
	if captured.match.RouteID != "user-detail" || captured.match.UpstreamID != "api" {
		t.Errorf("Context MatchResult = %+v", captured.match)
	}
	if captured.match.Params["id"] != "42" {
		t.Errorf("路径参数 id = %q, want 42", captured.match.Params["id"])
	}
}

func TestGatewayHandlerSelectsDifferentUpstream(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{
		{RouteID: "api", Method: http.MethodGet, Path: "/api", Upstream: "api"},
		{RouteID: "fallback", Path: "/*path", Upstream: "default"},
	})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 2)}
	handler := NewGatewayHandler(r, map[string]*url.URL{
		"api":     mustParseURL(t, "http://api.internal"),
		"default": mustParseURL(t, "http://default.internal"),
	}, rt, time.Second)

	for _, path := range []string{"/api", "/other"} {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.local"+path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d, want 200", path, rec.Code)
		}
	}

	first := <-rt.requests
	second := <-rt.requests
	if first.urlHost != "api.internal" || second.urlHost != "default.internal" {
		t.Fatalf("upstream 选择错误: first=%q second=%q", first.urlHost, second.urlHost)
	}
	if first.host != "" || second.host != "" {
		t.Errorf("preserveHost=false 时 Out.Host 应为空: first=%q second=%q", first.host, second.host)
	}
}

func TestGatewayHandlerRejectsIllegalPath(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "all", Path: "/*path", Upstream: "api"}})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 1)}
	handler := NewGatewayHandler(r, map[string]*url.URL{"api": mustParseURL(t, "http://api.internal")}, rt, time.Second)

	req := httptest.NewRequest(http.MethodGet, "http://gateway.local/users%2F42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	select {
	case request := <-rt.requests:
		t.Fatalf("非法路径不应发送 upstream 请求: %+v", request)
	default:
	}
}

func TestGatewayHandlerReturnsNotFound(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "users", Method: http.MethodGet, Path: "/users", Upstream: "api"}})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 1)}
	handler := NewGatewayHandler(r, map[string]*url.URL{"api": mustParseURL(t, "http://api.internal")}, rt, time.Second)

	req := httptest.NewRequest(http.MethodGet, "http://gateway.local/missing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusNotFound, "NOT_FOUND")
}

func TestGatewayHandlerReturnsBadGatewayForMissingTarget(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "users", Path: "/users", Upstream: "missing"}})
	handler := NewGatewayHandler(r, nil, &captureRoundTripper{requests: make(chan capturedRequest, 1)}, time.Second)

	req := httptest.NewRequest(http.MethodGet, "http://gateway.local/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusBadGateway, "BAD_GATEWAY")
}

func mustCompileRouter(t *testing.T, inputs []router.CompileInput) *router.Router {
	t.Helper()
	r, err := router.Compile(inputs)
	if err != nil {
		t.Fatalf("router.Compile 错误: %v", err)
	}
	return r
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) 错误: %v", raw, err)
	}
	return parsed
}

func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("状态码 = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	var body response.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v; body=%s", err, rec.Body.String())
	}
	if body.Code != wantCode {
		t.Errorf("错误 code = %q, want %q", body.Code, wantCode)
	}
}
