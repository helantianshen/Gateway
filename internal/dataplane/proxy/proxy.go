// Package proxy 实现网关数据面的反向代理核心逻辑。
//
// 职责：
//   - 基于 httputil.ReverseProxy 构建反向代理 Handler；
//   - 使用 Rewrite 模式（非旧式 Director）设置目标 URL 和转发头；
//   - 清除客户端伪造的 Forwarded 和 X-Forwarded-* 头后重新设置；
//   - 对每个请求应用总超时，超时返回 504，网络/协议错误返回 502；
//   - 所有请求复用同一个 ReverseProxy 实例和底层 Transport。
//
// 非职责：
//   - 不实现重试、熔断或负载均衡；
//   - 不实现路由匹配（所有请求转发到同一 upstream）；
//   - 不实现请求/响应内容改写。
//
// 设计意图：
//   - 直接使用标准库 httputil.ReverseProxy，不自造代理引擎；
//   - 只使用 Rewrite（Go 1.20+），不使用已废弃的 Director；
//   - 错误分类基于错误类型而非错误文本，确保分类稳定。
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

	"github.com/helantianshen/gateway/internal/dataplane/response"
)

// Proxy 封装 httputil.ReverseProxy，提供反向代理 HTTP Handler。
//
// 一个 Proxy 实例在应用生命周期内创建一次，所有请求复用。
// 内部的 *httputil.ReverseProxy 和 http.RoundTripper 均为并发安全，
// 不需要额外同步。
type Proxy struct {
	// rp 是底层 httputil.ReverseProxy 实例。
	// 配置了 Rewrite 和 ErrorHandler，复用传入的 Transport。
	rp *httputil.ReverseProxy

	// timeout 是单次请求的总超时时长。
	// 通过为每个请求创建带超时的 Context 实现。
	// 超时触发后 Context 被取消，RoundTrip 返回 DeadlineExceeded 错误，
	// ErrorHandler 据此返回 504。
	timeout time.Duration
}

// New 创建一个 Proxy 实例。
//
// 参数：
//   - target: upstream 目标地址，所有请求将转发到此地址；
//   - rt: 共享的 http.RoundTripper（通常是 *http.Transport），在所有请求间复用；
//   - timeout: 单次请求的总超时时长，必须为正值。
//
// target 会被浅拷贝，不会修改调用方传入的 url.URL。
// rt 必须非 nil，否则代理无法发送请求。
func New(target *url.URL, rt http.RoundTripper, timeout time.Duration) *Proxy {
	// 浅拷贝 target，避免修改调用方的 url.URL。
	// SetURL 会修改 ProxyRequest.Out.URL 的 Scheme/Host/Path，
	// 但不会修改 target 本身，这里拷贝是为了清晰和安全。
	t := *target

	rp := &httputil.ReverseProxy{
		// Transport 是所有 upstream 请求使用的 RoundTripper。
		// 必须非 nil，否则 httputil.ReverseProxy 会使用 http.DefaultTransport，
		// 这会绕过应用级共享 Transport，导致连接无法统一管理。
		Transport: rt,

		// Rewrite 使用新式 API 设置目标 URL 和转发头。
		// Go 标准库在调用 Rewrite 之前已经自动：
		//   1. 克隆请求（包括 Context）；
		//   2. 移除 hop-by-hop 头（Connection、Keep-Alive 等）；
		//   3. 移除 Forwarded、X-Forwarded-For、X-Forwarded-Host、X-Forwarded-Proto。
		Rewrite: func(r *httputil.ProxyRequest) {
			// SetURL 设置 outbound 请求的 scheme、host 和 path。
			// 它会将 r.Out.Host 设为空字符串，使 outbound 请求的 Host
			// 头自动使用目标地址的 host（即 "默认让 upstream Host 与目标地址一致"）。
			r.SetURL(&t)

			// SetXForwarded 设置正确的转发头：
			//   - X-Forwarded-For: 客户端真实 IP；
			//   - X-Forwarded-Host: 客户端请求的原始 Host；
			//   - X-Forwarded-Proto: 客户端请求的协议 (http/https)。
			//
			// 由于标准库在调用 Rewrite 前已清除这三个头，
			// SetXForwarded 设置的是干净的值，不会被客户端伪造值污染。
			r.SetXForwarded()

			// 额外清除其他可能的 X-Forwarded-* 变体头（如 X-Forwarded-Server、
			// X-Forwarded-Port 等），防止客户端注入非标准转发信息。
			// 标准库只清除三个主要的 X-Forwarded-* 头，这里补充清除其他变体。
			clearExtraForwardedHeaders(r.Out.Header)
		},

		// ErrorHandler 处理 upstream 请求失败的情况。
		// 该回调仅在响应头尚未写出时被调用（RoundTrip 错误、ModifyResponse 错误等）。
		// 响应头已写出后的 body copy 错误不会触发此回调，
		// 而是由标准库 panic(http.ErrAbortHandler) 中断连接，
		// 不会尝试重写状态码（满足"响应头写出后不二次改状态"的要求）。
		ErrorHandler: errorHandler,
	}

	return &Proxy{
		rp:      rp,
		timeout: timeout,
	}
}

// ServeHTTP 实现 http.Handler 接口，处理每个进入网关的请求。
//
// 每个请求的处理流程：
//  1. 基于请求的 Context 创建带总超时的新 Context；
//  2. 将带超时的 Context 附加到请求上，传递给 ReverseProxy；
//  3. ReverseProxy 的 Rewrite 设置目标 URL 和转发头；
//  4. ReverseProxy 使用共享 Transport 发送请求到 upstream；
//  5. 成功时透传 upstream 响应；失败时 ErrorHandler 返回 502 或 504。
//
// 超时机制说明：
//   - 总超时通过 context.WithTimeout 实现，覆盖从请求到达到 upstream 响应完成的整个链路；
//   - 超时触发后 Context 被取消，RoundTrip 返回 DeadlineExceeded 错误；
//   - 对于流式响应（SSE），超时触发时响应头可能已经写出，
//     此时 body copy 会因 Context 取消而失败，标准库 panic(http.ErrAbortHandler)
//     中断连接，客户端收到已写出的部分响应。
//
// Context 取消传播：
//   - 客户端取消请求时，原始 r.Context() 被取消；
//   - 超时 Context 派生自 r.Context()，因此也会被取消；
//   - ReverseProxy 使用此 Context 发送 upstream 请求，
//     upstream 端的 r.Context() 也会被取消，实现取消传播。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 基于原始请求 Context 创建带总超时的 Context。
	// 派生自 r.Context() 而非 context.Background()，
	// 确保客户端取消请求时超时 Context 也会被取消。
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()

	// 将带超时的 Context 附加到请求上。
	// ReverseProxy 会使用此 Context 作为 upstream 请求的 Context。
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// errorHandler 是 httputil.ReverseProxy 的错误处理回调。
//
// 调用时机（仅在响应头尚未写出时）：
//   - RoundTrip 返回错误（upstream 连接失败、TLS 错误、超时等）；
//   - ModifyResponse 返回错误（Phase 1 未设置 ModifyResponse）；
//   - 协议升级错误。
//
// 错误分类：
//   - 超时类错误（Context DeadlineExceeded、net.Error Timeout）→ 504；
//   - 其他所有错误（连接拒绝、DNS 失败、协议错误等）→ 502。
//
// 安全性：
//   - 不在响应中暴露 upstream 地址或底层错误文本；
//   - 使用通用错误消息，防止信息泄漏。
//   - 客户端取消（context.Canceled）不写错误响应，因为客户端已断开。
func errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	// 客户端主动取消请求时，Context 被取消。
	// 此时客户端已断开连接，写错误响应没有意义且可能导致日志噪声。
	if errors.Is(err, context.Canceled) {
		return
	}

	// 判断是否为超时错误。
	// context.DeadlineExceeded：请求总超时或 Transport 的 ResponseHeaderTimeout 触发。
	if errors.Is(err, context.DeadlineExceeded) {
		response.WriteError(w, http.StatusGatewayTimeout, "GATEWAY_TIMEOUT", "upstream request timed out")
		return
	}

	// net.Error 的 Timeout() 方法返回 true 时表示网络层超时
	//（如 Dial 超时、TLS 握手超时、ResponseHeader 超时）。
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		response.WriteError(w, http.StatusGatewayTimeout, "GATEWAY_TIMEOUT", "upstream request timed out")
		return
	}

	// 所有其他错误归类为 502 Bad Gateway。
	// 包括但不限于：连接被拒绝、DNS 解析失败、TLS 证书验证失败、协议错误。
	// 不暴露底层错误详情，使用通用消息。
	response.WriteError(w, http.StatusBadGateway, "BAD_GATEWAY", "upstream request failed")
}

// clearExtraForwardedHeaders 清除客户端注入的非标准 X-Forwarded-* 头。
//
// SetXForwarded 已设置可信的 X-Forwarded-For、X-Forwarded-Host 和
// X-Forwarded-Proto；本函数保留这三个标准头，仅删除其他变体。
func clearExtraForwardedHeaders(h http.Header) {
	for key := range h {
		if !strings.EqualFold(key, "Forwarded") &&
			strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
			// 跳过三个标准转发头，它们由 SetXForwarded 正确设置。
			lk := strings.ToLower(key)
			if lk == "x-forwarded-for" || lk == "x-forwarded-host" || lk == "x-forwarded-proto" {
				continue
			}
			h.Del(key)
		}
	}
}
