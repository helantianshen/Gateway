// Package bootstrap 负责网关进程的依赖装配与生命周期管理。
//
// 职责：
//   - 创建共享 Transport、Router、GatewayHandler 和 admin Handler；
//   - 创建 public 和 admin 两个 HTTP Server 及其底层 TCP 监听器；
//   - 启动两个 Server 并在收到 Context 取消信号后执行带超时的 Graceful Shutdown；
//   - Shutdown 时关闭 Transport 的 idle 连接，防止资源泄漏。
//
// 非职责（本轮明确不实现）：
//   - 不实现主动健康检查、SWRR、重试或限流；
//   - 不实现配置热更新和动态配置；
//   - 不实现可观测性（日志/指标/Trace）。
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/helantianshen/gateway/internal/config"
	"github.com/helantianshen/gateway/internal/dataplane/gateway"
	"github.com/helantianshen/gateway/internal/dataplane/server"
	"github.com/helantianshen/gateway/internal/dataplane/transport"
	"github.com/helantianshen/gateway/internal/dataplane/upstream"
	"github.com/helantianshen/gateway/internal/router"
)

// Application 是网关进程的核心运行时容器，持有 public 和 admin 两个 HTTP Server、
// 底层监听器、共享 Transport 以及编译后的 Router。
type Application struct {
	publicServer   *http.Server
	adminServer    *http.Server
	publicListener net.Listener
	adminListener  net.Listener
	transport      *http.Transport
	router         *router.Router
	upstreams      map[string]*upstream.CompiledUpstream
	config         *config.Config
}

// listenFunc 创建一个供 HTTP Server 使用的监听器。
// 生产代码通过 net.Listen 注入该函数；测试可以注入 Unix socket 或可控的
// fake listener，从而验证生命周期和错误清理，而不依赖随机 TCP 临时端口。
type listenFunc func(network, address string) (net.Listener, error)

// New 根据 Config 创建并初始化 Application。
//
// 该函数在返回前完成以下工作：
//  1. 创建应用级共享 Transport；
//  2. 将 ConfigSpec.Routes 编译为不可变 Router（含冲突检测和静态边压缩）；
//  3. 按路由实际使用的 Host 模式编译多 endpoint Upstream 和固定目标 Proxy；
//  4. 创建 GatewayHandler（使用 Router 和 CompiledUpstream map）；
//  5. 创建 admin Handler（/livez、/readyz）；
//  6. 创建 public 和 admin TCP 监听器与 HTTP Server。
//
// 如果端口被占用会立即返回错误，而不是延迟到 Run 时才发现。
// 如果路由编译失败（路径语法错误、冲突等），同样立即返回错误。
// 如果 admin 监听器创建失败，会先关闭已创建的 public 监听器以避免资源泄漏。
func New(cfg *config.Config) (*Application, error) {
	return newWithListen(cfg, net.Listen)
}

// newWithListen 根据 Config 创建 Application，并使用传入的 listener factory。
//
// listen 参数只负责创建监听器，调用方仍需保证返回的 listener 已经处于可接受
// 状态。函数按 public 后 admin 的顺序创建；admin 失败时立即关闭 public，并保留
// admin 创建错误作为装配失败原因。生产入口 New 传入 net.Listen，因此生产行为和
// 监听地址语义保持不变。
func newWithListen(cfg *config.Config, listen listenFunc) (*Application, error) {
	// 在创建 Transport 前校验 listener factory，避免无效依赖触发不必要的资源分配，
	// 同时把调用错误转换为明确的装配错误，而不是让 nil 函数调用产生 panic。
	if listen == nil {
		return nil, errors.New("创建 Application 失败：listener factory 不能为空")
	}
	if cfg == nil {
		return nil, errors.New("创建 Application 失败：配置不能为空")
	}
	if cfg.Spec == nil {
		return nil, errors.New("创建 Application 失败：声明式配置不能为空")
	}
	if cfg.RequestTimeout <= 0 {
		return nil, errors.New("创建 Application 失败：请求超时必须为正数")
	}
	if cfg.ShutdownTimeout <= 0 {
		return nil, errors.New("创建 Application 失败：停机超时必须为正数")
	}

	// 将 ConfigSpec.Routes 转换为 router.CompileInput 并编译为不可变 Router。
	// router.Compile 执行路径语法校验、Host/Method 归一化、冲突检测和静态边压缩。
	// 如果路由配置存在语法错误或冲突，在此立即终止启动。
	compileInputs := make([]router.CompileInput, 0, len(cfg.Spec.Routes))
	for _, route := range cfg.Spec.Routes {
		compileInputs = append(compileInputs, router.CompileInput{
			RouteID:      route.ID,
			Host:         route.Host,
			Method:       route.Method,
			Path:         route.Path,
			Upstream:     route.Upstream,
			Priority:     route.Priority,
			PreserveHost: route.PreserveHost,
		})
	}
	r, err := router.Compile(compileInputs)
	if err != nil {
		return nil, fmt.Errorf("创建 Application 失败: 路由编译失败: %w", err)
	}
	// 从路由表推导每个逻辑 upstream 实际需要的 Proxy Host 模式，同时再次验证
	// Router 的 UpstreamID 与强类型 target map 一致。未被路由引用的 upstream 仍会
	// 编译 endpoint/state，但不会创建无用 Proxy。
	proxyModes := make(map[string]upstream.ProxyMode, len(cfg.Upstreams))
	for _, route := range cfg.Spec.Routes {
		if _, exists := cfg.Upstreams[route.Upstream]; !exists {
			return nil, fmt.Errorf("创建 Application 失败: 路由 %q 引用的 upstream %q 没有编译目标", route.ID, route.Upstream)
		}
		mode := upstream.ProxyModeDefault
		if route.PreserveHost {
			mode = upstream.ProxyModePreserveHost
		}
		proxyModes[route.Upstream] |= mode
	}

	// 所有路由引用验证成功后才创建共享 Transport。每个 endpoint 的固定目标 Proxy
	// 都复用该实例；任何 runtime upstream 编译失败都在 listener 创建前返回，并先
	// 关闭可能产生的 idle connection。
	tr := transport.New()
	runtimeUpstreams := make(map[string]*upstream.CompiledUpstream, len(cfg.Upstreams))
	for upstreamID, target := range cfg.Upstreams {
		if target.ID != upstreamID {
			transport.CloseIdleConnections(tr)
			return nil, fmt.Errorf("创建 Application 失败: upstream map key %q 与编译 ID %q 不一致", upstreamID, target.ID)
		}
		endpointConfigs := make([]upstream.EndpointConfig, 0, len(target.Endpoints))
		for _, endpoint := range target.Endpoints {
			endpointConfigs = append(endpointConfigs, upstream.EndpointConfig{
				ID:     endpoint.ID,
				Target: endpoint.URL,
				Weight: endpoint.Weight,
			})
		}
		compiled, compileErr := upstream.NewCompiledUpstream(
			upstreamID,
			endpointConfigs,
			proxyModes[upstreamID],
			tr,
			cfg.RequestTimeout,
		)
		if compileErr != nil {
			transport.CloseIdleConnections(tr)
			return nil, fmt.Errorf("创建 Application 失败: %w", compileErr)
		}
		runtimeUpstreams[upstreamID] = compiled
	}

	gatewayHandler := gateway.NewGatewayHandler(r, runtimeUpstreams)

	// 创建 admin Handler，提供 /livez 和 /readyz 健康端点。
	adminHandler := server.NewAdminHandler()

	// 创建 TCP 监听器。
	// 在 New 阶段创建监听器，确保端口冲突能立即被发现。
	publicListener, err := listen("tcp", cfg.PublicAddr)
	if err != nil {
		transport.CloseIdleConnections(tr)
		return nil, fmt.Errorf("创建 public 监听器失败 (%s): %w", cfg.PublicAddr, err)
	}

	adminListener, err := listen("tcp", cfg.AdminAddr)
	if err != nil {
		// admin 监听失败时关闭已创建的 public 监听器和 Transport，防止资源泄漏。
		// 此处已无可继续运行的 Application；Close 的结果不会改变主错误分类。
		_ = publicListener.Close()
		transport.CloseIdleConnections(tr)
		return nil, fmt.Errorf("创建 admin 监听器失败 (%s): %w", cfg.AdminAddr, err)
	}

	return &Application{
		publicListener: publicListener,
		adminListener:  adminListener,
		transport:      tr,
		router:         r,
		upstreams:      runtimeUpstreams,
		publicServer:   server.NewPublicServer(gatewayHandler),
		adminServer:    server.NewAdminServer(adminHandler),
		config:         cfg,
	}, nil
}

// Run 启动 public 和 admin 两个 HTTP Server 的 Serve 循环，并阻塞直到
// Context 被取消或某个 Server 的 Serve 返回非预期错误。
//
// 退出逻辑：
//   - 当 ctx 被取消时（通常是收到 SIGINT/SIGTERM），执行带超时的 Graceful Shutdown，
//     返回 Shutdown 过程中产生的错误（若无错误则返回 nil）；
//   - 当某个 Server 的 Serve 返回错误时，先对两个 Server 执行 Shutdown，然后返回原始错误。
//     如果原始错误是 http.ErrServerClosed（由 Shutdown 触发），则返回 Shutdown 的错误。
//
// 错误通道：
//   - errCh 容量为 2，确保两个 Serve goroutine 在发送错误后都不会阻塞，
//     即使 Run 已经返回也不会泄漏 goroutine。
func (a *Application) Run(ctx context.Context) error {
	// errCh 容量为 2，与 Server 数量一致，保证 Serve goroutine 发送错误时不阻塞。
	errCh := make(chan error, 2)

	// 启动 public Server 的 Serve 循环。
	go func() {
		errCh <- a.publicServer.Serve(a.publicListener)
	}()

	// 启动 admin Server 的 Serve 循环。
	go func() {
		errCh <- a.adminServer.Serve(a.adminListener)
	}()

	select {
	case <-ctx.Done():
		// Context 被取消，执行 Graceful Shutdown。
		return a.shutdown()
	case err := <-errCh:
		// 某个 Server 的 Serve 返回了错误（可能是异常退出或 Shutdown 触发的关闭）。
		shutdownErr := a.shutdown()

		// 如果原始错误是 http.ErrServerClosed，说明 Serve 是被 Shutdown 关闭的，
		// 此时真正的错误来自 Shutdown 过程（如超时），返回 shutdownErr。
		if errors.Is(err, http.ErrServerClosed) {
			return shutdownErr
		}
		// 否则 Serve 是因为异常错误退出。使用 errors.Join 同时保留 Serve 错误
		// 与关闭另一个 Server 时可能出现的错误，避免故障诊断丢失生命周期信息。
		return errors.Join(err, shutdownErr)
	}
}

// shutdown 对 public 和 admin 两个 Server 执行并发的 Graceful Shutdown，
// 然后关闭 Transport 的 idle 连接。
//
// 使用 config.ShutdownTimeout 作为超时上限：如果在超时内活跃连接未全部关闭，
// Shutdown 会返回 context 超时错误，进程随后强制退出。
//
// 两个 Server 的 Shutdown 并发执行，共享同一个超时 Context。
// Shutdown 完成后关闭 Transport 的 idle 连接，确保不遗留空闲 TCP 连接。
func (a *Application) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.config.ShutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup
	var publicErr, adminErr error

	wg.Add(2)

	go func() {
		defer wg.Done()
		publicErr = a.publicServer.Shutdown(ctx)
	}()

	go func() {
		defer wg.Done()
		adminErr = a.adminServer.Shutdown(ctx)
	}()

	// 等待两个 Shutdown 都完成后再读取错误。
	wg.Wait()

	// 关闭 Transport 的 idle 连接。
	// 此时所有活跃请求已完成或超时，idle 连接可以被安全关闭。
	transport.CloseIdleConnections(a.transport)

	return errors.Join(publicErr, adminErr)
}

// PublicAddr 返回 public Server 的实际监听地址。
//
// 当配置地址为 ":0" 时，操作系统会分配随机端口，此方法返回包含实际端口地址。
// 主要用于测试和启动日志。
func (a *Application) PublicAddr() net.Addr {
	return a.publicListener.Addr()
}

// AdminAddr 返回 admin Server 的实际监听地址。
func (a *Application) AdminAddr() net.Addr {
	return a.adminListener.Addr()
}
