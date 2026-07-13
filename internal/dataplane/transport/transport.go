// Package transport 提供网关数据面的应用级共享 HTTP Transport。
//
// 职责：
//   - 创建一个配置了安全默认超时和连接池参数的 *http.Transport；
//   - 该 Transport 在整个应用生命周期内被所有代理请求复用；
//   - 提供关闭 idle 连接的方法，在 Graceful Shutdown 时调用。
//
// 非职责：
//   - 不实现自定义 Dial 逻辑（如服务发现）；
//   - 不实现 TLS 证书管理（使用默认 TLS 配置）；
//   - 不实现连接级熔断或重试。
//
// 设计意图：
//   - 复用 Transport 是反向代理性能的基础。每个请求创建新 Transport
//     会导致 TCP 连接无法复用，增加延迟和系统资源消耗；
//   - 安全超时防止慢 upstream 导致连接和 goroutine 泄漏；
//   - 连接池限制防止突发流量耗尽文件描述符。
package transport

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// Transport 安全默认值常量。
//
// 这些值是经过权衡的安全默认值：
//   - Dial 超时防止长时间等待 TCP 连接建立；
//   - TLS handshake 超时防止慢 TLS 协商阻塞请求；
//   - ResponseHeader 超时防止 upstream 接受连接但不返回响应头；
//   - IdleConn 超时确保空闲连接被及时回收，避免连接泄漏；
//   - 连接池参数控制每个 upstream 的最大连接数，防止资源耗尽。
const (
	// dialTimeout 是 TCP 连接建立的最大等待时长。
	dialTimeout = 10 * time.Second

	// tlsHandshakeTimeout 是 TLS 握手的最大等待时长。
	tlsHandshakeTimeout = 10 * time.Second

	// responseHeaderTimeout 是从发送请求到收到响应头的最大等待时长。
	// 如果 upstream 在此时间内未返回响应头，Transport 会返回超时错误。
	// 该超时独立于请求级总超时，用于快速检测慢 upstream。
	responseHeaderTimeout = 30 * time.Second

	// idleConnTimeout 是空闲连接在连接池中保持的最大时长。
	// 超时后连接被关闭，防止连接泄漏和 stale connection 问题。
	idleConnTimeout = 90 * time.Second

	// maxIdleConns 是连接池中所有 host 的最大空闲连接总数。
	maxIdleConns = 100

	// maxIdleConnsPerHost 是每个 host 的最大空闲连接数。
	// 默认值 http.DefaultMaxIdleConnsPerHost=2 过小，会导致连接复用率低。
	maxIdleConnsPerHost = 20

	// maxConnsPerHost 是每个 host 的最大活跃连接数（包括空闲和正在使用的）。
	// 设置有限上限，防止突发流量耗尽文件描述符；超出上限的请求会等待可用连接。
	maxConnsPerHost = 100
)

// New 创建一个配置了安全默认值的 *http.Transport。
//
// 返回的 Transport 应在整个应用生命周期内复用，不应为每个请求创建新实例。
// 在应用 Shutdown 时应调用 CloseIdleConnections 释放空闲连接。
//
// 配置说明：
//   - 使用 net.Dialer 设置 Dial 超时和 KeepAlive；
//   - ForceAttemptHTTP2 尝试 HTTP/2（如果 upstream 支持）；
//   - 连接池参数确保合理的连接复用率；
//   - TLS 使用默认配置（验证证书）。
func New() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		// DialContext 控制 TCP 连接建立。
		DialContext: dialer.DialContext,

		// TLSClientConfig 使用默认配置，验证 upstream 证书。
		// Phase 1 不支持自定义 CA 或跳过验证。
		TLSClientConfig: &tls.Config{
			// MinVersion 强制最低 TLS 1.2，拒绝不安全的旧版本。
			MinVersion: tls.VersionTLS12,
		},

		// TLSHandshakeTimeout 限制 TLS 握手时长。
		TLSHandshakeTimeout: tlsHandshakeTimeout,

		// ResponseHeaderTimeout 限制从请求发出到收到响应头的时长。
		// 超时后 RoundTrip 返回 net.Error（Timeout() == true）。
		ResponseHeaderTimeout: responseHeaderTimeout,

		// IdleConnTimeout 控制空闲连接的回收。
		IdleConnTimeout: idleConnTimeout,

		// 连接池参数。
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		MaxConnsPerHost:     maxConnsPerHost,

		// ForceAttemptHTTP2 在使用 HTTPS 时尝试 HTTP/2 协议协商。
		// 对于 HTTP upstream，Transport 会使用 HTTP/1.1。
		ForceAttemptHTTP2: true,
	}
}

// CloseIdleConnections 关闭 Transport 连接池中的所有空闲连接。
//
// 应在应用 Graceful Shutdown 时调用，确保不遗留空闲 TCP 连接。
// 正在使用的活跃连接不会被关闭，它们会在请求完成后自然释放。
func CloseIdleConnections(t *http.Transport) {
	if t != nil {
		t.CloseIdleConnections()
	}
}
