// Package gateway 实现网关数据面的请求分发层。
//
// 职责：
//   - 调用 router.ParsePath 执行路径安全检查（非法路径返回 400）；
//   - 调用 router.Router.Match 执行路由匹配（无匹配返回 404）；
//   - 根据 MatchResult 选择预创建的固定目标 Proxy 并委托转发；
//   - 不在 ReverseProxy Rewrite 内执行路由匹配（Rewrite 没有 ResponseWriter，无法返回 400/404）。
//
// 非职责：
//   - 不实现负载均衡（Phase 4）；
//   - 不实现路径 rewrite（延后）；
//   - 不实现限流、重试或熔断。
package gateway

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/proxy"
	"github.com/helantianshen/gateway/internal/dataplane/response"
	"github.com/helantianshen/gateway/internal/router"
)

// proxyKey 标识一个固定目标 Proxy：upstream ID 决定目标地址，preserveHost 决定
// 是否保留客户端原始 Host 头。相同 key 的 Proxy 在启动时创建一次，请求热路径只做查表。
type proxyKey struct {
	upstreamID   string
	preserveHost bool
}

// GatewayHandler 是网关数据面的入口 HTTP Handler。
//
// 它持有编译后的 Router 和按 {upstreamID, preserveHost} 预创建的 Proxy map。
// 所有 Proxy 共享同一个 Transport，禁止每请求创建 Proxy。
// 编译后的 Router 不可变，可被并发安全读取。
type GatewayHandler struct {
	router  *router.Router
	proxies map[proxyKey]*proxy.Proxy
}

type routeMatchContextKey struct{}

// MatchResultFromContext 返回 GatewayHandler 为当前请求选出的路由结果。
// 下游中间件和自定义 RoundTripper 可读取 RouteID、UpstreamID 和路径参数；返回值
// 仅供读取，不应修改其中的 Params map。
func MatchResultFromContext(ctx context.Context) (*router.MatchResult, bool) {
	result, ok := ctx.Value(routeMatchContextKey{}).(*router.MatchResult)
	return result, ok
}

// NewGatewayHandler 根据 Router、upstream URL map、共享 Transport 和请求超时创建 GatewayHandler。
//
// 为每个 upstream 创建两个 Proxy（preserveHost=true 和 false），即使某些组合
// 在路由配置中未被使用。Proxy 数量最多为 2×len(upstreamURLs)，在启动时一次性分配，
// 不影响运行时性能。
//
// 如果某个 upstream ID 在 upstreamURLs 中不存在，对应的路由匹配会在运行时返回 502。
func NewGatewayHandler(r *router.Router, upstreamURLs map[string]*url.URL, tr http.RoundTripper, timeout time.Duration) *GatewayHandler {
	proxies := make(map[proxyKey]*proxy.Proxy, len(upstreamURLs)*2)
	for upstreamID, target := range upstreamURLs {
		proxies[proxyKey{upstreamID: upstreamID, preserveHost: false}] = proxy.New(target, tr, timeout, false)
		proxies[proxyKey{upstreamID: upstreamID, preserveHost: true}] = proxy.New(target, tr, timeout, true)
	}
	return &GatewayHandler{
		router:  r,
		proxies: proxies,
	}
}

// ServeHTTP 处理每个进入网关的请求。
//
// 处理流程：
//  1. router.ParsePath 执行路径安全检查（%XX、编码斜杠、dot segment）。非法路径返回 400。
//  2. router.Router.Match 执行路由匹配（Host → Method → Path specificity）。
//     无匹配返回 404。
//  3. 根据 MatchResult 的 UpstreamID 和 PreserveHost 选择预创建的 Proxy。
//     如果 upstream 未配置（配置错误），返回 502。
//  4. 委托给 Proxy.ServeHTTP，由 Proxy 负责超时、Rewrite、转发头和 502/504。
//
// 路由匹配只读遍历不可变树，不修改请求。Proxy 在 Rewrite 中设置目标 URL 和转发头。
func (h *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 步骤 1：路径安全检查。ParsePath 内部执行 EscapedPath → 验证 %XX →
	// 拒绝编码斜杠 → 分段 → PathUnescape → UTF-8 → 拒绝 dot segment。
	segments, matchErr := router.ParsePath(r)
	if matchErr != nil {
		response.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "请求路径非法")
		return
	}

	// 步骤 2：路由匹配。Match 按 Host specificity → Method specificity →
	// 逐段 Path specificity → priority 的固定顺序选择唯一最优路由。
	result, matchErr := h.router.Match(r.Host, r.Method, segments)
	if matchErr != nil {
		response.WriteError(w, http.StatusNotFound, "NOT_FOUND", "没有匹配的路由")
		return
	}

	// 步骤 3：选择预创建的固定目标 Proxy。
	// MatchResult 中的 UpstreamID 和 PreserveHost 直接作为查表 key。
	p, ok := h.proxies[proxyKey{upstreamID: result.UpstreamID, preserveHost: result.PreserveHost}]
	if !ok {
		// 理论上不可达：config.Validate 已校验 route.upstream 引用存在，
		// 且 NewGatewayHandler 为每个 upstream 创建了所有 preserveHost 组合。
		// 仍保留防御性错误以防未来配置逻辑偏移。
		response.WriteError(w, http.StatusBadGateway, "BAD_GATEWAY", "upstream 未配置")
		return
	}

	// 将匹配结果写入请求 Context，供后续中间件、日志和自定义 RoundTripper 读取。
	// 不通过 Header 传递，避免把网关内部路由元数据泄露给 upstream。
	r = r.WithContext(context.WithValue(r.Context(), routeMatchContextKey{}, result))

	// 步骤 4：委托给 Proxy。
	// Proxy 负责超时、Rewrite（SetURL + 转发头 + preserveHost）、502/504 和流式响应。
	p.ServeHTTP(w, r)
}
