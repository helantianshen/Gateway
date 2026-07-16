// Package main 是 gateway 进程的唯一正式入口。
//
// 职责（Phase 5）：
//   - 严格加载、校验并编译配置；
//   - 只有完整配置成功后才创建 Application 和监听器；
//   - 使用 signal.NotifyContext 监听 SIGINT/SIGTERM；
//   - 使用 Application 的 zap logger 写结构化生命周期日志；
//   - 调用 Application.Run，并根据运行或优雅停机结果设置退出码。
//
// 本包不承载代理、路由、负载均衡、中间件或配置解析等业务逻辑。
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
	// logger 尚未创建前的配置错误使用标准错误输出；配置内容和 URL 原始值不会回显。
	cfg, err := config.Load()
	if err != nil {
		log.Printf("网关配置加载失败: %v", err)
		return 1
	}

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil {
		logger.Error("gateway stopped with error", zap.Error(err))
		return 1
	}

	logger.Info("gateway stopped")
	return 0
}
