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

// TestProxy_ForwardMethodPathQuery 验证方法、编码路径、query 与 body 的转发契约
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

	// 测试 GET with escaped path and query
	// 使用 RawPath 确保编码后的路径被原样转发
	// httptest.NewRequest 会解码 URL，需要手动设置 RawPath
	query := "name=gateway&filter=a%2Bb&sort=desc&empty=&list=1&list=2"
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/api/users%2Fgroups?"+query, nil)
	req.URL.RawPath = "/api/users%2Fgroups"
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if received.Method != http.MethodGet {
		t.Errorf("GET: upstream Method = %q, want %q", received.Method, http.MethodGet)
	}
	// URL.Path 表示解码后的语义路径；%2F 在语义层表现为普通字符
	if received.Path != "/api/users/groups" {
		t.Errorf("GET: upstream Path = %q, want %q", received.Path, "/api/users/groups")
	}
	// EscapedPath 和 RawPath 保留原始 %2F，验证代理没有二次编码为 %252F
	if received.EscapedPath != "/api/users%2Fgroups" {
		t.Errorf("GET: upstream EscapedPath = %q, want %q", received.EscapedPath, "/api/users%2Fgroups")
	}
	if received.RawPath != "/api/users%2Fgroups" {
		t.Errorf("GET: upstream RawPath = %q, want %q", received.RawPath, "/api/users%2Fgroups")
	}
	// 验证 RawQuery 完整转发
	if received.Query != query {
		t.Errorf("GET: upstream Query = %q, want %q", received.Query, query)
	}

	// 测试 POST with body
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

// TestProxy_HopByHopHeadersRemoved 验证逐跳 Header 不被转发
// TE: trailers 是 Trailer 转发所需的例外
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
	// 非 hop-by-hop 头应保留
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

// TestProxy_ForgedForwardedHeadersStripped 验证客户端提供的 Forwarded 字段被清除
// X-Forwarded-For 由直接连接地址重建；测试不覆盖可信前置代理策略
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
	// 客户端伪造的转发头
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

	// X-Forwarded-For 应只包含网关看到的客户端 IP，不包含伪造值
	if xff != "203.0.113.50" {
		t.Errorf("X-Forwarded-For = %q, want %q (客户端真实 IP)", xff, "203.0.113.50")
	}
	// X-Forwarded-Host 应为客户端请求的原始 Host
	if xfh != "gateway.example.com" {
		t.Errorf("X-Forwarded-Host = %q, want %q", xfh, "gateway.example.com")
	}
	// X-Forwarded-Proto 应为 http（测试使用 HTTP）
	if xfp != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want %q", xfp, "http")
	}
	// Forwarded 头应被移除（SetXForwarded 不设置 Forwarded 头）
	if forwarded != "" {
		t.Errorf("Forwarded = %q, want 空（应被移除）", forwarded)
	}
	// 非标准 X-Forwarded-* 头应从 outbound 请求中移除
	if forwardedServer != "" {
		t.Errorf("upstream X-Forwarded-Server = %q, want 空", forwardedServer)
	}
	if forwardedPort != "" {
		t.Errorf("upstream X-Forwarded-Port = %q, want 空", forwardedPort)
	}
}

// TestProxy_DuplicateForwardedHeaders 验证重复的 X-Forwarded-* 头
// 不导致错误或值拼接
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
	// 添加多个 X-Forwarded-For 头
	req.Header.Add("X-Forwarded-For", "10.0.0.1")
	req.Header.Add("X-Forwarded-For", "10.0.0.2")
	req.RemoteAddr = "203.0.113.50:54321"

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// 应只包含网关设置的客户端 IP，不包含伪造的重复值
	if xff != "203.0.113.50" {
		t.Errorf("X-Forwarded-For = %q, want %q", xff, "203.0.113.50")
	}
}

// TestProxy_StreamedRequestBodyIsNotPreBuffered 验证上游在客户端写完 body 前已读到首段
func TestProxy_StreamedRequestBodyIsNotPreBuffered(t *testing.T) {
	readStarted := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 读取第一个字节，记录时间
		buf := make([]byte, 1)
		n, _ := r.Body.Read(buf)
		if n > 0 {
			close(readStarted)
		}
		// 消耗剩余 body
		io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "ok")
	}))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	tr := transport.New()
	defer transport.CloseIdleConnections(tr)
	p := New(u, tr, 10*time.Second, false)

	// 使用 pipe 创建流式 body，模拟客户端逐步上传
	pr, pw := io.Pipe()

	req := httptest.NewRequest(http.MethodPost, "/upload", pr)
	req.RemoteAddr = "203.0.113.50:54321"

	// 上游必须在客户端发送剩余 body 前读到首段；超时只用于解除 pipe 阻塞
	streamed := make(chan bool, 1)
	go func() {
		defer pw.Close()
		// 先写一小块数据
		if _, err := pw.Write([]byte("first chunk ")); err != nil {
			streamed <- false
			return
		}
		// 等待 upstream 确认已读取
		select {
		case <-readStarted:
			streamed <- true
		case <-time.After(time.Second):
			streamed <- false
		}
		// 再写剩余数据
		_, _ = pw.Write([]byte("second chunk"))
	}()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusOK)
	}
	if !<-streamed {
		t.Error("上游未在剩余 body 发送前读到首段，请求体可能被预读")
	}
}

// TestProxy_CancelPropagation 验证入站请求取消会传给出站 Context
func TestProxy_CancelPropagation(t *testing.T) {
	outboundStarted := make(chan struct{})
	outboundCancelled := make(chan struct{})
	tr := &blockingRoundTripper{
		started:   outboundStarted,
		cancelled: outboundCancelled,
	}
	u, _ := url.Parse("http://upstream.invalid")
	p := New(u, tr, 30*time.Second, false)

	// 使用可控的 RoundTripper 阻塞在 outbound Context.Done()
	// 避免真实网络连接建立、响应和客户端断开时序带来的测试不确定性
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

// TestProxy_ConnectionFailure502 验证连接失败时的 502 和受控 JSON 错误
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

	// 验证 Content-Type
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want contains application/json", ct)
	}

	// 验证 JSON 错误体
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
	if body.Message == "" {
		t.Error("error message 为空")
	}

	// 验证错误体不包含内部地址
	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, "upstream.invalid") {
		t.Errorf("错误体不应包含 upstream 地址: %s", bodyStr)
	}
}

// TestProxy_ResponseHeaderTimeout504 验证请求发完后等待上游响应头超时返回 504
func TestProxy_ResponseHeaderTimeout504(t *testing.T) {
	// upstream 收到请求后不返回响应头；Transport 的 response-header timeout
	// 明显短于请求总超时，因此 504 必须由 Transport 超时触发
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

// TestProxy_SSEFirstEventVisible 验证首个 SSE 事件在响应结束前可见
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

// TestProxy_HeadersWrittenNoStatusChange 验证响应头写出后的 body 中断不改状态码
// ReverseProxy 以 ErrAbortHandler 终止连接，不再调用 ErrorHandler
func TestProxy_HeadersWrittenNoStatusChange(t *testing.T) {
	// 创建一个 upstream：写出 200 状态码和部分 body 后中断连接
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		// 写出部分 body，随后关闭连接，客户端应得到 unexpected EOF
		_, _ = fmt.Fprintf(w, "partial body")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 通过 hijack 中断连接，模拟 upstream 崩溃
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

	// 使用真实 HTTP client 而非 httptest.NewRecorder
	// 因为 httptest.NewRecorder 不会触发 panic(http.ErrAbortHandler) 的恢复逻辑
	// 创建一个 test server 使用 proxy 作为 handler
	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	resp, err := http.Get(proxyServer.URL)
	if err != nil {
		t.Fatalf("读取响应头失败: %v", err)
	}
	defer resp.Body.Close()

	// 状态码应为 200（upstream 写出的状态码），不应被改为 502
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

// TestProxy_ProxyAndTransportReuse 验证固定 Proxy 和底层连接的复用
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

	// 发送多个请求
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
// upstream 收到的 Host 头与目标地址一致
//
// SetURL 将 r.Out.Host 设为空字符串，使 Go HTTP client
// 使用 URL 中的 host 作为 Host 头
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

	// upstream 的 Host 应为目标地址的 host，不是客户端的 Host
	expectedHost := u.Host
	if receivedHost != expectedHost {
		t.Errorf("upstream Host = %q, want %q", receivedHost, expectedHost)
	}
}

// TestProxy_PreserveHost 验证 preserveHost=true 时，包含端口的客户端原始 Host
// 会在 SetURL 之后恢复并原样发送给 upstream
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

// TestProxy_RequestTimeoutAfterHeadersAbortsBody 验证响应头已写出后
// 请求总超时会中断 body 传输但不会改写已发送的状态码
// 与 ResponseHeaderTimeout 不同，这里 upstream 先 Flush 响应头
// 因此客户端观察到部分 body 和 unexpected EOF，而不是网关 504
func TestProxy_RequestTimeoutAfterHeadersAbortsBody(t *testing.T) {
	// upstream 先 Flush 响应头和部分 body，再阻塞 body；总超时触发后
	// 响应状态不能改写为 504，客户端应观察到部分 body 和 unexpected EOF
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

// blockingRoundTripper 是测试专用的 outbound 传输器
// 它不访问网络，而是阻塞等待请求 Context 取消，以确定性验证取消传播
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

// TestProxy_NoRetry 验证代理不实现重试逻辑
//
// 当 upstream 连接失败时，请求只发送一次，不会重试
// 通过统计 upstream 收到的请求数验证
func TestProxy_NoRetry(t *testing.T) {
	// 使用带调用计数的确定性 RoundTripper，既避免关闭端口带来的 TOCTOU 竞态
	// 又能直接证明一次入站请求只触发一次 outbound RoundTrip
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

	// 应返回 502（连接失败），并且只调用一次 RoundTrip，不执行重试
	if rec.Code != http.StatusBadGateway {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("RoundTrip 调用次数 = %d, want 1", got)
	}
}

// TestProxy_BodyForwarding 验证不同大小的请求体被完整转发
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

// TestProxy_ResponseHeadersForwarded 验证普通 upstream 响应头被转发，同时剥离
// upstream 伪造的 X-Request-ID；外层中间件预先设置响应 ID
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
// hop-by-hop 头被从转发到客户端的响应中移除
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

// TestProxy_RootPathForwarding 验证根路径的转发行为
func TestProxy_RootPathForwarding(t *testing.T) {
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

	// 测试根路径
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:54321"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if receivedPath != "/" {
		t.Errorf("upstream path = %q, want %q", receivedPath, "/")
	}
}

// TestProxy_UpstreamStatusesPassThrough 验证 4xx/5xx 业务响应不变成网关错误 JSON
func TestProxy_UpstreamStatusesPassThrough(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "not-found", status: http.StatusNotFound, body: "not found"},
		{name: "server-error", status: http.StatusInternalServerError, body: "internal server error"},
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, test := range tests {
			if r.URL.Path == "/"+test.name {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	tr := transport.New()
	t.Cleanup(tr.CloseIdleConnections)
	proxy := New(target, tr, 10*time.Second, false)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/"+test.name, nil))
			if recorder.Code != test.status || recorder.Body.String() != test.body {
				t.Fatalf("响应 = %d %q，want %d %q", recorder.Code, recorder.Body.String(), test.status, test.body)
			}
			if got := recorder.Header().Get("Content-Type"); got != "text/plain" {
				t.Errorf("Content-Type = %q, want text/plain", got)
			}
		})
	}
}

// TestProxy_WebSocketUpgradeFailureForwarded 验证 Upgrade 请求由 ReverseProxy
// 正常交给 upstream；upstream 拒绝升级时透传其 400，而不是 Gateway 崩溃或改写
// 本测试只覆盖协议升级握手，不验证双向 WebSocket 帧传输
func TestProxy_WebSocketUpgradeFailureForwarded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟 WebSocket 升级失败
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

	// 应正常转发（upstream 返回 400），不崩溃
	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestProxy_ConcurrentRequests 验证并发请求的正确性
//
// 多个 goroutine 同时通过同一 Proxy 发送请求
// 验证 Proxy 是并发安全的
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
