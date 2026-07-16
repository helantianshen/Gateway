package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/observability"
)

func BenchmarkHTTPMiddleware(b *testing.B) {
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	metrics, err := observability.NewMetrics(1, observability.DefaultRouteSeriesBudget)
	if err != nil {
		b.Fatalf("NewMetrics: %v", err)
	}

	jsonLogger := zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(io.Discard),
		zap.InfoLevel,
	))

	benchmarks := []struct {
		name    string
		handler http.Handler
	}{
		{name: "baseline", handler: terminal},
		{
			name: "phase5_nop_logger",
			handler: NewPublicHandler(terminal, Options{
				Logger:             zap.NewNop(),
				RequestIDGenerator: func() string { return "benchmark-request" },
			}),
		},
		{
			name: "phase5_nop_logger_with_metrics",
			handler: NewPublicHandler(terminal, Options{
				Logger:             zap.NewNop(),
				Observer:           metrics,
				RequestIDGenerator: func() string { return "benchmark-request" },
			}),
		},
		{
			name: "phase5_json_logger",
			handler: NewPublicHandler(terminal, Options{
				Logger:             jsonLogger,
				RequestIDGenerator: func() string { return "benchmark-request" },
			}),
		},
		{
			name: "phase5_json_logger_with_metrics",
			handler: NewPublicHandler(terminal, Options{
				Logger:             jsonLogger,
				Observer:           metrics,
				RequestIDGenerator: func() string { return "benchmark-request" },
			}),
		},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			request := httptest.NewRequest(http.MethodGet, "http://gateway.local/benchmark", nil)
			request.Header.Set(requestctx.RequestIDHeader, "benchmark-request")
			writer := newBenchmarkResponseWriter()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				writer.reset()
				benchmark.handler.ServeHTTP(writer, request)
			}
		})
	}
}

type benchmarkResponseWriter struct {
	header http.Header
	status int
}

func newBenchmarkResponseWriter() *benchmarkResponseWriter {
	return &benchmarkResponseWriter{header: make(http.Header)}
}

func (w *benchmarkResponseWriter) Header() http.Header { return w.header }

func (w *benchmarkResponseWriter) WriteHeader(status int) { w.status = status }

func (w *benchmarkResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

func (w *benchmarkResponseWriter) reset() {
	clear(w.header)
	w.status = 0
}
