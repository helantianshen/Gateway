package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/transport"
)

// TestProxy_ForwardMethodPathQuery 验证 GET/POST 方法、escaped path、query 和 body
// 被正确转发到 upstream。
//
// 这是反向代理最基本的正确性保证：
//   - HTTP 方法不变；
//   - 路径（包括 URL 编码的路径）不变；
//   - Query 参数不变；
//   - 请求体完整转发。
func TestProxy_ForwardMethodPathQuery(t *testing.T) {
	var received struct {
		Method      string
		Path        string
		EscapedPath string
		RawPath     string
		Query       string
		Body        string
		Host        string
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Method = r.Method
		received.Path = r.URL.Path
		received.EscapedPath = r.URL.EscapedPath()
		received.RawPath = r.URL.RawPath
		received.Query = r.URL.RawQuery
		received.Host = r.Host
		body, _ := io.ReadAll(r.Body)
		received.Body = string(body)
		fmt.Fprintf(w, "response from upstream")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 测试 GET with escaped path and query。
	// 使用 RawPath 确保编码后的路径被原样转发。
	// httptest.NewRequest 会解码 URL，需要手动设置 RawPath。
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/api/users%2Fgroups?name=gateway&ver=2", nil)
	req.URL.RawPath = "/api/users%2Fgroups"
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if received.Method != http.MethodGet {
		t.Errorf("GET: upstream Method = %q, want %q", received.Method, http.MethodGet)
	}
	// URL.Path 表示解码后的语义路径；%2F 在语义层表现为普通字符。
	if received.Path != "/api/users/groups" {
		t.Errorf("GET: upstream Path = %q, want %q", received.Path, "/api/users/groups")
	}
	// EscapedPath 和 RawPath 保留原始 %2F，验证代理没有二次编码为 %252F。
	if received.EscapedPath != "/api/users%2Fgroups" {
		t.Errorf("GET: upstream EscapedPath = %q, want %q", received.EscapedPath, "/api/users%2Fgroups")
	}
	if received.RawPath != "/api/users%2Fgroups" {
		t.Errorf("GET: upstream RawPath = %q, want %q", received.RawPath, "/api/users%2Fgroups")
	}
	// 验证 RawQuery 完整转发。
	if received.Query != "name=gateway&ver=2" {
		t.Errorf("GET: upstream Query = %q, want %q", received.Query, "name=gateway&ver=2")
	}

	// 测试 POST with body。
	postBody := `{"key":"value","num":42}`
	req2 := httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader(postBody))
	req2.RemoteAddr = "192.168.1.100:12345"
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)

	if received.Method != http.MethodPost {
		t.Errorf("POST: upstream Method = %q, want %q", received.Method, http.MethodPost)
	}
	if received.Body != postBody {
		t.Errorf("POST: upstream Body = %q, want %q", received.Body, postBody)
	}
}

// TestProxy_HopByHopHeadersRemoved 验证 hop-by-hop 头被从转发请求中移除。
//
// Hop-by-hop 头（Connection、Keep-Alive、Proxy-Authenticate 等）是单跳语义的，
// 代理必须移除它们，不能转发到 upstream。`TE: trailers` 是标准库为 HTTP Trailer
// 支持而保留的唯一例外，本测试会单独断言它被规范化为 trailers。
func TestProxy_HopByHopHeadersRemoved(t *testing.T) {
	var seenHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Authenticate", "Basic")
	req.Header.Set("Proxy-Authorization", "Basic dXNlcjpwYXNz")
	req.Header.Set("TE", "trailers")
	req.Header.Set("Trailer", "X-Test-Trailer")
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Upgrade", "websocket")
	// 非 hop-by-hop 头应保留。
	req.Header.Set("X-Custom", "should-keep")
	req.RemoteAddr = "192.168.1.100:12345"

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	hopByHop := []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Transfer-Encoding",
		"Upgrade",
	}
	for _, h := range hopByHop {
		if seenHeaders.Get(h) != "" {
			t.Errorf("hop-by-hop 头 %q 被转发到 upstream: %q", h, seenHeaders.Get(h))
		}
	}
	if got := seenHeaders.Get("TE"); !strings.EqualFold(got, "trailers") {
		t.Errorf("TE trailer 例外 = %q, want %q", got, "trailers")
	}
	if seenHeaders.Get("X-Custom") != "should-keep" {
		t.Errorf("非 hop-by-hop 头 X-Custom 被移除，值 = %q", seenHeaders.Get("X-Custom"))
	}
}

// TestProxy_ForgedForwardedHeadersStripped 验证客户端伪造的 Forwarded 和
// X-Forwarded-* 头不被直接信任，而是被清除后由网关重新设置。
//
// 安全风险：如果代理直接转发客户端的 X-Forwarded-For，
// 攻击者可以伪造 IP 地址，绕过基于 IP 的访问控制。
// 网关必须清除客户端提供的转发头，只设置自己验证过的值。
func TestProxy_ForgedForwardedHeadersStripped(t *testing.T) {
	var xff, xfh, xfp, forwarded, forwardedServer, forwardedPort string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xff = r.Header.Get("X-Forwarded-For")
		xfh = r.Header.Get("X-Forwarded-Host")
		xfp = r.Header.Get("X-Forwarded-Proto")
		forwarded = r.Header.Get("Forwarded")
		forwardedServer = r.Header.Get("X-Forwarded-Server")
		forwardedPort = r.Header.Get("X-Forwarded-Port")
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	// 客户端伪造的转发头。
	req.Header.Set("Forwarded", "for=10.0.0.1")
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	req.Header.Set("X-Forwarded-Host", "evil.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Server", "internal-server")
	req.Header.Set("X-Forwarded-Port", "8443")
	req.Host = "gateway.example.com"
	req.RemoteAddr = "203.0.113.50:54321"

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// X-Forwarded-For 应只包含网关看到的客户端 IP，不包含伪造值。
	if xff != "203.0.113.50" {
		t.Errorf("X-Forwarded-For = %q, want %q (客户端真实 IP)", xff, "203.0.113.50")
	}
	// X-Forwarded-Host 应为客户端请求的原始 Host。
	if xfh != "gateway.example.com" {
		t.Errorf("X-Forwarded-Host = %q, want %q", xfh, "gateway.example.com")
	}
	// X-Forwarded-Proto 应为 http（测试使用 HTTP）。
	if xfp != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want %q", xfp, "http")
	}
	// Forwarded 头应被移除（SetXForwarded 不设置 Forwarded 头）。
	if forwarded != "" {
		t.Errorf("Forwarded = %q, want 空（应被移除）", forwarded)
	}
	// 非标准 X-Forwarded-* 头应从 outbound 请求中移除。
	if forwardedServer != "" {
		t.Errorf("upstream X-Forwarded-Server = %q, want 空", forwardedServer)
	}
	if forwardedPort != "" {
		t.Errorf("upstream X-Forwarded-Port = %q, want 空", forwardedPort)
	}
}

// TestProxy_DuplicateForwardedHeaders 验证重复的 X-Forwarded-* 头
// 不导致错误或值拼接。
func TestProxy_DuplicateForwardedHeaders(t *testing.T) {
	var xff string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xff = r.Header.Get("X-Forwarded-For")
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// 添加多个 X-Forwarded-For 头。
	req.Header.Add("X-Forwarded-For", "10.0.0.1")
	req.Header.Add("X-Forwarded-For", "10.0.0.2")
	req.RemoteAddr = "203.0.113.50:54321"

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// 应只包含网关设置的客户端 IP，不包含伪造的重复值。
	if xff != "203.0.113.50" {
		t.Errorf("X-Forwarded-For = %q, want %q", xff, "203.0.113.50")
	}
}

// TestProxy_LargeBodyNotPreBuffered 验证大请求体不被网关完整预读缓存，
// 而是以流式方式转发。
//
// 如果代理预读整个请求体，大文件上传会消耗大量内存。
// httputil.ReverseProxy 将 req.Body 直接传递给 RoundTrip，
// Transport 以流式方式发送，不缓冲整个 body。
func TestProxy_LargeBodyNotPreBuffered(t *testing.T) {
	readStarted := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 读取第一个字节，记录时间。
		buf := make([]byte, 1)
		n, _ := r.Body.Read(buf)
		if n > 0 {
			close(readStarted)
		}
		// 消耗剩余 body。
		io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 使用 pipe 创建流式 body，模拟客户端逐步上传。
	pr, pw := io.Pipe()

	req := httptest.NewRequest(http.MethodPost, "/upload", pr)
	req.RemoteAddr = "203.0.113.50:54321"

	// 在 goroutine 中写入 body。
	go func() {
		// 先写一小块数据。
		pw.Write([]byte("first chunk "))
		// 等待 upstream 确认已读取。
		select {
		case <-readStarted:
		case <-time.After(5 * time.Second):
		}
		// 再写剩余数据。
		pw.Write([]byte("second chunk"))
		pw.Close()
	}()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestProxy_CancelPropagation 验证客户端取消请求时，
// 取消信号传播到 upstream 的 Context。
//
// 这是反向代理的重要行为：如果客户端断开连接，
// upstream 应该通过 Context 取消得知，从而及时停止处理，
// 避免浪费资源。
func TestProxy_CancelPropagation(t *testing.T) {
	outboundStarted := make(chan struct{})
	outboundCancelled := make(chan struct{})
	tr := &blockingRoundTripper{
		started:   outboundStarted,
		cancelled: outboundCancelled,
	}
	u, _ := url.Parse("http://upstream.invalid")
	p := New(u, tr, 30*time.Second, false)

	// 使用可控的 RoundTripper 阻塞在 outbound Context.Done()，
	// 避免真实网络连接建立、响应和客户端断开时序带来的测试不确定性。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/slow", nil).WithContext(ctx)
	req.RemoteAddr = "203.0.113.50:54321"
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-outboundStarted:
	case <-time.After(time.Second):
		t.Fatal("代理未开始发送 outbound 请求")
	}
	cancel()

	select {
	case <-outboundCancelled:
	case <-time.After(time.Second):
		t.Fatal("outbound RoundTripper 未收到 Context 取消")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("代理在 outbound 取消后未返回")
	}
}

// TestProxy_ConnectionFailure502 验证当 upstream 连接失败时，
// 网关返回 502 Bad Gateway 和 JSON 错误体。
//
// 连接失败场景：upstream 不可达（端口未监听）。
func TestProxy_ConnectionFailure502(t *testing.T) {
	u, _ := url.Parse("http://upstream.invalid")
	p := New(u, errorRoundTripper{err: errors.New("connection refused")}, 5*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusBadGateway)
	}

	// 验证 Content-Type。
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want contains application/json", ct)
	}

	// 验证 JSON 错误体。
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 解析失败: %v, body = %s", err, rec.Body.String())
	}
	if body.Code != "BAD_GATEWAY" {
		t.Errorf("error code = %q, want %q", body.Code, "BAD_GATEWAY")
	}

	// 验证错误体不包含内部地址。
	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, "upstream.invalid") {
		t.Errorf("错误体不应包含 upstream 地址: %s", bodyStr)
	}
}

// TestProxy_ResponseHeaderTimeout504 验证当 upstream 接受连接但
// 不在超时时间内返回响应头时，网关返回 504 Gateway Timeout。
//
// 该场景模拟慢 upstream：TCP 连接成功建立，但 HTTP 响应头迟迟不返回。
func TestProxy_ResponseHeaderTimeout504(t *testing.T) {
	// upstream 收到请求后不返回响应头；Transport 的 response-header timeout
	// 明显短于请求总超时，因此 504 必须由 Transport 超时触发。
	blocked := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(blocked)
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	tr.ResponseHeaderTimeout = 50 * time.Millisecond
	t.Cleanup(func() { transport.CloseIdleConnections(tr) })
	p := New(u, tr, 2*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusGatewayTimeout)
	}

	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 解析失败: %v, body = %s", err, rec.Body.String())
	}
	if body.Code != "GATEWAY_TIMEOUT" {
		t.Errorf("error code = %q, want %q", body.Code, "GATEWAY_TIMEOUT")
	}
}

// TestProxy_SSEFirstEventVisible 验证 SSE/流式响应的首个事件
// 在连接结束前对客户端可见。
//
// ReverseProxy 对 text/event-stream 响应使用即时 flush，
// 确保事件不被缓冲，客户端可以立即收到。
func TestProxy_SSEFirstEventVisible(t *testing.T) {
	firstSent := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseUpstream) }) })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: first event\n\n")
		flusher.Flush()
		close(firstSent)
		<-releaseUpstream
		_, _ = fmt.Fprint(w, "data: second event\n\n")
		flusher.Flush()
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	t.Cleanup(func() { transport.CloseIdleConnections(tr) })
	p := New(u, tr, 2*time.Second, false)
	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(proxyServer.URL + "/stream")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取首个 SSE 事件失败: %v", err)
	}
	if line != "data: first event\n" {
		t.Fatalf("首个事件 = %q, want %q", line, "data: first event\\n")
	}
	select {
	case <-firstSent:
	default:
		t.Fatal("客户端读到首事件前 upstream 未确认发送")
	}
	releaseOnce.Do(func() { close(releaseUpstream) })
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取剩余 SSE 失败: %v", err)
	}
	if !strings.Contains(string(body), "data: second event") {
		t.Errorf("剩余响应不包含第二个 SSE 事件: %q", body)
	}
}

// TestProxy_HeadersWrittenNoStatusChange 验证当 upstream 写出响应头后中断
// （body copy 失败），网关不会尝试重新写一个错误状态码。
//
// ReverseProxy 在响应头写出后的 body copy 错误时，
// 通过 panic(http.ErrAbortHandler) 中断，不会调用 ErrorHandler。
// 客户端收到的是 upstream 已写出的状态码和部分 body。
func TestProxy_HeadersWrittenNoStatusChange(t *testing.T) {
	// 创建一个 upstream：写出 200 状态码和部分 body 后中断连接。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		// 写出部分 body，随后关闭连接，客户端应得到 unexpected EOF。
		_, _ = fmt.Fprintf(w, "partial body")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 通过 hijack 中断连接，模拟 upstream 崩溃。
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream 不支持 Hijack")
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 使用真实 HTTP client 而非 httptest.NewRecorder，
	// 因为 httptest.NewRecorder 不会触发 panic(http.ErrAbortHandler) 的恢复逻辑。
	// 创建一个 test server 使用 proxy 作为 handler。
	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	resp, err := http.Get(proxyServer.URL)
	if err != nil {
		t.Fatalf("读取响应头失败: %v", err)
	}
	defer resp.Body.Close()

	// 状态码应为 200（upstream 写出的状态码），不应被改为 502。
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, want %d (upstream 已写出的状态码不应被改写)", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("读取部分响应体错误 = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if string(body) != "partial body" {
		t.Errorf("部分响应体 = %q, want %q", body, "partial body")
	}
}

// TestProxy_ProxyAndTransportReuse 验证多个请求复用同一个 Proxy 和 Transport。
//
// 通过统计 Transport 的 RoundTrip 调用次数验证 proxy 复用，
// 通过检查连接是否被复用验证 transport 复用。
func TestProxy_ProxyAndTransportReuse(t *testing.T) {
	var requestCount int64
	var newConnections int64

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		_, _ = fmt.Fprint(w, "ok")
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt64(&newConnections, 1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 发送多个请求。
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.50:54321"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("请求 %d: 状态码 = %d, want %d", i, rec.Code, http.StatusOK)
		}
	}

	if count := atomic.LoadInt64(&requestCount); count != 5 {
		t.Errorf("upstream 收到请求数 = %d, want 5", count)
	}
	if count := atomic.LoadInt64(&newConnections); count != 1 {
		t.Errorf("upstream 新建 TCP 连接数 = %d, want 1（多个请求应复用同一连接）", count)
	}
}

// TestProxy_UpstreamHostMatchesTarget 验证默认行为下
// upstream 收到的 Host 头与目标地址一致。
//
// SetURL 将 r.Out.Host 设为空字符串，使 Go HTTP client
// 使用 URL 中的 host 作为 Host 头。
func TestProxy_UpstreamHostMatchesTarget(t *testing.T) {
	var receivedHost string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "client.example.com"
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// upstream 的 Host 应为目标地址的 host，不是客户端的 Host。
	expectedHost := u.Host
	if receivedHost != expectedHost {
		t.Errorf("upstream Host = %q, want %q", receivedHost, expectedHost)
	}
}

// TestProxy_PreserveHost 验证 preserveHost=true 时，包含端口的客户端原始 Host
// 会在 SetURL 之后恢复并原样发送给 upstream。
func TestProxy_PreserveHost(t *testing.T) {
	var receivedHost string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(ts.Close)

	target, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(target, tr, 10*time.Second, true)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "client.example.com:8443"
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if receivedHost != req.Host {
		t.Errorf("upstream Host = %q, want 原始 Host %q", receivedHost, req.Host)
	}
}

// TestProxy_ErrorJSONFormat 验证错误响应的 JSON 格式正确。
func TestProxy_ErrorJSONFormat(t *testing.T) {
	u, _ := url.Parse("http://upstream.invalid")
	p := New(u, errorRoundTripper{err: errors.New("dial failed")}, 5*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// 验证 JSON 格式。
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 解析失败: %v, body = %s", err, rec.Body.String())
	}
	if body.Code == "" {
		t.Error("error code 为空")
	}
	if body.Message == "" {
		t.Error("error message 为空")
	}
}

// TestProxy_RequestTimeoutAfterHeadersAbortsBody 验证响应头已写出后，
// 请求总超时会中断 body 传输但不会改写已发送的状态码。
// 与 ResponseHeaderTimeout 不同，这里 upstream 先 Flush 响应头，
// 因此客户端观察到部分 body 和 unexpected EOF，而不是网关 504。
func TestProxy_RequestTimeoutAfterHeadersAbortsBody(t *testing.T) {
	// upstream 先 Flush 响应头和部分 body，再阻塞 body；总超时触发后，
	// 响应状态不能改写为 504，客户端应观察到部分 body 和 unexpected EOF。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "partial")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	t.Cleanup(func() { transport.CloseIdleConnections(tr) })
	p := New(u, tr, 100*time.Millisecond, false)
	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(proxyServer.URL)
	if err != nil {
		t.Fatalf("读取响应头失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("读取响应体错误 = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if string(body) != "partial" {
		t.Errorf("部分 body = %q, want %q", body, "partial")
	}
}

type errorRoundTripper struct {
	err   error
	calls *int64
}

func (r errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	if r.calls != nil {
		atomic.AddInt64(r.calls, 1)
	}
	return nil, r.err
}

// blockingRoundTripper 是测试专用的 outbound 传输器。
// 它不访问网络，而是阻塞等待请求 Context 取消，以确定性验证取消传播。
type blockingRoundTripper struct {
	started   chan<- struct{}
	cancelled chan<- struct{}
}

func (r *blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	close(r.started)
	<-req.Context().Done()
	close(r.cancelled)
	return nil, req.Context().Err()
}

// TestProxy_NoRetry 验证代理不实现重试逻辑。
//
// 当 upstream 连接失败时，请求只发送一次，不会重试。
// 通过统计 upstream 收到的请求数验证。
func TestProxy_NoRetry(t *testing.T) {
	// 使用带调用计数的确定性 RoundTripper，既避免关闭端口带来的 TOCTOU 竞态，
	// 又能直接证明一次入站请求只触发一次 outbound RoundTrip。
	var calls int64
	u, _ := url.Parse("http://upstream.invalid")
	p := New(u, errorRoundTripper{
		err:   errors.New("upstream unavailable"),
		calls: &calls,
	}, 5*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// 应返回 502（连接失败），并且只调用一次 RoundTrip，不执行重试。
	if rec.Code != http.StatusBadGateway {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("RoundTrip 调用次数 = %d, want 1", got)
	}
}

// TestProxy_BodyForwarding 验证不同大小的请求体被完整转发。
func TestProxy_BodyForwarding(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"空 body", ""},
		{"小 body", "hello"},
		{"大 body", strings.Repeat("a", 64*1024)}, // 64KB
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedBody string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				receivedBody = string(data)
				fmt.Fprintf(w, "ok")
			}))
			t.Cleanup(ts.Close)

			u, _ := url.Parse(ts.URL)
			tr := transport.New()
			defer transport.CloseIdleConnections(tr)
			p := New(u, tr, 10*time.Second, false)

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req.RemoteAddr = "203.0.113.50:54321"
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if receivedBody != tt.body {
				t.Errorf("upstream body 长度 = %d, want %d", len(receivedBody), len(tt.body))
			}
		})
	}
}

// TestProxy_QueryPreservation 验证复杂 query string 被完整转发。
func TestProxy_QueryPreservation(t *testing.T) {
	var receivedQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 包含特殊字符的 query。
	query := "name=gateway&filter=a%2Bb&sort=desc&empty=&list=1&list=2"
	req := httptest.NewRequest(http.MethodGet, "/search?"+query, nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if receivedQuery != query {
		t.Errorf("upstream query = %q, want %q", receivedQuery, query)
	}
}

// TestProxy_ResponseHeadersForwarded 验证普通 upstream 响应头被转发，同时剥离
// upstream 伪造的 X-Request-ID；可信关联 ID 由 Phase 5 外层中间件唯一设置。
func TestProxy_ResponseHeadersForwarded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Response", "value123")
		w.Header().Set("X-Request-Id", "abc-def")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "created")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusCreated)
	}
	if v := rec.Header().Get("X-Custom-Response"); v != "value123" {
		t.Errorf("X-Custom-Response = %q, want %q", v, "value123")
	}
	if values := rec.Header().Values("X-Request-ID"); len(values) != 0 {
		t.Errorf("upstream X-Request-ID 未被剥离: %v", values)
	}
}

// TestProxy_HopByHopResponseHeadersRemoved 验证 upstream 响应中的
// hop-by-hop 头被从转发到客户端的响应中移除。
func TestProxy_HopByHopResponseHeadersRemoved(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Custom", "keep-me")
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	for _, h := range []string{"Connection", "Keep-Alive", "Transfer-Encoding"} {
		if v := rec.Header().Get(h); v != "" {
			t.Errorf("hop-by-hop 响应头 %q 被转发: %q", h, v)
		}
	}
	if v := rec.Header().Get("X-Custom"); v != "keep-me" {
		t.Errorf("非 hop-by-hop 响应头 X-Custom = %q, want %q", v, "keep-me")
	}
}

// TestProxy_ClientDisconnectContext 验证客户端请求 Context 取消时，
// outbound 请求的 Context 也被取消。
//
// 使用阻塞在 Context.Done() 的自定义 RoundTripper，避免真实网络时序造成测试抖动。
func TestProxy_ClientDisconnectContext(t *testing.T) {
	outboundStarted := make(chan struct{})
	outboundCancelled := make(chan struct{})
	tr := &blockingRoundTripper{
		started:   outboundStarted,
		cancelled: outboundCancelled,
	}
	u, _ := url.Parse("http://upstream.invalid")
	p := New(u, tr, 30*time.Second, false)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-outboundStarted:
	case <-time.After(time.Second):
		t.Fatal("代理未开始发送 outbound 请求")
	}
	cancel()

	select {
	case <-outboundCancelled:
	case <-time.After(time.Second):
		t.Fatal("客户端取消未传播到 outbound Context")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("代理在客户端取消后未返回")
	}
}

// TestProxy_SSEStreamingWithFlusher 验证使用 http.ResponseController 的流式响应
// 能被正确代理，且 flush 间隔合理。
//
// httputil.ReverseProxy 对非 text/event-stream 响应使用立即 flush，
// 对 text/event-stream 响应使用自动 flush（Go 1.20+）。
func TestProxy_SSEStreamingWithFlusher(t *testing.T) {
	eventCount := 3

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher := w.(http.Flusher)
		for i := 0; i < eventCount; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			flusher.Flush()
		}
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 30*time.Second, false)

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	resp, err := http.Get(proxyServer.URL + "/stream")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	received := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: event-") {
			received++
		}
	}

	if received != eventCount {
		t.Errorf("收到事件数 = %d, want %d", received, eventCount)
	}
}

// TestProxy_EmptyPathAndRootPath 验证空路径和根路径的转发行为。
func TestProxy_EmptyPathAndRootPath(t *testing.T) {
	var receivedPath string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 测试根路径。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if receivedPath != "/" {
		t.Errorf("upstream path = %q, want %q", receivedPath, "/")
	}
}

// TestProxy_BodyPartialForward 验证请求体以流式方式转发，
// 不被完整预读到内存。
//
// 使用 reader 在读取时产生数据的方式验证流式转发。
func TestProxy_BodyPartialForward(t *testing.T) {
	var upstreamReadTimes []time.Time
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 10)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				mu.Lock()
				upstreamReadTimes = append(upstreamReadTimes, time.Now())
				mu.Unlock()
			}
			if err != nil {
				break
			}
			// 模拟 upstream 逐块处理。
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 使用逐步产生数据的 reader。
	reader := &slowReader{
		chunks: [][]byte{[]byte("chunk1-"), []byte("chunk2-"), []byte("chunk3")},
		delay:  20 * time.Millisecond,
	}

	req := httptest.NewRequest(http.MethodPost, "/upload", reader)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	mu.Lock()
	defer mu.Unlock()
	if len(upstreamReadTimes) < 2 {
		t.Errorf("upstream 读取次数 = %d, want >= 2（应分块读取而非一次性预读）", len(upstreamReadTimes))
	}

	// 验证各次读取之间有间隔，说明是流式转发而非一次性预读。
	if len(upstreamReadTimes) >= 2 {
		interval := upstreamReadTimes[1].Sub(upstreamReadTimes[0])
		if interval < 5*time.Millisecond {
			t.Errorf("读取间隔 = %v, 期望有间隔（流式转发）", interval)
		}
	}
}

// slowReader 是一个逐步产生数据的 io.Reader，用于测试流式转发。
//
// 每次读取产生一个 chunk 并等待 delay 时长，
// 模拟客户端逐步上传数据的场景。
type slowReader struct {
	chunks [][]byte
	delay  time.Duration
	index  int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	chunk := r.chunks[r.index]
	n := copy(p, chunk)
	r.index++
	return n, nil
}

// TestProxy_UpstreamReturns500 验证 upstream 返回 500 时，
// 网关原样转发状态码和响应体，不替换为网关自身的错误格式。
//
// 只有网关自身产生的错误（连接失败、超时）才使用 JSON 错误格式，
// upstream 的响应应原样透传。
func TestProxy_UpstreamReturns500(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "internal server error")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d, want %d（upstream 状态码应原样转发）", rec.Code, http.StatusInternalServerError)
	}
	if rec.Body.String() != "internal server error" {
		t.Errorf("响应体 = %q, want %q", rec.Body.String(), "internal server error")
	}
}

// TestProxy_UpstreamReturns404 验证 upstream 返回 404 时原样转发。
func TestProxy_UpstreamReturns404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "not found")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestProxy_WebSocketUpgradeFailureForwarded 验证 Upgrade 请求由 ReverseProxy
// 正常交给 upstream；upstream 拒绝升级时透传其 400，而不是 Gateway 崩溃或改写。
// 完整双向 WebSocket 数据帧联调仍属于后续专项验证。
func TestProxy_WebSocketUpgradeFailureForwarded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟 WebSocket 升级失败。
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "bad request")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// 应正常转发（upstream 返回 400），不崩溃。
	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestProxy_ConcurrentRequests 验证并发请求的正确性。
//
// 多个 goroutine 同时通过同一 Proxy 发送请求，
// 验证 Proxy 是并发安全的。
func TestProxy_ConcurrentRequests(t *testing.T) {
	var successCount int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&successCount, 1)
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "203.0.113.50:54321"
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("并发请求状态码 = %d, want %d", rec.Code, http.StatusOK)
			}
		}()
	}
	wg.Wait()

	if count := atomic.LoadInt64(&successCount); count != 20 {
		t.Errorf("upstream 成功请求数 = %d, want 20", count)
	}
}
