package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/response"
	"github.com/helantianshen/gateway/internal/dataplane/upstream"
	"github.com/helantianshen/gateway/internal/router"
)

type capturedRequest struct {
	method    string
	host      string
	urlHost   string
	urlPath   string
	match     *router.MatchResult
	selection UpstreamSelection
}

type captureRoundTripper struct {
	requests chan capturedRequest
}

func (rt *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	match, _ := MatchResultFromContext(req.Context())
	selection, _ := UpstreamSelectionFromContext(req.Context())
	rt.requests <- capturedRequest{
		method:    req.Method,
		host:      req.Host,
		urlHost:   req.URL.Host,
		urlPath:   req.URL.Path,
		match:     match,
		selection: selection,
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    req,
	}, nil
}

func TestGatewayHandlerRoutesToEndpointAndPropagatesContext(t *testing.T) {
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
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{
		"api":     mustCompileUpstream(t, "api", []string{"http://api.internal/base"}, upstream.ProxyModeDefault|upstream.ProxyModePreserveHost, rt),
		"default": mustCompileUpstream(t, "default", []string{"http://default.internal"}, upstream.ProxyModeDefault|upstream.ProxyModePreserveHost, rt),
	})

	// HEAD 没有显式路由，必须回退到 GET；向 endpoint 转发时仍保持 HEAD。
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
	if captured.match.RouteID != "user-detail" || captured.match.UpstreamID != "api" || captured.match.Params["id"] != "42" {
		t.Errorf("Context MatchResult = %+v", captured.match)
	}
	if captured.selection != (UpstreamSelection{UpstreamID: "api", EndpointID: "api-0"}) {
		t.Errorf("Context UpstreamSelection = %+v", captured.selection)
	}
}

func TestGatewayHandlerSelectsDifferentUpstream(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{
		{RouteID: "api", Method: http.MethodGet, Path: "/api", Upstream: "api"},
		{RouteID: "fallback", Path: "/*path", Upstream: "default"},
	})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 2)}
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{
		"api":     mustCompileUpstream(t, "api", []string{"http://api.internal"}, upstream.ProxyModeDefault, rt),
		"default": mustCompileUpstream(t, "default", []string{"http://default.internal"}, upstream.ProxyModeDefault, rt),
	})

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

func TestGatewayHandlerRoundRobinEndpoints(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "api", Path: "/api", Upstream: "api"}})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 6)}
	compiled := mustCompileUpstream(t, "api", []string{
		"http://api-0.internal",
		"http://api-1.internal",
		"http://api-2.internal",
	}, upstream.ProxyModeDefault, rt)
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{"api": compiled})

	for range 6 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/api", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("状态码 = %d, want 200", recorder.Code)
		}
	}

	wantHosts := []string{"api-0.internal", "api-1.internal", "api-2.internal", "api-0.internal", "api-1.internal", "api-2.internal"}
	for index, wantHost := range wantHosts {
		captured := <-rt.requests
		if captured.urlHost != wantHost || captured.selection.EndpointID != "api-"+strconv.Itoa(index%3) {
			t.Fatalf("第 %d 次 endpoint = host %q selection %+v, want host %q", index, captured.urlHost, captured.selection, wantHost)
		}
	}
}

func TestGatewayHandlerSkipsUnhealthyEndpoints(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "api", Path: "/api", Upstream: "api"}})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 4)}
	compiled := mustCompileUpstream(t, "api", []string{
		"http://api-0.internal",
		"http://api-1.internal",
		"http://api-2.internal",
	}, upstream.ProxyModeDefault, rt)
	unhealthy, _ := compiled.Endpoint("api-1")
	unhealthy.State().SetHealthy(false)
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{"api": compiled})

	for range 4 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/api", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("状态码 = %d, want 200", recorder.Code)
		}
	}
	for index, want := range []string{"api-0", "api-2", "api-0", "api-2"} {
		if got := (<-rt.requests).selection.EndpointID; got != want {
			t.Fatalf("第 %d 次 endpoint = %q, want %q", index, got, want)
		}
	}
}

func TestGatewayHandlerReturnsServiceUnavailableWhenAllEndpointsUnhealthy(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "api", Path: "/api", Upstream: "api"}})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 1)}
	compiled := mustCompileUpstream(t, "api", []string{"http://api-0.internal", "http://api-1.internal"}, upstream.ProxyModeDefault, rt)
	for _, id := range []string{"api-0", "api-1"} {
		endpoint, _ := compiled.Endpoint(id)
		endpoint.State().SetHealthy(false)
	}
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{"api": compiled})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/api", nil))
	assertErrorResponse(t, recorder, http.StatusServiceUnavailable, "NO_HEALTHY_UPSTREAM")
	select {
	case request := <-rt.requests:
		t.Fatalf("全 unhealthy 不应发送请求: %+v", request)
	default:
	}
}

func TestGatewayHandlerRejectsIllegalPath(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "all", Path: "/*path", Upstream: "api"}})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 1)}
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{
		"api": mustCompileUpstream(t, "api", []string{"http://api.internal"}, upstream.ProxyModeDefault, rt),
	})

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
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{
		"api": mustCompileUpstream(t, "api", []string{"http://api.internal"}, upstream.ProxyModeDefault, rt),
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.local/missing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusNotFound, "NOT_FOUND")
}

func TestGatewayHandlerReturnsBadGatewayForMissingRuntimeUpstream(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "users", Path: "/users", Upstream: "missing"}})
	handler := NewGatewayHandler(r, nil)

	req := httptest.NewRequest(http.MethodGet, "http://gateway.local/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusBadGateway, "BAD_GATEWAY")
}

func TestGatewayHandlerReturnsBadGatewayForUncompiledProxyMode(t *testing.T) {
	r := mustCompileRouter(t, []router.CompileInput{{RouteID: "users", Path: "/users", Upstream: "api", PreserveHost: true}})
	rt := &captureRoundTripper{requests: make(chan capturedRequest, 1)}
	handler := NewGatewayHandler(r, map[string]*upstream.CompiledUpstream{
		"api": mustCompileUpstream(t, "api", []string{"http://api.internal"}, upstream.ProxyModeDefault, rt),
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/users", nil))
	assertErrorResponse(t, recorder, http.StatusBadGateway, "BAD_GATEWAY")
	select {
	case request := <-rt.requests:
		t.Fatalf("缺失 Proxy 模式不应发送请求: %+v", request)
	default:
	}
}

func mustCompileRouter(t *testing.T, inputs []router.CompileInput) *router.Router {
	t.Helper()
	r, err := router.Compile(inputs)
	if err != nil {
		t.Fatalf("router.Compile 错误: %v", err)
	}
	return r
}

func mustCompileUpstream(
	t *testing.T,
	id string,
	targets []string,
	modes upstream.ProxyMode,
	transport http.RoundTripper,
) *upstream.CompiledUpstream {
	t.Helper()
	configs := make([]upstream.EndpointConfig, 0, len(targets))
	for index, rawTarget := range targets {
		configs = append(configs, upstream.EndpointConfig{
			ID:     id + "-" + strconv.Itoa(index),
			Target: mustParseURL(t, rawTarget),
			Weight: 1,
		})
	}
	compiled, err := upstream.NewCompiledUpstream(id, configs, modes, transport, time.Second)
	if err != nil {
		t.Fatalf("upstream.NewCompiledUpstream(%q) 错误: %v", id, err)
	}
	return compiled
}

func mustParseURL(t *testing.T, raw string) url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) 错误: %v", raw, err)
	}
	return *parsed
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
