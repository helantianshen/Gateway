// Package observability 创建应用私有的结构化日志与 Prometheus 组件
package observability

import (
	"errors"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// NewProductionLogger 创建 stdout JSON logger。Access middleware 只写受控字段
// 不把 Header、query、body 或底层网络错误直接交给 logger
func NewProductionLogger() (*zap.Logger, error) {
	config := zap.NewProductionConfig()
	config.Encoding = "json"
	config.Sampling = nil
	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncoderConfig.MessageKey = "message"
	config.EncoderConfig.LevelKey = "level"
	config.DisableCaller = true
	config.InitialFields = map[string]any{"service": "gateway"}
	return config.Build()
}

// SyncLogger 刷新日志并忽略终端 stdout/stderr 不支持 fsync 的预期错误
func SyncLogger(logger *zap.Logger) error {
	if logger == nil {
		return nil
	}
	err := logger.Sync()
	if err == nil || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY) {
		return nil
	}
	return err
}
