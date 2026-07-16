package middleware

import (
	"io"
	"net/http"
	"time"

	"github.com/felixge/httpsnoop"
	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
)

// Observe 在 Recovery 外侧记录最终响应状态、字节、耗时、access log 和指标。
func Observe(logger *zap.Logger, observer RequestObserver) func(http.Handler) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, metadata := requestctx.Ensure(r.Context(), 1)
			r = r.WithContext(ctx)
			started := time.Now()
			if observer != nil {
				observer.RequestStarted()
			}

			wrapped := wrapResponseWriter(w, metadata)
			completed := false

			defer func() {
				// 只有 Handler 正常返回时，未写响应才是 net/http 的隐式 200。
				// ErrAbortHandler 的 panic 路径必须保留 status=0/_none。
				if completed {
					metadata.FinalizeResponse()
				}
				snapshot := metadata.Snapshot()
				metadata.SetOutcome(classifyOutcome(snapshot))
				snapshot = metadata.Snapshot()
				duration := time.Since(started)

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
			return func(source io.Reader) (int64, error) {
				metadata.MarkResponseStarted(http.StatusOK)
				written, err := next(source)
				metadata.AddBytesOut(written)
				return written, err
			}
		},
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
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

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	read, err := r.ReadCloser.Read(buffer)
	r.metadata.AddBytesIn(int64(read))
	return read, err
}

type countingReadCloserWriterTo struct {
	*countingReadCloser
	writerTo io.WriterTo
}

func (r *countingReadCloserWriterTo) WriteTo(destination io.Writer) (int64, error) {
	return r.writerTo.WriteTo(&countingWriter{Writer: destination, metadata: r.metadata})
}

type countingWriter struct {
	io.Writer
	metadata *requestctx.RequestContext
}

func (w *countingWriter) Write(buffer []byte) (int, error) {
	written, err := w.Writer.Write(buffer)
	w.metadata.AddBytesIn(int64(written))
	return written, err
}
