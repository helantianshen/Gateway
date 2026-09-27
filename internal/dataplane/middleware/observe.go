package middleware

import (
	"io"
	"net/http"
	"time"

	"github.com/felixge/httpsnoop"
	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
)

// Observe 在 Recovery 外侧收集响应状态、字节、耗时、access log 和指标
// 当前首次 WriteHeader 会被记录为响应状态，包含 1xx 的情况
func Observe(logger *zap.Logger, observer RequestObserver) func(http.Handler) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 复用外层已经创建的 RequestContext；直接使用此 Handler 时才补建
			// 计时在调用下游前开始，defer 在正常返回或 panic 展开时都执行
			ctx, metadata := requestctx.Ensure(r.Context(), 1)
			r = r.WithContext(ctx)
			started := time.Now()
			if observer != nil {
				observer.RequestStarted()
			}

			// 包装 ResponseWriter 可观察显式状态、隐式 200 和实际写出的字节
			// completed 仅表示下游正常返回，panic 时不能把未写响应补记为 200
			wrapped := wrapResponseWriter(w, metadata)
			completed := false

			defer func() {
				// 只有 Handler 正常返回时，未写响应才是 net/http 的隐式 200
				// ErrAbortHandler 的 panic 路径必须保留 status=0/_none
				if completed {
					metadata.FinalizeResponse()
				}
				// 先用状态快照计算 outcome，再取包含 outcome 的快照供日志与指标共用
				snapshot := metadata.Snapshot()
				metadata.SetOutcome(classifyOutcome(snapshot))
				snapshot = metadata.Snapshot()
				duration := time.Since(started)

				// 日志仅使用受控字段；指标复用相同快照，避免重新解析请求或路由
				if checked := logger.Check(zap.InfoLevel, "request completed"); checked != nil {
					checked.Write(
						zap.String("request_id", snapshot.RequestID),
						zap.String("trace_id", snapshot.TraceID),
						zap.Uint64("config_version", snapshot.ConfigVersion),
						zap.String("route_id", snapshot.RouteID),
						zap.String("path_template", snapshot.PathTemplate),
						zap.String("method", r.Method),
						zap.Int("status", snapshot.ResponseStatus),
						zap.Float64("duration_ms", float64(duration)/float64(time.Millisecond)),
						zap.String("upstream_id", snapshot.UpstreamID),
						zap.String("endpoint_id", snapshot.EndpointID),
						zap.Int("attempts", snapshot.Attempts),
						zap.Int64("bytes_in", snapshot.BytesIn),
						zap.Int64("bytes_out", snapshot.BytesOut),
						zap.String("outcome", snapshot.Outcome),
						zap.String("error_kind", snapshot.ErrorKind),
						zap.Float64("upstream_duration_ms", float64(snapshot.UpstreamDuration)/float64(time.Millisecond)),
					)
				}
				if observer != nil {
					observer.RequestFinished(r.Method, snapshot, duration)
				}
			}()

			next.ServeHTTP(wrapped, r)
			completed = true
		})
	}
}

func wrapResponseWriter(w http.ResponseWriter, metadata *requestctx.RequestContext) http.ResponseWriter {
	// httpsnoop 保留底层 ResponseWriter 的可选接口，避免影响 Flush 和 ReadFrom
	// 只统计底层实际写出的字节，写入失败时不按待写入长度计数
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				metadata.MarkResponseStarted(status)
				next(status)
			}
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				metadata.MarkResponseStarted(http.StatusOK)
				written, err := next(body)
				metadata.AddBytesOut(int64(written))
				return written, err
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			// io.Copy 可能走 ReadFrom，必须与 Write 路径采用同一状态和字节语义
			return func(source io.Reader) (int64, error) {
				metadata.MarkResponseStarted(http.StatusOK)
				written, err := next(source)
				metadata.AddBytesOut(written)
				return written, err
			}
		},
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			// 未显式写 Header 的首次 Flush 也会提交隐式 200
			return func() {
				metadata.MarkResponseStarted(http.StatusOK)
				next()
			}
		},
	})
}

func classifyOutcome(snapshot requestctx.Snapshot) string {
	if snapshot.ErrorKind == "CLIENT_CANCELED" {
		return "canceled"
	}
	if snapshot.ErrorKind == "RESPONSE_ABORTED" {
		return "aborted"
	}
	if snapshot.ErrorKind != "" {
		if snapshot.ResponseStatus >= 500 {
			return "gateway_error"
		}
		return "rejected"
	}
	if snapshot.Attempts > 0 && snapshot.ResponseStatus >= 400 {
		return "upstream_response_error"
	}
	if snapshot.ResponseStatus >= 500 {
		return "internal_error"
	}
	if snapshot.ResponseStatus >= 400 {
		return "rejected"
	}
	return "success"
}

type countingReadCloser struct {
	io.ReadCloser
	metadata *requestctx.RequestContext
}

func wrapRequestBody(body io.ReadCloser, metadata *requestctx.RequestContext) io.ReadCloser {
	counting := &countingReadCloser{ReadCloser: body, metadata: metadata}
	if writerTo, ok := body.(io.WriterTo); ok {
		return &countingReadCloserWriterTo{countingReadCloser: counting, writerTo: writerTo}
	}
	return counting
}

// Read 将实际读取的字节计入请求元数据
func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	read, err := r.ReadCloser.Read(buffer)
	r.metadata.AddBytesIn(int64(read))
	return read, err
}

type countingReadCloserWriterTo struct {
	*countingReadCloser
	writerTo io.WriterTo
}

// WriteTo 保留底层快速路径并统计交付的字节
func (r *countingReadCloserWriterTo) WriteTo(destination io.Writer) (int64, error) {
	return r.writerTo.WriteTo(&countingWriter{Writer: destination, metadata: r.metadata})
}

type countingWriter struct {
	io.Writer
	metadata *requestctx.RequestContext
}

// Write 按目标 Writer 实际接受的字节数更新请求元数据
func (w *countingWriter) Write(buffer []byte) (int, error) {
	written, err := w.Writer.Write(buffer)
	w.metadata.AddBytesIn(int64(written))
	return written, err
}
