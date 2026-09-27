package middleware

import (
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

// Recovery 把响应开始前的普通 panic 转为标准 500；响应开始后只中断连接
// http.ErrAbortHandler 必须原样重新 panic，让 net/http 静默终止流式响应
func Recovery(logger *zap.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}

				// ErrAbortHandler 用于通知 net/http 中断连接，也可能来自已开始的代理响应
				// 保留该信号，不能再写 500 或记录成普通 panic
				metadata, _ := requestctx.FromContext(r.Context())
				if recovered == http.ErrAbortHandler {
					if metadata != nil {
						metadata.SetErrorKind("RESPONSE_ABORTED")
					}
					panic(http.ErrAbortHandler)
				}

				// 普通 panic 只记录类型与堆栈，不把 panic 原文写入客户端响应
				logger.Error("panic recovered",
					zap.String("request_id", requestID(metadata)),
					zap.String("panic_type", fmt.Sprintf("%T", recovered)),
					zap.Stack("stack"),
				)
				if metadata != nil {
					metadata.SetErrorKind("INTERNAL_ERROR")
				}
				// 已提交响应头时无法安全改写状态，只能中断连接
				// 当前 ResponseStarted 也会把 1xx 计为已开始，见请求元数据的状态语义
				if metadata != nil && metadata.ResponseStarted() {
					panic(http.ErrAbortHandler)
				}

				response.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}()

			next.ServeHTTP(w, r)
		})
	}
}

func requestID(metadata *requestctx.RequestContext) string {
	if metadata == nil {
		return ""
	}
	return metadata.RequestID()
}
