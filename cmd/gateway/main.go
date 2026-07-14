// Package main 是 gateway 进程的唯一正式入口。
//
// 职责（Phase 2）：
//   - 先读取环境变量中的本地启动参数，再严格加载、校验并编译 YAML 业务配置；
//   - 只有完整配置管线成功后才创建 Application，确保非法配置不会绑定监听端口；
//   - 使用 signal.NotifyContext 监听 SIGINT/SIGTERM；
//   - 调用 Application.Run，并根据运行或优雅停机结果设置退出码。
//
// 非职责：
//   - 不承载反向代理、路由匹配、负载均衡或配置解析等业务逻辑；
//   - 不直接管理 HTTP Server 的启动和关闭，这些生命周期操作由 Application 负责。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/helantianshen/gateway/internal/bootstrap"
	"github.com/helantianshen/gateway/internal/config"
)

func main() {
	// Load 按“环境变量启动参数 → 严格 YAML → 结构与语义校验 → 强类型编译”的
	// 固定顺序生成运行时配置。此调用必须位于 bootstrap.New 之前：文件不存在、未知
	// YAML 字段、无效引用或当前阶段不支持的拓扑都应在创建 listener 前终止启动。
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("网关配置加载失败: %v", err)
	}

	// 创建 Application，此阶段完成 TCP 监听器绑定。
	// 如果端口被占用，New 会立即返回错误。
	app, err := bootstrap.New(cfg)
	if err != nil {
		log.Fatalf("网关启动失败: %v", err)
	}

	log.Printf("gateway 启动中: public=%s admin=%s", app.PublicAddr(), app.AdminAddr())

	// 使用 signal.NotifyContext 将 SIGINT 和 SIGTERM 转换为 Context 取消信号。
	// 收到信号后 ctx 会被取消，Application.Run 内部检测到取消后执行 Graceful Shutdown。
	// defer stop 在 main 退出时注销信号通知并释放相关资源；本阶段不额外实现
	// “第二次信号立即强制退出”，未完成的请求最终由 ShutdownTimeout 约束。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run 阻塞直到 Context 取消或 Server 异常退出。
	if err := app.Run(ctx); err != nil {
		log.Printf("gateway 异常退出: %v", err)
		os.Exit(1)
	}

	log.Println("gateway 已正常退出")
}
