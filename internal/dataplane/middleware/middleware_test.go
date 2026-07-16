package middleware

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

type recordingObserver struct {
	started  int
	finished int
	method   string
	snapshot requestctx.Snapshot
	duration time.Duration
}

func (o *recordingObserver) RequestStarted() {
	o.started++
}

func (o *recordingObserver) RequestFinished(method string, snapshot requestctx.Snapshot, duration time.Duration) {
	o.finished++
	o.method = method
	o.snapshot = snapshot
	o.duration = duration
}

func testLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}

func TestPublicHandlerPropagatesContextAndWritesSingleAccessLog(t *testing.T) {
	logger, logs := testLogger()
	metrics := &recordingObserver{}
	traceParent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	var upstreamRequestID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metadata, ok := requestctx.FromContext(r.Context())
		if !ok {
			t.Fatal("业务 Handler 未收到 RequestContext")
		}
		metadata.SetRoute("users", "/users/:id", "user-service")
		metadata.SetEndpoint("user-1")
		metadata.BeginUpstream(time.Now())
		metadata.FinishUpstream(time.Now().Add(time.Millisecond))
		upstreamRequestID = r.Header.Get(requestctx.RequestIDHeader)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})
	handler := NewPublicHandler(next, Options{
		Logger:             logger,
		Observer:           metrics,
		RequestIDGenerator: func() string { return "generated-id" },
		ConfigVersion:      1,
	})

	request := httptest.NewRequest(http.MethodPost, "http://gateway.local/users/42?token=query-secret", strings.NewReader("body-secret"))
	request.Header.Set(requestctx.RequestIDHeader, "client-request-1")
	request.Header.Set("traceparent", traceParent)
	request.Header.Set("Authorization", "Bearer jwt-secret")
	request.Header.Set("Cookie", "session=cookie-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated || recorder.Body.String() != "created" {
		t.Fatalf("响应 = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(requestctx.RequestIDHeader); got != "client-request-1" || upstreamRequestID != got {
		t.Fatalf("request ID 传播错误: response=%q upstream=%q", got, upstreamRequestID)
	}
	if metrics.started != 1 || metrics.finished != 1 || metrics.method != http.MethodPost {
		t.Fatalf("observer 调用 = started %d finished %d method %q", metrics.started, metrics.finished, metrics.method)
	}
	snapshot := metrics.snapshot
	if snapshot.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || snapshot.RouteID != "users" || snapshot.PathTemplate != "/users/:id" {
		t.Fatalf("Context 快照错误: %+v", snapshot)
	}
	if snapshot.ResponseStatus != http.StatusCreated || snapshot.BytesIn != int64(len("body-secret")) || snapshot.BytesOut != int64(len("created")) {
		t.Fatalf("字节/状态快照错误: %+v", snapshot)
	}
	if snapshot.Outcome != "success" || snapshot.Attempts != 1 || snapshot.UpstreamDuration <= 0 {
		t.Fatalf("结果快照错误: %+v", snapshot)
	}

	accessLogs := logs.FilterMessage("request completed").All()
	if len(accessLogs) != 1 {
		t.Fatalf("access log 数量 = %d, want 1; all=%v", len(accessLogs), logs.All())
	}
	fields := accessLogs[0].ContextMap()
	if fields["request_id"] != "client-request-1" || fields["path_template"] != "/users/:id" {
		t.Fatalf("access log 字段错误: %+v", fields)
	}
	serialized, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("序列化日志字段: %v", err)
	}
	for _, secret := range []string{"jwt-secret", "cookie-secret", "body-secret", "query-secret", "/users/42"} {
		if strings.Contains(string(serialized), secret) {
			t.Errorf("access log 泄露敏感值 %q: %s", secret, serialized)
		}
	}
}

func TestPublicHandlerReplacesInvalidRequestID(t *testing.T) {
	var received string
	handler := NewPublicHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get(requestctx.RequestIDHeader)
		w.WriteHeader(http.StatusNoContent)
	}), Options{RequestIDGenerator: func() string { return "generated-safe-id" }})

	request := httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil)
	request.Header.Set(requestctx.RequestIDHeader, "invalid\nrequest-id")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if received != "generated-safe-id" || recorder.Header().Get(requestctx.RequestIDHeader) != received {
		t.Fatalf("替换后的 request ID: handler=%q response=%q", received, recorder.Header().Get(requestctx.RequestIDHeader))
	}
}

func TestGuardRejectsKnownOversizeRequests(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*http.Request)
		limits     Limits
		wantStatus int
		wantCode   string
	}{
		{
			name:       "body",
			prepare:    func(*http.Request) {},
			limits:     Limits{MaxHeaderFields: 10, MaxRequestBodyBytes: 3},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "PAYLOAD_TOO_LARGE",
		},
		{
			name: "headers",
			prepare: func(request *http.Request) {
				request.Header.Set("X-One", "1")
				request.Header.Set("X-Two", "2")
			},
			limits:     Limits{MaxHeaderFields: 1, MaxRequestBodyBytes: 100},
			wantStatus: http.StatusRequestHeaderFieldsTooLarge,
			wantCode:   "REQUEST_HEADER_FIELDS_TOO_LARGE",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			handler := NewPublicHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}), Options{
				RequestIDGenerator: func() string { return "request-guard" },
				Limits:             test.limits,
			})
			request := httptest.NewRequest(http.MethodPost, "http://gateway.local/", strings.NewReader("1234"))
			test.prepare(request)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if called {
				t.Fatal("超限请求调用了业务 Handler")
			}
			if recorder.Code != test.wantStatus {
				t.Fatalf("状态码 = %d, want %d", recorder.Code, test.wantStatus)
			}
			var body response.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("错误响应 JSON: %v", err)
			}
			if body.Code != test.wantCode || body.RequestID != "request-guard" {
				t.Fatalf("错误响应 = %+v", body)
			}
		})
	}
}

func TestGuardLimitsUnknownLengthBodyWithoutPrebuffering(t *testing.T) {
	var readError error
	handler := NewPublicHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readError = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}), Options{
		RequestIDGenerator: func() string { return "request-stream" },
		Limits:             Limits{MaxHeaderFields: 10, MaxRequestBodyBytes: 3},
	})
	request := httptest.NewRequest(http.MethodPost, "http://gateway.local/", strings.NewReader("1234"))
	request.ContentLength = -1
	handler.ServeHTTP(httptest.NewRecorder(), request)

	var maxBytesError *http.MaxBytesError
	if !errors.As(readError, &maxBytesError) {
		t.Fatalf("未知长度超限错误 = %v, want *http.MaxBytesError", readError)
	}
}

func TestObserveFinalizesNormalEmptyResponseAsImplicit200(t *testing.T) {
	metrics := &recordingObserver{}
	handler := NewPublicHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), Options{
		Observer:           metrics,
		RequestIDGenerator: func() string { return "request-empty" },
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))

	if recorder.Code != http.StatusOK || metrics.snapshot.ResponseStatus != http.StatusOK || !metrics.snapshot.ResponseStarted {
		t.Fatalf("空成功响应 = code %d, snapshot %+v", recorder.Code, metrics.snapshot)
	}
	if metrics.snapshot.Outcome != "success" {
		t.Fatalf("空成功响应 outcome = %q, want success", metrics.snapshot.Outcome)
	}
}

func TestRecoveryReturnsStandardErrorBeforeResponse(t *testing.T) {
	logger, logs := testLogger()
	metrics := &recordingObserver{}
	handler := NewPublicHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret panic value")
	}), Options{
		Logger:             logger,
		Observer:           metrics,
		RequestIDGenerator: func() string { return "request-panic" },
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", recorder.Code)
	}
	var body response.ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应 JSON: %v", err)
	}
	if body.Code != "INTERNAL_ERROR" || body.RequestID != "request-panic" {
		t.Fatalf("panic 错误响应 = %+v", body)
	}
	if metrics.snapshot.ErrorKind != "INTERNAL_ERROR" || metrics.snapshot.Outcome != "gateway_error" {
		t.Fatalf("panic 快照 = %+v", metrics.snapshot)
	}
	if len(logs.FilterMessage("request completed").All()) != 1 || len(logs.FilterMessage("panic recovered").All()) != 1 {
		t.Fatalf("panic/access 日志数量错误: %v", logs.All())
	}
	panicFields := logs.FilterMessage("panic recovered").All()[0].ContextMap()
	if serialized, _ := json.Marshal(panicFields); strings.Contains(string(serialized), "secret panic value") {
		t.Fatalf("panic 日志泄露 panic value: %s", serialized)
	}
}

func TestRecoveryDoesNotRewriteStartedResponse(t *testing.T) {
	metrics := &recordingObserver{}
	handler := NewPublicHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("after write")
	}), Options{Observer: metrics, RequestIDGenerator: func() string { return "request-started" }})
	recorder := httptest.NewRecorder()

	recovered := capturePanic(func() {
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))
	})
	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
	}
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("已写状态被改写为 %d", recorder.Code)
	}
	if metrics.snapshot.ErrorKind != "INTERNAL_ERROR" || metrics.snapshot.ResponseStatus != http.StatusAccepted {
		t.Fatalf("响应开始后 panic 快照 = %+v", metrics.snapshot)
	}
}

func TestRecoveryPreservesErrAbortHandler(t *testing.T) {
	metrics := &recordingObserver{}
	logger, logs := testLogger()
	handler := NewPublicHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), Options{Logger: logger, Observer: metrics, RequestIDGenerator: func() string { return "request-abort" }})

	recovered := capturePanic(func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))
	})
	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
	}
	if metrics.snapshot.ErrorKind != "RESPONSE_ABORTED" || metrics.snapshot.Outcome != "aborted" ||
		metrics.snapshot.ResponseStatus != 0 || metrics.snapshot.ResponseStarted {
		t.Fatalf("ErrAbortHandler 快照 = %+v", metrics.snapshot)
	}
	entries := logs.FilterMessage("request completed").All()
	if len(entries) != 1 {
		t.Fatalf("access log 数量 = %d, want 1", len(entries))
	}
	if got := entries[0].ContextMap()["status"]; got != int64(0) {
		t.Fatalf("access log status = %#v, want 0", got)
	}
}

func TestLimitRequestBodyWriterToEnforcesLimit(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		wantBody  string
		wantError bool
	}{
		{name: "exact", body: "123", wantBody: "123"},
		{name: "over", body: "1234", wantBody: "123", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			limited := limitRequestBody(io.NopCloser(strings.NewReader(test.body)), 3)
			writerTo, ok := limited.(io.WriterTo)
			if !ok {
				t.Fatal("limitRequestBody 未保留 WriterTo")
			}
			var destination strings.Builder
			written, err := writerTo.WriteTo(&destination)
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) != test.wantError {
				t.Fatalf("WriteTo error = %v, want max bytes=%v", err, test.wantError)
			}
			if written != int64(len(test.wantBody)) || destination.String() != test.wantBody {
				t.Fatalf("WriteTo = n %d body %q, want n %d body %q", written, destination.String(), len(test.wantBody), test.wantBody)
			}
		})
	}
}

func TestObservePreservesRequestBodyWriterTo(t *testing.T) {
	metrics := &recordingObserver{}
	handler := NewPublicHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writerTo, ok := r.Body.(io.WriterTo)
		if !ok {
			t.Fatal("request body io.WriterTo 未保留")
		}
		if _, err := writerTo.WriteTo(io.Discard); err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}), Options{Observer: metrics, RequestIDGenerator: func() string { return "request-writer-to" }})

	request := httptest.NewRequest(http.MethodPost, "http://gateway.local/", strings.NewReader("writer-to-body"))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if metrics.snapshot.BytesIn != int64(len("writer-to-body")) {
		t.Fatalf("bytes_in = %d", metrics.snapshot.BytesIn)
	}
}

func TestObservePreservesOptionalResponseWriterInterfaces(t *testing.T) {
	writer := newOptionalWriter()
	handler := NewPublicHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("Flusher 未保留")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("Hijacker 未保留")
		}
		if _, ok := w.(http.Pusher); !ok {
			t.Error("Pusher 未保留")
		}
		if _, ok := w.(io.ReaderFrom); !ok {
			t.Error("ReaderFrom 未保留")
		}
		_, _ = io.Copy(w, strings.NewReader("stream"))
		w.(http.Flusher).Flush()
	}), Options{RequestIDGenerator: func() string { return "request-interfaces" }})

	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))
	if !writer.flushed || writer.body.String() != "stream" {
		t.Fatalf("可选接口执行结果: flushed=%v body=%q", writer.flushed, writer.body.String())
	}
}

func capturePanic(run func()) (recovered any) {
	defer func() { recovered = recover() }()
	run()
	return nil
}

type optionalWriter struct {
	header  http.Header
	body    strings.Builder
	status  int
	flushed bool
}

func newOptionalWriter() *optionalWriter {
	return &optionalWriter{header: make(http.Header)}
}

func (w *optionalWriter) Header() http.Header { return w.header }

func (w *optionalWriter) WriteHeader(status int) { w.status = status }

func (w *optionalWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(body)
}

func (w *optionalWriter) Flush() { w.flushed = true }

func (w *optionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("test hijack")
}

func (w *optionalWriter) Push(string, *http.PushOptions) error { return http.ErrNotSupported }

func (w *optionalWriter) ReadFrom(source io.Reader) (int64, error) {
	return io.Copy(&w.body, source)
}
