package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
