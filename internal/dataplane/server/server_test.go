package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminHandler_HealthEndpoints(t *testing.T) {
	handler := NewAdminHandler(nil)
	for _, test := range []struct{ path, status string }{
		{path: "/livez", status: "ok"},
		{path: "/readyz", status: "ready"},
	} {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != http.StatusOK {
				t.Errorf("状态码 = %d, want %d", recorder.Code, http.StatusOK)
			}
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("JSON 解析失败: %v", err)
			}
			if body["status"] != test.status {
				t.Errorf("status = %q, want %q", body["status"], test.status)
			}
		})
	}
}

func TestAdminHandler_UnknownPath404(t *testing.T) {
	handler := NewAdminHandler(nil)
	for _, path := range []string{"/unknown", "/"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Errorf("路径 %q: 状态码 = %d, want %d", path, recorder.Code, http.StatusNotFound)
			}
		})
	}
}

func TestAdminHandler_Metrics(t *testing.T) {
	calls := 0
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte("gateway_requests_total 1\n"))
	})
	handler := NewAdminHandler(metrics)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK || calls != 1 || recorder.Body.String() != "gateway_requests_total 1\n" {
		t.Fatalf("metrics 响应 = code %d calls %d body %q", recorder.Code, calls, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if recorder.Code != http.StatusMethodNotAllowed || calls != 1 {
		t.Fatalf("POST /metrics = code %d calls %d", recorder.Code, calls)
	}
}

func TestServer_Timeouts(t *testing.T) {
	for _, test := range []struct {
		name   string
		server *http.Server
	}{
		{name: "public", server: NewPublicServer(http.NotFoundHandler())},
		{name: "admin", server: NewAdminServer(NewAdminHandler(nil))},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := test.server
			if srv.ReadHeaderTimeout != DefaultReadHeaderTimeout {
				t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, DefaultReadHeaderTimeout)
			}
			if srv.IdleTimeout != DefaultIdleTimeout {
				t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, DefaultIdleTimeout)
			}
			if srv.MaxHeaderBytes != DefaultMaxHeaderBytes {
				t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, DefaultMaxHeaderBytes)
			}
			if srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
				t.Errorf("读写超时 = (%v, %v), want (0, 0) 以支持流式传输", srv.ReadTimeout, srv.WriteTimeout)
			}
		})
	}
}

func TestAdminHandler_Methods(t *testing.T) {
	handler := NewAdminHandler(nil)
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
		for _, test := range methods {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(test.method, path, nil))
				if recorder.Code != test.wantStatus {
					t.Errorf("方法 %s %s: 状态码 = %d, want %d", test.method, path, recorder.Code, test.wantStatus)
				}
				if allow := recorder.Header().Get("Allow"); allow != test.wantAllow {
					t.Errorf("方法 %s %s: Allow = %q, want %q", test.method, path, allow, test.wantAllow)
				}
			})
		}
	}
}
