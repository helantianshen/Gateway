package transport

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestNew_ReturnsValidTransport 固定 Transport 的超时、池容量和 TLS 参数
func TestNew_ReturnsValidTransport(t *testing.T) {
	tr := New()

	if tr == nil {
		t.Fatal("New 返回 nil")
	}

	// 验证关键超时参数
	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout, tlsHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	if tr.IdleConnTimeout != idleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, idleConnTimeout)
	}

	// 验证连接池参数
	if tr.MaxIdleConns != maxIdleConns {
		t.Errorf("MaxIdleConns = %v, want %v", tr.MaxIdleConns, maxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %v, want %v", tr.MaxIdleConnsPerHost, maxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != maxConnsPerHost {
		t.Errorf("MaxConnsPerHost = %v, want %v", tr.MaxConnsPerHost, maxConnsPerHost)
	}

	// 验证 HTTP/2 尝试已启用
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}

	// 验证 DialContext 已设置（非 nil）
	if tr.DialContext == nil {
		t.Error("DialContext 为 nil")
	}

	// 验证 TLSClientConfig 已设置且强制最低 TLS 1.2
	if tr.TLSClientConfig == nil {
		t.Error("TLSClientConfig 为 nil")
	} else if tr.TLSClientConfig.MinVersion < 0x0303 {
		t.Errorf("TLS MinVersion = %x, want >= 0x0303 (TLS 1.2)", tr.TLSClientConfig.MinVersion)
	}
}

// TestCloseIdleConnections_ClosesConnections 验证 CloseIdleConnections
// 关闭已建立的空闲连接，且后续请求会建立新连接
func TestCloseIdleConnections_ClosesConnections(t *testing.T) {
	var mu sync.Mutex
	states := make(map[net.Conn]http.ConnState)
	newConnections := 0

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		states[conn] = state
		if state == http.StateNew {
			newConnections++
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	tr := New()
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	doRequest := func() {
		t.Helper()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			resp.Body.Close()
			t.Fatalf("读取响应失败: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("关闭响应体失败: %v", err)
		}
	}

	doRequest()
	waitForConnState(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, state := range states {
			if state == http.StateIdle {
				return true
			}
		}
		return false
	}, "首个连接未进入 idle 状态")

	CloseIdleConnections(tr)
	waitForConnState(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, state := range states {
			if state == http.StateClosed {
				return true
			}
		}
		return false
	}, "CloseIdleConnections 后连接未关闭")

	doRequest()
	mu.Lock()
	gotConnections := newConnections
	mu.Unlock()
	if gotConnections != 2 {
		t.Fatalf("TCP 连接数 = %d, want 2（关闭 idle 连接后应新建连接）", gotConnections)
	}
}

func waitForConnState(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}
