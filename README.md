# Gateway

一个以 Go `net/http` 与 `httputil.ReverseProxy` 为数据面核心的 API 网关，支持双 Server 生命周期、优雅停机、共享连接池、不可变压缩 Radix Tree、健康感知 Round Robin、请求上下文、结构化日志和 Prometheus 指标。

> 当前版本：**Phase 5** — 中间件、结构化日志与 Prometheus

## 文档导航与当前边界

- [当前实现与 AI 开发导航](.agent/PROJECT.md)：模块依赖、请求链路、状态所有权和修改入口。
- [当前代码审查](docs/10-current-architecture-review.md)：五项已复现且尚未修复的缺陷、测试不足和验证边界。
- [目标架构](docs/02-architecture-design.md)与[阶段路线](docs/03-development-roadmap.md)：包含未来规划，不代表全部已实现。
- [贡献指南](CONTRIBUTING.md)：开发环境与检查命令。

当前配置只在启动时加载，修改后需要重启。没有控制面、热更新、实际限流、主动健康检查或应用级重试。admin 默认 `:9090` 监听全部接口且无认证，应限制其网络可达范围；`/readyz` 只表示运维 Handler 可服务，不验证上游健康。入口 Server 当前提供明文 HTTP。

## 架构

```text
Client
  -> RequestContext / X-Request-ID / traceparent placeholder
  -> Access Log + Request Metrics
  -> Recovery
  -> Header & Body Guard
  -> ParsePath
  -> Router.Match
  -> Compiled Route Policy Chain
  -> CompiledUpstream.Select (healthy filter + Round Robin)
  -> Fixed Endpoint Proxy
  -> Shared http.Transport
  -> Upstream

admin Server (:9090)
  -> /livez
  -> /readyz
  -> /metrics (private Prometheus Registry)
```

## 快速开始

模块要求 Go `1.26.5`，CI 固定使用该版本（没有 `toolchain` 指令）。默认配置包含三个 mock endpoint，需要分别启动三个进程：

```bash
# 终端 1 / 2 / 3
make run-mock-1  # :18080, X-Mock-Instance: mock-1
make run-mock-2  # :18081, X-Mock-Instance: mock-2
make run-mock-3  # :18082, X-Mock-Instance: mock-3

# 终端 4：使用默认 configs/gateway.yaml 启动 gateway
make run-gateway
```

若本机设置了 `HTTP_PROXY`/`HTTPS_PROXY`，下面的本地 curl 命令应追加 `--noproxy '*'`，避免自定义 `Host` 被代理软件截获。

验证 Round Robin 和路由匹配：

```bash
# 连续六次请求的 X-Mock-Instance 应为 mock-1/2/3/1/2/3
for i in 1 2 3 4 5 6; do
  curl -sD - -o /dev/null 'http://127.0.0.1:8080/hello?name=gateway' \
    | grep -i '^X-Mock-Instance:'
done

# 精确 GET 路由
curl -i 'http://127.0.0.1:8080/hello?name=gateway'

# 精确 POST 路由并透传 body
curl -i -X POST 'http://127.0.0.1:8080/echo' -d 'hello body'

# exact Host 匹配 + preserve_host
curl -i -H 'Host: admin.example.com:8443' 'http://127.0.0.1:8080/hello'

# wildcard Host 匹配（单层 label）
curl -i -H 'Host: app.example.com' 'http://127.0.0.1:8080/hello'

# GET catch-all 路由
curl -i 'http://127.0.0.1:8080/anything/here'

# 非法编码斜杠返回 400；Header 和 JSON body 使用同一个 request ID
curl -i -H 'X-Request-ID: demo-bad-path' 'http://127.0.0.1:8080/objects%2F123'

# 未配置的非 GET 请求返回 404
curl -i -X DELETE 'http://127.0.0.1:8080/anything/here'

# 健康端点与 Prometheus 指标
curl -i http://127.0.0.1:9090/livez
curl -i http://127.0.0.1:9090/readyz
curl -s http://127.0.0.1:9090/metrics | grep '^gateway_'
```

## 配置

YAML 是 upstream、endpoint、route 和 policy 等业务配置的唯一事实来源；环境变量只保存随部署环境变化的本地启动参数。Phase 5 沿用严格 schema，不把日志或指标开关塞入环境变量。默认示例位于 [`configs/gateway.yaml`](configs/gateway.yaml)。

### YAML 业务配置

```yaml
api_version: v1

upstreams:
  - id: mock-service
    endpoints:
      - id: mock-1
        url: http://127.0.0.1:18080
        weight: 100
      - id: mock-2
        url: http://127.0.0.1:18081
        weight: 100
      - id: mock-3
        url: http://127.0.0.1:18082
        weight: 100

routes:
  - id: hello
    method: GET
    path: /hello
    upstream: mock-service

  - id: object-detail
    method: GET
    path: /objects/:id
    upstream: mock-service

  - id: admin-hello
    host: admin.example.com
    method: GET
    path: /hello
    upstream: mock-service
    preserve_host: true

  - id: tenant-hello
    host: "*.example.com"
    method: GET
    path: /hello
    upstream: mock-service

  - id: fallback
    method: GET
    path: /*path
    upstream: mock-service

policies:
  request_timeout: 3s
  rate: 0
  burst: 0
```

加载器使用 `gopkg.in/yaml.v3` 的严格字段模式，未知字段、空文档、`null` 和多文档输入都会导致启动失败。完整配置还会经过以下校验：

- `api_version` 当前必须为 `v1`；
- upstream、endpoint 和 route 的 ID 必须非空，并在各自作用域内唯一；
- route 引用的 upstream 必须存在；
- endpoint URL 必须是带 host 的 `http` 或 `https` 绝对 URL，且不允许包含 userinfo；
- 每个 upstream 至少包含一个 endpoint；
- `weight` 必须为正整数，省略时默认取 `100`；Phase 4 普通 RR 不读取权重，SWRR 属于扩展阶段；
- `request_timeout` 必须是正数 Go duration；
- `rate` 和 `burst` 必须为非负整数；当前只解析和校验，没有实际限流效果。

已知边界：YAML merge 中的显式 weight 可能被默认值覆盖；`request_timeout` 从进入 Proxy 开始计时，目前不能保证中断停滞的客户端上传。详见当前代码审查 R1/R5。

路由配置还经过 `router.Compile` 的语法校验和冲突检测：

- 路径模式支持 `static`、`:param`、`*catchAll` 三种段类型；
- 参数名必须非空、只含 `[a-zA-Z][a-zA-Z0-9_]*`，同一路径模式中不可重复；
- catch-all 只能出现在最后一段；
- Host 支持 `exact`、`*.wildcard` 和空（任意）三种形式；
- Method 支持标准 HTTP method、自定义 token 和空（任意 Method）；
- HEAD 请求自动回退到 GET 路由（固定顺序：HEAD → GET → any）；
- 两条路由在相同 Host+Method+Path specificity 且相同 priority 时视为冲突，启动时被拒绝。

### 环境变量启动参数

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `GATEWAY_CONFIG_FILE` | `configs/gateway.yaml` | YAML 业务配置文件路径 |
| `GATEWAY_PUBLIC_ADDR` | `:8080` | public Server 监听地址 |
| `GATEWAY_ADMIN_ADDR` | `:9090` | admin Server 监听地址 |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | Graceful Shutdown 超时，必须为正数 Go duration |

Phase 0/1 的 `GATEWAY_UPSTREAM_URL` 和 `GATEWAY_REQUEST_TIMEOUT` 已移除。若部署环境仍声明任一旧变量，启动会返回迁移错误。

## 项目结构

```text
.
├── .agent/
│   ├── PROJECT.md          # 当前实现与 AI 开发导航
│   └── tasks/              # 本次审查状态与复现证据
├── cmd/
│   ├── gateway/            # 网关进程入口
│   └── mock-service/       # 本地演示 upstream
├── configs/
│   └── gateway.yaml        # Phase 5 三 endpoint YAML 示例配置
├── internal/
│   ├── bootstrap/          # 应用生命周期：双 Server、Router、GatewayHandler、Run、Graceful Shutdown
│   ├── config/             # 严格 YAML、启动参数、校验与强类型编译
│   ├── dataplane/
│   │   ├── gateway/        # ParsePath → Router → Policy → Upstream → Proxy
│   │   ├── middleware/     # Request ID、Observe、Recovery、Guard
│   │   ├── requestctx/     # 请求级 Route/Endpoint/结果元数据
│   │   ├── policy/         # 不可变 route policy chain
│   │   ├── balancer/       # 原子 CAS Round Robin
│   │   ├── upstream/       # CompiledUpstream、EndpointState 与 active request
│   │   ├── proxy/          # httputil.ReverseProxy 封装（含 preserveHost）
│   │   ├── server/         # public/admin Server 与运维端点
│   │   ├── transport/      # 共享 http.Transport
│   │   └── response/       # 含 request_id 的统一 JSON 错误
│   ├── observability/      # zap 与私有 Prometheus Registry
│   └── router/             # 不可变压缩 Radix Tree 路由匹配器
│       ├── spec.go         # MatchResult、MatchError、Router 类型定义
│       ├── tree.go         # buildNode → compressStaticEdges → freezeNode
│       ├── compile.go      # 路由编译、冲突检测、Host/Method 分组
│       ├── match.go        # 只读匹配：Host → Method → Path specificity
│       ├── path.go         # 路径安全检查（EscapedPath → %XX → dot segment）
│       ├── host.go         # Host 归一化（IPv6、端口、尾点、通配单层）
│       ├── method.go       # Method 校验和 HEAD 回退
│       ├── reference_test.go # 独立参考 matcher（外部测试包 router_test）
│       ├── diff_test.go    # Radix vs reference 差分测试
│       ├── fuzz_test.go    # Fuzz 测试
│       └── bench_test.go   # Benchmark
├── benchmarks/
│   └── results/
│       ├── router/         # Router benchmark 与 fuzz 原始结果
│       ├── balancer/       # Round Robin benchmark 原始结果
│       └── observability/  # Phase 5 真实进程联调证据
├── docs/
│   ├── 01-technology-selection.md
│   ├── 02-architecture-design.md
│   ├── 03-development-roadmap.md
│   ├── 04-phase-0-1-implementation-plan.md
│   ├── 05-phase-2-implementation-plan.md
│   ├── 06-phase-3-implementation-plan.md
│   ├── 07-phase-4-implementation-plan.md
│   ├── 08-phase-5-implementation-plan.md
│   ├── 09-phase-0-5-retrospective-audit.md
│   └── 10-current-architecture-review.md
├── .github/workflows/ci.yml
├── Makefile                # verify / audit / benchmark / fuzz / run
├── go.mod
├── LICENSE                 # MIT
└── CONTRIBUTING.md
```

## 核心设计

### 不可变压缩 Radix Tree

路由匹配器在启动时将所有路由编译为一棵不可变的压缩 Radix Tree：

- **builder → compress → freeze**：构建阶段使用可变 `buildNode`，静态边压缩合并连续 static-only 子链，冻结阶段深拷贝为只读 `node`，不保留 builder 或配置引用；
- **Host → Method → Path** 分层 specificity：exact host > wildcard > any；显式 method > GET fallback > any；static > param > catch-all；更长前缀 > 更短前缀；priority 仅在前述语义层级完全相同时生效；
- **编译期冲突检测**：两条路由在相同 Host+Method+Path specificity 且相同 priority 时视为冲突，启动时被拒绝；
- **插入顺序无关**：子节点固定排序（static > param > catchAll，同类型按字典序），叶子按 priority 降序排列；
- **参数名不影响结构**：`/:id` 与 `/:name` 共享同一个 param 分支，参数名在匹配成功后从叶子的 `compiledRoute.paramNames` 绑定；
- **独立参考 matcher**：差分测试使用外部测试包 `router_test` 中的独立线性 matcher，不复用生产实现的解析、冲突或比较函数，收集全部候选后用独立 specificity 比较器选出唯一最优项。

### 路径安全检查

请求路径经过固定顺序的安全检查：

1. `URL.EscapedPath()`（非 `URL.Path`，后者已解码，无法区分编码斜杠）
2. 验证所有 `%XX` 转义是否合法
3. 拒绝解码后为 `/` 或 `\` 的转义（`%2F`、`%5C`）
4. 按字面 `/` 分段
5. 每段 `PathUnescape`
6. 验证 UTF-8 有效性
7. 拒绝解码后的 `.` 和 `..`

非法路径返回 `400 Bad Request`，无匹配路由返回 `404 Not Found`。

### 请求上下文与全局中间件

每个请求在最外层创建独立 `RequestContext`。GatewayHandler 把编译期 `route_id/path_template/upstream_id` 和实际 `endpoint_id` 写入同一个对象，外层日志和指标在 Handler 返回后读取，不重新匹配原始 URL。

实际链路为 `Request ID → Trace 占位 → Observe → Recovery → Guard → GatewayHandler`。Observe 位于 Recovery 外侧，因此 panic 转成 500 后能记录正确最终状态；`http.ErrAbortHandler` 和响应开始后的 panic 只中断连接，不二次改写响应。

`X-Request-ID` 只接受长度 1–128 的 `[A-Za-z0-9._-]`；缺失或非法时生成 32 位小写十六进制 ID。该 ID 的设计契约是进入响应 Header、网关错误 JSON、access log 和 upstream 请求 Header；当前 1xx 响应和 Connection token 场景存在缺口，见审查 R3/R4。

Server 限制请求头大小 1 MiB；Guard 按不同 Header 名称计数，最多 100 个（不计注入的 Request ID），请求体最大 64 MiB。未知长度 body 仍为流式读取，超限返回 413，不缓存完整 body。

### 结构化日志与 Prometheus

production access log 使用 zap JSON，每请求最多一条 `request completed`，包含 request/trace/route/upstream/endpoint、status、duration、attempts 和字节数。该 access log 不读取 Authorization、Cookie、JWT、query value、原始 path 或 body。ReverseProxy 另有默认标准日志出口，异常 Trailer 可记录上游原文，见审查 R2。

每个 Application 使用独立 Prometheus Registry。核心指标包括请求量、耗时、并发、拒绝、route、upstream、endpoint health/active 和静态 config version。Method 非标准值聚合为 `_OTHER`；route 数超过 1,000 时 `route_id/path_template` 聚合为 `_other`，原始 URL、request ID、用户 ID、IP 和错误文本永不作为 label。

### Upstream 与 Round Robin

Router 只返回逻辑 `UpstreamID`；`CompiledUpstream` 再用私有 Round Robin cursor 从 endpoint pool 中选择健康节点。普通 RR 使用单调 `atomic.Uint64` 和 CAS，把 cursor 推进到实际选中位置之后：固定健康集合的序列确定，并发请求不会消费同一游标状态。选择热路径不分配内存。

`CompiledEndpoint` 的 ID、URL、weight 和 Proxy 在启动后不变；独立 `EndpointState` 用 atomic 保存 healthy 与 active request。Phase 4 默认全部健康，测试可注入状态；主动健康检查在 Phase 8 接入同一状态对象。所有 endpoint 不健康时立即返回 503，不等待且不重试。

### preserveHost

`preserve_host: true` 时，Proxy 的 `Rewrite` 在 `SetURL` 之后恢复客户端原始 Host 头，使 upstream 收到的 Host 与客户端请求一致。Application 只为路由实际使用的 `{upstreamID, preserveHost}` 模式预创建每个 endpoint 的固定目标 Proxy；所有 Proxy 共享同一个 Transport，禁止每请求创建。

### 错误分类

| 场景 | 状态码 | 说明 |
|---|---|---|
| Header 字段数超限 | 431 | `REQUEST_HEADER_FIELDS_TOO_LARGE` |
| 请求体超过 64 MiB | 413 | `PAYLOAD_TOO_LARGE`，已知长度不执行 Proxy |
| 非法路径（编码斜杠、dot segment） | 400 | `BAD_REQUEST` |
| 无匹配路由 | 404 | `NOT_FOUND` |
| 所有 endpoint 不健康 | 503 | `NO_HEALTHY_UPSTREAM`，不执行 RoundTrip |
| upstream 连接失败 | 502 | `BAD_GATEWAY` |
| upstream 超时 | 504 | `GATEWAY_TIMEOUT` |
| 未写响应前发生 panic | 500 | `INTERNAL_ERROR` |
| 客户端取消 | 内部记为 499 | 不向已断开的客户端写新响应 |
| 响应头已写出后中断 | 原状态码 | 不二次改写，客户端收到部分响应 |

上表状态描述最终响应尚未写出时的应用层行为；1xx 的状态记录缺陷见审查 R3。net/http 在进入中间件前拒绝的请求不保证统一 JSON 或 Request ID。

所有由应用错误辅助函数产生的 JSON 错误均包含 `code`、`message` 和 `request_id`。

### 优雅停机

收到 `SIGINT`/`SIGTERM` 后，`Application.Run` 取消 Context，并发对 public/admin 两个 Server 执行 `Shutdown`，等待活跃请求完成或超时后关闭 Transport 空闲连接、刷新 production logger，然后退出。

## 开发

```bash
# 格式化
make fmt

# 静态检查
make vet

# 单元测试
make test

# 竞态检测
make race

# 构建
make build

# Router Benchmark（结果保存到 benchmarks/results/router/）
make bench

# Round Robin Benchmark（结果保存到 benchmarks/results/balancer/）
make bench-balancer

# Phase 5 中间件 Benchmark（结果保存到 benchmarks/results/observability/）
make bench-observability

# Router Fuzz 测试（各 15 秒）
make fuzz

# 本地核心门禁（不临时下载额外工具）
make verify

# 固定版本 staticcheck + govulncheck
make audit
```

## 技术约束

数据面代理和 HTTP 生命周期继续使用 Go 标准库；严格 YAML 使用 `gopkg.in/yaml.v3`，结构化日志使用 `zap`，指标使用 Prometheus client，ResponseWriter 透明包装使用 `httpsnoop`。路由匹配器、route policy chain 和普通 Round Robin 均为项目内实现。Phase 5 不实现 JWT、限流、真正的 OTel SDK、主动健康检查、重试或动态配置。

## 开发路线

| Phase | 主题 | 状态 |
|---:|---|---|
| 0 | 工程骨架与质量基线 | ✅ 完成 |
| 1 | 最小反向代理链路 | ✅ 完成 |
| 2 | 配置模型与严格 YAML | ✅ 完成 |
| 3 | 路由语义与 Radix Tree | ✅ 完成 |
| 4 | Upstream 与 Round Robin | ✅ 完成 |
| 5 | 中间件、日志与 Prometheus | ✅ 完成 |
| 6 | Gin 控制面与 etcd 发布 | — |
| 7 | Watch、ConfigSnapshot 与 LKG | — |

完整路线见 [开发路线图](docs/03-development-roadmap.md)。Phase 0–5 的独立回溯发现、修复与验证证据见 [全量回溯审查报告](docs/09-phase-0-5-retrospective-audit.md)。

## License

[MIT](LICENSE)
