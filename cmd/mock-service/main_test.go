package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWithInstanceID(t *testing.T) {
	handlerCalled := false
	handler := withInstanceID("mock-2", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://mock.local/", nil))

	if !handlerCalled {
		t.Fatal("withInstanceID 未调用下游 Handler")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d, want 204", recorder.Code)
	}
	if got := recorder.Header().Get(mockInstanceHeader); got != "mock-2" {
		t.Fatalf("%s = %q, want %q", mockInstanceHeader, got, "mock-2")
	}
}

func TestNewMockHandler_HelloAndNotFound(t *testing.T) {
	handler := newMockHandler("mock-3")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://mock.local/hello?name=Gateway", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/hello 状态码 = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get(mockInstanceHeader); got != "mock-3" {
		t.Fatalf("%s = %q, want mock-3", mockInstanceHeader, got)
	}
	var body map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("解析 /hello JSON: %v", err)
	}
	if got := body["message"]; got != "Hello, Gateway!" {
		t.Fatalf("message = %q, want %q", got, "Hello, Gateway!")
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://mock.local/missing", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未注册路径状态码 = %d, want 404", recorder.Code)
	}
	if got := recorder.Header().Get(mockInstanceHeader); got != "mock-3" {
		t.Fatalf("404 %s = %q, want mock-3", mockInstanceHeader, got)
	}
}

func TestNewMockHandler_EchoStreamsBody(t *testing.T) {
	handler := newMockHandler("mock-1")
	req := httptest.NewRequest(http.MethodPost, "http://mock.local/echo", strings.NewReader("streamed body"))
	req.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	if got := recorder.Body.String(); got != "streamed body" {
		t.Fatalf("body = %q, want %q", got, "streamed body")
	}
}

func TestNewMockHandler_Slow(t *testing.T) {
	handler := newMockHandler("mock-1")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://mock.local/slow?delay=1ms", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("有效 delay 状态码 = %d, want 200", recorder.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("解析 /slow JSON: %v", err)
	}
	if body["delay"] != "1ms" || body["message"] != "done" {
		t.Fatalf("/slow body = %#v", body)
	}

	for _, delay := range []string{"invalid", "0s", "-1s"} {
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://mock.local/slow?delay="+delay, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("delay=%q 状态码 = %d, want 400", delay, recorder.Code)
		}
	}
}

func TestNewMockHandler_Stream(t *testing.T) {
	handler := newMockHandler("mock-1")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://mock.local/stream?count=2&interval=1ms", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := recorder.Body.String(); got != "data: event-0\n\ndata: event-1\n\n" {
		t.Fatalf("SSE body = %q", got)
	}
}

func TestHandleStream_RequiresFlusher(t *testing.T) {
	writer := &nonFlushingResponseWriter{header: make(http.Header)}
	handleStream(writer, httptest.NewRequest(http.MethodGet, "http://mock.local/stream", nil))

	if writer.status != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", writer.status)
	}
	if got := writer.body.String(); !strings.Contains(got, "streaming not supported") {
		t.Fatalf("body = %q, want streaming not supported", got)
	}
}

func TestNewMockHandler_CancellationStopsWaiting(t *testing.T) {
	handler := newMockHandler("mock-1")
	for _, path := range []string{
		"http://mock.local/slow?delay=1h",
		"http://mock.local/stream?count=100&interval=1h",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			ctx, cancel := context.WithCancel(req.Context())
			cancel()
			req = req.WithContext(ctx)

			done := make(chan struct{})
			go func() {
				handler.ServeHTTP(httptest.NewRecorder(), req)
				close(done)
			}()

			timer := time.NewTimer(500 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				t.Fatal("请求取消后 handler 未及时返回")
			}
		})
	}
}

type nonFlushingResponseWriter struct {
	header http.Header
	body   strings.Builder
	status int
}

func (w *nonFlushingResponseWriter) Header() http.Header {
	return w.header
}

func (w *nonFlushingResponseWriter) WriteHeader(statusCode int) {
	if w.status == 0 {
		w.status = statusCode
	}
}

func (w *nonFlushingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}
