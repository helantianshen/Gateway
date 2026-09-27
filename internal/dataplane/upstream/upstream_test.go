package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mustURL(t *testing.T, raw string) url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析测试 URL %q: %v", raw, err)
	}
	return *parsed
}

func endpointConfigs(t *testing.T, count int) []EndpointConfig {
	t.Helper()
	configs := make([]EndpointConfig, 0, count)
	for i := 0; i < count; i++ {
		configs = append(configs, EndpointConfig{
			ID:     string(rune('a' + i)),
			Target: mustURL(t, "http://127.0.0.1:18080"),
			Weight: i + 1,
		})
	}
	return configs
}

func TestNewCompiledUpstreamAndDeterministicSelection(t *testing.T) {
	configs := endpointConfigs(t, 3)
	compiled, err := NewCompiledUpstream(
		"users",
		configs,
		ProxyModeDefault,
		roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("不应执行请求")
		}),
		time.Second,
	)
	if err != nil {
		t.Fatalf("NewCompiledUpstream 返回错误: %v", err)
	}
	if compiled.ID() != "users" || compiled.EndpointCount() != 3 {
		t.Fatalf("编译结果错误: id=%q endpoints=%d", compiled.ID(), compiled.EndpointCount())
	}

	first, ok := compiled.Endpoint("a")
	if !ok {
		t.Fatal("未找到 endpoint a")
	}
	if !first.State().Healthy() || first.State().ActiveRequests() != 0 || first.Weight() != 1 {
		t.Fatalf("endpoint 初始状态错误: healthy=%v active=%d weight=%d", first.State().Healthy(), first.State().ActiveRequests(), first.Weight())
	}

	// 构造后修改输入值，不能改变运行时 target
	configs[0].Target.Host = "mutated.invalid"
	if got := first.Target().Host; got != "127.0.0.1:18080" {
		t.Fatalf("runtime target 被输入切片修改: %q", got)
	}

	for index, want := range []string{"a", "b", "c", "a", "b", "c"} {
		endpoint, selectErr := compiled.Select()
		if selectErr != nil || endpoint.ID() != want {
			t.Fatalf("第 %d 次 Select = (%v, %v), want endpoint %q", index, endpoint, selectErr, want)
		}
	}
}

func TestCompiledUpstreamFiltersHealthAndReturnsNoHealthy(t *testing.T) {
	compiled, err := NewCompiledUpstream("users", endpointConfigs(t, 3), 0, nil, time.Second)
	if err != nil {
		t.Fatalf("NewCompiledUpstream 返回错误: %v", err)
	}

	middle, _ := compiled.Endpoint("b")
	middle.State().SetHealthy(false)
	for index, want := range []string{"a", "c", "a", "c"} {
		endpoint, selectErr := compiled.Select()
		if selectErr != nil || endpoint.ID() != want {
			t.Fatalf("跳过 unhealthy：第 %d 次 = (%v, %v), want %q", index, endpoint, selectErr, want)
		}
	}

	for _, id := range []string{"a", "c"} {
		endpoint, _ := compiled.Endpoint(id)
		endpoint.State().SetHealthy(false)
	}
	if endpoint, selectErr := compiled.Select(); !errors.Is(selectErr, ErrNoHealthyEndpoint) || endpoint != nil {
		t.Fatalf("全 unhealthy Select = (%v, %v), want (nil, ErrNoHealthyEndpoint)", endpoint, selectErr)
	}

	middle.State().SetHealthy(true)
	endpoint, selectErr := compiled.Select()
	if selectErr != nil || endpoint.ID() != "b" {
		t.Fatalf("恢复单一 endpoint 后 Select = (%v, %v)", endpoint, selectErr)
	}
}

func TestCompiledEndpointServeHTTPUsesRequestedHostMode(t *testing.T) {
	hosts := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts <- r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	compiled, err := NewCompiledUpstream(
		"users",
		[]EndpointConfig{{ID: "a", Target: mustURL(t, server.URL), Weight: 1}},
		ProxyModeDefault|ProxyModePreserveHost,
		server.Client().Transport,
		time.Second,
	)
	if err != nil {
		t.Fatalf("NewCompiledUpstream 返回错误: %v", err)
	}
	endpoint, _ := compiled.Endpoint("a")

	for _, tc := range []struct {
		name         string
		preserveHost bool
		wantHost     string
	}{
		{name: "default target host", preserveHost: false, wantHost: strings.TrimPrefix(server.URL, "http://")},
		{name: "preserve original host", preserveHost: true, wantHost: "client.example:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://gateway.local/resource", nil)
			request.Host = "client.example:8443"
			if served := endpoint.ServeHTTP(recorder, request, tc.preserveHost); !served {
				t.Fatal("ServeHTTP 返回 false")
			}
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("状态码 = %d, want 204", recorder.Code)
			}
			if got := <-hosts; got != tc.wantHost {
				t.Fatalf("upstream Host = %q, want %q", got, tc.wantHost)
			}
			if active := endpoint.State().ActiveRequests(); active != 0 {
				t.Fatalf("请求结束后 active requests = %d", active)
			}
		})
	}
}

func TestCompiledEndpointTracksActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})
	compiled, err := NewCompiledUpstream(
		"users",
		[]EndpointConfig{{ID: "a", Target: mustURL(t, "http://upstream.local"), Weight: 1}},
		ProxyModeDefault,
		transport,
		time.Second,
	)
	if err != nil {
		t.Fatalf("NewCompiledUpstream 返回错误: %v", err)
	}
	endpoint, _ := compiled.Endpoint("a")
	done := make(chan struct{})
	go func() {
		defer close(done)
		endpoint.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil), false)
	}()

	<-entered
	if active := endpoint.State().ActiveRequests(); active != 1 {
		t.Fatalf("请求处理中 active requests = %d, want 1", active)
	}
	close(release)
	<-done
	if active := endpoint.State().ActiveRequests(); active != 0 {
		t.Fatalf("请求结束后 active requests = %d, want 0", active)
	}
}

func TestCompiledEndpointCancellationReleasesActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(entered)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	compiled, err := NewCompiledUpstream(
		"users",
		[]EndpointConfig{{ID: "a", Target: mustURL(t, "http://upstream.local"), Weight: 1}},
		ProxyModeDefault,
		transport,
		time.Second,
	)
	if err != nil {
		t.Fatalf("NewCompiledUpstream 返回错误: %v", err)
	}
	endpoint, _ := compiled.Endpoint("a")
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		endpoint.ServeHTTP(httptest.NewRecorder(), request, false)
	}()

	<-entered
	cancel()
	<-done
	if active := endpoint.State().ActiveRequests(); active != 0 {
		t.Fatalf("取消后 active requests = %d, want 0", active)
	}
}

func TestCompiledEndpointRejectsUncompiledProxyModeWithoutRoundTrip(t *testing.T) {
	var roundTrips atomic.Int64
	compiled, err := NewCompiledUpstream(
		"users",
		[]EndpointConfig{{ID: "a", Target: mustURL(t, "http://upstream.local"), Weight: 1}},
		ProxyModeDefault,
		roundTripFunc(func(*http.Request) (*http.Response, error) {
			roundTrips.Add(1)
			return nil, errors.New("unexpected")
		}),
		time.Second,
	)
	if err != nil {
		t.Fatalf("NewCompiledUpstream 返回错误: %v", err)
	}
	endpoint, _ := compiled.Endpoint("a")
	served := endpoint.ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil),
		true,
	)
	if served || roundTrips.Load() != 0 || endpoint.State().ActiveRequests() != 0 {
		t.Fatalf("未编译模式执行结果: served=%v roundTrips=%d active=%d", served, roundTrips.Load(), endpoint.State().ActiveRequests())
	}
}

func TestNewCompiledUpstreamRejectsInvalidInput(t *testing.T) {
	validTransport := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	validEndpoint := EndpointConfig{ID: "a", Target: mustURL(t, "http://upstream.local"), Weight: 1}
	tests := []struct {
		name      string
		id        string
		endpoints []EndpointConfig
		modes     ProxyMode
		transport http.RoundTripper
		timeout   time.Duration
	}{
		{name: "empty upstream id", id: " ", endpoints: []EndpointConfig{validEndpoint}, modes: ProxyModeDefault, transport: validTransport, timeout: time.Second},
		{name: "empty endpoints", id: "users", modes: ProxyModeDefault, transport: validTransport, timeout: time.Second},
		{name: "invalid mode", id: "users", endpoints: []EndpointConfig{validEndpoint}, modes: ProxyMode(128), transport: validTransport, timeout: time.Second},
		{name: "nil transport", id: "users", endpoints: []EndpointConfig{validEndpoint}, modes: ProxyModeDefault, timeout: time.Second},
		{name: "invalid timeout", id: "users", endpoints: []EndpointConfig{validEndpoint}, modes: ProxyModeDefault, transport: validTransport},
		{name: "empty endpoint id", id: "users", endpoints: []EndpointConfig{{Target: validEndpoint.Target, Weight: 1}}, modes: ProxyModeDefault, transport: validTransport, timeout: time.Second},
		{name: "duplicate endpoint id", id: "users", endpoints: []EndpointConfig{validEndpoint, validEndpoint}, modes: ProxyModeDefault, transport: validTransport, timeout: time.Second},
		{name: "invalid weight", id: "users", endpoints: []EndpointConfig{{ID: "a", Target: validEndpoint.Target}}, modes: ProxyModeDefault, transport: validTransport, timeout: time.Second},
		{name: "invalid target", id: "users", endpoints: []EndpointConfig{{ID: "a", Target: mustURL(t, "/relative"), Weight: 1}}, modes: ProxyModeDefault, transport: validTransport, timeout: time.Second},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := NewCompiledUpstream(test.id, test.endpoints, test.modes, test.transport, test.timeout)
			if err == nil || compiled != nil {
				t.Fatalf("NewCompiledUpstream = (%v, %v), want (nil, error)", compiled, err)
			}
		})
	}
}
