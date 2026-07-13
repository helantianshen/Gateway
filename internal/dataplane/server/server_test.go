package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdminHandler_Livez 验证 /livez 返回 200 和正确的 JSON 响应。
func TestAdminHandler_Livez(t *testing.T) {
	handler := NewAdminHandler()

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

// TestAdminHandler_Readyz 验证 /readyz 返回 200 和正确的 JSON 响应。
func TestAdminHandler_Readyz(t *testing.T) {
	handler := NewAdminHandler()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %q, want %q", body["status"], "ready")
	}
}

// TestAdminHandler_UnknownPath404 验证未知 admin 路径返回 404。
func TestAdminHandler_UnknownPath404(t *testing.T) {
	handler := NewAdminHandler()

	tests := []string{"/unknown", "/admin", "/health", "/"}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("路径 %q: 状态码 = %d, want %d", path, rec.Code, http.StatusNotFound)
			}
		})
	}
}

// TestPublicServer_Timeouts 验证 public Server 配置了安全超时。
func TestPublicServer_Timeouts(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := NewPublicServer(handler)

	if srv.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	}
	if srv.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, DefaultIdleTimeout)
	}
	// 确保未设置会破坏 SSE 的超时。
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0（不应设置，会破坏 SSE）", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0（不应设置，会破坏大请求体上传）", srv.ReadTimeout)
	}
}

// TestAdminServer_Timeouts 验证 admin Server 配置了安全超时。
func TestAdminServer_Timeouts(t *testing.T) {
	handler := NewAdminHandler()
	srv := NewAdminServer(handler)

	if srv.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	}
	if srv.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, DefaultIdleTimeout)
	}
}

// TestAdminHandler_Methods 验证两个健康端点的完整方法语义矩阵，包括成功方法、
// 非允许方法和 RFC 需要暴露给客户端的 Allow 头，避免只覆盖单一路径导致回归遗漏。
func TestAdminHandler_Methods(t *testing.T) {
	handler := NewAdminHandler()
	methods := []struct {
		name       string
		method     string
		wantStatus int
		wantAllow  string
	}{
		{name: "GET", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "HEAD", method: http.MethodHead, wantStatus: http.StatusOK},
		{name: "POST", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "PUT", method: http.MethodPut, wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "DELETE", method: http.MethodDelete, wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "PATCH", method: http.MethodPatch, wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
	}

	for _, path := range []string{"/livez", "/readyz"} {
		for _, tc := range methods {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, path, nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != tc.wantStatus {
					t.Errorf("方法 %s %s: 状态码 = %d, want %d", tc.method, path, rec.Code, tc.wantStatus)
				}
				if allow := rec.Header().Get("Allow"); allow != tc.wantAllow {
					t.Errorf("方法 %s %s: Allow = %q, want %q", tc.method, path, allow, tc.wantAllow)
				}
			})
		}
	}
}
