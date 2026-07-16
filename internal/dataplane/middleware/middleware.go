// Package middleware 提供 public/admin HTTP 请求的全局中间件链。
package middleware

import (
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
)

// RequestObserver 是 Access 层向 Prometheus 上报请求生命周期的最小接口。
// observability.Metrics 实现该接口；admin 链传 nil，避免 scrape 自身污染 public 指标。
type RequestObserver interface {
	RequestStarted()
	RequestFinished(method string, snapshot requestctx.Snapshot, duration time.Duration)
}

// Options 控制中间件运行依赖。零值 Logger/Generator 会使用安全默认值。
type Options struct {
	Logger             *zap.Logger
	Observer           RequestObserver
	RequestIDGenerator func() string
	ConfigVersion      uint64
	Limits             Limits
}

// NewPublicHandler 按固定顺序构建 public 数据面中间件链。
func NewPublicHandler(next http.Handler, options Options) http.Handler {
	options = normalizeOptions(options)

	handler := Guard(options.Limits)(next)
	handler = Recovery(options.Logger)(handler)
	handler = Observe(options.Logger, options.Observer)(handler)
	handler = TraceContext()(handler)
	handler = RequestID(options.RequestIDGenerator)(handler)
	handler = InitializeRequestContext(options.ConfigVersion)(handler)
	return handler
}

// NewAdminHandler 构建 admin 链。它保留 Request ID、访问日志和 Recovery，但不执行
// public body/header Guard，也不计入 gateway public request metrics。
func NewAdminHandler(next http.Handler, options Options) http.Handler {
	options = normalizeOptions(options)

	handler := Recovery(options.Logger)(next)
	handler = Observe(options.Logger, nil)(handler)
	handler = TraceContext()(handler)
	handler = RequestID(options.RequestIDGenerator)(handler)
	handler = InitializeRequestContext(options.ConfigVersion)(handler)
	return handler
}

func normalizeOptions(options Options) Options {
	if options.Logger == nil {
		options.Logger = zap.NewNop()
	}
	if options.RequestIDGenerator == nil {
		options.RequestIDGenerator = requestctx.NewRequestID
	}
	if options.ConfigVersion == 0 {
		options.ConfigVersion = 1
	}
	options.Limits = options.Limits.withDefaults()
	return options
}

// InitializeRequestContext 在任何业务中间件之前安装请求私有元数据指针。
func InitializeRequestContext(configVersion uint64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, _ := requestctx.Ensure(r.Context(), configVersion)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestID 校验或生成 request ID，并同步写入 request/response Header。
func RequestID(generate func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(requestctx.RequestIDHeader)
			if !requestctx.ValidRequestID(requestID) {
				requestID = generate()
				if !requestctx.ValidRequestID(requestID) {
					requestID = requestctx.NewRequestID()
				}
			}

			if metadata, ok := requestctx.FromContext(r.Context()); ok {
				metadata.SetRequestID(requestID)
			}
			r.Header.Set(requestctx.RequestIDHeader, requestID)
			w.Header().Set(requestctx.RequestIDHeader, requestID)
			next.ServeHTTP(w, r)
		})
	}
}

// TraceContext 只提取合法 W3C traceparent 的 trace ID，不创建 Span。
func TraceContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if traceID, ok := requestctx.TraceIDFromTraceParent(r.Header.Get("traceparent")); ok {
				if metadata, exists := requestctx.FromContext(r.Context()); exists {
					metadata.SetTraceID(traceID)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
