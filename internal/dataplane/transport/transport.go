// Package transport 提供应用内共享的上游 HTTP 连接池
package transport

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

const (
	// dialTimeout 是 TCP 连接建立的最大等待时长
	dialTimeout = 10 * time.Second

	// tlsHandshakeTimeout 是 TLS 握手的最大等待时长
	tlsHandshakeTimeout = 10 * time.Second

	// responseHeaderTimeout 从写完请求及请求体后开始计算，不限制请求体上传
	responseHeaderTimeout = 30 * time.Second

	// idleConnTimeout 限制连接池中空闲连接的保留时间
	idleConnTimeout = 90 * time.Second

	// maxIdleConns 是连接池中所有 host 的最大空闲连接总数
	maxIdleConns = 100

	// maxIdleConnsPerHost 是每个 host 的最大空闲连接数
	maxIdleConnsPerHost = 20

	// maxConnsPerHost 包含拨号中、活跃和空闲连接；超出上限的请求等待连接
	maxConnsPerHost = 100
)

// New 创建应用内共享的 Transport，调用方负责在停机时关闭空闲连接
// 未启用环境代理；HTTPS 保留证书验证并尝试 HTTP/2
func New() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		DialContext: dialer.DialContext,

		// TLSClientConfig 保留系统证书验证，最低接受 TLS 1.2
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},

		TLSHandshakeTimeout: tlsHandshakeTimeout,

		ResponseHeaderTimeout: responseHeaderTimeout,

		IdleConnTimeout: idleConnTimeout,

		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		MaxConnsPerHost:     maxConnsPerHost,

		ForceAttemptHTTP2: true,
	}
}

// CloseIdleConnections 只关闭空闲连接，不中断仍在使用的连接
func CloseIdleConnections(t *http.Transport) {
	if t != nil {
		t.CloseIdleConnections()
	}
}
