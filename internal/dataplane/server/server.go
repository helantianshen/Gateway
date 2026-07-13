// Package server 提供网关数据面的 HTTP Server 构建和健康端点。
//
// 职责：
//   - 创建 public HTTP Server，挂载反向代理 Handler；
//   - 创建 admin HTTP Server，提供 /livez 和 /readyz 健康端点；
//   - 配置安全超时（ReadHeaderTimeout、IdleTimeout），不设置 WriteTimeout
//     和 ReadTimeout，以避免破坏 SSE/流式响应和大请求体上传。
//
// 非职责：
//   - 不创建 TCP 监听器（由 bootstrap 负责）；
//   - 不实现路由匹配（public Handler 即代理 Handler，转发所有请求）；
//   - 不实现主动健康检查（/readyz 只反映本地初始化状态，不检查 upstream）。
//
// 超时设计说明：
//   - ReadHeaderTimeout: 限制客户端发送请求头的时长，防止 Slowloris 攻击；
//   - IdleTimeout: 限制空闲连接的保持时长，防止连接泄漏；
//   - 不设置 WriteTimeout: 因为它会影响流式响应（SSE）和慢响应体，
//     WriteTimeout 从请求开始计算到响应写完，会中断长时间的流式响应；
//   - 不设置 ReadTimeout: 因为它会影响大请求体上传，
//     ReadTimeout 从请求开始计算到 body 读完，会中断大文件上传。
package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// 安全超时默认值。
//
// 这些值在安全性和功能之间取得平衡：
//   - ReadHeaderTimeout 足够短以防止 Slowloris 攻击，
//     又足够长以容纳正常网络延迟；
//   - IdleTimeout 保持连接复用的同时确保空闲连接被回收。
const (
	// DefaultReadHeaderTimeout 是读取请求头的最大时长。
	// 超过后客户端连接被关闭，防止 Slowloris 慢速攻击。
	DefaultReadHeaderTimeout = 10 * time.Second

	// DefaultIdleTimeout 是 keep-alive 空闲连接的最大保持时长。
	// 超过后连接被关闭，客户端需要重新建立连接。
	DefaultIdleTimeout = 120 * time.Second
)

// NewPublicServer 创建面向业务流量的 public HTTP Server。
//
// 参数：
//   - handler: public Server 的 Handler，通常是反向代理 Handler。
//
// 返回的 *http.Server 已配置安全超时但未绑定监听器，
// 调用方需要通过 Serve(listener) 启动。
//
// 不设置 WriteTimeout 和 ReadTimeout：
//   - WriteTimeout 会中断 SSE/流式响应和慢响应体传输；
//   - ReadTimeout 会中断大请求体上传；
//   - 请求级超时由 Proxy 的 Context 超时处理，而非 Server 级超时。
func NewPublicServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		IdleTimeout:       DefaultIdleTimeout,
	}
}

// NewAdminServer 创建面向运维管理的 admin HTTP Server。
//
// 参数：
//   - handler: admin Server 的 Handler，由 NewAdminHandler 创建。
//
// admin Server 与 public Server 使用相同的超时配置，
// 但 admin 请求通常短小（健康检查、指标查询），
// 不会有流式响应或大请求体。
func NewAdminServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		IdleTimeout:       DefaultIdleTimeout,
	}
}

// NewAdminHandler 返回 admin Server 的 Handler。
//
// 提供以下端点：
//   - GET /livez: 存活探针，表示进程已启动。始终返回 200；
//   - GET /readyz: 就绪探针，表示进程已完成初始化可接受流量。始终返回 200；
//   - 其他路径: 返回 404。
//
// 就绪状态说明：
//   - /readyz 返回 200 表示网关本地初始化完成（监听器就绪、代理配置加载成功）；
//   - /readyz 不检查 upstream 健康，upstream 不可达不影响就绪状态；
//   - 这是因为即使 upstream 不可达，网关也应能接受请求并返回 502/504，
//     而不是在负载均衡器层面被摘除。
//
// 返回的 Handler 使用 http.ServeMux，
// /livez 和 /readyz 精确匹配，其他路径返回 404。
func NewAdminHandler() http.Handler {
	mux := http.NewServeMux()

	// /livez 存活探针。
	// 只要进程在运行就返回 200，用于 Kubernetes liveness probe。
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		if !isHealthMethod(r.Method) {
			writeMethodNotAllowed(w)
			return
		}
		writeHealthResponse(w, "ok")
	})

	// /readyz 就绪探针。
	// 表示网关已初始化完成，可以接受流量。用于 Kubernetes readiness probe。
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !isHealthMethod(r.Method) {
			writeMethodNotAllowed(w)
			return
		}
		writeHealthResponse(w, "ready")
	})

	// ServeMux 的默认行为：未匹配的路径返回 404。
	// 但 ServeMux 对 "/" 有特殊处理（作为前缀匹配），
	// 这里显式注册 "/" 返回 404，确保根路径也返回 404 而非 404 with body。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeNotFound(w)
	})

	return mux
}

// isHealthMethod 判断健康端点是否接受该 HTTP 方法。
// 健康探针只允许 GET 和 HEAD，避免其他方法产生误导性成功响应。
func isHealthMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// writeMethodNotAllowed 写入健康端点不支持方法时的 405 响应。
func writeMethodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, HEAD")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    "METHOD_NOT_ALLOWED",
		"message": "method not allowed",
	})
}

// writeHealthResponse 写入健康检查的 JSON 响应。
//
// 格式：{"status": "ok"} 或 {"status": "ready"}
// 使用 JSON 格式便于自动化工具解析。
func writeHealthResponse(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// writeNotFound 写入 404 JSON 响应。
//
// admin Server 的未知路径返回 JSON 格式的 404，
// 保持与网关错误响应格式一致。
func writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    "NOT_FOUND",
		"message": "path not found",
	})
}
