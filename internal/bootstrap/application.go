// Package bootstrap 装配网关运行时并管理 public、admin 两个 Server 的生命周期
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

// Application 拥有双 Server、监听器、共享 Transport、运行时路由和观测组件
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

// listenFunc 创建一个供 HTTP Server 使用的监听器
// 生产代码通过 net.Listen 注入该函数；测试可以注入 Unix socket 或可控的
// fake listener，从而验证生命周期和错误清理，而不依赖随机 TCP 临时端口
type listenFunc func(network, address string) (net.Listener, error)

const staticConfigVersion uint64 = 1

// New 装配 Application，并在返回前绑定 public 和 admin 监听器
// 装配失败时返回错误并清理已创建的监听器；成功后调用方负责 Run 生命周期
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

// newWithListen 根据 Config 创建 Application，并使用传入的 listener factory
//
// listen 参数只负责创建监听器，调用方仍需保证返回的 listener 已经处于可接受状态
// 函数按 public 后 admin 的顺序创建
// admin 失败时立即关闭 public，并保留 admin 创建错误作为装配失败原因，该入口使用 Nop logger
// 生产 New 通过 newWithRuntime 传入 net.Listen 和 production logger
func newWithListen(cfg *config.Config, listen listenFunc) (*Application, error) {
	return newWithRuntime(cfg, listen, zap.NewNop(), false)
}

func newWithRuntime(cfg *config.Config, listen listenFunc, logger *zap.Logger, ownsLogger bool) (*Application, error) {
	// 装配按“校验输入 → 编译只读结构 → 创建共享资源 → 绑定监听器”进行
	// 前两步失败时尚未占用端口，也无需清理 Transport
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

	// Router 必须在创建监听器前完成编译，避免带无效路由启动
	// Config.Spec 保留原指针，Router 则持有独立的冻结树
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
	// 扫描路由得到每个 upstream 实际需要的 Host 模式
	// 未被引用的 upstream 仍保留 endpoint 状态，但无需创建 Proxy
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

	// 只创建一个 Transport；后续 endpoint Proxy 共享它的连接池
	// 从这里开始的失败路径都需要清理已经创建的 Transport
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

	// Collector 直接读取刚创建的 EndpointState，使观测值与转发选择使用同一对象
	// 绑定必须在监听器创建前完成，避免抓取到部分装配的 endpoint 集合
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

	// 当前 route policy chain 为空；每条路由仍有独立的策略入口
	routeMiddlewares := make(map[string][]policy.Middleware, len(cfg.Spec.Routes))
	for _, route := range cfg.Spec.Routes {
		routeMiddlewares[route.ID] = nil
	}
	gatewayHandler, err := gateway.NewGatewayHandlerWithPolicies(r, runtimeUpstreams, routeMiddlewares)
	if err != nil {
		transport.CloseIdleConnections(tr)
		return nil, fmt.Errorf("创建 Application 失败: %w", err)
	}

	// public 链处理代理请求及指标，admin 链只处理本地运维端点
	// 两个 Handler 共享日志，但 public 请求指标不统计 admin 流量
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

	// 最后按 public → admin 绑定监听器，确保 Run 接手的是完整装配的 Application
	// admin 绑定失败时，必须关闭已经成功绑定的 public 监听器
	publicListener, err := listen("tcp", cfg.PublicAddr)
	if err != nil {
		transport.CloseIdleConnections(tr)
		return nil, fmt.Errorf("创建 public 监听器失败 (%s): %w", cfg.PublicAddr, err)
	}

	adminListener, err := listen("tcp", cfg.AdminAddr)
	if err != nil {
		// 保留 admin bind 错误；public listener 的 Close 结果不覆盖主错误
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

// Run 启动双 Server，等待取消或任一 Serve 退出，然后并发停机
// 返回前收齐两个 Serve 结果，并合并非 ErrServerClosed 错误与停机错误
func (a *Application) Run(ctx context.Context) error {
	// errCh 容量为 2，与 Server 数量一致，保证 Serve goroutine 发送错误时不阻塞
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
		// Context 被取消后统一进入 shutdown；ctx.Err() 是正常停机信号，不作为错误返回
	case err := <-errCh:
		remaining--
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}

	shutdownErr := a.shutdown()
	// Shutdown（包括超时后的强制 Close）必须让两个 Serve 循环都退出。收齐结果后
	// 再返回，既不遗留 goroutine，也不会丢失与停机同时发生的第二个 Serve 错误
	for ; remaining > 0; remaining-- {
		err := <-errCh
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, err)
		}
	}
	return errors.Join(serveErr, shutdownErr)
}

// shutdown 对 public 和 admin 两个 Server 执行并发的 Graceful Shutdown
// 然后关闭 Transport idle 连接并刷新 Application 拥有的 production logger
//
// 使用 config.ShutdownTimeout 作为超时上限：如果在超时内活跃连接未全部关闭
// Shutdown 会返回 context 超时错误，进程随后强制退出
//
// 两个 Server 的 Shutdown 并发执行，共享同一个超时 Context。任一 Shutdown
// 返回错误时再强制 Close 对应 Server，最后关闭 Transport idle 连接并刷新日志
func (a *Application) shutdown() error {
	// Run 使用的 Context 此时可能已取消，停机需要独立的超时预算
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

	// 两个 Shutdown 共享预算并发执行；等待完成后才读取各自错误
	wg.Wait()

	// Shutdown 超时不会主动终止仍存活的连接。此时必须强制 Close，促使真实
	// Proxy handler 的请求 Context 取消并终止 active upstream RoundTrip
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

	// Server 停机完成后再回收共享 Transport 的空闲连接
	// 生产 logger 由 Application 拥有时才在这里刷新
	transport.CloseIdleConnections(a.transport)

	var loggerErr error
	if a.ownsLogger {
		loggerErr = observability.SyncLogger(a.logger)
	}
	return errors.Join(publicErr, adminErr, publicCloseErr, adminCloseErr, loggerErr)
}

// PublicAddr 返回 public Server 的实际监听地址
//
// 当配置地址为 ":0" 时，操作系统会分配随机端口，此方法返回包含实际端口地址
// 主要用于测试和启动日志
func (a *Application) PublicAddr() net.Addr {
	return a.publicListener.Addr()
}

// AdminAddr 返回 admin Server 的实际监听地址
func (a *Application) AdminAddr() net.Addr {
	return a.adminListener.Addr()
}

// Logger 返回 Application 使用的结构化 logger，供进程入口写生命周期日志
// 调用方不得替换或自行关闭该实例；Application/进程退出路径负责 Sync
func (a *Application) Logger() *zap.Logger {
	return a.logger
}
