// Package proxy 封装固定目标 ReverseProxy 与出站请求超时和错误分类
package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

// Proxy 在应用生命周期内复用固定目标的 ReverseProxy 和共享 Transport
type Proxy struct {
	rp *httputil.ReverseProxy

	// timeout 限制出站代理调用；客户端请求体读取可能在取消后继续阻塞
	timeout time.Duration

	// preserveHost 决定上游请求使用客户端 Host 还是目标地址的 Host
	preserveHost bool
}

// New 为固定目标创建 Proxy，并共享调用方提供的 RoundTripper
// 调用方需提供非 nil 的 target、rt 和正数 timeout；本函数不校验这些前提
func New(target *url.URL, rt http.RoundTripper, timeout time.Duration, preserveHost bool) *Proxy {
	// 保留目标 URL 的独立值，避免代理持有调用方的可变指针
	t := *target

	p := &Proxy{
		timeout:      timeout,
		preserveHost: preserveHost,
	}

	rp := &httputil.ReverseProxy{
		// nil Transport 会使 ReverseProxy 回退到 http.DefaultTransport
		Transport: rt,

		// Rewrite 收到的出站请求已由标准库移除逐跳 Header 和部分转发 Header
		Rewrite: func(r *httputil.ProxyRequest) {
			// SetURL 清空 Out.Host；默认使用目标地址的 Host
			r.SetURL(&t)

			// SetURL 之后才能恢复客户端 Host
			if p.preserveHost {
				r.Out.Host = r.In.Host
			}

			// SetXForwarded 使用直接连接的来源地址；这里没有可信前置代理策略
			r.SetXForwarded()

			// 标准库保留的其他 X-Forwarded-* 字段仍可来自客户端
			clearExtraForwardedHeaders(r.Out.Header)
		},

		// 删除上游提供的 Request ID，避免覆盖网关已设置的值
		// 1xx 后标准库会清空响应 Header，当前路径不会重新设置该值
		ModifyResponse: func(responseMessage *http.Response) error {
			responseMessage.Header.Del(requestctx.RequestIDHeader)
			return nil
		},

		// ErrorHandler 只处理最终响应头写出前的代理错误；body copy 错误由标准库中断连接
		// 后者可能经 ReverseProxy 的默认 ErrorLog 写出未经筛选的错误文本
		ErrorHandler: errorHandler,
	}

	p.rp = rp
	return p
}

// ServeHTTP 将请求 Context 的 deadline 传给 ReverseProxy
// 响应已开始时发生超时，客户端可能只收到部分响应；停滞的客户端上传可能在取消后继续阻塞
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 派生自入站 Context，使客户端取消能传播到上游
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()

	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// errorHandler 只在最终响应头写出前分类代理错误
// 请求体超限返回 413，超时返回 504，其余代理错误返回 502；客户端取消只记录状态
func errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		if metadata, ok := requestctx.FromContext(r.Context()); ok {
			metadata.SetErrorKind("CLIENT_CANCELED")
			metadata.SetObservedStatus(499)
		}
		return
	}

	// Guard 的流式 reader 在读取时返回 *http.MaxBytesError
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		response.WriteError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request body too large")
		return
	}

	if errors.Is(err, context.DeadlineExceeded) {
		response.WriteError(w, r, http.StatusGatewayTimeout, "GATEWAY_TIMEOUT", "upstream request timed out")
		return
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		response.WriteError(w, r, http.StatusGatewayTimeout, "GATEWAY_TIMEOUT", "upstream request timed out")
		return
	}

	response.WriteError(w, r, http.StatusBadGateway, "BAD_GATEWAY", "upstream request failed")
}

// clearExtraForwardedHeaders 保留 SetXForwarded 设置的三个字段并清除其他变体
func clearExtraForwardedHeaders(h http.Header) {
	for key := range h {
		if !strings.EqualFold(key, "Forwarded") &&
			strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {

			lk := strings.ToLower(key)
			if lk == "x-forwarded-for" || lk == "x-forwarded-host" || lk == "x-forwarded-proto" {
				continue
			}
			h.Del(key)
		}
	}
}
