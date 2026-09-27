// Package server 构建 public、admin HTTP Server 和本地运维端点
package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/response"
)

const (
	// DefaultReadHeaderTimeout 是读取请求头的最大时长
	// 超过后客户端连接被关闭，防止 Slowloris 慢速攻击
	DefaultReadHeaderTimeout = 10 * time.Second

	// DefaultIdleTimeout 是 keep-alive 空闲连接的最大保持时长
	// 超过后连接被关闭，客户端需要重新建立连接
	DefaultIdleTimeout = 120 * time.Second

	// DefaultMaxHeaderBytes 限制请求头总字节，Header 字段数由 middleware.Guard 限制
	DefaultMaxHeaderBytes = 1 << 20
)

// NewPublicServer 构建尚未绑定监听器的业务 Server
// 未设置读写超时以允许流式传输；客户端请求体读取没有服务端 deadline
func NewPublicServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		MaxHeaderBytes:    DefaultMaxHeaderBytes,
	}
}

// NewAdminServer 构建尚未绑定监听器的运维 Server，使用与 public 相同的超时参数
func NewAdminServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		MaxHeaderBytes:    DefaultMaxHeaderBytes,
	}
}

// NewAdminHandler 提供 GET/HEAD /livez、/readyz 和可选的 /metrics
// /readyz 恒返回本地 ready，不检查上游健康；未知路径返回 404
func NewAdminHandler(metricsHandler http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		setAdminRoute(r, "admin_livez", "/livez")
		if !isHealthMethod(r.Method) {
			writeMethodNotAllowed(w, r)
			return
		}
		writeHealthResponse(w, "ok")
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		setAdminRoute(r, "admin_readyz", "/readyz")
		if !isHealthMethod(r.Method) {
			writeMethodNotAllowed(w, r)
			return
		}
		writeHealthResponse(w, "ready")
	})

	// Metrics Handler 由 Application 注入；未提供时不暴露 /metrics 路由
	if metricsHandler != nil {
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			setAdminRoute(r, "admin_metrics", "/metrics")
			if !isHealthMethod(r.Method) {
				writeMethodNotAllowed(w, r)
				return
			}
			metricsHandler.ServeHTTP(w, r)
		})
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		setAdminRoute(r, "admin_not_found", "")
		writeNotFound(w, r)
	})

	return mux
}

// isHealthMethod 判断健康端点是否接受该 HTTP 方法
// 健康探针只允许 GET 和 HEAD，避免其他方法产生误导性成功响应
func isHealthMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// writeMethodNotAllowed 写入健康端点不支持方法时的 405 响应
func writeMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET, HEAD")
	response.WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
}

func writeHealthResponse(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func writeNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "path not found")
}

func setAdminRoute(r *http.Request, routeID, pathTemplate string) {
	if metadata, ok := requestctx.FromContext(r.Context()); ok {
		metadata.SetRoute(routeID, pathTemplate, "")
	}
}
