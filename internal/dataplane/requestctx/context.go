// Package requestctx 提供网关单次请求内共享的可变元数据。
//
// RequestContext 与请求一一对应，最外层中间件创建后通过 context.Context 传递。
// Router、Gateway、Proxy、Recovery 和观测层修改同一个对象，使外层 Access Log 在
// Handler 返回后可以读取内层选择出的 Route、Upstream 和 Endpoint，而无需重新匹配。
package requestctx

import (
	"context"
	"time"
)

type contextKey struct{}

// RequestContext 保存一次请求的受控观测元数据。
// 它不能跨请求复用，也不能放入全局 Registry 或配置快照。
type RequestContext struct {
	requestID     string
	traceID       string
	configVersion uint64

	routeID      string
	pathTemplate string
	upstreamID   string
	endpointID   string
	attempts     int

	errorKind string
	outcome   string

	responseStatus  int
	responseStarted bool
	bytesIn         int64
	bytesOut        int64

	upstreamStarted  time.Time
	upstreamDuration time.Duration
}

// Snapshot 是 RequestContext 在请求结束时的值拷贝，供日志和指标读取。
type Snapshot struct {
	RequestID        string
	TraceID          string
	ConfigVersion    uint64
	RouteID          string
	PathTemplate     string
	UpstreamID       string
	EndpointID       string
	Attempts         int
	ErrorKind        string
	Outcome          string
	ResponseStatus   int
	ResponseStarted  bool
	BytesIn          int64
	BytesOut         int64
	UpstreamDuration time.Duration
}

// New 创建一个请求私有的 RequestContext。
func New(configVersion uint64) *RequestContext {
	return &RequestContext{configVersion: configVersion}
}

// WithContext 把 RequestContext 指针附加到标准 context.Context。
func WithContext(ctx context.Context, requestContext *RequestContext) context.Context {
	return context.WithValue(ctx, contextKey{}, requestContext)
}

// FromContext 读取请求元数据。
func FromContext(ctx context.Context) (*RequestContext, bool) {
	if ctx == nil {
		return nil, false
	}
	requestContext, ok := ctx.Value(contextKey{}).(*RequestContext)
	return requestContext, ok && requestContext != nil
}

// Ensure 返回现有 RequestContext；不存在时创建并附加一个新对象。
func Ensure(ctx context.Context, configVersion uint64) (context.Context, *RequestContext) {
	if requestContext, ok := FromContext(ctx); ok {
		return ctx, requestContext
	}
	requestContext := New(configVersion)
	return WithContext(ctx, requestContext), requestContext
}

// SetRequestID 设置校验后的 request ID。
func (c *RequestContext) SetRequestID(requestID string) {
	if c != nil {
		c.requestID = requestID
	}
}

// RequestID 返回当前 request ID。
func (c *RequestContext) RequestID() string {
	if c == nil {
		return ""
	}
	return c.requestID
}

// SetTraceID 设置从合法 traceparent 中提取的 trace ID。
func (c *RequestContext) SetTraceID(traceID string) {
	if c != nil {
		c.traceID = traceID
	}
}

// SetRoute 记录编译后的路由身份和逻辑 upstream。
func (c *RequestContext) SetRoute(routeID, pathTemplate, upstreamID string) {
	if c == nil {
		return
	}
	c.routeID = routeID
	c.pathTemplate = pathTemplate
	c.upstreamID = upstreamID
}

// SetEndpoint 记录本次实际选择的 endpoint。
func (c *RequestContext) SetEndpoint(endpointID string) {
	if c != nil {
		c.endpointID = endpointID
	}
}

// BeginUpstream 开始一次 endpoint 尝试。
func (c *RequestContext) BeginUpstream(now time.Time) {
	if c == nil {
		return
	}
	c.attempts++
	c.upstreamStarted = now
}

// FinishUpstream 结束当前 endpoint 尝试并记录耗时。
func (c *RequestContext) FinishUpstream(now time.Time) {
	if c == nil || c.upstreamStarted.IsZero() {
		return
	}
	c.upstreamDuration = now.Sub(c.upstreamStarted)
	c.upstreamStarted = time.Time{}
}

// SetErrorKind 记录受控、低基数的网关错误代码。
func (c *RequestContext) SetErrorKind(errorKind string) {
	if c != nil {
		c.errorKind = errorKind
	}
}

// SetOutcome 设置最终请求结果分类。
func (c *RequestContext) SetOutcome(outcome string) {
	if c != nil {
		c.outcome = outcome
	}
}

// MarkResponseStarted 记录第一次响应状态；后续 WriteHeader 不覆盖首次状态。
func (c *RequestContext) MarkResponseStarted(status int) {
	if c == nil || c.responseStarted {
		return
	}
	c.responseStarted = true
	c.responseStatus = status
}

// ResponseStarted 返回响应头是否已经提交。
func (c *RequestContext) ResponseStarted() bool {
	return c != nil && c.responseStarted
}

// SetObservedStatus 为未向客户端写响应的终态设置观测状态，例如客户端取消使用 499。
// 它不会把 ResponseStarted 标为 true，也不会实际调用 ResponseWriter.WriteHeader。
func (c *RequestContext) SetObservedStatus(status int) {
	if c != nil && !c.responseStarted {
		c.responseStatus = status
	}
}

// AddBytesIn 累加数据面实际读取的请求体字节。
func (c *RequestContext) AddBytesIn(count int64) {
	if c != nil && count > 0 {
		c.bytesIn += count
	}
}

// AddBytesOut 累加 Handler 交给 ResponseWriter 的响应体字节。
func (c *RequestContext) AddBytesOut(count int64) {
	if c != nil && count > 0 {
		c.bytesOut += count
	}
}

// FinalizeResponse 为没有显式写 Header/Body 的成功 Handler 固定隐式 200。
func (c *RequestContext) FinalizeResponse() {
	if c != nil && !c.responseStarted && c.responseStatus == 0 {
		c.MarkResponseStarted(200)
	}
}

// Snapshot 返回当前状态的值拷贝。
func (c *RequestContext) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	return Snapshot{
		RequestID:        c.requestID,
		TraceID:          c.traceID,
		ConfigVersion:    c.configVersion,
		RouteID:          c.routeID,
		PathTemplate:     c.pathTemplate,
		UpstreamID:       c.upstreamID,
		EndpointID:       c.endpointID,
		Attempts:         c.attempts,
		ErrorKind:        c.errorKind,
		Outcome:          c.outcome,
		ResponseStatus:   c.responseStatus,
		ResponseStarted:  c.responseStarted,
		BytesIn:          c.bytesIn,
		BytesOut:         c.bytesOut,
		UpstreamDuration: c.upstreamDuration,
	}
}
