package requestctx

import (
	"context"
	"regexp"
	"testing"
	"time"
)

func TestRequestContextLifecycle(t *testing.T) {
	standardContext, metadata := Ensure(context.Background(), 1)
	if metadata == nil {
		t.Fatal("Ensure 未创建 RequestContext")
	}
	if existing, ok := FromContext(standardContext); !ok || existing != metadata {
		t.Fatalf("FromContext = (%p, %v), want (%p, true)", existing, ok, metadata)
	}
	ensuredContext, ensured := Ensure(standardContext, 2)
	if ensuredContext != standardContext || ensured != metadata {
		t.Fatal("Ensure 替换了已有 RequestContext")
	}

	metadata.SetRequestID("request-1")
	metadata.SetTraceID("0123456789abcdef0123456789abcdef")
	metadata.SetRoute("users", "/users/:id", "user-service")
	metadata.SetEndpoint("user-1")
	metadata.SetErrorKind("BAD_GATEWAY")
	metadata.SetOutcome("gateway_error")
	metadata.AddBytesIn(12)
	metadata.AddBytesOut(34)
	metadata.MarkResponseStarted(502)
	metadata.MarkResponseStarted(200)
	started := time.Unix(100, 0)
	metadata.BeginUpstream(started)
	metadata.FinishUpstream(started.Add(25 * time.Millisecond))

	snapshot := metadata.Snapshot()
	if snapshot.RequestID != "request-1" || snapshot.TraceID == "" || snapshot.ConfigVersion != 1 {
		t.Fatalf("请求身份快照错误: %+v", snapshot)
	}
	if snapshot.RouteID != "users" || snapshot.PathTemplate != "/users/:id" || snapshot.UpstreamID != "user-service" || snapshot.EndpointID != "user-1" {
		t.Fatalf("路由/upstream 快照错误: %+v", snapshot)
	}
	if snapshot.Attempts != 1 || snapshot.UpstreamDuration != 25*time.Millisecond {
		t.Fatalf("upstream 尝试快照错误: %+v", snapshot)
	}
	if snapshot.ResponseStatus != 502 || !snapshot.ResponseStarted || snapshot.BytesIn != 12 || snapshot.BytesOut != 34 {
		t.Fatalf("响应快照错误: %+v", snapshot)
	}
	if snapshot.ErrorKind != "BAD_GATEWAY" || snapshot.Outcome != "gateway_error" {
		t.Fatalf("结果快照错误: %+v", snapshot)
	}
}

func TestRequestContextFinalizeImplicitResponse(t *testing.T) {
	metadata := New(1)
	metadata.FinalizeResponse()
	if snapshot := metadata.Snapshot(); snapshot.ResponseStatus != 200 || !snapshot.ResponseStarted {
		t.Fatalf("FinalizeResponse 快照 = %+v", snapshot)
	}
}

func TestRequestContextObservedStatusDoesNotWriteResponse(t *testing.T) {
	metadata := New(1)
	metadata.SetObservedStatus(499)
	metadata.FinalizeResponse()
	snapshot := metadata.Snapshot()
	if snapshot.ResponseStatus != 499 || snapshot.ResponseStarted {
		t.Fatalf("观测终态快照 = %+v", snapshot)
	}
}

func TestValidRequestID(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "simple", value: "request-123.A_B", valid: true},
		{name: "empty", value: "", valid: false},
		{name: "space", value: "request 123", valid: false},
		{name: "newline", value: "request\n123", valid: false},
		{name: "unicode", value: "请求-123", valid: false},
		{name: "too long", value: string(make([]byte, maxRequestIDLength+1)), valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidRequestID(test.value); got != test.valid {
				t.Fatalf("ValidRequestID(%q) = %v, want %v", test.value, got, test.valid)
			}
		})
	}
}

func TestNewRequestID(t *testing.T) {
	requestID := NewRequestID()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(requestID) {
		t.Fatalf("NewRequestID() = %q, want 32 位小写 hex", requestID)
	}
	if !ValidRequestID(requestID) {
		t.Fatalf("生成的 request ID 未通过自身校验: %q", requestID)
	}
}

func TestTraceIDFromTraceParent(t *testing.T) {
	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if traceID, ok := TraceIDFromTraceParent(valid); !ok || traceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("合法 traceparent = (%q, %v)", traceID, ok)
	}

	invalid := []string{
		"",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
	}
	for _, value := range invalid {
		if traceID, ok := TraceIDFromTraceParent(value); ok || traceID != "" {
			t.Errorf("非法 traceparent %q = (%q, %v)", value, traceID, ok)
		}
	}
}
