package middleware

import (
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

// Recovery 把响应开始前的普通 panic 转为标准 500；响应开始后只中断连接。
// http.ErrAbortHandler 必须原样重新 panic，让 net/http 静默终止流式响应。
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

				metadata, _ := requestctx.FromContext(r.Context())
				if recovered == http.ErrAbortHandler {
					if metadata != nil {
						metadata.SetErrorKind("RESPONSE_ABORTED")
					}
					panic(http.ErrAbortHandler)
				}

				logger.Error("panic recovered",
					zap.String("request_id", requestID(metadata)),
					zap.String("panic_type", fmt.Sprintf("%T", recovered)),
					zap.Stack("stack"),
				)
				if metadata != nil {
					metadata.SetErrorKind("INTERNAL_ERROR")
				}
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
