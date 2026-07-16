package middleware

import (
	"io"
	"net/http"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

const (
	// DefaultMaxHeaderFields 限制 Header map 中不同字段名数量。
	DefaultMaxHeaderFields = 100

	// DefaultMaxRequestBodyBytes 保持大请求流式传输，同时设置 64 MiB 硬上限。
	DefaultMaxRequestBodyBytes int64 = 64 << 20
)

// Limits 是 public Header/Body Guard 的固定运行参数。
type Limits struct {
	MaxHeaderFields     int
	MaxRequestBodyBytes int64
}

func (limits Limits) withDefaults() Limits {
	if limits.MaxHeaderFields <= 0 {
		limits.MaxHeaderFields = DefaultMaxHeaderFields
	}
	if limits.MaxRequestBodyBytes <= 0 {
		limits.MaxRequestBodyBytes = DefaultMaxRequestBodyBytes
	}
	return limits
}

// Guard 在代理前拒绝已知超限请求，并对未知长度 body 安装流式硬上限。
func Guard(limits Limits) func(http.Handler) http.Handler {
	limits = limits.withDefaults()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerFields := len(r.Header)
			// RequestID 中间件会在 Guard 前写入该内部关联 Header；它不占客户端预算。
			if r.Header.Get(requestctx.RequestIDHeader) != "" {
				headerFields--
			}
			if headerFields > limits.MaxHeaderFields {
				response.WriteError(
					w,
					r,
					http.StatusRequestHeaderFieldsTooLarge,
					"REQUEST_HEADER_FIELDS_TOO_LARGE",
					"too many request headers",
				)
				return
			}

			if r.ContentLength > limits.MaxRequestBodyBytes {
				response.WriteError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request body too large")
				return
			}
			if r.Body != nil && r.Body != http.NoBody {
				limitedBody := limitRequestBody(r.Body, limits.MaxRequestBodyBytes)
				if metadata, ok := requestctx.FromContext(r.Context()); ok {
					r.Body = wrapRequestBody(limitedBody, metadata)
				} else {
					r.Body = limitedBody
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// limitRequestBody 与 http.MaxBytesReader 使用相同的 limit+1 判定语义，同时在原始
// body 支持 io.WriterTo 时保留该快速路径。它不会预读或缓存完整请求体。
func limitRequestBody(body io.ReadCloser, limit int64) io.ReadCloser {
	limited := &maxBytesReadCloser{body: body, initial: limit, remaining: limit}
	if writerTo, ok := body.(io.WriterTo); ok {
		return &maxBytesReadCloserWriterTo{maxBytesReadCloser: limited, writerTo: writerTo}
	}
	return limited
}

type maxBytesReadCloser struct {
	body      io.ReadCloser
	initial   int64
	remaining int64
	err       error
}

func (r *maxBytesReadCloser) Read(buffer []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if int64(len(buffer))-1 > r.remaining {
		buffer = buffer[:r.remaining+1]
	}
	read, err := r.body.Read(buffer)
	if int64(read) <= r.remaining {
		r.remaining -= int64(read)
		r.err = err
		return read, err
	}

	read = int(r.remaining)
	r.remaining = 0
	r.err = &http.MaxBytesError{Limit: r.initial}
	return read, r.err
}

func (r *maxBytesReadCloser) Close() error {
	return r.body.Close()
}

type maxBytesReadCloserWriterTo struct {
	*maxBytesReadCloser
	writerTo io.WriterTo
}

func (r *maxBytesReadCloserWriterTo) WriteTo(destination io.Writer) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	written, err := r.writerTo.WriteTo(&maxBytesWriter{destination: destination, reader: r.maxBytesReadCloser})
	if r.err != nil {
		return written, r.err
	}
	return written, err
}

type maxBytesWriter struct {
	destination io.Writer
	reader      *maxBytesReadCloser
}

func (w *maxBytesWriter) Write(buffer []byte) (int, error) {
	if w.reader.err != nil {
		return 0, w.reader.err
	}
	if int64(len(buffer)) <= w.reader.remaining {
		written, err := w.destination.Write(buffer)
		w.reader.remaining -= int64(written)
		if err != nil {
			w.reader.err = err
			return written, err
		}
		if written != len(buffer) {
			w.reader.err = io.ErrShortWrite
			return written, w.reader.err
		}
		return written, nil
	}

	allowed := int(w.reader.remaining)
	written, err := w.destination.Write(buffer[:allowed])
	w.reader.remaining -= int64(written)
	if err != nil {
		w.reader.err = err
		return written, err
	}
	if written != allowed {
		w.reader.err = io.ErrShortWrite
		return written, w.reader.err
	}
	w.reader.err = &http.MaxBytesError{Limit: w.reader.initial}
	return written, w.reader.err
}
