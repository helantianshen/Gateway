// Package bootstrap 负责网关进程的依赖装配与生命周期管理。
//
// 职责：
//   - 创建 zap、私有 Prometheus Registry、全局中间件和请求上下文；
//   - 创建共享 Transport、Router、GatewayHandler 和 admin Handler；
//   - 创建 public 和 admin 两个 HTTP Server 及其底层 TCP 监听器；
//   - 启动两个 Server 并在收到 Context 取消信号后执行带超时的 Graceful Shutdown；
//   - Shutdown 时关闭 Transport 的 idle 连接，防止资源泄漏。
//
// 非职责（本轮明确不实现）：
//   - 不实现主动健康检查、SWRR、重试或限流；
//   - 不实现配置热更新和动态配置；
//   - 不实现真正的 OpenTelemetry Span（Phase 5 只保留 Trace Context 占位）。
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/config"
	"github.com/helantianshen/gateway/internal/dataplane/gateway"
	"github.com/helantianshen/gateway/internal/dataplane/middleware"
	"github.com/helantianshen/gateway/internal/dataplane/policy"
	"github.com/helantianshen/gateway/internal/dataplane/server"
	"github.com/helantianshen/gateway/internal/dataplane/transport"
	"github.com/helantianshen/gateway/internal/dataplane/upstream"
	"github.com/helantianshen/gateway/internal/observability"
	"github.com/helantianshen/gateway/internal/router"
)

// Application 是网关进程的核心运行时容器，持有 public 和 admin 两个 HTTP Server、
// 底层监听器、共享 Transport、Router、Upstream、zap logger 和私有 Metrics。
type Application struct {
	publicServer   *http.Server
	adminServer    *http.Server
	publicListener net.Listener
	adminListener  net.Listener
	transport      *http.Transport
	router         *router.Router
	upstreams      map[string]*upstream.CompiledUpstream
	logger         *zap.Logger
	metrics        *observability.Metrics
	ownsLogger     bool
	config         *config.Config
}

// listenFunc 创建一个供 HTTP Server 使用的监听器。
// 生产代码通过 net.Listen 注入该函数；测试可以注入 Unix socket 或可控的
// fake listener，从而验证生命周期和错误清理，而不依赖随机 TCP 临时端口。
type listenFunc func(network, address string) (net.Listener, error)

const staticConfigVersion uint64 = 1

// New 根据 Config 创建并初始化 Application。
//
// 该函数在返回前完成以下工作：
//  1. 创建 production zap logger 和私有 Prometheus Registry；
//  2. 将 ConfigSpec.Routes 编译为不可变 Router 和空 route policy chain；
//  3. 创建共享 Transport、多 endpoint Upstream 和 endpoint 状态 Collector；
//  4. 创建 GatewayHandler 与 public 全局中间件；
//  5. 创建 admin Handler（/livez、/readyz、/metrics）与中间件；
//  6. 创建 public 和 admin TCP 监听器与 HTTP Server。
//
// 如果端口被占用会立即返回错误，而不是延迟到 Run 时才发现。
// 如果路由编译失败（路径语法错误、冲突等），同样立即返回错误。
// 如果 admin 监听器创建失败，会先关闭已创建的 public 监听器以避免资源泄漏。
func New(cfg *config.Config) (*Application, error) {
	logger, err := observability.NewProductionLogger()
	if err != nil {
		return nil, fmt.Errorf("创建 Application 失败: 创建 production logger: %w", err)
	}
	application, applicationErr := newWithRuntime(cfg, net.Listen, logger, true)
	if applicationErr != nil {
		return nil, errors.Join(applicationErr, observability.SyncLogger(logger))
	}
	return application, nil
}

// newWithListen 根据 Config 创建 Application，并使用传入的 listener factory。
//
// listen 参数只负责创建监听器，调用方仍需保证返回的 listener 已经处于可接受
// 状态。函数按 public 后 admin 的顺序创建；admin 失败时立即关闭 public，并保留
// admin 创建错误作为装配失败原因。该入口使用 Nop logger；生产 New 通过
// newWithRuntime 传入 net.Listen 和 production logger。
func newWithListen(cfg *config.Config, listen listenFunc) (*Application, error) {
	return newWithRuntime(cfg, listen, zap.NewNop(), false)
}

func newWithRuntime(cfg *config.Config, listen listenFunc, logger *zap.Logger, ownsLogger bool) (*Application, error) {
	// 在创建 Transport 前校验 listener factory，避免无效依赖触发不必要的资源分配，
	// 同时把调用错误转换为明确的装配错误，而不是让 nil 函数调用产生 panic。
	if listen == nil {
		return nil, errors.New("创建 Application 失败：listener factory 不能为空")
	}
	if cfg == nil {
		return nil, errors.New("创建 Application 失败：配置不能为空")
	}
	if logger == nil {
		return nil, errors.New("创建 Application 失败：logger 不能为空")
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

	runtimeMetrics, err := observability.NewMetrics(len(cfg.Spec.Routes), observability.DefaultRouteSeriesBudget)
	if err != nil {
		return nil, fmt.Errorf("创建 Application 失败: 初始化 Prometheus: %w", err)
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

	endpointSources := make([]observability.EndpointSource, 0)
	for upstreamID, target := range cfg.Upstreams {
		compiled := runtimeUpstreams[upstreamID]
		for _, endpointTarget := range target.Endpoints {
			endpoint, exists := compiled.Endpoint(endpointTarget.ID)
			if !exists {
				transport.CloseIdleConnections(tr)
				return nil, fmt.Errorf("创建 Application 失败: endpoint %q/%q 缺少运行时状态", upstreamID, endpointTarget.ID)
			}
			endpointSources = append(endpointSources, observability.EndpointSource{
				UpstreamID: upstreamID,
				EndpointID: endpointTarget.ID,
				State:      endpoint.State(),
			})
		}
	}
	if err := runtimeMetrics.BindEndpoints(endpointSources); err != nil {
		transport.CloseIdleConnections(tr)
		return nil, fmt.Errorf("创建 Application 失败: %w", err)
	}

	// Phase 5 为每条 route 预编译空策略链；Phase 9A 再注入具体 JWT/限流中间件。
	routeMiddlewares := make(map[string][]policy.Middleware, len(cfg.Spec.Routes))
	for _, route := range cfg.Spec.Routes {
		routeMiddlewares[route.ID] = nil
	}
	gatewayHandler, err := gateway.NewGatewayHandlerWithPolicies(r, runtimeUpstreams, routeMiddlewares)
	if err != nil {
		transport.CloseIdleConnections(tr)
		return nil, fmt.Errorf("创建 Application 失败: %w", err)
	}

	publicHandler := middleware.NewPublicHandler(gatewayHandler, middleware.Options{
		Logger:        logger,
		Observer:      runtimeMetrics,
		ConfigVersion: staticConfigVersion,
	})
	adminBaseHandler := server.NewAdminHandler(runtimeMetrics.Handler())
	adminHandler := middleware.NewAdminHandler(adminBaseHandler, middleware.Options{
		Logger:        logger,
		ConfigVersion: staticConfigVersion,
	})

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
		logger:         logger,
		metrics:        runtimeMetrics,
		ownsLogger:     ownsLogger,
		publicServer:   server.NewPublicServer(publicHandler),
		adminServer:    server.NewAdminServer(adminHandler),
		config:         cfg,
	}, nil
}

// Run 启动 public 和 admin 两个 HTTP Server 的 Serve 循环，并阻塞直到
// Context 被取消或某个 Server 的 Serve 返回非预期错误。
//
// 退出逻辑：
//   - 当 ctx 被取消时（通常是收到 SIGINT/SIGTERM），执行带超时的 Graceful Shutdown；
//   - Shutdown 超时后强制 Close 活跃连接；
//   - 当任一 Serve 返回错误时，保留该错误并关闭另一 Server；
//   - 返回前收齐两个 Serve 结果，非 ErrServerClosed 错误与 shutdown 错误全部合并。
func (a *Application) Run(ctx context.Context) error {
	// errCh 容量为 2，与 Server 数量一致，保证 Serve goroutine 发送错误时不阻塞。
	errCh := make(chan error, 2)
	go func() {
		errCh <- a.publicServer.Serve(a.publicListener)
	}()
	go func() {
		errCh <- a.adminServer.Serve(a.adminListener)
	}()

	remaining := 2
	var serveErr error
	select {
	case <-ctx.Done():
		// Context 被取消后统一进入 shutdown；ctx.Err() 是正常停机信号，不作为错误返回。
	case err := <-errCh:
		remaining--
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}

	shutdownErr := a.shutdown()
	// Shutdown（包括超时后的强制 Close）必须让两个 Serve 循环都退出。收齐结果后
	// 再返回，既不遗留 goroutine，也不会丢失与停机同时发生的第二个 Serve 错误。
	for ; remaining > 0; remaining-- {
		err := <-errCh
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, err)
		}
	}
	return errors.Join(serveErr, shutdownErr)
}

// shutdown 对 public 和 admin 两个 Server 执行并发的 Graceful Shutdown，
// 然后关闭 Transport idle 连接并刷新 Application 拥有的 production logger。
//
// 使用 config.ShutdownTimeout 作为超时上限：如果在超时内活跃连接未全部关闭，
// Shutdown 会返回 context 超时错误，进程随后强制退出。
//
// 两个 Server 的 Shutdown 并发执行，共享同一个超时 Context。任一 Shutdown
// 返回错误时再强制 Close 对应 Server，最后关闭 Transport idle 连接并刷新日志。
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

	// Shutdown 超时不会主动终止仍存活的连接。此时必须强制 Close，促使真实
	// Proxy handler 的请求 Context 取消并终止 active upstream RoundTrip。
	var publicCloseErr, adminCloseErr error
	if publicErr != nil {
		publicCloseErr = a.publicServer.Close()
		if errors.Is(publicCloseErr, http.ErrServerClosed) || errors.Is(publicCloseErr, net.ErrClosed) {
			publicCloseErr = nil
		}
	}
	if adminErr != nil {
		adminCloseErr = a.adminServer.Close()
		if errors.Is(adminCloseErr, http.ErrServerClosed) || errors.Is(adminCloseErr, net.ErrClosed) {
			adminCloseErr = nil
		}
	}

	transport.CloseIdleConnections(a.transport)

	var loggerErr error
	if a.ownsLogger {
		loggerErr = observability.SyncLogger(a.logger)
	}
	return errors.Join(publicErr, adminErr, publicCloseErr, adminCloseErr, loggerErr)
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

// Logger 返回 Application 使用的结构化 logger，供进程入口写生命周期日志。
// 调用方不得替换或自行关闭该实例；Application/进程退出路径负责 Sync。
func (a *Application) Logger() *zap.Logger {
	return a.logger
}
