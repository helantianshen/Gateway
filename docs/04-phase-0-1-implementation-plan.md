# Phase 0–1 实施计划

> 状态：已完成，独立复审与主智能体最终验收通过  
> 目标版本：v0.1.0（工程骨架与最小反向代理）  
> Go Module：`github.com/helantianshen/gateway`  
> Go Toolchain：`go1.26.3 linux/amd64`  
> License：MIT

## 1. 执行方式

本计划由主智能体负责任务拆解、验收和最终复核，源代码修改按顺序在同一工作区完成。

### 执行记录

- xf-maas 多次返回 503，无法稳定提供原计划中的模型服务；
- 用户批准由 GLM-5.2 子智能体完成 Phase 0/1 主体实现；
- `gpt-5.6-sol` 子智能体完成多轮测试补强、端口耗尽修复、注释校正和生命周期故障注入验证；
- 最终独立 `gpt-5.6-sol` reviewer 使用新鲜上下文只读复审全部 Phase 0/1 文件，结论为“无 blocker/concern/suggestion”；
- 主智能体完成源码语义复核、全部自动化门禁和真实双进程手工演示，Phase 0/1 最终验收通过。

### 最终验收结果

- `gofmt`、`go vet`、普通测试、race 测试、全包构建和 Makefile 聚合门禁全部通过；
- proxy、bootstrap、transport 三个核心包连续运行 10 次通过；
- Application 关闭 upstream idle connection 的集成回归测试连续运行 20 次通过；
- 真实 mock-service 与 gateway 联调验证 200 转发、请求体透传、SSE、健康端点、405/Allow、504 JSON 和 SIGTERM 优雅退出；
- `go list -m all` 仅包含主模块，没有第三方依赖；
- 未实现任何 Phase 2+ 运行时能力。

## 2. 中文注释规范

实现代码必须包含详细的全中文注释，但注释应解释设计意图、边界和并发语义，不能只是逐字翻译代码。

必须满足：

- 每个 package 使用中文包注释说明职责和非职责；
- 每个导出类型、函数、方法和常量使用中文 Go Doc；
- 进程生命周期、Context 取消、Graceful Shutdown、连接池复用、Header 清理和错误分类必须写中文原因注释；
- 测试中的特殊场景使用中文注释说明风险和预期；
- 配置字段说明、默认值和安全边界使用中文注释；
- 不写与代码不一致、无信息量或声称未来能力已经完成的注释。

## 3. Phase 0：工程骨架与质量基线

### 3.1 目标

建立最小、可构建、可测试、可优雅退出的 Go 工程，不提前创建控制面或 Phase 2+ 抽象。

### 3.2 预计文件

```text
.git/
.gitignore
.github/workflows/ci.yml
LICENSE
CONTRIBUTING.md
Makefile
go.mod
go.sum（存在依赖时生成）
cmd/gateway/main.go
internal/bootstrap/application.go
internal/bootstrap/application_test.go
internal/config/bootstrap.go
internal/config/bootstrap_test.go
```

实际文件可以根据最小实现调整，但不能创建大批空目录、空接口或未使用依赖。

### 3.3 功能

- 初始化 Git；
- 初始化模块 `github.com/helantianshen/gateway`；
- 固定 Go 1.26.3/toolchain 基线；
- 创建唯一正式进程入口 `cmd/gateway`；
- 从环境变量读取最小启动配置；
- 使用 signal context 管理 SIGINT/SIGTERM；
- 启动 public/admin 两个 HTTP Server；
- Context 取消后执行带超时的 Graceful Shutdown；
- 创建 `fmt`、`vet`、`test`、`race`、`build` Make 目标；
- 添加 GitHub Actions；
- 添加 MIT License 和贡献说明。

### 3.4 验收标准

- [x] `go build ./...` 成功；
- [x] `go test ./...` 成功；
- [x] `go test -race ./...` 成功；
- [x] `go vet ./...` 成功；
- [x] gateway 收到取消信号后在 shutdown timeout 内退出；
- [x] `cmd/gateway` 只负责装配，不承载代理细节；
- [x] 未创建 `cmd/control-plane`；
- [x] 未引入 etcd、Redis、Gin、zap、Prometheus 或 OpenTelemetry；
- [x] 关键实现具有详细中文注释。

## 4. Phase 1：最小反向代理链路

### 4.1 目标

打通以下链路并冻结代理基础契约：

```text
Client -> Gateway(net/http) -> ReverseProxy -> mock-service
```

### 4.2 预计文件

```text
cmd/mock-service/main.go
internal/dataplane/server/server.go
internal/dataplane/server/server_test.go
internal/dataplane/proxy/proxy.go
internal/dataplane/proxy/proxy_test.go
internal/dataplane/transport/transport.go
internal/dataplane/transport/transport_test.go
internal/dataplane/response/error.go
internal/config/bootstrap.go（扩展最小代理配置）
README.md（补充 Phase 1 启动与演示命令）
```

### 4.3 配置范围

Phase 1 只提供启动配置，不引入 YAML：

```text
GATEWAY_PUBLIC_ADDR       默认 :8080
GATEWAY_ADMIN_ADDR        默认 :9090
GATEWAY_UPSTREAM_URL      默认 http://127.0.0.1:18080
GATEWAY_REQUEST_TIMEOUT   默认 3s
GATEWAY_SHUTDOWN_TIMEOUT  默认 10s
```

Transport 的 dial、TLS handshake、response header、idle timeout 和连接池参数使用有明确注释的安全默认值；每个 upstream 的活跃连接上限为 100。完整 YAML 配置属于 Phase 2。

### 4.4 功能

#### HTTP Server

- public listener 处理代理流量；
- admin listener 提供 `/livez` 和 `/readyz`；
- public/admin 使用独立 `http.Server`；
- readiness 表示程序和代理配置已成功初始化，不代表 upstream 健康检查。

#### ReverseProxy

- 直接构造 `httputil.ReverseProxy`；
- 只使用 `Rewrite`，不使用旧式 `Director`；
- 使用 `ProxyRequest.SetURL`；
- 删除客户端传入的 `Forwarded`、`X-Forwarded-*` 后调用 `SetXForwarded`；
- 默认让 upstream Host 与目标地址一致；
- 所有请求复用同一个 ReverseProxy 和 Transport；
- 暂不实现重试、熔断、JWT、路由树和动态配置。

#### 错误处理

统一 JSON：

```json
{
  "code": "BAD_GATEWAY",
  "message": "upstream request failed"
}
```

- 上游连接、协议和普通网络错误返回 502；
- Context Deadline 或可识别的超时返回 504；
- 响应头已经写出后发生 body copy 错误时，不尝试重新写一个 502；
- 对外错误不暴露内部 upstream 地址和底层错误文本。

#### Transport

- 使用 `net.Dialer`；
- 设置连接、TLS、ResponseHeader 和 IdleConn 超时；
- 启用连接池与 HTTP/2 尝试；
- Transport 在应用生命周期内复用；
- Shutdown 时关闭 idle connections；
- 不为每个请求创建 Transport。

### 4.5 自动化测试

必须覆盖：

- [x] GET/POST method、escaped path、query 和 body 正确转发；
- [x] Hop-by-hop Header 被清理；
- [x] 客户端伪造或重复 `X-Forwarded-*` 不被直接信任；
- [x] 大请求体不被网关完整预读缓存；
- [x] 客户端取消传播到 upstream Context；
- [x] 连接失败返回 502 JSON；
- [x] 响应头超时返回 504 JSON（使用独立短 ResponseHeaderTimeout 验证）；
- [x] SSE/流式响应能在 upstream 结束前由客户端读取到首个事件；
- [x] upstream 写出响应头后中断时，不再改写状态码；
- [x] public/admin 健康端点行为正确；
- [x] ReverseProxy 和 Transport 跨请求复用（通过 ConnState 证明同一 TCP 连接）；
- [x] Graceful Shutdown 不泄漏监听器或后台 goroutine；
- [x] `go test -race ./...` 通过。

### 4.6 手工演示

```bash
make run-mock
make run-gateway
curl -i http://127.0.0.1:8080/hello?name=gateway
curl -i http://127.0.0.1:9090/livez
curl -i http://127.0.0.1:9090/readyz
```

预期：业务请求由 Gateway 转发至 mock-service；两个健康端点返回 200。

## 5. 审查契约

Reviewer 使用新鲜上下文，只读检查实际 diff，并按以下维度报告：

1. Phase 0/1 范围是否完整且没有越界；
2. ReverseProxy Rewrite 和转发头处理是否正确；
3. Timeout、Context 取消和 ErrorHandler 是否存在竞态或错误分类；
4. Transport 是否真正复用，是否会泄漏连接；
5. Graceful Shutdown 是否覆盖 public/admin server；
6. 测试是否真实验证声明，而不是只覆盖 happy path；
7. 中文注释是否详细、准确且不与代码漂移；
8. README、Makefile 和 CI 是否可执行。

blocker/concern 必须修复；纯风格 suggestion 由主智能体判断是否纳入。

## 6. 最终复核命令

以下命令均已实际执行并通过：

```bash
gofmt -w .
gofmt -l .
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./...
make fmt-check vet test race build
go test -count=10 ./internal/dataplane/proxy ./internal/bootstrap ./internal/dataplane/transport
go test -count=20 ./internal/bootstrap -run '^TestApplication_ShutdownClosesUpstreamIdleConnection$'
go list -m all
```

## 7. 非目标

本轮明确不实现：

- Gin 控制面；
- YAML 动态配置；
- Radix Tree；
- 多 upstream 和负载均衡；
- 主动健康检查；
- JWT、限流、Redis；
- zap、Prometheus、OpenTelemetry；
- 重试、熔断、服务发现；
- Docker Compose 完整环境。
