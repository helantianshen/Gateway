package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/middleware"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

type bodyReadingTransport struct{}

type spoofingRequestIDTransport struct{}

func (spoofingRequestIDTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Header: http.Header{
			"X-Request-Id": []string{"spoofed-upstream-id", "second-spoofed-id"},
		},
		Body:    http.NoBody,
		Request: request,
	}, nil
}

func (bodyReadingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	_, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    request,
	}, nil
}

func TestProxyEnforcesGatewayRequestIDOnSuccessResponse(t *testing.T) {
	target, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	handler := middleware.NewPublicHandler(
		New(target, spoofingRequestIDTransport{}, time.Second, false),
		middleware.Options{RequestIDGenerator: func() string { return "trusted-gateway-id" }},
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil))

	values := recorder.Result().Header.Values("X-Request-ID")
	if len(values) != 1 || values[0] != "trusted-gateway-id" {
		t.Fatalf("响应 X-Request-ID = %v, want 单值 trusted-gateway-id", values)
	}
}

func TestProxyMapsStreamingBodyLimitToPayloadTooLarge(t *testing.T) {
	target, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	proxy := New(target, bodyReadingTransport{}, time.Second, false)
	handler := middleware.NewPublicHandler(proxy, middleware.Options{
		RequestIDGenerator: func() string { return "request-body-limit" },
		Limits: middleware.Limits{
			MaxHeaderFields:     10,
			MaxRequestBodyBytes: 3,
		},
	})
	request := httptest.NewRequest(http.MethodPost, "http://gateway.local/upload", strings.NewReader("1234"))
	request.ContentLength = -1
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, want 413; body=%s", recorder.Code, recorder.Body.String())
	}
	var body response.ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应 JSON: %v", err)
	}
	if body.Code != "PAYLOAD_TOO_LARGE" || body.RequestID != "request-body-limit" {
		t.Fatalf("错误响应 = %+v", body)
	}
}
