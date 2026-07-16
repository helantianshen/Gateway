# Phase 5 实施计划：中间件、结构化日志与 Prometheus

> 状态：实现、验证与最终复核已完成
>
> 目标版本：v0.5.0 / MVP `v0.1.0` 检查点
>
> 基线提交：`7b251f7 Implement Phase 4: multi-endpoint upstreams and round-robin balancing`
>
> Go Toolchain：初始 `go1.26.3`；Phase 0–5 安全审查后升级为 `go1.26.5 linux/amd64`

## 1. 目标

在不改变 Phase 1–4 代理、路由和负载均衡语义的前提下，为 public/admin 请求建立一致的请求上下文和基础可运维闭环：

```text
Request Context / Request ID / Trace placeholder
  -> Access Log + Request Metrics
  -> Recovery
  -> Header & Body Guard
  -> Router -> Upstream -> Endpoint -> Proxy
```

核心交付：

- 每个网关错误响应都携带稳定 request ID；
- 每个请求最多一条主要完成日志；
- 私有 Prometheus Registry 和 admin `/metrics`；
- 请求、延迟、并发、拒绝、upstream、endpoint 状态和配置版本指标；
- route 指标有固定 series 预算，绝不使用原始 path；
- Recovery、观测 ResponseWriter 与 SSE/流式/取消契约兼容；
- 提供不可变路由策略链编译接口，但不实现具体 JWT/限流策略。

## 2. 范围边界

### 2.1 本阶段实现

- 请求级 `RequestContext`；
- Request ID 生成、校验、响应和 upstream 传播；
- W3C `traceparent` 的只读 Trace ID 占位提取；
- Recovery；
- Access Log；
- 请求和响应字节统计；
- Header 数量与请求体大小 Guard；
- 标准错误 JSON 增加 `request_id`；
- zap production logger 与敏感字段排除；
- Prometheus 私有 Registry；
- `/metrics` admin endpoint；
- endpoint healthy/active request scrape collector；
- 空路由策略链的编译模型；
- 流式、取消、panic、错误路径、并发和指标测试。

### 2.2 本阶段不实现

- JWT、JWKS、认证和授权；
- 本地或 Redis 限流、429；
- 真正的 OpenTelemetry SDK、Span 创建和出口 instrumentation；
- 动态配置、ConfigSnapshot 或 config version 切换；
- 主动/被动健康检查；
- retry、熔断和 SWRR；
- 日志采集后端、Prometheus Server、Grafana 或 Compose；
- 请求/响应 body 日志；
- admin token。

## 3. 依赖

固定当前稳定版本：

```text
go.uber.org/zap                         v1.28.0
github.com/prometheus/client_golang     v1.23.2
github.com/felixge/httpsnoop            v1.1.0
```

`httpsnoop` 只用于透明包装 `http.ResponseWriter`，必须保留底层实际支持的 `Flusher`、`Hijacker`、`Pusher`、`ReaderFrom` 等接口，避免自制包装破坏 SSE、流式 copy 或协议升级。

## 4. 请求上下文

新增 `internal/dataplane/requestctx/`：

```go
type Context struct {
    RequestID       string
    TraceID         string
    ConfigVersion   uint64
    RouteID         string
    PathTemplate    string
    UpstreamID      string
    EndpointID      string
    Attempts        int
    ErrorKind       string
    Outcome         string
    ResponseStatus  int
    ResponseStarted bool
    BytesIn         int64
    BytesOut        int64
    UpstreamDuration time.Duration
}
```

所有权与并发约束：

- 每个请求创建一个独立 `*Context`；
- 指针在最外层写入 `context.Context`，内层只修改该请求自己的对象；
- Access Log/Metrics 在 Handler 返回后读取同一指针；
- 不把可变请求上下文放进 ConfigSnapshot 或全局 map；
- 当前处理链同步读写，不为每个字段增加无意义锁；
- endpoint healthy/active 等跨请求状态继续使用 Phase 4 atomic。

Router 的 `MatchResult` 增加 `PathTemplate`，值来自编译期 Route path，不从原始 URL 推导。

## 5. 中间件顺序

实际包装顺序采用：

```text
Initialize RequestContext
  -> Request ID
  -> Trace Context placeholder
  -> Observe（外层：status/bytes/duration/log/metrics）
  -> Recovery（内层：把 panic 转成最终 500）
  -> Header & Body Guard
  -> GatewayHandler
```

Recovery 必须位于 Observe 内侧：Recovery 写出 500 后返回，Observe 才能记录正确的最终 status。若把 Recovery 放在最外层，内层 Access Log 会在 panic 展开时先执行，错误地记录默认 200。

admin 链使用相同 RequestContext、Request ID、Observe 和 Recovery，但不计入 public gateway request metrics；`/metrics` scrape 只写 access log。

## 6. Request ID

Header 固定为 `X-Request-ID`：

- 仅接受长度 1–128、字符集 `[A-Za-z0-9._-]` 的客户端值；
- 非法或缺失值生成 16 字节随机 ID，并编码为 32 位十六进制；
- `crypto/rand` 失败时使用进程内 atomic counter 与时间组合兜底；
- 写入响应 Header；
- 写回 request Header，使 upstream 收到同一个 ID；
- 写入所有网关自身错误 JSON；
- 日志中只记录校验后的值，避免换行或日志注入。

## 7. Trace 占位

Phase 5 不创建 Span，只解析合法 W3C `traceparent`：

```text
00-<32 hex trace id>-<16 hex parent id>-<2 hex flags>
```

合法时把 trace ID 写入 RequestContext 和 access log；缺失或非法时保持空。Phase 10 在该边界接入 OTel。

## 8. Recovery 与响应观测

Observe 使用 `httpsnoop.Wrap` hook 记录：

- 首次 `WriteHeader` 的状态码；
- 隐式 200；
- 写出字节；
- Flush 前的响应开始状态；
- 总耗时。

Recovery 规则：

- 普通 panic 且响应未开始：返回 `500 INTERNAL_ERROR`；
- 普通 panic 且响应已开始：记录诊断日志并转换为 `http.ErrAbortHandler` 中断连接，不二次写响应；
- 原始 `http.ErrAbortHandler`：标记 `RESPONSE_ABORTED` 后原样重新 panic；响应前中断保留观测状态 `0/_none`，不得伪装为隐式 200；
- panic 日志只记录类型和 stack，不记录任意 panic value，避免敏感值泄露。

## 9. Header 与 Body Guard

固定 Phase 5 默认值：

```text
http.Server.MaxHeaderBytes      = 1 MiB
DefaultMaxHeaderFields          = 100
DefaultMaxRequestBodyBytes      = 64 MiB
```

行为：

- Header 字段数超限：`431 REQUEST_HEADER_FIELDS_TOO_LARGE`；
- 已知 Content-Length 超限：`413 PAYLOAD_TOO_LARGE`，不调用 Proxy；
- 未知长度 body 使用等价的 limit+1 流式包装，返回标准 `*http.MaxBytesError`，并保留 `io.WriterTo`；
- `http.MaxBytesError` 在 Proxy ErrorHandler 中映射为 413，不误报 502；
- 不预读或缓存完整 body；
- 测试证明合法 SSE、POST body 和取消传播不回归。

## 10. 标准错误结构

扩展为：

```json
{
  "code": "NO_HEALTHY_UPSTREAM",
  "message": "upstream 暂无健康 endpoint",
  "request_id": "0123456789abcdef0123456789abcdef"
}
```

`request_id` 在未经过中间件的低层单元测试中可为空并通过 `omitempty` 省略。`response.WriteError` 同时把受控 `code` 写入 RequestContext.ErrorKind，供日志和指标使用。

## 11. Access Log

production 使用 JSON zap logger。每个请求最多一条 `request completed` 主日志：

```text
request_id, trace_id, config_version,
route_id, path_template, method,
status, duration_ms,
upstream_id, endpoint_id, attempts,
bytes_in, bytes_out, outcome, error_kind
```

禁止读取或记录：

```text
Authorization, Cookie, Proxy-Authorization,
完整 JWT, query value, 原始 path,
请求/响应 body, 密码, 密钥, 底层网络错误文本
```

404 等未匹配请求的 `route_id/path_template` 为空，不回退到原始 URL。

## 12. Prometheus

新增 `internal/observability/`，每个 Application 创建独立 `prometheus.Registry`，禁止使用 `prometheus.DefaultRegisterer` 或 `promauto` 全局注册。

首批指标：

```text
gateway_requests_total
gateway_request_duration_seconds
gateway_inflight_requests
gateway_rejections_total
gateway_route_requests_total
gateway_upstream_requests_total
gateway_upstream_duration_seconds
gateway_upstream_active_requests
gateway_upstream_health
gateway_config_version
```

核心 label：

```text
method, status_class, upstream_id, error_kind
```

高基数规则：

- 非常见/自定义 Method 归一为 `_OTHER`；
- 空 label 归一为 `_none`；
- `route_id/path_template` 只来自配置；
- 默认 route series 预算 1,000；路由数超限时全部聚合到 `_other`；
- 禁止原始 URL、query、request ID、用户 ID、IP、错误文本作为 label；
- endpoint ID 来自静态配置，可用于 upstream 指标。

Phase 5 静态配置的 `gateway_config_version` 固定为 1；Phase 7 改为实际 Snapshot version。

endpoint active/health 使用自定义 Collector 在 scrape 时读取 Phase 4 `EndpointState` atomics，不在请求热路径重复维护 Gauge。

## 13. Upstream 观测

GatewayHandler 在 endpoint 代理调用前后记录：

```text
Attempts += 1
UpstreamDuration = endpoint.ServeHTTP 完整时长
```

Access/Metrics 在请求结束后统一读取：

- endpoint 连接失败/超时：ErrorKind 分别为 `BAD_GATEWAY`/`GATEWAY_TIMEOUT`；
- 客户端取消：`CLIENT_CANCELED`，不写新响应；
- upstream 自身返回 5xx：status 反映真实响应，ErrorKind 为空，Outcome 为 `upstream_response_error`；
- 不记录底层错误字符串；
- 不增加 retry，attempts 当前只能为 0 或 1。

## 14. 路由策略链

新增 `internal/dataplane/policy/`：

```go
type Middleware interface {
    Wrap(next Handler) Handler
}

type CompiledChain struct { /* immutable */ }
```

编译时固定中间件顺序并拒绝 nil。Phase 5 所有 route 编译为空策略链；Phase 9A 再注入 JWT/本地限流，不在请求中解析策略配置。

## 15. Bootstrap 与生命周期

Application 装配顺序：

```text
Config + Router
  -> production zap logger / private Metrics
  -> shared Transport
  -> CompiledUpstreams + endpoint collector binding
  -> GatewayHandler
  -> public middleware chain
  -> admin handler (/livez /readyz /metrics)
  -> admin middleware chain
  -> listeners / servers
```

Application 拥有 production logger，并在 shutdown 后执行受控 Sync；测试的 `newWithListen` 使用 `zap.NewNop()`，不污染测试输出。

任何 observability 初始化失败都发生在 listener 创建前。

## 16. 文件计划

新增：

```text
internal/dataplane/requestctx/
internal/dataplane/middleware/
internal/dataplane/policy/
internal/observability/
docs/08-phase-5-implementation-plan.md
```

主要修改：

```text
go.mod / go.sum
cmd/gateway/main.go
internal/router/{spec,compile,match,...tests}.go
internal/dataplane/response/error.go
internal/dataplane/gateway/handler.go
internal/dataplane/proxy/proxy.go
internal/dataplane/upstream/upstream.go
internal/dataplane/server/server.go
internal/bootstrap/application.go
README.md
configs/gateway.yaml（只更新注释，不扩展 YAML schema）
docs/03-development-roadmap.md
```

## 17. 验收标准

- [x] 每个 public/admin 响应都有合法 `X-Request-ID`；
- [x] 所有网关 4xx/5xx JSON 包含同一个 request ID；
- [x] request ID 传播到 upstream；
- [x] 每请求最多一条主要 access log；
- [x] 日志不含 Authorization、Cookie、JWT、body 和 query value；
- [x] panic 前未写响应时返回标准 500；
- [x] `http.ErrAbortHandler` 和响应开始后 panic 不二次改状态；
- [x] ResponseWriter 包装不破坏 Flusher/Hijacker/Pusher/ReaderFrom；
- [x] body/header guard 返回 413/431，合法流式请求不缓存；
- [x] `/metrics` 可从 admin Server 抓取；
- [x] 两个 Metrics 实例可在同一测试进程创建，无重复注册 panic；
- [x] 原始 path/request ID/错误文本不出现在 label；
- [x] route 数超过预算时聚合 `_other`；
- [x] endpoint healthy/active metric 直接反映 atomic state；
- [x] 404/502/503/504 保持统一错误结构；
- [x] HEAD、preserveHost、RR、SSE、取消和 Graceful Shutdown 不回归；
- [x] 全量普通测试、race、vet、build 和 Makefile 门禁通过；
- [x] 无 JWT、限流、OTel SDK、动态配置或健康检查越界实现。

## 18. 最终复核命令

```bash
gofmt -w .
git diff --check
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./...
go test -count=100 ./internal/dataplane/requestctx ./internal/dataplane/middleware ./internal/dataplane/policy ./internal/observability
make verify
make audit
```

## 19. 后续阶段衔接

- Phase 6：Gin 控制面与 etcd 发布；
- Phase 7：ConfigSnapshot version 替换静态 version=1；
- Phase 8：更新 EndpointState，现有 endpoint health metric 自动反映；
- Phase 9A：向已编译 route policy chain 注入 JWT/本地限流；
- Phase 10：在 Trace placeholder 边界接入 OTel。

## 20. 验收结果

- `go mod verify`、gofmt、diff check、vet、普通测试、race、build、核心包连续 100 次和 Makefile 门禁全部通过；
- `govulncheck` 首轮发现 go1.26.3 标准库的 3 个可达漏洞，工具链升级到 go1.26.5、间接 x/sys 升级到 v0.44.0 后复扫为 0；
- 回溯修复后覆盖率：全仓 `86.7%`；RequestContext `85.5%`、Middleware `82.6%`、Policy `100%`、Observability `93.8%`、Gateway `93.1%`；
- Nop logger 链约 `1.6 µs/op`、`1296 B/op`、`20 allocs/op`；
- production JSON logger + Metrics 约 `4.0–4.1 µs/op`、`2456 B/op`、`22 allocs/op`（Go 1.26.5）；
- 三实例真实联调解析 12 条 access log，验证每个已知 request ID 恰好一条完成日志且敏感哨兵均未出现；
- RR、错误 JSON、可信 request ID、template-only label、SSE、metrics 和 SIGTERM 均通过；
- Phase 0–5 独立回溯发现的停机超时、非法 wildcard Host、RR 回绕和 pre-write abort 观测问题均已修复并由独立 verifier 确认；
- 原始 benchmark 与联调证据保存在 `benchmarks/results/observability/`。
