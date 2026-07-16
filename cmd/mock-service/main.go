// Package main 是 mock-service 进程入口，用于演示和测试网关反向代理。
//
// 提供以下端点：
//   - GET /hello?name=xxx: 返回 JSON 格式的问候语；
//   - POST /echo: 原样返回请求体和 Content-Type；
//   - GET /slow?delay=duration: 等待指定时长后返回，用于测试超时；
//   - GET /stream?count=N: 返回 SSE 格式的流式响应，用于测试流式代理。
//
// 仅使用 Go 标准库，不引入任何第三方依赖。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const mockInstanceHeader = "X-Mock-Instance"

func main() {
	addr := flag.String("addr", ":18080", "监听地址")
	instanceID := flag.String("id", "mock-1", "实例 ID（通过 X-Mock-Instance 响应头返回）")
	flag.Parse()
	if strings.TrimSpace(*instanceID) == "" {
		log.Fatal("mock-service 实例 ID 不能为空")
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           newMockHandler(*instanceID),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("mock-service %s 启动于 %s", *instanceID, *addr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("mock-service 启动失败: %v", err)
	}
}

func newMockHandler(instanceID string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", handleHello)
	mux.HandleFunc("/echo", handleEcho)
	mux.HandleFunc("/slow", handleSlow)
	mux.HandleFunc("/stream", handleStream)
	return withInstanceID(instanceID, mux)
}

// withInstanceID 为所有响应写入稳定实例 ID，便于通过真实 HTTP 请求观察网关的
// endpoint 选择序列。该标识只属于本地 mock-service，不由 Gateway 注入。
func withInstanceID(instanceID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(mockInstanceHeader, instanceID)
		next.ServeHTTP(w, r)
	})
}

// handleHello 返回 JSON 格式的问候语。
//
// Query 参数 name 指定问候对象，默认为 "world"。
// 响应格式：{"message": "Hello, name!"}
func handleHello(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Hello, %s!", name),
	})
}

// handleEcho 以流式 copy 原样返回请求体，不把完整 body 缓存在内存。
func handleEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, r.Body)
}

// handleSlow 等待指定时长后返回。
//
// Query 参数 delay 指定等待时长（如 "2s"），默认 1s。
// 用于测试网关的请求超时行为。
func handleSlow(w http.ResponseWriter, r *http.Request) {
	delayStr := r.URL.Query().Get("delay")
	if delayStr == "" {
		delayStr = "1s"
	}
	delay, err := time.ParseDuration(delayStr)
	if err != nil || delay <= 0 {
		http.Error(w, "invalid delay", http.StatusBadRequest)
		return
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		return
	case <-timer.C:
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "done",
		"delay":   delayStr,
	})
}

// handleStream 返回 SSE 格式的流式响应。
//
// Query 参数 count 指定事件数量（默认 5），interval 指定事件间隔（默认 100ms）。
// 用于测试网关对流式响应的代理能力，验证首个事件在连接结束前对客户端可见。
func handleStream(w http.ResponseWriter, r *http.Request) {
	countStr := r.URL.Query().Get("count")
	count, err := strconv.Atoi(countStr)
	if err != nil || count <= 0 {
		count = 5
	}

	intervalStr := r.URL.Query().Get("interval")
	interval, err := time.ParseDuration(intervalStr)
	if err != nil || interval <= 0 {
		interval = 100 * time.Millisecond
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for i := 0; i < count; i++ {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		fmt.Fprintf(w, "data: event-%d\n\n", i)
		flusher.Flush()
	}
}
