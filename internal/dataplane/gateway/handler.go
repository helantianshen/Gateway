// Package gateway 负责请求路由、上游选择和代理调用
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/policy"
	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
	"github.com/helantianshen/gateway/internal/dataplane/upstream"
	"github.com/helantianshen/gateway/internal/router"
)

// GatewayHandler 是网关数据面的入口 HTTP Handler
type GatewayHandler struct {
	router        *router.Router
	upstreams     map[string]*upstream.CompiledUpstream
	routePolicies map[string]*policy.CompiledChain
}

type routeMatchContextKey struct{}
type upstreamSelectionContextKey struct{}

// UpstreamSelection 是本次请求实际选中的逻辑 upstream 和物理 endpoint 身份
type UpstreamSelection struct {
	UpstreamID string
	EndpointID string
}

// MatchResultFromContext 返回 GatewayHandler 为当前请求选出的路由结果
func MatchResultFromContext(ctx context.Context) (*router.MatchResult, bool) {
	result, ok := ctx.Value(routeMatchContextKey{}).(*router.MatchResult)
	return result, ok
}

// UpstreamSelectionFromContext 返回 GatewayHandler 为当前请求实际选择的 endpoint
func UpstreamSelectionFromContext(ctx context.Context) (UpstreamSelection, bool) {
	selection, ok := ctx.Value(upstreamSelectionContextKey{}).(UpstreamSelection)
	return selection, ok
}

// NewGatewayHandler 创建不带具体 route policy 的 Handler，供低层测试和简单装配使用
func NewGatewayHandler(r *router.Router, upstreams map[string]*upstream.CompiledUpstream) *GatewayHandler {
	compiledUpstreams := make(map[string]*upstream.CompiledUpstream, len(upstreams))
	for id, compiled := range upstreams {
		compiledUpstreams[id] = compiled
	}
	return &GatewayHandler{
		router:        r,
		upstreams:     compiledUpstreams,
		routePolicies: make(map[string]*policy.CompiledChain),
	}
}

// NewGatewayHandlerWithPolicies 在启动期为每个 route 编译不可变策略链
// 当前 route middleware 为空，CompiledChain 仍保持独立路由入口
func NewGatewayHandlerWithPolicies(
	r *router.Router,
	upstreams map[string]*upstream.CompiledUpstream,
	routeMiddlewares map[string][]policy.Middleware,
) (*GatewayHandler, error) {
	handler := NewGatewayHandler(r, upstreams)
	for routeID, middlewares := range routeMiddlewares {
		chain, err := policy.Compile(http.HandlerFunc(handler.forward), middlewares)
		if err != nil {
			return nil, fmt.Errorf("编译路由 %q 策略链失败: %w", routeID, err)
		}
		handler.routePolicies[routeID] = chain
	}
	return handler, nil
}

// ServeHTTP 执行安全路径解析、路由匹配和 route policy chain
func (h *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segments, matchErr := router.ParsePath(r)
	if matchErr != nil {
		response.WriteError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求路径非法")
		return
	}

	result, matchErr := h.router.Match(r.Host, r.Method, segments)
	if matchErr != nil {
		switch matchErr.Code {
		case router.MatchErrIllegalHost, router.MatchErrIllegalPath:
			response.WriteError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求 Host 或路径非法")
		case router.MatchErrMethodNotAllowed:
			w.Header().Set("Allow", strings.Join(matchErr.AllowedMethods, ", "))
			response.WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "请求方法不允许")
		default:
			response.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "没有匹配的路由")
		}
		return
	}

	// 在 policy 前写入路由结果：策略和外层日志读取同一份编译期身份，不重新匹配 URL
	ctx := context.WithValue(r.Context(), routeMatchContextKey{}, result)
	r = r.WithContext(ctx)
	if metadata, ok := requestctx.FromContext(ctx); ok {
		metadata.SetRoute(result.RouteID, result.PathTemplate, result.UpstreamID)
	}

	if chain, ok := h.routePolicies[result.RouteID]; ok {
		chain.ServeHTTP(w, r)
		return
	}
	h.forward(w, r)
}

// forward 在 route policy 通过后选择 endpoint 并执行固定目标 Proxy
func (h *GatewayHandler) forward(w http.ResponseWriter, r *http.Request) {
	// 路由阶段已把 MatchResult 放入 Context；缺失表示内部链路未正确装配
	result, ok := MatchResultFromContext(r.Context())
	if !ok || result == nil {
		response.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "route context unavailable")
		return
	}

	compiledUpstream, ok := h.upstreams[result.UpstreamID]
	if !ok || compiledUpstream == nil {
		response.WriteError(w, r, http.StatusBadGateway, "BAD_GATEWAY", "upstream 未配置")
		return
	}
	// 每次请求只选一个健康 endpoint；无健康节点与运行时装配错误分别响应
	endpoint, selectErr := compiledUpstream.Select()
	if errors.Is(selectErr, upstream.ErrNoHealthyEndpoint) {
		response.WriteError(w, r, http.StatusServiceUnavailable, "NO_HEALTHY_UPSTREAM", "upstream 暂无健康 endpoint")
		return
	}
	if selectErr != nil || endpoint == nil {
		response.WriteError(w, r, http.StatusBadGateway, "BAD_GATEWAY", "upstream 选择失败")
		return
	}

	// 选择结果供内层代理与外层观测共享，避免重新选择或从请求 URL 推断 endpoint
	ctx := context.WithValue(r.Context(), upstreamSelectionContextKey{}, UpstreamSelection{
		UpstreamID: result.UpstreamID,
		EndpointID: endpoint.ID(),
	})
	r = r.WithContext(ctx)
	metadata, _ := requestctx.FromContext(ctx)
	if metadata != nil {
		metadata.SetEndpoint(endpoint.ID())
	}

	if served := serveEndpoint(endpoint, w, r, result.PreserveHost, metadata); !served {
		response.WriteError(w, r, http.StatusBadGateway, "BAD_GATEWAY", "upstream 代理未配置")
	}
}

func serveEndpoint(
	endpoint *upstream.CompiledEndpoint,
	w http.ResponseWriter,
	r *http.Request,
	preserveHost bool,
	metadata *requestctx.RequestContext,
) bool {
	if metadata != nil {
		metadata.BeginUpstream(time.Now())
		defer func() { metadata.FinishUpstream(time.Now()) }()
	}
	return endpoint.ServeHTTP(w, r, preserveHost)
}
