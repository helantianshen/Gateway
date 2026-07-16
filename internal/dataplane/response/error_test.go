package response

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
)

func TestWriteErrorIncludesRequestIDAndRecordsErrorKind(t *testing.T) {
	metadata := requestctx.New(1)
	metadata.SetRequestID("request-error-1")
	request := httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil)
	request = request.WithContext(requestctx.WithContext(request.Context(), metadata))
	recorder := httptest.NewRecorder()

	WriteError(recorder, request, http.StatusBadGateway, "BAD_GATEWAY", "upstream request failed")

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, want 502", recorder.Code)
	}
	if got := recorder.Header().Get(requestctx.RequestIDHeader); got != "request-error-1" {
		t.Fatalf("响应 request ID = %q", got)
	}
	var body ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应 JSON: %v", err)
	}
	if body.RequestID != "request-error-1" || body.Code != "BAD_GATEWAY" {
		t.Fatalf("错误响应 = %+v", body)
	}
	if snapshot := metadata.Snapshot(); snapshot.ErrorKind != "BAD_GATEWAY" {
		t.Fatalf("RequestContext ErrorKind = %q", snapshot.ErrorKind)
	}
}

func TestWriteErrorWithoutRequestOmitsRequestID(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteError(recorder, nil, http.StatusNotFound, "NOT_FOUND", "not found")

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应 JSON: %v", err)
	}
	if _, exists := body["request_id"]; exists {
		t.Fatalf("nil request 响应不应包含 request_id: %v", body)
	}
}
