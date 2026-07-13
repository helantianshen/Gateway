// Package response 提供网关数据面的统一 HTTP 错误响应工具。
//
// 职责：
//   - 定义标准 JSON 错误响应格式；
//   - 提供 WriteError 函数，将错误以 JSON 格式写入 http.ResponseWriter。
//
// 非职责：
//   - 不处理成功响应的格式化（由代理或 handler 直接透传 upstream 响应）；
//   - 不实现错误日志或指标上报（属于 Phase 2+ 可观测性）。
package response

import (
	"encoding/json"
	"net/http"
)

// ErrorBody 是对外暴露的统一 JSON 错误结构体。
//
// 所有网关自身产生的错误（非 upstream 返回的响应）都使用此格式。
// 不包含内部实现细节（如 upstream 地址、底层错误文本），避免信息泄漏。
type ErrorBody struct {
	// Code 是机器可读的错误代码，如 "BAD_GATEWAY"、"GATEWAY_TIMEOUT"。
	Code string `json:"code"`

	// Message 是人类可读的错误描述，使用通用文案，不暴露内部细节。
	Message string `json:"message"`
}

// WriteError 向客户端写入 JSON 格式的错误响应。
//
// 参数：
//   - w: 目标 ResponseWriter。调用时响应头必须尚未写出，
//     否则 WriteHeader 不会生效（Go HTTP server 会打印警告）；
//   - statusCode: HTTP 状态码，如 502、504；
//   - code: 机器可读的错误代码；
//   - message: 人类可读的错误描述。
//
// 该函数设置 Content-Type 为 application/json，写出状态码和 JSON body。
// 如果 JSON 编码失败（理论上不会发生，因为 ErrorBody 结构简单），
// 会回退到固定的最小 JSON 字节串，确保响应格式仍然稳定。
func WriteError(w http.ResponseWriter, statusCode int, code, message string) {
	body := ErrorBody{
		Code:    code,
		Message: message,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)

	// json.Marshal 对 ErrorBody 结构不会失败，
	// 但为了防御性编程仍处理错误。
	if data, err := json.Marshal(body); err == nil {
		_, _ = w.Write(data)
	} else {
		// 回退到固定 JSON，避免在异常路径暴露原始 message。
		_, _ = w.Write([]byte(`{"code":"` + code + `","message":"internal error"}`))
	}
}
