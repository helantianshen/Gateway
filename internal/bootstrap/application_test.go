package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helantianshen/gateway/internal/config"
	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
	"github.com/helantianshen/gateway/internal/dataplane/server"
	"github.com/helantianshen/gateway/internal/dataplane/transport"
)

const testOperationTimeout = 5 * time.Second

// testConfig 创建用于 bootstrap 测试的最小 Config
// TCP 端口 0 不会有监听服务，public 代理会确定性返回 502；这既验证
// public HTTP Server 可访问，也避免为一般生命周期测试额外创建 upstream
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	u, err := url.Parse("http://127.0.0.1:0")
	if err != nil {
		t.Fatalf("解析测试 upstream URL 失败: %v", err)
	}

	return &config.Config{
		PublicAddr: "127.0.0.1:0",
		AdminAddr:  "127.0.0.1:0",
		Upstreams: map[string]config.UpstreamTarget{
			"mock-service": {
				ID: "mock-service",
				Endpoints: []config.EndpointTarget{{
					ID: "mock-1", URL: *u, Weight: 100,
				}},
			},
		},
		RequestTimeout:  testOperationTimeout,
		ShutdownTimeout: testOperationTimeout,
		Spec: &config.ConfigSpec{
			APIVersion: "v1",
			Upstreams: []config.UpstreamSpec{{
				ID: "mock-service",
				Endpoints: []config.EndpointSpec{{
					ID:     "mock-1",
					URL:    "http://127.0.0.1:0",
					Weight: 100,
				}},
			}},
			Routes: []config.RouteSpec{{
				ID:       "default",
				Path:     "/*catch",
				Upstream: "mock-service",
			}},
			Policies: config.PolicySpec{
				RequestTimeout: "5s",
			},
		},
	}
}

// newUnixListenFunc 返回一个按调用顺序创建 public/admin Unix domain listener 的工厂
//
// newWithListen 仍以 network="tcp" 和配置地址调用工厂，以完整验证生产装配契约
// 测试工厂只替换底层传输为 Unix socket。socket 位于 t.TempDir 中，不消耗 TCP 临时
// 端口，也不会产生 TIME_WAIT。每个 listener 同时注册兜底清理，测试正常完成时
// Application.Shutdown 会先关闭它们，兜底 Close 的重复调用结果可安全忽略
func newUnixListenFunc(t *testing.T) listenFunc {
	t.Helper()

	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "public.sock"),
		filepath.Join(dir, "admin.sock"),
	}
	call := 0

	return func(network, address string) (net.Listener, error) {
		if network != "tcp" {
			t.Fatalf("listener factory network = %q, want %q", network, "tcp")
		}
		if call >= len(paths) {
			t.Fatalf("listener factory 调用次数超过 public/admin 两次")
		}

		listener, err := net.Listen("unix", paths[call])
		call++
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			_ = listener.Close()
		})
		return listener, nil
	}
}

// newUnixHTTPClient 创建只连接指定 Unix socket 的 HTTP Client
// URL 中的 host 只是满足 net/http 的请求格式，DialContext 会忽略传入的 TCP 地址
// 使用 Context 感知的 Unix domain dial；这样仍经过真实 HTTP Transport、Serve 和连接
// 关闭流程，但不会占用本机 TCP 临时端口
func newUnixHTTPClient(t *testing.T, socketPath string) *http.Client {
	t.Helper()

	dialer := &net.Dialer{}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)

	return &http.Client{
		Transport: tr,
		Timeout:   testOperationTimeout,
	}
}

// getAndRead 完成请求并读取完整响应体。读取到 EOF 后 Transport 才能复用连接
// 这同时确保响应体错误不会被状态码断言掩盖
func getAndRead(t *testing.T, client *http.Client, requestURL string) (int, string) {
	t.Helper()

	resp, err := client.Get(requestURL)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", requestURL, err)
	}

	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("读取 %s 响应体失败: %v", requestURL, readErr)
	}
	if closeErr != nil {
		t.Fatalf("关闭 %s 响应体失败: %v", requestURL, closeErr)
	}

	return resp.StatusCode, string(body)
}

// waitForRun 等待 Application.Run 退出，统一验证 Context 取消确实触发了
// Graceful Shutdown，且生命周期不会无限阻塞
func waitForRun(t *testing.T, errCh <-chan error) {
	t.Helper()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(testOperationTimeout):
		t.Fatalf("Run 在 %s 内未返回，Graceful Shutdown 可能挂起", testOperationTimeout)
	}
}

// assertListenerClosed 直接在原 listener 上调用 Accept 验证它已经关闭
// 该断言适用于 TCP 和 Unix listener，且不会创建额外连接来间接推断关闭状态
func assertListenerClosed(t *testing.T, name string, listener net.Listener) {
	t.Helper()

	conn, err := listener.Accept()
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("关闭意外建立的 %s 连接失败: %v", name, closeErr)
		}
		t.Errorf("%s 监听器在 Shutdown 后仍可接受连接", name)
		return
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("%s 监听器关闭后 Accept 错误 = %v, want net.ErrClosed", name, err)
	}
}

// TestApplication_LifecycleAndEndpoints 使用单个 Application 生命周期验证完整成功路径
// public/admin Server 经真实 HTTP 可访问、Context 取消触发 Graceful Shutdown，且 Run
// 返回前两个底层 listener 均已关闭。成功代理链已由 proxy 包覆盖；此处连接 TCP
// 端口 0 确定性返回 502，避免 bootstrap 再创建 TCP upstream
func TestApplication_LifecycleAndEndpoints(t *testing.T) {
	app, err := newWithListen(testConfig(t), newUnixListenFunc(t))
	if err != nil {
		t.Fatalf("newWithListen 失败: %v", err)
	}
	if app.Logger() == nil {
		t.Fatal("Application Logger 为空")
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- app.Run(ctx)
	}()

	// listener 已在 newWithListen 中同步创建。即使 Serve goroutine 尚未调度，Unix
	// 连接也会进入 listener backlog，随后由 Serve 接收，因此无需 readiness 轮询
	adminClient := newUnixHTTPClient(t, app.AdminAddr().String())
	for _, tc := range []struct {
		path       string
		wantStatus int
	}{
		{path: "/livez", wantStatus: http.StatusOK},
		{path: "/readyz", wantStatus: http.StatusOK},
		{path: "/unknown", wantStatus: http.StatusNotFound},
	} {
		status, _ := getAndRead(t, adminClient, "http://admin.local"+tc.path)
		if status != tc.wantStatus {
			t.Errorf("admin %s 状态码 = %d, want %d", tc.path, status, tc.wantStatus)
		}
	}

	publicClient := newUnixHTTPClient(t, app.PublicAddr().String())
	status, body := getAndRead(t, publicClient, "http://public.local/test")
	if status != http.StatusBadGateway {
		t.Errorf("public 状态码 = %d, want %d", status, http.StatusBadGateway)
	}
	var errorBody response.ErrorBody
	if err := json.Unmarshal([]byte(body), &errorBody); err != nil {
		t.Fatalf("public 错误响应不是合法 JSON: %v; body=%s", err, body)
	}
	if errorBody.Code != "BAD_GATEWAY" || errorBody.Message != "upstream request failed" || !requestctx.ValidRequestID(errorBody.RequestID) {
		t.Errorf("public 错误响应体 = %+v", errorBody)
	}

	cancel()
	waitForRun(t, runErrCh)
	assertListenerClosed(t, "public", app.publicListener)
	assertListenerClosed(t, "admin", app.adminListener)
}

func TestApplication_ObservabilityEndpoints(t *testing.T) {
	app, err := newWithListen(testConfig(t), newUnixListenFunc(t))
	if err != nil {
		t.Fatalf("newWithListen 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	publicClient := newUnixHTTPClient(t, app.PublicAddr().String())
	request, err := http.NewRequest(http.MethodGet, "http://public.local/observe-secret", nil)
	if err != nil {
		t.Fatalf("创建 public request: %v", err)
	}
	request.Header.Set(requestctx.RequestIDHeader, "application-request-1")
	responseMessage, err := publicClient.Do(request)
	if err != nil {
		t.Fatalf("public request: %v", err)
	}
	responseBody, readErr := io.ReadAll(responseMessage.Body)
	closeErr := responseMessage.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("读取/关闭 public response: read=%v close=%v", readErr, closeErr)
	}
	if responseMessage.StatusCode != http.StatusBadGateway || responseMessage.Header.Get(requestctx.RequestIDHeader) != "application-request-1" {
		t.Fatalf("public response = status %d request-id %q body=%s", responseMessage.StatusCode, responseMessage.Header.Get(requestctx.RequestIDHeader), responseBody)
	}
	var gatewayError response.ErrorBody
	if err := json.Unmarshal(responseBody, &gatewayError); err != nil || gatewayError.RequestID != "application-request-1" {
		t.Fatalf("public error body = %+v, err=%v", gatewayError, err)
	}

	adminClient := newUnixHTTPClient(t, app.AdminAddr().String())
	metricsResponse, err := adminClient.Get("http://admin.local/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	metricsBody, readErr := io.ReadAll(metricsResponse.Body)
	closeErr = metricsResponse.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("读取/关闭 metrics response: read=%v close=%v", readErr, closeErr)
	}
	if metricsResponse.StatusCode != http.StatusOK || !requestctx.ValidRequestID(metricsResponse.Header.Get(requestctx.RequestIDHeader)) {
		t.Fatalf("metrics response = status %d request-id %q", metricsResponse.StatusCode, metricsResponse.Header.Get(requestctx.RequestIDHeader))
	}
	metricsText := string(metricsBody)
	for _, want := range []string{
		`gateway_config_version 1`,
		`gateway_requests_total{error_kind="BAD_GATEWAY",method="GET",status_class="5xx",upstream_id="mock-service"} 1`,
		`gateway_upstream_active_requests{endpoint_id="mock-1",upstream_id="mock-service"} 0`,
		`gateway_upstream_health{endpoint_id="mock-1",upstream_id="mock-service"} 1`,
	} {
		if !strings.Contains(metricsText, want) {
			t.Errorf("metrics 缺少 %q", want)
		}
	}
	if strings.Contains(metricsText, "/observe-secret") {
		t.Errorf("metrics 泄露原始 path")
	}

	adminRequest, err := http.NewRequest(http.MethodGet, "http://admin.local/unknown", nil)
	if err != nil {
		t.Fatalf("创建 admin request: %v", err)
	}
	adminRequest.Header.Set(requestctx.RequestIDHeader, "admin-error-request")
	adminResponse, err := adminClient.Do(adminRequest)
	if err != nil {
		t.Fatalf("admin unknown request: %v", err)
	}
	adminBody, readErr := io.ReadAll(adminResponse.Body)
	closeErr = adminResponse.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("读取/关闭 admin error response: read=%v close=%v", readErr, closeErr)
	}
	var adminError response.ErrorBody
	if err := json.Unmarshal(adminBody, &adminError); err != nil || adminResponse.StatusCode != http.StatusNotFound || adminError.RequestID != "admin-error-request" {
		t.Fatalf("admin error response = status %d body %+v err=%v", adminResponse.StatusCode, adminError, err)
	}

	cancel()
	waitForRun(t, runDone)
}

// TestApplication_AlreadyCancelledContext 验证传入已取消的 Context 时，Run 仍会
// 对 public/admin Server 执行 Graceful Shutdown 并及时关闭两个 Unix listener
// 此路径不建立 HTTP 连接，同时验证取消先于 Serve 调度也不会遗留监听资源
func TestApplication_AlreadyCancelledContext(t *testing.T) {
	app, err := newWithListen(testConfig(t), newUnixListenFunc(t))
	if err != nil {
		t.Fatalf("newWithListen 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx)
	}()

	waitForRun(t, done)
	assertListenerClosed(t, "public", app.publicListener)
	assertListenerClosed(t, "admin", app.adminListener)
}

// TestApplication_ShutdownClosesUpstreamIdleConnection 使用真实 Unix upstream 验证
// Application.shutdown 会关闭共享 http.Transport 的空闲连接。测试记录同一个 upstream
// net.Conn 先进入 StateIdle、再进入 StateClosed；只有 Shutdown 中的
// CloseIdleConnections 才会在 upstream Server 仍然运行时触发后一个状态，避免测试在
// 清理阶段调用 Server.Close 后因连接被动关闭而误通过
func TestApplication_ShutdownClosesUpstreamIdleConnection(t *testing.T) {
	upstreamDir := t.TempDir()
	upstreamPath := filepath.Join(upstreamDir, "upstream.sock")
	upstreamListener, err := net.Listen("unix", upstreamPath)
	if err != nil {
		t.Fatalf("创建 Unix upstream listener 失败: %v", err)
	}

	type connStateEvent struct {
		conn  net.Conn
		state http.ConnState
	}
	stateEvents := make(chan connStateEvent, 16)
	upstreamServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "upstream response")
		}),
		ConnState: func(conn net.Conn, state http.ConnState) {
			// ConnState 回调在 HTTP Server 的连接状态机中同步执行；将连接指针
			// 一并记录，后续才能证明 idle 与 closed 属于同一条物理连接
			select {
			case stateEvents <- connStateEvent{conn: conn, state: state}:
			default:
				t.Errorf("upstream ConnState 事件通道已满，丢失状态 %s", state)
			}
		},
	}
	upstreamServeDone := make(chan error, 1)
	go func() {
		upstreamServeDone <- upstreamServer.Serve(upstreamListener)
	}()

	// 失败路径仍关闭 upstream 资源；成功路径会在 idle->closed 断言之后显式执行
	// 同样的关闭和等待，确保测试结束时没有遗留 Serve goroutine 或文件描述符
	upstreamClosed := false
	t.Cleanup(func() {
		if !upstreamClosed {
			_ = upstreamServer.Close()
			select {
			case <-upstreamServeDone:
			case <-time.After(testOperationTimeout):
				t.Errorf("清理 upstream Server 超时")
			}
		}
	})

	cfg := testConfig(t)
	parsedTarget, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatalf("解析合法 upstream URL 失败: %v", err)
	}
	target := cfg.Upstreams["mock-service"]
	target.Endpoints[0].URL = *parsedTarget
	cfg.Upstreams["mock-service"] = target
	app, err := newWithListen(cfg, newUnixListenFunc(t))
	if err != nil {
		t.Fatalf("newWithListen 失败: %v", err)
	}
	// newWithListen 创建的 Proxy 已绑定 app.transport；仅替换 DialContext，使
	// 合法的 upstream.local URL 通过 Unix socket 连接真实 Server，不接触 TCP
	app.transport.Proxy = nil
	dialer := &net.Dialer{}
	app.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", upstreamPath)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	publicClient := newUnixHTTPClient(t, app.PublicAddr().String())
	status, body := getAndRead(t, publicClient, "http://public.local/idle-close")
	if status != http.StatusOK {
		t.Fatalf("public 代理状态码 = %d, want %d", status, http.StatusOK)
	}
	if body != "upstream response" {
		t.Fatalf("public 代理响应体 = %q, want %q", body, "upstream response")
	}

	var idleConn net.Conn
	select {
	case event := <-stateEvents:
		if event.state != http.StateIdle {
			// 连接状态事件可能包含新连接和 active 状态，继续从统一循环中
			// 筛选目标状态；该分支仅用于保持 select 超时边界明确
			for idleConn == nil {
				select {
				case next := <-stateEvents:
					if next.state == http.StateIdle {
						idleConn = next.conn
					}
				case <-time.After(testOperationTimeout):
					t.Fatal("upstream 连接在请求完成后未进入 StateIdle")
				}
			}
		} else {
			idleConn = event.conn
		}
	case <-time.After(testOperationTimeout):
		t.Fatal("upstream 连接在请求完成后未进入 StateIdle")
	}

	cancel()
	waitForRun(t, runDone)

	closed := false
	deadline := time.NewTimer(testOperationTimeout)
	defer deadline.Stop()
	for !closed {
		select {
		case event := <-stateEvents:
			if event.conn == idleConn && event.state == http.StateClosed {
				closed = true
			}
		case <-deadline.C:
			t.Fatal("Application shutdown 后同一 upstream idle 连接未进入 StateClosed")
		}
	}

	// 只有在已经观察到 shutdown 导致的连接关闭后，才关闭 upstream Server
	// 因而 Server.Close 不可能伪造本测试要求的 idle->closed 证据
	if err := upstreamServer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("关闭 upstream Server 失败: %v", err)
	}
	select {
	case serveErr := <-upstreamServeDone:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Fatalf("upstream Serve 返回错误: %v", serveErr)
		}
	case <-time.After(testOperationTimeout):
		t.Fatal("等待 upstream Serve 退出超时")
	}
	if err := upstreamListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("关闭 upstream listener 失败: %v", err)
	}
	upstreamClosed = true
}

// trackingListener 是只用于装配失败路径的 fake listener
// 测试不会启动 Serve，因此 Accept 若被调用就返回明确错误；Close 记录调用次数
// 用于直接证明 admin 创建失败时已创建的 public listener 被准确关闭一次
type trackingListener struct {
	closeCalls int
}

func (l *trackingListener) Accept() (net.Conn, error) {
	return nil, errors.New("测试 trackingListener 不应调用 Accept")
}

func (l *trackingListener) Close() error {
	l.closeCalls++
	return nil
}

func (l *trackingListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "fake-public.sock", Net: "unix"}
}

// TestApplication_New_NilDependencies 验证无效依赖在分配资源前返回错误
func TestApplication_New_NilDependencies(t *testing.T) {
	if _, err := newWithListen(testConfig(t), nil); err == nil {
		t.Fatal("nil listener factory 未返回错误")
	} else if !strings.Contains(err.Error(), "listener factory 不能为空") {
		t.Fatalf("nil listener factory 错误 = %v, want 明确错误", err)
	}

	neverListen := func(string, string) (net.Listener, error) {
		t.Fatal("无效配置不应调用 listener factory")
		return nil, nil
	}
	if _, err := newWithListen(nil, neverListen); err == nil {
		t.Fatal("nil 配置未返回错误")
	} else if !strings.Contains(err.Error(), "配置不能为空") {
		t.Fatalf("nil 配置错误 = %v, want 明确错误", err)
	}

	withoutSpec := testConfig(t)
	withoutSpec.Spec = nil
	if _, err := newWithListen(withoutSpec, neverListen); err == nil || !strings.Contains(err.Error(), "声明式配置不能为空") {
		t.Fatalf("nil Spec 错误 = %v", err)
	}

	invalidTimeout := testConfig(t)
	invalidTimeout.RequestTimeout = 0
	if _, err := newWithListen(invalidTimeout, neverListen); err == nil || !strings.Contains(err.Error(), "请求超时必须为正数") {
		t.Fatalf("非法请求超时错误 = %v", err)
	}

	missingTarget := testConfig(t)
	delete(missingTarget.Upstreams, "mock-service")
	if _, err := newWithListen(missingTarget, neverListen); err == nil || !strings.Contains(err.Error(), "没有编译目标") {
		t.Fatalf("缺失 upstream target 错误 = %v", err)
	}

	mismatchedTarget := testConfig(t)
	target := mismatchedTarget.Upstreams["mock-service"]
	target.ID = "other"
	mismatchedTarget.Upstreams["mock-service"] = target
	if _, err := newWithListen(mismatchedTarget, neverListen); err == nil || !strings.Contains(err.Error(), "map key") {
		t.Fatalf("upstream ID 不一致错误 = %v", err)
	}

	emptyEndpoints := testConfig(t)
	target = emptyEndpoints.Upstreams["mock-service"]
	target.Endpoints = nil
	emptyEndpoints.Upstreams["mock-service"] = target
	if _, err := newWithListen(emptyEndpoints, neverListen); err == nil || !strings.Contains(err.Error(), "至少需要一个 endpoint") {
		t.Fatalf("空 endpoint runtime target 错误 = %v", err)
	}
}

// signalListener 是可控的 admin listener：测试发出信号后 Accept 返回 sentinel
// 从而让 admin Serve 确定性地结束，同时 Close 仍能解除 Shutdown 期间的阻塞
type signalListener struct {
	signal     chan struct{}
	closed     chan struct{}
	acceptDone chan struct{}
	closeOnce  sync.Once
	doneOnce   sync.Once
}

func newSignalListener() *signalListener {
	return &signalListener{
		signal:     make(chan struct{}),
		closed:     make(chan struct{}),
		acceptDone: make(chan struct{}),
	}
}

func (l *signalListener) Accept() (net.Conn, error) {
	select {
	case <-l.signal:
		l.doneOnce.Do(func() { close(l.acceptDone) })
		return nil, errInjectedServe
	case <-l.closed:
		l.doneOnce.Do(func() { close(l.acceptDone) })
		return nil, net.ErrClosed
	}
}

func (l *signalListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	l.doneOnce.Do(func() { close(l.acceptDone) })
	return nil
}

func (l *signalListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "injected-admin.sock", Net: "unix"}
}

var errInjectedServe = errors.New("测试注入的 Serve 错误")

// watchedListener 在真实 Unix listener 外包一层退出通知，用于等待 public Serve
// 在请求释放、底层 listener 关闭后彻底退出，避免测试只验证 Run 返回而遗留 goroutine
type watchedListener struct {
	net.Listener
	serveDone chan struct{}
	once      sync.Once
}

func (l *watchedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		l.once.Do(func() { close(l.serveDone) })
	}
	return conn, err
}

// TestApplication_Run_JoinsServeAndShutdownErrors 验证并发 Serve 和 Shutdown 错误均被返回
// 真实 Unix 连接使活跃请求参与 net/http 的停机状态机
func TestApplication_Run_JoinsServeAndShutdownErrors(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "public.sock")
	publicBase, err := net.Listen("unix", publicPath)
	if err != nil {
		t.Fatalf("创建 Unix public listener 失败: %v", err)
	}
	publicListener := &watchedListener{Listener: publicBase, serveDone: make(chan struct{})}
	t.Cleanup(func() { _ = publicListener.Close() })

	adminListener := newSignalListener()
	started := make(chan struct{})
	requestDone := make(chan struct{})
	publicHandler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(requestDone)
	})

	cfg := testConfig(t)
	cfg.ShutdownTimeout = 30 * time.Millisecond
	app := &Application{
		publicServer:   server.NewPublicServer(publicHandler),
		adminServer:    server.NewAdminServer(server.NewAdminHandler(nil)),
		publicListener: publicListener,
		adminListener:  adminListener,
		transport:      transport.New(),
		config:         cfg,
	}
	defer transport.CloseIdleConnections(app.transport)

	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background()) }()

	conn, err := net.Dial("unix", publicPath)
	if err != nil {
		t.Fatalf("连接 Unix public listener 失败: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /blocked HTTP/1.1\r\nHost: public.local\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("写入阻塞请求失败: %v", err)
	}
	select {
	case <-started:
	case <-time.After(testOperationTimeout):
		t.Fatal("public handler 未开始处理请求")
	}

	close(adminListener.signal)
	var runErr error
	select {
	case runErr = <-runDone:
	case <-time.After(testOperationTimeout):
		t.Fatal("Run 未在 ShutdownTimeout 后返回")
	}
	if !errors.Is(runErr, errInjectedServe) {
		t.Errorf("Run 错误 = %v，不包含注入的 Serve 错误", runErr)
	}
	if !errors.Is(runErr, context.DeadlineExceeded) {
		t.Errorf("Run 错误 = %v，不包含 Shutdown 超时错误", runErr)
	}

	// Shutdown 超时后必须强制关闭连接并取消活跃请求 Context
	if err := conn.SetReadDeadline(time.Now().Add(testOperationTimeout)); err != nil {
		t.Fatalf("设置连接读取 deadline: %v", err)
	}
	if _, err := io.ReadAll(conn); err != nil {
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatalf("Shutdown 超时后客户端连接仍未关闭: %v", err)
		}
	}
	select {
	case <-requestDone:
	case <-time.After(testOperationTimeout):
		t.Fatal("Shutdown 超时后活跃请求 Context 未取消")
	}
	select {
	case <-publicListener.serveDone:
	case <-time.After(testOperationTimeout):
		t.Fatal("public Serve 未彻底结束")
	}
	select {
	case <-adminListener.acceptDone:
	case <-time.After(testOperationTimeout):
		t.Fatal("admin Serve 未彻底结束")
	}
}

// TestApplication_New_ListenFailure 用可控 listener 验证 bind 错误与 public 清理路径
func TestApplication_New_ListenFailure(t *testing.T) {
	listenErr := errors.New("测试 listener 创建失败")

	t.Run("public listener", func(t *testing.T) {
		calls := 0
		listen := func(network, address string) (net.Listener, error) {
			calls++
			if network != "tcp" || address != "127.0.0.1:0" {
				t.Errorf("public listen 参数 = (%q, %q), want (%q, %q)", network, address, "tcp", "127.0.0.1:0")
			}
			return nil, listenErr
		}

		_, newErr := newWithListen(testConfig(t), listen)
		if !errors.Is(newErr, listenErr) {
			t.Errorf("newWithListen 错误 = %v, want 包含 %v", newErr, listenErr)
		}
		if newErr == nil || !strings.Contains(newErr.Error(), "创建 public 监听器失败") {
			t.Errorf("newWithListen 错误 = %v, want public 监听器错误", newErr)
		}
		if calls != 1 {
			t.Errorf("public 创建失败时 factory 调用次数 = %d, want 1", calls)
		}
	})

	t.Run("admin listener", func(t *testing.T) {
		publicListener := &trackingListener{}
		calls := 0
		listen := func(network, address string) (net.Listener, error) {
			calls++
			if network != "tcp" {
				t.Errorf("listen network = %q, want %q", network, "tcp")
			}
			switch calls {
			case 1:
				if address != "127.0.0.1:0" {
					t.Errorf("public listen address = %q, want %q", address, "127.0.0.1:0")
				}
				return publicListener, nil
			case 2:
				if address != "127.0.0.1:0" {
					t.Errorf("admin listen address = %q, want %q", address, "127.0.0.1:0")
				}
				return nil, listenErr
			default:
				t.Fatalf("listener factory 调用次数超过 public/admin 两次")
				return nil, nil
			}
		}

		_, newErr := newWithListen(testConfig(t), listen)
		if !errors.Is(newErr, listenErr) {
			t.Errorf("newWithListen 错误 = %v, want 包含 %v", newErr, listenErr)
		}
		if newErr == nil || !strings.Contains(newErr.Error(), "创建 admin 监听器失败") {
			t.Errorf("newWithListen 错误 = %v, want admin 监听器错误", newErr)
		}
		if calls != 2 {
			t.Errorf("admin 创建失败时 factory 调用次数 = %d, want 2", calls)
		}
		if publicListener.closeCalls != 1 {
			t.Errorf("admin 创建失败后 public Close 调用次数 = %d, want 1", publicListener.closeCalls)
		}
	})
}

// TestApplication_InvalidConfigDoesNotCreateListeners 验证非法配置不会触发 bind
func TestApplication_InvalidConfigDoesNotCreateListeners(t *testing.T) {
	var factoryCalls int
	listen := func(network, address string) (net.Listener, error) {
		factoryCalls++
		return nil, errors.New("不应到达此处")
	}

	// nil 配置必须在创建 Transport 或 listener 前返回错误
	_, err := newWithListen(nil, listen)
	if err == nil {
		t.Fatal("nil 配置未返回错误")
	}
	if factoryCalls != 0 {
		t.Errorf("nil 配置触发了 %d 次 listener factory 调用, want 0", factoryCalls)
	}

	// nil listener factory 也必须在分配任何资源前返回错误
	_, err = newWithListen(testConfig(t), nil)
	if err == nil {
		t.Fatal("nil listener factory 未返回错误")
	}
	if factoryCalls != 0 {
		t.Errorf("nil factory 触发了 %d 次 listener 调用, want 0", factoryCalls)
	}
}

// TestApplication_ValidYAMLConfigDrivesProxy 验证 YAML 到代理调用的完整静态链路
// 测试用 Unix socket 和自定义 DialContext 保持 HTTP 协议路径，同时避免 TCP 端口竞争
func TestApplication_ValidYAMLConfigDrivesProxy(t *testing.T) {
	// 创建 Unix socket upstream server，返回确定性响应体
	upstreamDir := t.TempDir()
	upstreamPath := filepath.Join(upstreamDir, "upstream.sock")
	upstreamListener, err := net.Listen("unix", upstreamPath)
	if err != nil {
		t.Fatalf("创建 Unix upstream listener 失败: %v", err)
	}
	const wantBody = "yaml-driven-response"
	upstreamServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, wantBody)
		}),
	}
	upstreamServeDone := make(chan error, 1)
	go func() { upstreamServeDone <- upstreamServer.Serve(upstreamListener) }()
	t.Cleanup(func() {
		_ = upstreamServer.Close()
		<-upstreamServeDone
	})

	// 使用 Unix listener 作为 public 和 admin 监听器，避免 TCP 临时端口
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "public.sock")
	adminPath := filepath.Join(dir, "admin.sock")
	publicBase, err := net.Listen("unix", publicPath)
	if err != nil {
		t.Fatalf("创建 Unix public listener 失败: %v", err)
	}
	adminBase, err := net.Listen("unix", adminPath)
	if err != nil {
		t.Fatalf("创建 Unix admin listener 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = publicBase.Close()
		_ = adminBase.Close()
	})

	var callCount int
	listen := func(network, address string) (net.Listener, error) {
		// newWithListen 按 public 后 admin 的顺序调用 factory；用计数器确保每次返回不同的 listener
		callCount++
		switch callCount {
		case 1:
			return publicBase, nil
		case 2:
			return adminBase, nil
		default:
			return nil, fmt.Errorf("意外的 listener factory 调用次数: %d", callCount)
		}
	}

	// 配置使用合法的 TCP URL，由测试 Transport 重定向到 Unix socket
	// 通过覆盖共享 Transport DialContext 将其重定向到 Unix socket
	upstreamURL, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatalf("解析 upstream URL 失败: %v", err)
	}
	cfg := &config.Config{
		PublicAddr: "127.0.0.1:0",
		AdminAddr:  "127.0.0.1:0",
		Upstreams: map[string]config.UpstreamTarget{
			"mock-service": {
				ID: "mock-service",
				Endpoints: []config.EndpointTarget{{
					ID: "mock-1", URL: *upstreamURL, Weight: 100,
				}},
			},
		},
		RequestTimeout:  testOperationTimeout,
		ShutdownTimeout: testOperationTimeout,
		Spec: &config.ConfigSpec{
			APIVersion: "v1",
			Upstreams: []config.UpstreamSpec{{
				ID: "mock-service",
				Endpoints: []config.EndpointSpec{{
					ID:     "mock-1",
					URL:    "http://upstream.local",
					Weight: 100,
				}},
			}},
			Routes: []config.RouteSpec{{
				ID:       "default",
				Path:     "/*catch",
				Upstream: "mock-service",
			}},
			Policies: config.PolicySpec{
				RequestTimeout: "5s",
			},
		},
	}

	app, err := newWithListen(cfg, listen)
	if err != nil {
		t.Fatalf("newWithListen 失败: %v", err)
	}
	// 覆盖 Transport 的 DialContext，使 upstream.local 重定向到 Unix socket
	app.transport.Proxy = nil
	dialer := &net.Dialer{}
	app.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", upstreamPath)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	// 通过 Unix socket 向 public Server 发送请求
	publicClient := newUnixHTTPClient(t, app.PublicAddr().String())
	status, body := getAndRead(t, publicClient, "http://public.local/test")
	if status != http.StatusOK {
		t.Fatalf("public 状态码 = %d, want %d", status, http.StatusOK)
	}
	if body != wantBody {
		t.Fatalf("public 响应体 = %q, want %q", body, wantBody)
	}

	// 验证 admin 健康端点也可达
	adminClient := newUnixHTTPClient(t, app.AdminAddr().String())
	adminStatus, _ := getAndRead(t, adminClient, "http://admin.local/livez")
	if adminStatus != http.StatusOK {
		t.Fatalf("admin /livez 状态码 = %d, want %d", adminStatus, http.StatusOK)
	}

	cancel()
	waitForRun(t, runDone)
}

func TestApplication_MultipleEndpointsRoundRobinAndProxyModes(t *testing.T) {
	newEndpointServer := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "%s|%s", id, r.Host)
		}))
	}
	endpointA := newEndpointServer("a")
	defer endpointA.Close()
	endpointB := newEndpointServer("b")
	defer endpointB.Close()

	urlA, err := url.Parse(endpointA.URL)
	if err != nil {
		t.Fatalf("解析 endpoint A URL: %v", err)
	}
	urlB, err := url.Parse(endpointB.URL)
	if err != nil {
		t.Fatalf("解析 endpoint B URL: %v", err)
	}

	cfg := testConfig(t)
	cfg.Upstreams["mock-service"] = config.UpstreamTarget{
		ID: "mock-service",
		Endpoints: []config.EndpointTarget{
			{ID: "a", URL: *urlA, Weight: 100},
			{ID: "b", URL: *urlB, Weight: 1},
		},
	}
	cfg.Spec.Upstreams[0].Endpoints = []config.EndpointSpec{
		{ID: "a", URL: endpointA.URL, Weight: 100},
		{ID: "b", URL: endpointB.URL, Weight: 1},
	}
	cfg.Spec.Routes = []config.RouteSpec{
		{ID: "default-host", Method: http.MethodGet, Path: "/default", Upstream: "mock-service"},
		{ID: "preserve-host", Method: http.MethodGet, Path: "/preserve", Upstream: "mock-service", PreserveHost: true},
	}

	app, err := newWithListen(cfg, newUnixListenFunc(t))
	if err != nil {
		t.Fatalf("newWithListen 失败: %v", err)
	}
	app.transport.Proxy = nil
	if compiled := app.upstreams["mock-service"]; compiled == nil || compiled.EndpointCount() != 2 {
		t.Fatalf("运行时 endpoint pool = %#v", compiled)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	client := newUnixHTTPClient(t, app.PublicAddr().String())
	wantDefault := []string{
		"a|" + urlA.Host,
		"b|" + urlB.Host,
		"a|" + urlA.Host,
		"b|" + urlB.Host,
	}
	for index, wantBody := range wantDefault {
		status, body := getAndRead(t, client, "http://public.local/default")
		if status != http.StatusOK || body != wantBody {
			t.Fatalf("第 %d 次默认 Host 请求 = (%d, %q), want (200, %q)", index, status, body, wantBody)
		}
	}

	// cursor 在四次请求后回到 endpoint A；该路由必须使用创建期预编译的 preserveHost Proxy
	status, body := getAndRead(t, client, "http://public.local/preserve")
	if status != http.StatusOK || body != "a|public.local" {
		t.Fatalf("preserveHost 请求 = (%d, %q), want (200, %q)", status, body, "a|public.local")
	}

	cancel()
	waitForRun(t, runDone)
}
