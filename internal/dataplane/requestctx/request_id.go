package requestctx

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

const (
	// RequestIDHeader 是 client、Gateway 和 upstream 共用的关联 ID Header
	RequestIDHeader = "X-Request-ID"

	maxRequestIDLength = 128
)

var fallbackRequestIDCounter atomic.Uint64

// ValidRequestID 判断客户端提供的 ID 是否可安全进入响应头和结构化日志
func ValidRequestID(requestID string) bool {
	if len(requestID) == 0 || len(requestID) > maxRequestIDLength {
		return false
	}
	for index := 0; index < len(requestID); index++ {
		character := requestID[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// NewRequestID 生成 128-bit 随机 request ID，并编码为固定 32 位十六进制
// crypto/rand 极端失败时使用时间和单调 counter 兜底，保证中间件仍可形成关联 ID
func NewRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}

	timestamp := uint64(time.Now().UnixNano())
	counter := fallbackRequestIDCounter.Add(1)
	return fmt.Sprintf("%016x%016x", timestamp, counter)
}
