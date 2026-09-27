// Package response 提供网关数据面的统一 HTTP 错误响应工具
//
// 本包写出的应用错误使用同一 JSON schema，并从请求上下文附带 request ID
// 底层错误文本不由本包写入客户端响应
package response

import (
	"encoding/json"
	"net/http"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
)

// ErrorBody 是对外暴露的统一 JSON 错误结构体
type ErrorBody struct {
	// Code 是机器可读且低基数的错误代码，如 BAD_GATEWAY
	Code string `json:"code"`

	// Message 是通用错误描述，不包含 upstream 地址或底层错误文本
	Message string `json:"message"`

	// RequestID 用于关联 access log；低层测试未安装中间件时省略
	RequestID string `json:"request_id,omitempty"`
}

// WriteError 写入统一 JSON 错误响应
//
// r 可以为 nil；存在 RequestContext 时，本函数同步记录 ErrorKind，并确保响应 Header
// 与 JSON body 使用同一个 X-Request-ID。调用时响应头必须尚未写出
func WriteError(w http.ResponseWriter, r *http.Request, statusCode int, code, message string) {
	requestID := ""
	if r != nil {
		if metadata, ok := requestctx.FromContext(r.Context()); ok {
			metadata.SetErrorKind(code)
			requestID = metadata.RequestID()
		}
	}

	body := ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	}
	data, err := json.Marshal(body)
	if err != nil {
		// ErrorBody 只包含 string，正常情况下不可达；固定回退体不拼接外部文本
		data = []byte(`{"code":"INTERNAL_ERROR","message":"internal error"}`)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if requestID != "" {
		w.Header().Set(requestctx.RequestIDHeader, requestID)
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(data)
}
