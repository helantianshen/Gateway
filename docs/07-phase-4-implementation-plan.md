# Phase 4 实施计划：Upstream Pool 与 Round Robin

> 状态：实现、验证与最终复核已完成
>
> 目标版本：v0.4.0（多 Endpoint 与 Round Robin）
>
> 基线提交：`56f098f Implement Phase 3: immutable radix routing and multi-route data plane`
>
> Go Toolchain：`go1.26.3 linux/amd64`

## 1. 目标

把 Phase 3 的“逻辑 upstream 对应唯一 endpoint”扩展为“逻辑 upstream 对应不可变 endpoint pool”，在不修改 Router 语义的前提下完成：

```text
Route -> logical upstreamID -> Round Robin -> healthy endpoint -> fixed Proxy
```

核心交付：

- 每个 upstream 支持一个或多个 endpoint；
- 普通 Round Robin 使用确定性序列选择健康 endpoint；
- 健康状态和 active request 作为独立可变运行时状态；
- 全部 endpoint 不健康时立即返回 503；
- 所有 endpoint Proxy 继续共享应用级 `http.Transport`；
- 不回归 Phase 1 的超时、取消、流式响应、转发头和 502/504 契约；
- 不修改 Phase 3 Router：路由结果仍只包含逻辑 `UpstreamID`。

## 2. 范围边界

### 2.1 本阶段实现

- 解除每个 upstream 恰好一个 endpoint 的约束，改为至少一个；
- 将 YAML endpoint URL 全部编译为强类型 target；
- 新增并发安全 Round Robin；
- 选择时跳过 unhealthy endpoint；
- 注入所有 unhealthy 时返回标准 503 JSON；
- 每个 endpoint 记录 active request；
- 为路由实际使用的 `{upstreamID, preserveHost}` 模式预创建 endpoint Proxy；
- 将 endpoint ID 写入请求 Context，供 Phase 5 日志和指标使用；
- 确定性、并发、分布、race、benchmark 和真实多进程联调。

### 2.2 本阶段不实现

- Smooth Weighted Round Robin；`weight` 继续严格校验，但普通 RR 不使用权重；
- 主动健康探测和恢复调度（Phase 8）；
- 被动失败统计、熔断、半开探测；
- retry；一次请求仍只进行一次 endpoint 尝试；
- least-connections、一致性 Hash；
- 动态配置、状态复用和原子 Snapshot 切换（Phase 6/7）；
- 每 endpoint 独立 Transport；本阶段继续共享单一 Transport；
- 服务发现和远程注册中心。

## 3. 当前基线与迁移点

Phase 3 当前结构：

```go
type Config struct {
    UpstreamURLs map[string]*url.URL
    Spec         *ConfigSpec
}

type GatewayHandler struct {
    router  *router.Router
    proxies map[proxyKey]*proxy.Proxy // {upstreamID, preserveHost}
}
```

当前每个 upstream 只有一个 URL，因此 GatewayHandler 可以直接由 `UpstreamID` 找固定 Proxy。Phase 4 需要把“target 配置”和“运行时选择状态”拆开。

## 4. 分层设计

### 4.1 配置层：强类型 target，不持有运行状态

位于 `internal/config/bootstrap.go`：

```go
type EndpointTarget struct {
    ID     string
    URL    url.URL
    Weight int
}

type UpstreamTarget struct {
    ID        string
    Endpoints []EndpointTarget
}

type Config struct {
    PublicAddr      string
    AdminAddr       string
    Upstreams       map[string]UpstreamTarget
    RequestTimeout  time.Duration
    ShutdownTimeout time.Duration
    Spec            *ConfigSpec
}
```

约束：

- URL 使用值类型，避免外部修改指针；
- map、slice 和 URL 均由 `Compile` 新建，不引用 YAML 解码切片；
- endpoint ID 在一个 upstream 内唯一；
- 每个 upstream 至少一个 endpoint；
- `weight > 0`，但普通 RR 明确忽略 weight。

### 4.2 Balancer 层：只选择索引

位于 `internal/dataplane/balancer/round_robin.go`：

```go
type RoundRobin struct {
    cursor atomic.Uint64
}

func (r *RoundRobin) Select(count int, available func(index int) bool) (int, bool)
```

算法：

1. Load 单调 cursor；
2. 从 cursor 对 endpoint 数量取模的位置开始扫描；
3. 找到第一个 available endpoint；
4. CAS 将 cursor 推进到“选中位置 + 1”；
5. CAS 失败说明并发请求抢先推进，重新选择；
6. 扫描一轮仍无 available 时返回 false，不推进 cursor。

使用单调 `uint64` 而不是反复写回 `[0,n)`，避免并发 ABA。健康状态在选择后变化是允许的：一次请求使用已选 endpoint 完成单次尝试，不在本阶段重试。

### 4.3 Upstream 运行时层

位于 `internal/dataplane/upstream/upstream.go`：

```go
type EndpointState struct {
    healthy       atomic.Bool
    activeRequest atomic.Int64
}

type CompiledEndpoint struct {
    id                string
    target            url.URL
    weight            int
    state             *EndpointState
    defaultProxy      *proxy.Proxy
    preserveHostProxy *proxy.Proxy
}

type CompiledUpstream struct {
    id        string
    endpoints []*CompiledEndpoint
    balancer  balancer.RoundRobin
}
```

公开能力：

```go
func NewCompiledUpstream(...) (*CompiledUpstream, error)
func (u *CompiledUpstream) Select() (*CompiledEndpoint, error)
func (u *CompiledUpstream) Endpoint(id string) (*CompiledEndpoint, bool)
func (e *CompiledEndpoint) State() *EndpointState
func (e *CompiledEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request, preserveHost bool) bool
```

运行时规则：

- EndpointState 初始 healthy=true；
- `SetHealthy` 仅供测试注入，Phase 8 才接主动探测；
- `ServeHTTP` 在调用 Proxy 前 active +1，返回后 -1；流式请求在 Proxy 返回前持续计数；
- preserveHost 所需 Proxy 只按实际路由模式创建；调用未编译模式返回 false，由 GatewayHandler 防御性映射为 502；
- endpoint 和 state 指针在 Application 生命周期内稳定。

### 4.4 GatewayHandler

Phase 4 结构：

```go
type GatewayHandler struct {
    router    *router.Router
    upstreams map[string]*upstream.CompiledUpstream
}
```

请求流程：

```text
ParsePath
  -> Router.Match
  -> upstreams[match.UpstreamID]
  -> CompiledUpstream.Select
     -> no healthy: 503 NO_HEALTHY_UPSTREAM
  -> Context 写入 Route Match + Endpoint Selection
  -> endpoint.ServeHTTP(preserveHost)
```

错误映射：

| 场景 | 状态码 | code |
|---|---:|---|
| 非法路径 | 400 | `BAD_REQUEST` |
| 无路由 | 404 | `NOT_FOUND` |
| 路由引用不存在的 runtime upstream | 502 | `BAD_GATEWAY` |
| 所有 endpoint unhealthy | 503 | `NO_HEALTHY_UPSTREAM` |
| endpoint Proxy 模式缺失 | 502 | `BAD_GATEWAY` |
| endpoint 网络错误 | 502 | `BAD_GATEWAY` |
| endpoint 超时 | 504 | `GATEWAY_TIMEOUT` |

### 4.5 请求 Context

保留 Phase 3 的 `MatchResultFromContext`，新增：

```go
type UpstreamSelection struct {
    UpstreamID string
    EndpointID string
}

func UpstreamSelectionFromContext(ctx context.Context) (UpstreamSelection, bool)
```

Context 只在进程内传递，不写入 upstream Header，避免泄露内部 ID。

## 5. Proxy 与 Transport 所有权

- `Application` 仍创建唯一共享 `*http.Transport`；
- 每个 endpoint/Host 模式拥有固定 `Proxy`，Proxy 捕获不可变 target；
- 不在请求中创建 ReverseProxy 或 Transport；
- Application Shutdown 继续在 Server Shutdown 后统一 `CloseIdleConnections`；
- Phase 4 静态配置无 runtime retire；旧 Proxy 引用回收属于 Phase 7 Snapshot 生命周期。

## 6. 配置与装配流程

```text
Load YAML
  -> Validate: upstream >=1 endpoint
  -> config.Compile: all endpoints -> UpstreamTarget
  -> bootstrap.New
     -> router.Compile
     -> derive ProxyMode per upstream from routes
     -> shared Transport
     -> NewCompiledUpstream for every target
     -> NewGatewayHandler(router, runtime upstream map)
     -> listeners / servers
```

任何 endpoint runtime 编译失败都发生在 listener 创建前；已经创建的 Transport 必须关闭 idle connections 后返回。

## 7. 文件计划

新增：

```text
internal/dataplane/balancer/
├── round_robin.go
├── round_robin_test.go
└── bench_test.go

internal/dataplane/upstream/
├── upstream.go
└── upstream_test.go

docs/07-phase-4-implementation-plan.md
benchmarks/results/balancer/bench.txt
```

修改：

- `internal/config/spec.go`
- `internal/config/validate.go`
- `internal/config/validate_test.go`
- `internal/config/bootstrap.go`
- `internal/config/bootstrap_test.go`
- `internal/bootstrap/application.go`
- `internal/bootstrap/application_test.go`
- `internal/dataplane/gateway/handler.go`
- `internal/dataplane/gateway/handler_test.go`
- `configs/gateway.yaml`
- `cmd/mock-service/main.go` — 增加可选实例 ID 响应头，便于真实 RR 联调
- `README.md`
- `Makefile`
- `docs/03-development-roadmap.md`

不修改语义：

- `internal/router/`；Router 仍只返回逻辑 upstream ID；
- `internal/dataplane/proxy/proxy.go`；继续固定 target、preserveHost 和共享 Transport；
- public/admin Server 生命周期。

## 8. 实施步骤

### 步骤 1：配置模型迁移

- 删除 Phase 3 single endpoint 错误；
- 要求 endpoint 数量至少为 1；
- `Config.UpstreamURLs` 迁移为 `Config.Upstreams`；
- 解析并深拷贝全部 URL、ID 和 weight；
- 更新配置测试和多 endpoint YAML。

### 步骤 2：Round Robin

- 实现原子 CAS cursor；
- 测试 1/2/3 endpoint 确定性序列；
- 测试 unhealthy skip、全部 unhealthy、恢复；
- 测试并发分布和 `-race`；
- benchmark 1/10/100 endpoint 和稀疏健康场景。

### 步骤 3：CompiledUpstream

- 创建稳定 EndpointState；
- 按实际 preserveHost 模式创建 Proxy；
- 实现选择、Endpoint 查找和 active request；
- 用阻塞 RoundTripper 验证 in-flight 计数；
- 保证一个请求只触发一个 RoundTrip。

### 步骤 4：数据面接入

- Application 编译 runtime upstream map；
- GatewayHandler 选择 endpoint；
- 增加 503 和 selection Context；
- 回归 400/404/502/504、HEAD、preserveHost；
- 回归流式响应和客户端取消。

### 步骤 5：验证与文档

- 为 mock-service 增加可选 `-id`，通过 `X-Mock-Instance` 观察真实选择结果；
- 真实启动三个 mock-service，验证 RR 序列；
- 测试注入 unhealthy 后只命中健康节点；
- 保存 benchmark 环境、命令、ns/op、B/op、allocs/op；
- 更新 README、默认 YAML、开发路线和 Makefile；
- 执行完整质量门禁。

## 9. 验收标准

- [x] 每个 upstream 支持至少 1 个 endpoint，多 endpoint 不再被拒绝；
- [x] RR 对固定输入产生确定性序列；
- [x] 总选择次数可被 endpoint 数量整除时，各健康 endpoint 次数完全一致；
- [x] unhealthy endpoint 从不被选择；
- [x] 全部 unhealthy 返回 503，不执行任何 RoundTrip；
- [x] 并发选择通过 race，且无重复状态写入错误或索引越界；
- [x] active request 在正常、阻塞、取消路径最终回到 0；
- [x] Router API 和匹配结果不变；
- [x] HEAD fallback 后 upstream 仍收到 HEAD；
- [x] preserveHost true/false 在每个 endpoint 上正确；
- [x] 所有 Proxy 共享同一 Transport，不按请求创建；
- [x] 现有 400/404/502/504、流式和取消测试不回归；
- [x] Round Robin benchmark 记录 1/10/100 endpoint 的 `ns/op`、`B/op`、`allocs/op`；
- [x] 选择热路径为 0 allocs/op；
- [x] `gofmt`、`git diff --check`、`go vet`、普通测试、race、build 和 Makefile 门禁全部通过；
- [x] 无 SWRR、主动健康、重试或动态配置越界实现。

## 10. 风险与防护

| 风险 | 防护 |
|---|---|
| atomic cursor 在 unhealthy skip 时产生偏斜 | CAS 推进到实际选中位置之后，并用确定性序列验证 |
| health 在 Select 后变化 | 允许单次请求完成；不在 Phase 4 引入 retry |
| Proxy 数量随 endpoint 增长 | 只创建路由实际需要的 preserveHost 模式；benchmark 和文件图记录数量 |
| Config 与 Runtime 交叉持有可变引用 | target 值拷贝；runtime 自建 endpoint/state/proxy |
| 全不健康错误被误判为 502 | 使用独立 sentinel 和明确 503 JSON 测试 |
| active request 泄漏 | defer 对称减计数，覆盖正常/取消/阻塞测试 |
| 多 endpoint 改坏 Phase 1 代理契约 | 保留 Proxy 和 Transport 实现，执行全量回归与真实进程联调 |

## 11. 最终复核命令

```bash
gofmt -w .
git diff --check
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./...
go test -count=100 ./internal/dataplane/balancer ./internal/dataplane/upstream ./internal/dataplane/gateway
make fmt-check vet test race build
make bench-balancer
```

## 12. 后续阶段衔接

- Phase 5 使用 Context 中的 RouteID、UpstreamID、EndpointID 生成日志和低基数指标；
- Phase 7 将 CompiledUpstream 放入不可变 ConfigSnapshot，并通过 RuntimeRegistry 复用状态；
- Phase 8 负责更新 EndpointState.healthy，不修改 balancer 或 Router；
- 扩展版 SWRR 使用独立 BalancerState，weight 或算法变化必须重置状态。

## 13. 验收结果

- 全量普通测试、race、vet、build、Makefile 门禁和核心包连续 100 次测试全部通过；
- 语句覆盖率：Balancer `100%`、Upstream `86.0%`、Gateway `93.8%`、Bootstrap `94.9%`、Config `88.0%`；
- 纯 RR 健康选择约 `4.4–5.7 ns/op`，完整 Upstream 选择约 `7.2–8.1 ns/op`；
- 100 endpoint 稀疏健康场景的完整选择约 `259–263 ns/op`；全部选择 benchmark 均为 `0 B/op, 0 allocs/op`；
- 三个真实 mock-service 的六次选择序列为 `mock-1,mock-2,mock-3,mock-1,mock-2,mock-3`；
- benchmark 与真实进程证据分别保存于 `benchmarks/results/balancer/bench.txt` 和 `integration.txt`。
