# Gateway

一个用 Go 标准库实现的 API 网关，基于 `net/http` 与 `httputil.ReverseProxy` 构建数据面，支持双 HTTP Server 生命周期、优雅停机、连接池复用、统一错误处理和自研不可变压缩 Radix Tree 路由匹配。

> 当前版本：**Phase 3** — 路由语义与 Radix Tree

## 架构

```text
客户端
  │
  ▼
┌───────────────────────────────────────────────┐
│  Gateway                                       │
│                                                │
│  public Server (:8080)                         │
│  ├─ GatewayHandler                             │
│  │  ├─ ParsePath（%XX、编码斜杠、dot segment）  │
│  │  ├─ Router.Match（Host → Method → Path）     │
│  │  ├─ 400 Bad Request（非法路径）              │
│  │  ├─ 404 Not Found（无匹配路由）              │
│  │  └─ 委托给固定目标 Proxy                      │
│  │     ├─ Rewrite（SetURL + 转发头 + preserveHost）│
│  │     ├─ 请求总超时 (Context)                    │
│  │     └─ 502 / 504 错误分类                     │
│  │              │                               │
│  │              ▼                               │
│  共享 http.Transport                            │
│  ├─ 连接池 (MaxConnsPerHost)                   │
│  ├─ TLS / HTTP2 协商                            │
│  └─ CloseIdleConnections (停机时)              │
│                                                │
│  admin Server (:9090)                          │
│  ├─ GET/HEAD /livez                            │
│  └─ GET/HEAD /readyz                          │
└───────────────────────────────────────────────┘
  │
  ▼
upstream 服务
```

## 快速开始

```bash
# 终端 1：启动 mock-service（默认监听 :18080）
make run-mock

# 终端 2：使用默认 configs/gateway.yaml 启动 gateway
make run-gateway
```

验证路由匹配（默认配置只依赖上述一个 mock-service）：

```bash
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

# 非法编码斜杠返回 400
curl -i 'http://127.0.0.1:8080/objects%2F123'

# 未配置的非 GET 请求返回 404
curl -i -X DELETE 'http://127.0.0.1:8080/anything/here'

# 健康端点
curl -i http://127.0.0.1:9090/livez
curl -i http://127.0.0.1:9090/readyz
```

## 配置

Phase 3 将配置明确分为两类：YAML 是 upstream、route 和 policy 等业务配置的唯一事实来源；环境变量只保存随部署环境变化的本地启动参数。默认示例位于 [`configs/gateway.yaml`](configs/gateway.yaml)。

### YAML 业务配置

```yaml
api_version: v1

upstreams:
  - id: mock-service
    endpoints:
      - id: mock-1
        url: http://127.0.0.1:18080
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
- `weight` 必须为正整数，省略时默认取 `100`；
- `request_timeout` 必须是正数 Go duration；
- `rate` 和 `burst` 必须为非负整数；
- 每个 upstream 恰好包含一个 endpoint（Phase 4 将解除多 endpoint 限制）。

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
├── cmd/
│   ├── gateway/            # 网关进程入口
│   └── mock-service/       # 本地演示 upstream
├── configs/
│   └── gateway.yaml        # Phase 3 默认 YAML 示例配置
├── internal/
│   ├── bootstrap/          # 应用生命周期：双 Server、Router、GatewayHandler、Run、Graceful Shutdown
│   ├── config/             # 严格 YAML、启动参数、校验与强类型编译
│   ├── dataplane/
│   │   ├── gateway/        # GatewayHandler：ParsePath → Router.Match → 委托 Proxy
│   │   ├── proxy/          # httputil.ReverseProxy 封装（含 preserveHost）
│   │   ├── server/         # HTTP Server 与健康端点
│   │   ├── transport/      # 共享 http.Transport
│   │   └── response/       # 统一 JSON 错误响应
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
│   └── results/router/     # 原始 benchmark 结果
├── docs/
│   ├── 01-technology-selection.md
│   ├── 02-architecture-design.md
│   ├── 03-development-roadmap.md
│   ├── 04-phase-0-1-implementation-plan.md
│   ├── 05-phase-2-implementation-plan.md
│   └── 06-phase-3-implementation-plan.md
├── .github/workflows/ci.yml
├── Makefile                # fmt / vet / test / race / build / bench / fuzz
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

### preserveHost

`preserve_host: true` 时，Proxy 的 `Rewrite` 在 `SetURL` 之后恢复客户端原始 Host 头，使 upstream 收到的 Host 与客户端请求一致。每个 `{upstreamID, preserveHost}` 组合在启动时预创建一个固定目标 Proxy，禁止每请求创建 Proxy。

### 错误分类

| 场景 | 状态码 | 说明 |
|---|---|---|
| 非法路径（编码斜杠、dot segment） | 400 | `ParsePath` 检测到非法编码 |
| 无匹配路由 | 404 | `Router.Match` 未找到路由 |
| upstream 连接失败 | 502 | 网络或协议错误 |
| upstream 超时 | 504 | 响应头超时或请求总超时 |
| 响应头已写出后中断 | 原状态码 | 不二次改写，客户端收到部分响应 |

### 优雅停机

收到 `SIGINT`/`SIGTERM` 后，`Application.Run` 取消 Context，并发对 public/admin 两个 Server 执行 `Shutdown`，等待活跃请求完成或超时后关闭 Transport 空闲连接，进程退出。

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

# Benchmark（结果保存到 benchmarks/results/router/）
make bench

# Fuzz 测试（各 15 秒）
make fuzz

# 一键全部检查
make fmt-check vet test race build
```

## 技术约束

数据面代理和 HTTP 生命周期继续使用 Go 标准库；Phase 2 仅为严格 YAML 解析引入固定版本的 `gopkg.in/yaml.v3`。Phase 3 的路由匹配器完全自研，不引入第三方路由框架。本阶段不实现多 endpoint 负载均衡、动态配置、限流执行或路径 rewrite。

## 开发路线

| Phase | 主题 | 状态 |
|---:|---|---|
| 0 | 工程骨架与质量基线 | ✅ 完成 |
| 1 | 最小反向代理链路 | ✅ 完成 |
| 2 | 配置模型与严格 YAML | ✅ 完成 |
| 3 | 路由语义与 Radix Tree | ✅ 完成 |
| 4 | Upstream 与 Round Robin | — |
| 5 | 中间件、日志与 Prometheus | — |
| 6 | Gin 控制面与 etcd 发布 | — |
| 7 | Watch、ConfigSnapshot 与 LKG | — |

完整路线见 [开发路线图](docs/03-development-roadmap.md)。

## License

[MIT](LICENSE)
