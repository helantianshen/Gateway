// Package gateway 实现网关数据面的请求分发层。
//
// 职责：
//   - 调用 router.ParsePath 执行路径安全检查（非法路径返回 400）；
//   - 调用 router.Router.Match 执行路由匹配（无匹配返回 404）；
//   - 根据逻辑 UpstreamID 执行健康过滤和 Round Robin endpoint 选择；
//   - 把请求委托给 endpoint 上预创建的固定目标 Proxy；
//   - 不在 ReverseProxy Rewrite 内执行路由或负载均衡。
//
// 非职责：
//   - 不实现健康探测；健康状态由 upstream.EndpointState 提供；
//   - 不实现路径 rewrite、重试、限流或熔断。
package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/helantianshen/gateway/internal/dataplane/response"
	"github.com/helantianshen/gateway/internal/dataplane/upstream"
	"github.com/helantianshen/gateway/internal/router"
)

// GatewayHandler 是网关数据面的入口 HTTP Handler。
// 编译后的 Router 与 Upstream 都不会在请求期间修改，可被并发安全读取；每个
// CompiledUpstream 内部仅由原子 Round Robin cursor 和 EndpointState 承载可变状态。
type GatewayHandler struct {
	router    *router.Router
	upstreams map[string]*upstream.CompiledUpstream
}

type routeMatchContextKey struct{}
type upstreamSelectionContextKey struct{}

// UpstreamSelection 是本次请求实际选中的逻辑 upstream 和物理 endpoint 身份。
// 该值只写入进程内 Context，不会作为 Header 发送到 upstream。
type UpstreamSelection struct {
	UpstreamID string
	EndpointID string
}

// MatchResultFromContext 返回 GatewayHandler 为当前请求选出的路由结果。
// 下游中间件和自定义 RoundTripper 可读取 RouteID、UpstreamID 和路径参数；返回值
// 仅供读取，不应修改其中的 Params map。
func MatchResultFromContext(ctx context.Context) (*router.MatchResult, bool) {
	result, ok := ctx.Value(routeMatchContextKey{}).(*router.MatchResult)
	return result, ok
}

// UpstreamSelectionFromContext 返回 GatewayHandler 为当前请求实际选择的 endpoint。
func UpstreamSelectionFromContext(ctx context.Context) (UpstreamSelection, bool) {
	selection, ok := ctx.Value(upstreamSelectionContextKey{}).(UpstreamSelection)
	return selection, ok
}

// NewGatewayHandler 根据不可变 Router 和已编译 upstream map 创建 GatewayHandler。
// map 会被浅拷贝；其中的 CompiledUpstream 指针在 Application 生命周期内保持稳定。
func NewGatewayHandler(r *router.Router, upstreams map[string]*upstream.CompiledUpstream) *GatewayHandler {
	compiledUpstreams := make(map[string]*upstream.CompiledUpstream, len(upstreams))
	for id, compiled := range upstreams {
		compiledUpstreams[id] = compiled
	}
	return &GatewayHandler{router: r, upstreams: compiledUpstreams}
}

// ServeHTTP 处理每个进入网关的请求。
//
// 处理流程：
//  1. ParsePath 执行路径安全检查，非法路径返回 400；
//  2. Router.Match 执行 Host/Method/Path 路由，无匹配返回 404；
//  3. CompiledUpstream.Select 通过 Round Robin 选择健康 endpoint；
//  4. 将 Route 与 Endpoint 身份写入 Context；
//  5. endpoint Proxy 负责 Rewrite、共享 Transport、超时和 502/504。
func (h *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 步骤 1：EscapedPath → 验证 %XX → 拒绝编码斜杠 → 分段 →
	// PathUnescape → UTF-8 → 拒绝 dot segment。
	segments, matchErr := router.ParsePath(r)
	if matchErr != nil {
		response.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "请求路径非法")
		return
	}

	// 步骤 2：Host specificity → Method specificity → Path specificity → priority。
	result, matchErr := h.router.Match(r.Host, r.Method, segments)
	if matchErr != nil {
		response.WriteError(w, http.StatusNotFound, "NOT_FOUND", "没有匹配的路由")
		return
	}

	// 步骤 3：由逻辑 upstream 选择物理 endpoint。route.upstream 引用已在配置校验，
	// 这里仍防御 runtime map 与 Router 编译结果不一致。
	compiledUpstream, ok := h.upstreams[result.UpstreamID]
	if !ok || compiledUpstream == nil {
		response.WriteError(w, http.StatusBadGateway, "BAD_GATEWAY", "upstream 未配置")
		return
	}
	endpoint, selectErr := compiledUpstream.Select()
	if errors.Is(selectErr, upstream.ErrNoHealthyEndpoint) {
		response.WriteError(w, http.StatusServiceUnavailable, "NO_HEALTHY_UPSTREAM", "upstream 暂无健康 endpoint")
		return
	}
	if selectErr != nil || endpoint == nil {
		response.WriteError(w, http.StatusBadGateway, "BAD_GATEWAY", "upstream 选择失败")
		return
	}

	// 步骤 4：Context 元数据供日志、指标和自定义 RoundTripper 读取；不通过 Header
	// 传递，避免把内部 route/endpoint ID 泄露给业务服务。
	ctx := context.WithValue(r.Context(), routeMatchContextKey{}, result)
	ctx = context.WithValue(ctx, upstreamSelectionContextKey{}, UpstreamSelection{
		UpstreamID: result.UpstreamID,
		EndpointID: endpoint.ID(),
	})
	r = r.WithContext(ctx)

	// 步骤 5：调用创建期固定 target 和 Host 模式的 Proxy。false 理论上不可达，
	// 表示 bootstrap 未根据路由表编译所需的 preserveHost 模式。
	if served := endpoint.ServeHTTP(w, r, result.PreserveHost); !served {
		response.WriteError(w, http.StatusBadGateway, "BAD_GATEWAY", "upstream 代理未配置")
	}
}
