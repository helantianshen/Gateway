// Package main 启动 gateway 进程并管理信号与退出状态
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/bootstrap"
	"github.com/helantianshen/gateway/internal/config"
	"github.com/helantianshen/gateway/internal/observability"
)

func main() {
	os.Exit(run())
}

func run() int {
	// logger 尚未创建前的配置错误使用标准错误输出；配置内容和 URL 原始值不会回显
	cfg, err := config.Load()
	if err != nil {
		log.Printf("网关配置加载失败: %v", err)
		return 1
	}

	// 配置完整编译后才装配 Application；New 成功时两个监听器已经绑定
	app, err := bootstrap.New(cfg)
	if err != nil {
		log.Printf("网关启动失败: %v", err)
		return 1
	}
	logger := app.Logger()
	defer func() {
		if err := observability.SyncLogger(logger); err != nil {
			log.Printf("刷新网关日志失败: %v", err)
		}
	}()

	logger.Info("gateway starting",
		zap.String("public_addr", app.PublicAddr().String()),
		zap.String("admin_addr", app.AdminAddr().String()),
	)

	// 信号只负责取消运行 Context，双 Server 停机与资源回收由 Application.Run 处理
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil {
		logger.Error("gateway stopped with error", zap.Error(err))
		return 1
	}

	logger.Info("gateway stopped")
	return 0
}
