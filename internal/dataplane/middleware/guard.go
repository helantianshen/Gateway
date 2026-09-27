package middleware

import (
	"io"
	"net/http"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

const (
	// DefaultMaxHeaderFields 限制 Header map 中不同字段名数量
	DefaultMaxHeaderFields = 100

	// DefaultMaxRequestBodyBytes 保持大请求流式传输，同时设置 64 MiB 硬上限
	DefaultMaxRequestBodyBytes int64 = 64 << 20
)

// Limits 是 public Header/Body Guard 的固定运行参数
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

// Guard 先检查 Header 字段名数量和已知的请求体长度，再包装请求体交给下游
// 长度未知或实际内容超过声明长度时，上限在下游读取 body 的过程中生效
func Guard(limits Limits) func(http.Handler) http.Handler {
	limits = limits.withDefaults()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// net/http 已完成 Header 解析；这里统计不同字段名，而非原始 Header 行数
			headerFields := len(r.Header)
			// RequestID 位于 Guard 外层，进入此处时该字段可能已由中间件写入或保留
			// 关联字段不计入客户端 Header 字段名预算
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

			// 已声明的长度超过上限时直接拒绝，不建立上游请求
			// 未知长度无法在这里判定，交由下面的流式包装器在读取时检查
			if r.ContentLength > limits.MaxRequestBodyBytes {
				response.WriteError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request body too large")
				return
			}
			if r.Body != nil && r.Body != http.NoBody {
				// 所有非空 body 都安装读取上限，避免已声明长度与实际读取量不一致
				// 包装器不预读 body，下游仍可按流式方式向上游发送
				limitedBody := limitRequestBody(r.Body, limits.MaxRequestBodyBytes)
				if metadata, ok := requestctx.FromContext(r.Context()); ok {
					// 有请求元数据时再包装计数器，统计下游真正读取的字节数
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
// body 支持 io.WriterTo 时保留该快速路径。它不会预读或缓存完整请求体
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

// Read 最多交付 limit 字节，超过时返回 *http.MaxBytesError
func (r *maxBytesReadCloser) Read(buffer []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	// 最多读取 remaining+1 字节：额外一个字节用于区分“恰好达到上限”和“真正超限”
	if int64(len(buffer))-1 > r.remaining {
		buffer = buffer[:r.remaining+1]
	}
	read, err := r.body.Read(buffer)
	if int64(read) <= r.remaining {
		r.remaining -= int64(read)
		r.err = err
		return read, err
	}

	// 超出的探测字节不交付下游；错误保存在 reader 中，后续读取保持相同结果
	read = int(r.remaining)
	r.remaining = 0
	r.err = &http.MaxBytesError{Limit: r.initial}
	return read, r.err
}

// Close 关闭原始请求体
func (r *maxBytesReadCloser) Close() error {
	return r.body.Close()
}

type maxBytesReadCloserWriterTo struct {
	*maxBytesReadCloser
	writerTo io.WriterTo
}

// WriteTo 保留底层快速路径，同时对交付字节实施相同上限
func (r *maxBytesReadCloserWriterTo) WriteTo(destination io.Writer) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	// 底层 WriterTo 仍负责流式复制，所有写入都先经过带剩余额度的 Writer
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

// Write 只向目标写入上限内的字节，并记录实际写入量
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

	// 超限时只交付剩余额度；底层短写或写错误优先返回，成功写满后再报告体积超限
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
