# API 网关项目架构设计

> 状态说明（2026-09-27）：本文描述长期目标。当前只实现 Phase 5 静态数据面，public/admin 同属一个进程；Gin 控制面、etcd/Redis、ConfigSnapshot、Watch/LKG 等尚未实现。当前模块与依赖以 [AI 架构导航](../.agent/PROJECT.md) 为准。

> 架构目标：用范围可控的实现，形成“控制面发布配置 → 数据面原子切换 → 请求治理与代理 → 可观测与回滚”的完整闭环。

## 1. 架构原则

1. **控制面与数据面分离**：管理流量和用户流量使用不同进程、端口和资源预算。
2. **数据面热路径本地化**：请求处理不访问 etcd；只有启用分布式限流时才访问 Redis。
3. **配置不可变**：新配置完整校验、编译成功后，通过 `atomic.Pointer` 一次切换不可变 `ConfigSnapshot`。
4. **一个请求只使用一个配置版本**：请求开始时获取一次 Config Snapshot；健康、连接数等运行时状态允许在请求期间独立变化。
5. **最后正确配置优先**：新版本失败时继续使用 Last Known Good，不因配置错误中断已有转发。
6. **故障语义显式**：etcd、Redis、upstream、OTel Collector 故障时的行为必须可配置、可观测、可测试。
7. **热路径克制分配和锁**：路由树、策略链和 upstream 引用在发布阶段预编译；“无锁”仅指配置根指针切换与配置读取，不代表所有健康/负载均衡状态都无锁。
8. **可运维优先于功能数量**：每个能力必须有日志、指标、Trace、测试和演示场景。

## 2. 系统上下文

```text
                                Management Network

  Operator / curl / OpenAPI Client
                 |
                 v
       +---------------------+
       |   Control Plane     |
       | Gin Admin API       |
       | validate / publish  |
       | publish / rollback  |
       +----------+----------+
                  |
                  | etcd Txn / CAS
                  v
       +---------------------+
       |        etcd         |
       | versioned snapshots |
       | current config ver. |
       +----------+----------+
                  |
                  | Watch /current
                  v
       +---------------------+          +--------------------+
       | Gateway Data Plane  |--------->| Redis              |
       | net/http            | optional | distributed quota  |
       | config snapshot     |          +--------------------+
       +----------+----------+
                  |
                  | reverse proxy
          +-------+--------+
          |                |
          v                v
   +-------------+   +-------------+
   | user-service|   | order-service|
   +-------------+   +-------------+

  Gateway / Control Plane
        | logs     | metrics       | OTLP traces
        v          v               v
       stdout   Prometheus   OpenTelemetry Collector -> Jaeger
                       |
                       v
                    Grafana
```

## 3. 进程与端口

项目采用一个仓库、一个 Go Module、多个独立二进制：

| 进程 | 默认端口 | 职责 |
|---|---:|---|
| `gateway` | `:8080` | 公网/业务流量入口，执行路由、策略、负载均衡和反向代理 |
| `gateway` admin | `:9090` | `/livez`、`/readyz`、`/metrics`、受保护的调试信息 |
| `control-plane` | `:8081` | 管理 API，配置校验、发布、版本和回滚 |
| `mock-service` | `:18080+` | 本地演示 upstream，不属于正式网关能力 |

控制面和数据面不共享 HTTP Server，也不在同一端口复用路由。

## 4. 控制面设计

### 4.1 主要职责

- 接收声明式网关配置；
- 执行结构校验和跨资源语义校验；
- 检测路由冲突、缺失 upstream、非法 URL 和不合理 timeout；
- 使用 etcd transaction/CAS 发布新的单调递增 `config_version`；
- 查询当前版本和历史版本；
- 回滚到指定历史版本；
- 汇总数据面实例的配置收敛状态；
- 记录管理操作审计日志。

控制面不参与每个代理请求，也不主动向数据面推送可变对象。

### 4.2 管理 API

MVP 优先使用“整份配置文档”API，保证跨资源更新的原子性：

```text
GET    /admin/v1/config
POST   /admin/v1/config/validate
PUT    /admin/v1/config?expected_config_version={version}
GET    /admin/v1/config/versions
GET    /admin/v1/config/versions/{config_version}
POST   /admin/v1/config/rollback/{config_version}
GET    /admin/v1/instances
GET    /livez
GET    /readyz
```

后续可以增加 routes/upstreams 的便捷 CRUD，但其内部仍应生成完整的新 `config_version`，而不是直接修改数据面正在使用的零散 key。

### 4.3 发布流程

```text
Client sends ConfigSpec + expected_config_version
                |
                v
       Decode + strict validate
                |
                v
       Cross-resource validate
       - route conflict
       - upstream reference
       - policy reference
       - timeout relationship
                |
                v
       Canonical JSON + checksum
                |
                v
       etcd transaction
       IF current.config_version == expected_config_version
         PUT /versions/{new_config_version}
         PUT /current = {new_config_version, checksum}
       ELSE reject 409 Conflict
                |
                v
       Return config_version + checksum
```

并发更新使用 `expected_config_version`，并通过 etcd transaction 比较 `/current` 的值或 ModRevision；冲突时返回 HTTP 409，由调用方重新读取配置后决定是否重试。

### 4.4 etcd Key 与版本语义

```text
/gateway/v1/config/current
/gateway/v1/config/versions/{config_version}
/gateway/v1/instances/{instance_id}
```

必须区分三个概念：

| 名称 | 类型 | 含义 |
|---|---|---|
| `config_version` | 单调递增业务版本 | 每次 publish/rollback 都创建新版本，永不倒退 |
| `etcd_mod_revision` | etcd MVCC revision | 用于 CAS、Watch resume 和 compaction 处理 |
| `checksum` | 内容摘要 | 标识规范化配置内容，相同内容可以出现在不同业务版本 |

- `current`：保存 `{config_version, checksum}`；其 ModRevision 单独作为 etcd 并发/Watch 游标；
- `versions/{config_version}`：完整、规范化后的 JSON 配置和发布元数据；
- `instances/{instance_id}`：带 lease 的数据面 ACK，至少包含 `config_version`、`checksum`、`source_etcd_revision`、`apply_status` 和 `apply_error`。

回滚固定采用“复制历史内容、创建新版本”的语义：若当前版本为 N，回滚到历史版本 M，则发布 N+1，内容复制自 M，并记录 `rolled_back_from=M`。这样业务版本保持单调递增，Watch、审计和收敛指标不会因版本倒退而混乱。

历史版本默认保留最近 50 个；`current` 指向的版本和显式标记为 protected 的版本不清理。清理任务只删除不再被引用的旧版本，并记录审计日志。

配置内容存 JSON，YAML 只用于本地启动配置和导入/导出。完整快照方案比拆成大量零散 key 更容易保证跨资源一致性，也足以覆盖本项目规模。

## 5. 数据面设计

### 5.1 请求链路

用户给出的中间件链应拆成“全局链”和“路由策略链”。JWT 和精细限流依赖路由配置，不能全部在路由匹配前执行。

```text
GET /api/user/profile
          |
          v
+-------------------------+
| net/http Server         |
+-------------------------+
          |
          v
+-------------------------+
| Global Middleware       |
| 1. Request Context / ID |
| 2. Trace Context        |
| 3. Access Log / Metrics |
| 4. Recovery             |
| 5. Header & Body Guard  |
+-------------------------+
          |
          v
+-------------------------+
| Immutable Route Match   |
| host + method + path    |
+-------------------------+
          |
          v
+-------------------------+
| Route Policy Chain      |
| 1. local admission      |
| 2. JWT / identity       |
| 3. distributed limit    |
| 4. timeout budget       |
+-------------------------+
          |
          v
+-------------------------+
| Load Balancer           |
| healthy endpoint select |
+-------------------------+
          |
          v
+-------------------------+
| ReverseProxy            |
| rewrite / transport     |
| error mapping / trace   |
+-------------------------+
          |
          v
      user-service
```

Access Log 和 Metrics 使用外层包装，Recovery 位于其内侧：panic 先被转换为最终 500，外层再记录正确状态、耗时和字节。

### 5.2 Config Snapshot 与 Runtime Registry

配置状态和运行时状态分开管理：

```go
type ConfigSnapshot struct {
    ConfigVersion      uint64
    SourceEtcdRevision int64
    Router             Router
    Upstreams          map[string]CompiledUpstream
    Policies           map[string]CompiledPolicyChain
    LoadedAt           time.Time
    Checksum           string
}

type RuntimeRegistry struct {
    EndpointStates  EndpointStateRegistry
    BalancerStates  BalancerStateRegistry
    Transports      TransportRegistry
    HealthTasks     HealthTaskRegistry
    LocalLimiters   LocalLimiterRegistry
}
```

所有权约定：

| 对象 | 可变性与生命周期 |
|---|---|
| `ConfigSnapshot` | 完全不可变；完整编译成功后由 `atomic.Pointer` 切换 |
| `EndpointState` | 进程级可变状态；健康和 active request 使用 atomic/受控锁保护 |
| `BalancerState` | 按 upstream 管理；Round Robin/SWRR 明确重置与复用规则 |
| `TransportRegistry` | 进程级共享；按 transport fingerprint 引用计数和延迟清理 |
| 健康检查任务 | 由 Registry 管理；不直接归属于单个请求或单个快照 |

关键约束：

- Config Snapshot 构建完成前不可被请求读取；
- `atomic.Pointer[ConfigSnapshot]` 只在完整编译成功后 Store；
- 每个请求在入口处 Load 一次同一份配置版本，并把引用传到后续步骤；
- 健康状态、active request 等运行时状态允许在请求期间变化；
- 新配置失败时不覆盖旧指针；
- 指标分别暴露 `config_version`、`source_etcd_revision`、加载耗时和失败原因；
- endpoint 只有在 ID、地址、协议和 TLS 身份未变化时才复用状态；健康检查配置变化会重启对应任务；
- weight 或算法变化时重置相应 Balancer State，避免旧 SWRR current weight 污染新配置；
- 删除 upstream 时先从新配置移除，旧快照的 in-flight 请求完成后再释放引用并关闭 idle connections。

### 5.3 配置 Watch 与热更新

```text
Startup
  -> connect etcd
  -> read /current + its etcd_mod_revision
  -> fetch /versions/{config_version}
  -> validate again
  -> compile ConfigSnapshot
  -> reconcile RuntimeRegistry
  -> atomic Store
  -> ready = true
  -> Watch /current from etcd_mod_revision + 1

Watch event
  -> read config_version/checksum from event
  -> fetch target version
  -> checksum / validate
  -> compile router, policies, upstream refs
  -> reconcile reusable runtime state
  -> atomic Store
  -> report instance ACK with config_version + source_etcd_revision

Watch compacted / disconnected
  -> exponential backoff
  -> full resync /current
  -> resume from the new etcd_mod_revision + 1
  -> continue serving Last Known Good
```

数据面必须二次校验配置，不能完全信任控制面，因为 etcd 中的数据可能来自其他工具或旧版本程序。

## 6. 路由设计

### 6.1 支持范围

v1 支持：

- Host：精确域名、单层通配域名；
- Method：指定 HTTP Method 或任意 Method；
- Path：静态段、`:param`、`*catch-all`；
- Route Priority：只用于无法通过固定语义区分的显式优先级；
- Rewrite：保留路径、去除前缀、替换前缀。

v1 不支持任意正则路由和脚本 DSL。

### 6.2 匹配优先级

固定语义优先于插入顺序：

```text
精确 Host > 通配 Host > 任意 Host
指定 Method > 任意 Method
静态 Path > 参数 Path > 前缀/ catch-all
更长的确定性前缀 > 更短前缀
显式 priority 仅在语义层级相同的情况下参与比较
```

同等优先级下仍可能匹配同一请求的路由，在配置发布阶段直接判定为冲突，不把结果交给运行时插入顺序。

### 6.3 Router 接口

```go
type Router interface {
    Match(host, method, path string) (MatchResult, bool)
}

type RouterCompiler interface {
    Compile(routes []RouteSpec) (Router, error)
}
```

自研 Radix Tree 只负责已校验配置的高效匹配。复杂冲突检查放在编译阶段，并通过独立的线性 reference matcher 做差分测试；reference matcher 不复用 Radix 的解析、冲突或比较函数。

### 6.4 路由规范表

Phase 3 编码前必须把以下语义固化为表驱动测试 Oracle：

| 输入维度 | v1 固定语义 |
|---|---|
| Host 大小写 | ASCII lowercase 后匹配 |
| Host 端口 | 使用规范化 hostname，不把请求端口作为路由条件 |
| Host 尾点 | 去除单个 DNS trailing dot 后匹配 |
| 非法请求 Host | 非法 authority、DNS 字符或 label 边界归一化为空；不能命中 exact/wildcard，仍可由任意 Host 路由接收 |
| 通配 Host | `*.example.com` 只匹配一个合法 DNS label，不匹配 `example.com`、`a.b.example.com` 或 `_bad.example.com` |
| Method | 先精确匹配；HEAD 无显式路由时可回退到 GET 路由，但仍向 upstream 发送 HEAD |
| Query | 完全不参与路由匹配，原样传递给 upstream |
| Path 来源 | 使用 `URL.EscapedPath()` 建立稳定边界；非法转义直接 400 |
| 编码斜杠 | v1 拒绝 `%2F`、`%5C` 等会改变分段语义的编码，避免网关与 upstream 解释不一致 |
| 参数解码 | 按 segment 匹配后再 PathUnescape；要求有效 UTF-8 |
| Dot segment | 拒绝 `.`、`..` 及其编码变体，不自动 Clean |
| 重复斜杠 | 不自动合并；未显式配置时通常 404 |
| 尾斜杠 | `/a` 与 `/a/` 是不同路由，不自动重定向 |
| Priority | 仅在固定语义层级相同时比较；同 priority 仍歧义则拒绝发布 |
| Upstream Host | 默认改为 target host；只有路由显式 `preserveHost` 时保留原 Host |

该规范同时用于 Radix、reference matcher、Rewrite 和安全测试，避免两个实现“共同采用错误假设”仍通过差分测试。

## 7. Upstream、负载均衡与健康检查

### 7.1 Upstream 模型

一个 Upstream 包含：

- 唯一 ID；
- 负载均衡算法；
- 多个 Endpoint；
- 连接和响应超时；
- 主动健康检查配置；
- 被动失败阈值；
- 可选重试策略。

Endpoint 至少包含：

```text
id, scheme, host, port, weight, metadata
```

禁止从请求参数动态拼接任意 upstream URL，避免 SSRF。

### 7.2 算法路线

1. 核心版：Round Robin；
2. 扩展版：Smooth Weighted Round Robin；
3. 可选 v1.1：Least Connections 或一致性 Hash。

算法只选择健康 endpoint。所有 endpoint 都不可用时立即返回 503，不进行无边界等待。

### 7.3 健康状态

- **主动检查（核心版）**：周期请求指定 path，连续失败 N 次标记 unhealthy，连续成功 M 次恢复；
- **被动检查（扩展版）**：连接失败、超时和特定 5xx 增加失败统计；
- **半开恢复（扩展版）**：允许少量探测请求，避免刚恢复就接收全部流量；
- **状态复用**：配置更新时，ID 和地址未变化的 endpoint 尽量复用健康状态和 Transport。

## 8. Reverse Proxy 与连接管理

### 8.1 ReverseProxy 使用约束

- 使用 `ReverseProxy.Rewrite`，不为新代码使用旧式 `Director`；
- 通过 `ProxyRequest.SetURL` 设置目标；
- 先清理客户端伪造的转发头，再按可信代理策略设置 `X-Forwarded-*` 或 `Forwarded`；
- 使用统一 `ErrorHandler` 输出标准网关错误；
- 使用 `ModifyResponse` 完成有限的响应头处理和错误分类；
- 不在每个请求中创建 ReverseProxy 或 Transport。

### 8.2 Transport Registry

连接池按 upstream/transport 配置指纹复用：

```text
TransportKey = scheme + TLS config + timeout config + proxy config
```

配置切换时：

- 未变化的 Transport 继续复用；
- 被删除的 Transport 先停止接收新快照请求；等待旧快照的 in-flight 引用归零后，再执行 `CloseIdleConnections()`；
- 必须用“长请求进行中删除 upstream/切换配置”的测试证明旧请求可完成且最终无资源泄漏；
- 避免每次配置变更导致连接风暴；
- `otelhttp.NewTransport` 包装最终 RoundTripper，保留出口 Trace。

### 8.3 超时预算

区分：

- Client request total timeout；
- Upstream single-attempt timeout；
- Dial timeout；
- TLS handshake timeout；
- Response header timeout；
- Idle connection timeout。

若启用重试，每次尝试共享总预算，不能每次重新获得完整 timeout。客户端取消时应立即取消 upstream request。

### 8.4 重试边界

默认只对幂等请求考虑重试：GET、HEAD、OPTIONS 等。以下情况默认不重试：

- 已开始向客户端写响应；
- 请求体无法安全重放；
- POST 未被显式声明为幂等；
- 超过总 timeout budget；
- 重试会违反限流或业务约束。

## 9. 中间件与流量治理

### 9.1 中间件接口

```go
type Middleware interface {
    Wrap(next http.Handler) http.Handler
}

type CompiledChain struct {
    handler http.Handler
}
```

编译配置时为每条路由生成不可变策略链，避免每请求解析策略配置。

### 9.2 JWT

网关只验证 Token，不负责签发 Token：

- 优先 RS256/ES256 或受信 JWKS；
- 显式限制允许算法；
- 校验 `exp`、issuer、audience；
- `kid` 只能在受信 key set 内查找；
- key/JWKS 使用带过期时间的本地缓存；
- 不记录完整 Token、Authorization 或 Cookie；
- 身份认证与路由授权分开表达；
- 核心版只承诺身份认证，不实现通用授权和 403；扩展版可增加范围受控的 `required_claim` 策略。

### 9.3 限流

采用两层保护：

1. **本地保护**：route/upstream 级 Token Bucket 或并发限制，防止单实例过载；
2. **分布式额度**：Redis Lua/GCRA 或 Token Bucket，控制多实例共享额度。

限流 key 可由 `route_id + subject/tenant/api_key/ip` 组成，但必须有 TTL 和长度限制。禁止在 Prometheus Label 中放 subject、用户 ID 或 IP。

Redis 故障策略按路由配置：

- `fail_open`：核心可用性优先，记录降级指标，同时保留本地保护；
- `fail_closed`：安全或成本敏感接口优先，Redis 不可用时拒绝请求；
- Redis 调用设置很短的独立 timeout，不能拖满整条请求预算。

429 响应应在可以计算时返回 `Retry-After`。

## 10. 配置领域模型

建议的外部配置模型：

```yaml
apiVersion: gateway.example.io/v1alpha1
kind: GatewayConfig
metadata:
  name: default
spec:
  routes:
    - id: user-profile
      match:
        hosts: [api.example.com]
        methods: [GET]
        path: /api/user/profile
      upstream: user-service
      policies: [user-jwt, user-rate-limit]
      timeout: 2s

  upstreams:
    - id: user-service
      algorithm: round_robin
      endpoints:
        - id: user-1
          url: http://user-service-1:8080
          weight: 100
        - id: user-2
          url: http://user-service-2:8080
          weight: 50
      healthCheck:
        path: /healthz
        interval: 5s
        timeout: 500ms

  policies:
    - id: user-jwt
      type: jwt
      jwt:
        issuer: gateway-demo
        audiences: [user-api]
        algorithms: [RS256]

    - id: user-rate-limit
      type: rate_limit
      rateLimit:
        mode: redis
        key: subject
        rate: 100
        burst: 20
        failureMode: fail_open
```

内部模型应将“外部 Spec”和“编译后的 Runtime”分开，避免请求热路径处理 YAML、字符串 duration、URL 和弱类型 map。

## 11. 错误模型

数据面统一返回：

```json
{
  "code": "GATEWAY_TIMEOUT",
  "message": "upstream request timed out",
  "request_id": "0123456789abcdef0123456789abcdef"
}
```

建议映射：

| HTTP | code | 场景 |
|---:|---|---|
| 400 | `BAD_REQUEST` | 非法路径或请求格式 |
| 401 | `UNAUTHENTICATED` | JWT 缺失或无效（Phase 9A） |
| 404 | `NOT_FOUND` | 无匹配路由 |
| 413 | `PAYLOAD_TOO_LARGE` | 超过 body 限制 |
| 429 | `RATE_LIMITED` | 本地或 Redis 限流（Phase 9A/9B） |
| 431 | `REQUEST_HEADER_FIELDS_TOO_LARGE` | Header 字段数超限 |
| 500 | `INTERNAL_ERROR` | 响应开始前发生 panic |
| 502 | `BAD_GATEWAY` | upstream 无效响应或连接异常 |
| 503 | `NO_HEALTHY_UPSTREAM` | 无健康 endpoint |
| 504 | `GATEWAY_TIMEOUT` | upstream 超时 |

对外 message 不暴露内部地址、堆栈或依赖细节；详细原因进入结构化日志和 Trace。

## 12. 可观测性契约

### 12.1 日志字段

每个请求最多输出一条完成访问日志，关键字段：

```text
request_id, trace_id, route_id, config_version,
method, path_template, status, duration_ms,
upstream_id, endpoint_id, attempts,
bytes_in, bytes_out, outcome, error_kind
```

不记录：Authorization、Cookie、完整 JWT、默认请求/响应 body、密码或密钥。

### 12.2 Prometheus 指标

建议首批指标：

```text
gateway_requests_total
gateway_request_duration_seconds
gateway_inflight_requests
gateway_rejections_total
gateway_upstream_requests_total
gateway_upstream_duration_seconds
gateway_upstream_active_requests
gateway_upstream_health
gateway_config_version
gateway_config_source_etcd_revision
gateway_config_reload_total
gateway_config_apply_duration_seconds
gateway_etcd_watch_reconnect_total
gateway_rate_limit_decisions_total
gateway_redis_request_duration_seconds
```

`method`、`status_class`、`upstream_id`、`error_kind` 属于有界标签；`route_id` 只是受配置约束，并非天然低基数。核心指标默认按 upstream/status 聚合；可选的 per-route 指标仅在启动配置允许且路由数不超过阈值时启用，超出预算时拒绝启用或聚合到 `_other`。禁止使用原始 URL、request ID、用户 ID、IP 或错误文本作为 Label。

### 12.3 Trace

建议 Span：

```text
HTTP server span
  -> route.match
  -> auth.jwt
  -> ratelimit.local
  -> ratelimit.redis
  -> upstream.select
  -> HTTP client span
```

通过 W3C Trace Context 向 upstream 传播上下文。Exporter 失败不能阻塞代理主链路。

## 13. 可用性与故障行为

| 故障 | 数据面行为 | 可观测信号 |
|---|---|---|
| etcd 短暂不可用 | 继续使用 Last Known Good；后台重连 | watch reconnect、stale config version、日志 |
| 启动时无有效配置 | `/livez` 正常，`/readyz` 失败，不接业务流量 | readiness、config error |
| 新配置非法 | 拒绝切换，旧配置继续服务 | apply failure、config version、原因 |
| Redis 不可用 | 按路由 fail-open/closed，本地 limiter 继续保护 | Redis error、degraded decision |
| 单个 endpoint 失败 | 摘除并选择其他健康节点 | health、upstream error |
| 所有 endpoint 失败 | 快速返回 503 | unavailable counter |
| OTel Collector 不可用 | 丢弃/缓冲受限，不阻塞请求 | exporter error |
| Prometheus 未抓取 | 不影响请求 | 无特殊业务影响 |
| 控制面宕机 | 数据面继续使用已有配置 | 管理 API 不可用但代理正常 |

## 14. 安全设计

- 公网数据端口与管理端口分离；
- 管理 API 核心版至少使用静态 Admin Token；扩展版再评估更完整认证；
- 配置文件不保存明文生产密钥，使用环境变量或挂载 secret；
- 明确 `TrustedProxies`，不信任任意 `X-Forwarded-For`；
- upstream URL 只允许 `http`/`https` 和受控地址；
- 限制 Header 数量/大小、请求体大小和管理 API 配置体大小；
- 禁止将 Debug Config 无保护暴露到公网；
- JWT 算法白名单，优先非对称签名；
- 管理操作记录操作者、`config_version`、`source_etcd_revision`、checksum 和结果；
- 开发环境可 HTTP，生产意义说明中必须明确 TLS 终止位置。

## 15. 项目目录设计

```text
Gateway/
├── cmd/
│   ├── gateway/                 # 数据面进程入口，只做装配
│   ├── control-plane/           # 控制面进程入口，只做装配
│   └── mock-service/            # 演示 upstream
├── internal/
│   ├── bootstrap/               # 进程启动、生命周期、graceful shutdown
│   ├── config/
│   │   ├── model/               # 外部 ConfigSpec 与基础类型
│   │   ├── loader/              # YAML/env 启动配置
│   │   ├── validation/          # 结构和跨资源校验
│   │   ├── store/               # etcd repository、config version、CAS
│   │   ├── watcher/             # 数据面 Watch 与全量 resync
│   │   └── compiler/            # ConfigSpec -> ConfigSnapshot
│   ├── controlplane/
│   │   ├── handler/             # Gin handlers
│   │   ├── service/             # 发布、回滚、实例状态用例
│   │   └── middleware/          # admin auth、logging、trace
│   ├── dataplane/
│   │   ├── server/              # public/admin HTTP server
│   │   ├── runtime/             # ConfigSnapshot、RuntimeRegistry、请求上下文
│   │   ├── router/              # Radix Tree、匹配和冲突模型
│   │   ├── middleware/          # 全局链和路由策略链
│   │   ├── balancer/            # RR；扩展阶段加入 SWRR
│   │   ├── health/              # 主动/被动健康状态
│   │   ├── proxy/               # ReverseProxy、Rewrite、ErrorHandler
│   │   └── transport/           # Transport registry 和连接生命周期
│   ├── auth/                    # JWT/JWKS 校验
│   ├── ratelimit/               # local 与 Redis limiter
│   ├── observability/           # zap、Prometheus、OTel 初始化
│   └── platform/                # clock、ID 等极少量跨模块适配器
├── api/
│   └── openapi/                 # 控制面 OpenAPI
├── configs/                     # 开发环境 YAML 示例
├── deployments/
│   └── compose/                 # compose.yaml 与容器配置
├── observability/
│   ├── prometheus/
│   ├── grafana/
│   └── otel-collector/
├── tests/
│   ├── integration/
│   ├── e2e/
│   └── fault/
├── benchmarks/                  # 路由和代理压测脚本/结果
├── scripts/                     # 可复现 demo 与 CI 辅助脚本
├── docs/
├── Makefile
├── go.mod
└── README.md
```

初期不创建 `pkg/`。只有确定需要被仓库外部复用的稳定 API 才迁入 `pkg/`，避免过早承诺公共接口。

## 16. 模块依赖方向

```text
cmd/*
  -> bootstrap
     -> controlplane OR dataplane

controlplane
  -> config/model
  -> config/validation
  -> config/store
  -> observability

dataplane
  -> config/model
  -> config/compiler
  -> router / middleware / balancer / proxy
  -> auth / ratelimit / observability

config/model
  -> 尽量只依赖标准库
```

禁止：

- `config/model` 反向依赖 Gin、etcd 或 Redis；
- 数据面依赖控制面 handler；
- 控制面直接修改数据面的内存对象；
- 业务包导入 `cmd/*`；
- 为了“整洁架构”制造大量只有一个实现的空接口。

## 17. 初始性能目标

这些是工程目标，不是尚未测试就可写进简历的结论：

- 请求热路径不访问 etcd；
- 未启用 Redis 策略时，治理链不产生网络调用；
- 路由匹配在配置生效后只读，不持有配置写锁；
- 10,000 条路由 benchmark 先建立本机基线，再锁定 `ns/op`、`B/op` 和 `allocs/op` 的回归阈值；
- 配置更新期间请求无中断且无 data race；
- 代理压测必须同时报告直连 upstream 与经过 gateway 的额外延迟；
- 记录 p50/p95/p99、吞吐、错误率、CPU、内存、GC 和 goroutine；
- 所有简历数字都保存原始脚本、环境参数和结果文件。

## 18. 明确不做

v1 不包含：

- 自研 HTTP/TLS/HTTP2 协议栈；
- Kubernetes Operator、CRD 或 Ingress Controller；
- Service Mesh、Sidecar 注入和完整 mTLS PKI；
- WASM/Lua 动态插件；
- 图形化管理后台；
- 多租户计费平台；
- 任意正则路由 DSL；
- gRPC、WebSocket、HTTP/3 的全量协议矩阵；
- 跨地域多活控制面；
- 自研 Raft 或配置中心。

这些能力可以写入“未来方向”，但不能挤占配置闭环、代理正确性和测试基线。
