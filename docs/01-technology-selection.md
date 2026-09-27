# API 网关技术选型

> 状态：已确定；Phase 0–5 审查后更新运行时与已落地依赖
> 调研日期：2026-07-12；最近审查：2026-07-16
> 历史审查环境：`go version go1.26.5 linux/amd64`

> 2026-09-27 状态说明：本文件包含后续阶段的候选选型，不是当前依赖清单；版本建议未在本次重新联网调研。当前实现见 [AI 架构导航](../.agent/PROJECT.md)，实际依赖见本文第 7 节和 go.mod。

## 1. 选型目标

本项目的技术选型优先级如下：

1. **能形成完整企业闭环**：配置发布、数据面生效、流量治理、故障降级、可观测和回滚。
2. **适合学生独立完成**：不把项目扩张为完整云原生平台。
3. **有面试深挖价值**：重点自研路由语义、配置快照、负载均衡和代理执行契约。
4. **不重复造基础设施**：HTTP 协议、连接池、配置中心客户端、Redis 协议、Telemetry SDK 使用成熟实现。
5. **可量化**：所有性能亮点都必须由 benchmark、race、fuzz 和压力测试证明。

## 2. 最终技术栈

### 2.1 运行时与工程

| 领域 | 最终选择 | 版本基线 | 决策说明 |
|---|---|---:|---|
| Go | 官方 Go Toolchain | `go1.26.5` | 固定到修复已知标准库漏洞的 patch 版本；`go.mod` 与 CI 保持一致 |
| 仓库组织 | 单仓库、单 Go Module、多个二进制 | - | 初期不使用多 Module 或 `go.work`，降低依赖与发布复杂度 |
| 构建 | Go Modules + Makefile | - | Makefile 只封装高频命令，不隐藏核心构建逻辑 |
| 静态检查 | `go vet` + staticcheck | `honnef.co/go/tools v0.7.0` | CI 使用固定版本；golangci-lint 如后续引入需单独评估规则与误报 |
| 漏洞扫描 | govulncheck | `golang.org/x/vuln v1.6.0` | 扫描可达标准库/依赖漏洞；Phase 0–5 审查据此升级 Go 与 x/sys |
| CI | GitHub Actions | checkout v4 / setup-go v5 | 当前执行 module verify、whitespace、format、vet、staticcheck、govulncheck、unit、race、build；integration/image 在 Phase 11 加入 |

### 2.2 控制面

| 领域 | 最终选择 | 推荐模块/版本 | 决策说明 |
|---|---|---|---|
| Web 框架 | Gin | `github.com/gin-gonic/gin v1.12.0` | 只用于管理 API，不进入代理热路径 |
| 参数校验 | Gin binding + validator | `github.com/go-playground/validator/v10` | 结构校验后仍需执行跨资源语义校验 |
| API 契约 | OpenAPI 3.x 文档 | `api/openapi/control-plane.yaml` | 优先维护显式 API 契约，初期不强制引入复杂代码生成 |
| 持久化 | etcd 官方 Client v3 | `v3.7.0` 候选，Phase 6 锁定 | Go 已升级到 1.26.5，不再受 toolchain 阻塞；仍需通过 Watch/CAS/重连集成测试后写入 go.mod |

控制面使用 `gin.New()`，显式安装 Recovery、zap、Trace 和管理端认证中间件，不使用 `gin.Default()`，避免默认日志与项目日志重复。

### 2.3 数据面

| 领域 | 最终选择 | 推荐模块/版本 | 决策说明 |
|---|---|---|---|
| HTTP Server | Go 标准库 | `net/http` | 语义标准、生态兼容、便于理解连接和超时模型 |
| 反向代理 | Go 标准库 | `net/http/httputil.ReverseProxy` | 使用 `Rewrite`、`ProxyRequest.SetURL` 和自定义 Transport |
| 路由 | 自研不可变 Radix Tree | `internal/router` | 展示数据结构、冲突检测和请求侧无配置写锁的匹配能力 |
| 路由基准对照 | httprouter / ServeMux | `github.com/julienschmidt/httprouter v1.3.0` | 只用于行为差分和 benchmark，不作为主运行时依赖 |
| 负载均衡 | 自研 Round Robin / Smooth Weighted Round Robin | 内部实现 | 与健康状态、连接数和配置快照结合，面试价值高 |
| 本地限流 | Token Bucket | `golang.org/x/time/rate v0.15.0` | 用于单实例保护和 Redis 故障时的本地兜底 |
| JWT | golang-jwt | `github.com/golang-jwt/jwt/v5 v5.3.1` | 显式限制算法、issuer、audience、expiration |

明确不使用 `fasthttp`。该项目更重视标准 HTTP 语义、兼容性、可维护性和可解释性，而不是为简历追求不可靠的微基准数字。

### 2.4 配置与状态

| 领域 | 最终选择 | 推荐模块/版本 | 责任边界 |
|---|---|---|---|
| 启动配置 | YAML v3 | `gopkg.in/yaml.v3 v3.0.1` | 当前严格加载 upstream/route/policy；未来 bootstrap 连接参数仍属于 YAML/环境部署边界 |
| 动态配置真相源 | etcd | client/server 同 minor，Phase 6 锁定 | 路由、上游、策略和版本指针；低频、强一致、可 Watch |
| 高频运行时状态 | Redis | `github.com/redis/go-redis/v9 v9.21.0` | 分布式限流额度、短期 denylist 或幂等键 |
| 数据面配置状态 | 不可变配置快照 | `atomic.Pointer[ConfigSnapshot]` | 每个请求只读取一次配置版本；健康、连接和限流状态由独立 Runtime Registry 管理 |

职责必须保持单一：

- YAML 不是动态配置真相源；
- etcd 不承担限流计数和普通缓存；
- Redis Pub/Sub 不替代 etcd Watch；
- 数据面请求热路径不查询 etcd。

### 2.5 可观测性

| 领域 | 最终选择 | 推荐模块/版本 | 说明 |
|---|---|---|---|
| 结构化日志 | zap | `go.uber.org/zap v1.28.0` | 当前每请求一条完成日志并显式禁用采样；未来只有在保留审计契约时才能增加受控采样 |
| 指标 | Prometheus client_golang | `github.com/prometheus/client_golang v1.23.2` | 使用项目私有 Registry，避免全局注册冲突 |
| Trace | OpenTelemetry Go | core/SDK/exporter `v1.44.0` | 通过 OTLP/gRPC 发送到 OTel Collector |
| HTTP instrumentation | OTel contrib | `otelhttp/otelgin v0.69.0` | 数据面入口/出口用 `otelhttp`，控制面用 `otelgin` |
| Trace 后端 | Jaeger（开发环境） | Compose 中锁定镜像 | 不在应用中绑定 Jaeger 专用 exporter |
| 指标展示 | Prometheus + Grafana | Compose 中锁定镜像 | 作为本地演示和故障观察工具 |

核心版先完成 zap + Prometheus；扩展版再让 OpenTelemetry 承担 Trace。指标直接由 Prometheus Client 暴露。暂不接入 OTel Logs，也不同时维护两套重复指标。

### 2.6 测试、压测和部署

| 领域 | 最终选择 | 说明 |
|---|---|---|
| 单元测试 | `testing` + table-driven tests | 不为简单逻辑引入重量级测试框架 |
| HTTP 测试 | `net/http/httptest` | 模拟 upstream、超时、断连和异常响应 |
| 集成测试 | testcontainers-go | 启动真实 etcd/Redis 验证 Watch、CAS 和 Lua 原子性 |
| Goroutine 泄漏 | `go.uber.org/goleak v1.3.0` | 重点检查 Watch、后台健康检查和连接生命周期 |
| 并发安全 | `go test -race ./...` | 配置热更新、健康状态和负载均衡必须覆盖 |
| Fuzz | Go 原生 fuzz | 路径解析、路由冲突、URL Rewrite、YAML 和 Header 处理 |
| 微基准 | `go test -bench=. -benchmem` + benchstat | 比较路由查找、负载均衡和中间件额外开销 |
| HTTP 压测 | Vegeta `v12.13.0` | 固定到达率，适合延迟和吞吐基线 |
| 场景压测（扩展版） | k6 CLI `v1.8.0` | Vegeta 基线完成后再引入；使用 `constant-arrival-rate`，不作为 Go 运行时依赖 |
| 本地编排 | Docker Compose V2 | 使用 `compose.yaml`，不写遗留的顶层 `version:` |

Docker Compose 只代表本地开发和集成环境，不在简历中表述为“生产编排”。核心版只启动网关、控制面、etcd、mock upstream 和 Prometheus；Redis、Grafana、Collector、Jaeger 按扩展阶段加入。

## 3. 交付优先级

技术选型不等于所有组件同时进入第一版：

- **核心版（12–16 周）**：`net/http`、ReverseProxy、自研路由、Round Robin、YAML、etcd、zap、Prometheus、主动健康检查；策略链只选择 JWT 或本地限流中的一个完成闭环。
- **扩展版（完整路线约 20–30 周）**：SWRR、Redis 分布式限流、OpenTelemetry、被动健康、更多压测和 Dashboard。
- **可选 v1.1**：受限重试、熔断、JWKS 自动刷新、Least Connections/一致性 Hash。

依赖只在对应阶段引入，避免出现“go.mod 中组件很多，但项目没有完整使用和验证”的技术堆叠。

## 4. 版本策略

### 4.1 etcd v3.7 候选的锁定条件

Phase 0–5 安全审查已把 Go 升级到 1.26.5，原先阻止 etcd v3.7.0 的 toolchain 前提不再成立。Phase 6 可以从 v3.7.0 候选开始，但不得只因版本更新就直接写入运行依赖。

正式锁定条件：

1. Watch compaction、lease、transaction、CAS 和断线重连集成测试通过；
2. Compose 中 etcd server 与 client 使用同一 minor，并完成兼容性验证；
3. `govulncheck`、race 和控制面集成测试无可达问题。

### 4.2 依赖锁定规则

- 文档中的版本是初始化基线，真正可复现版本以 `go.mod`、`go.sum` 和镜像 digest 为准；
- 禁止在 Compose 中使用 `latest`；
- OTel core、SDK、exporter 保持相同 `v1.x` 版本；contrib 单独锁定兼容版本；
- 升级依赖必须经过 `go test -race ./...`、集成测试和核心 benchmark 对比；
- 不为了“使用最新版”牺牲可复现性。

## 5. 自研与复用边界

### 5.1 值得自研

1. **配置模型、校验、版本发布与回滚**
   - 路由冲突检查；
   - 上游引用检查；
   - etcd CAS 发布；
   - Watch 断线/compaction 后全量同步；
   - Last Known Good；
   - 不可变配置快照编译和原子切换；
   - 将 config version、etcd MVCC revision 和 checksum 明确分离。

2. **路由语义与不可变 Radix Tree**
   - Host、Method、Path 联合匹配；
   - static、`:param`、`*catch-all`；
   - 明确优先级；
   - 发布阶段拒绝歧义；
   - 热路径只读且不持写锁。

3. **负载均衡与健康摘除**
   - Round Robin；
   - Smooth Weighted Round Robin；
   - 主动健康检查；
   - 被动失败统计；
   - endpoint 状态复用。

4. **代理执行契约**
   - Header 改写；
   - timeout budget；
   - 客户端取消传播；
   - 幂等请求重试边界；
   - 统一 429/502/503/504 错误模型；
   - 优雅停机与连接池生命周期。

5. **策略组合和故障语义**
   - 全局与路由级中间件；
   - 本地/Redis 限流组合；
   - fail-open / fail-closed；
   - 可观测字段和错误分类。

### 5.2 应直接复用成熟实现

- HTTP 解析、HTTP/2、TLS、连接池：Go 标准库；
- Reverse Proxy：`httputil.ReverseProxy`；
- 控制面框架：Gin；
- etcd/Redis 客户端；
- YAML/JWT 加解码与密码学实现；
- zap、Prometheus、OpenTelemetry SDK；
- 通用 Token Bucket；
- 测试容器和压测工具。

本项目不自研 HTTP 协议栈、Raft、Redis 协议、Telemetry exporter 或通用日志框架。

## 6. 关键替代方案及不选原因

| 候选 | 是否采用 | 原因 |
|---|---|---|
| Gin 承载数据面 | 否 | 增加额外抽象，弱化对 `net/http`、连接池和代理流程的掌握 |
| fasthttp | 否 | 非标准 HTTP 语义和生态兼容成本，不符合项目学习主线 |
| httprouter 直接作为最终路由 | 否 | 能快速交付，但减少核心算法和冲突语义展示；保留为对照基准 |
| 自研完整 HTTP Proxy | 否 | 风险高、价值低，应在 ReverseProxy 周边建立执行契约 |
| Redis 保存路由配置 | 否 | 配置需要版本、强一致和 Watch；etcd 更合适 |
| etcd 实现限流 | 否 | 高频计数会给 etcd 带来不合理负载 |
| OTel 同时做 Trace/Metric/Log | 暂不 | 初期复杂度过高；Trace + Prometheus 已足够形成可观测闭环 |
| Kubernetes Operator / CRD | 暂不 | 超出学生项目核心目标，Docker Compose 已可完成演示 |
| Web 管理后台 | 暂不 | 管理 API 和 OpenAPI 更能体现后端能力，前端会稀释主线 |
| WASM/动态插件系统 | 暂不 | 生命周期、安全和 ABI 复杂度过高，v1 不需要 |

## 7. 当前已落地的直接 Go 依赖

以下与本次审查基线的 go.mod 一致，不是推荐升级清单：

| 模块 | 版本 | 使用位置 |
|---|---|---|
| `gopkg.in/yaml.v3` | `v3.0.1` | config 严格 YAML 解码 |
| `github.com/felixge/httpsnoop` | `v1.1.0` | middleware 响应写入观测与接口透传 |
| `go.uber.org/zap` | `v1.28.0` | observability 与 middleware 日志 |
| `github.com/prometheus/client_golang` | `v1.23.2` | observability Registry、Collector 和 HTTP exposition |

其余间接依赖见 go.mod/go.sum。Gin、etcd、Redis、JWT、OTel SDK、x/time 以及第三方路由基准库当前均未引入；应在相应阶段重新核实版本、集成测试及必要性。当前 YAML 直接依赖不是 `go.yaml.in/yaml/v3`。

## 8. 参考资料

- [Go ReverseProxy](https://pkg.go.dev/net/http/httputil#ReverseProxy)
- [Go http.Transport](https://pkg.go.dev/net/http#Transport)
- [Gin](https://github.com/gin-gonic/gin)
- [etcd Client v3](https://github.com/etcd-io/etcd/tree/main/client/v3)
- [etcd API guarantees](https://etcd.io/docs/v3.6/learning/api_guarantees/)
- [go-redis](https://github.com/redis/go-redis)
- [zap](https://github.com/uber-go/zap)
- [Prometheus Go client](https://github.com/prometheus/client_golang)
- [Prometheus instrumentation practices](https://prometheus.io/docs/practices/instrumentation/)
- [OpenTelemetry Go](https://github.com/open-telemetry/opentelemetry-go)
- [golang-jwt](https://github.com/golang-jwt/jwt)
- [YAML Go](https://github.com/yaml/go-yaml)
- [Docker Compose Specification](https://docs.docker.com/compose/compose-file/)
- [k6 constant-arrival-rate](https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/constant-arrival-rate/)
