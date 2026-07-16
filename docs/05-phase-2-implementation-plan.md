# Phase 2 实施计划：配置模型与严格 YAML

> 状态：已完成，独立复审与主智能体最终复核通过
>
> 目标版本：v0.2.0（配置模型与严格 YAML）
>
> Go Module：`github.com/helantianshen/gateway`
>
> Go Toolchain：`go1.26.3 linux/amd64`
>
> License：MIT

## 1. 目标

让网关由类型安全的静态 YAML 配置驱动，替换 Phase 0/1 的纯环境变量配置方式。YAML 配置经过严格解析和校验后驱动现有反向代理，不合法配置在启动阶段被拒绝，不会启动 public listener。

## 2. 范围

### 2.1 本阶段实现

- 定义 `ConfigSpec`、`UpstreamSpec`、`EndpointSpec`、`RouteSpec`、`PolicySpec` 配置模型；
- 使用 `gopkg.in/yaml.v3` 的 `KnownFields(true)` 严格解析 YAML；
- 环境变量负责本地 BootstrapConfig（监听地址、配置文件路径、shutdown 超时）；
- 结构校验（必填字段、类型）和语义校验（引用存在性、URL 协议、范围）；
- URL、duration、weight 在启动阶段解析成强类型；
- 输出清晰、可定位的配置错误；
- YAML 配置驱动现有代理（upstream URL 和请求超时来自 YAML）；
- 路由规则在配置中定义并校验引用，但实际路由匹配延后到 Phase 3。

### 2.2 Phase 2 运行约束

Phase 2 定义完整的、面向 Phase 3/4 兼容的 YAML schema，但编译阶段只接受以下拓扑：

- 恰好一条 catch-all route（`path: /`），引用一个 upstream；
- 该 upstream 恰好有一个 endpoint；
- route 的 `host`、`method` 等字段留空或未设置。

其他合法但尚不支持的拓扑（多路由、多 endpoint、非 catch-all path）在编译阶段返回明确错误，说明"Phase 2 暂不支持，请等待 Phase 3/4"。这样 Phase 3 解除 route 限制、Phase 4 解除 endpoint 限制时，YAML schema 不需要破坏性变更。

### 2.3 本阶段不实现

- Radix Tree 路由匹配（Phase 3）；
- 多 upstream 负载均衡（Phase 4）；
- 动态配置、etcd、控制面（Phase 6+）；
- JWT、限流执行、指标（Phase 5/9+）；
- 重试、熔断、服务发现。

### 2.4 与 Phase 0/1 的关系

Phase 0/1 使用环境变量配置单一 upstream。Phase 2 将业务配置来源从环境变量切换为 YAML 文件，但 **代理核心代码（ReverseProxy、Transport、Server、Application）不改动**。配置只是换了驱动方式：

```text
Phase 0/1: 环境变量 → Config → Application
Phase 2:   YAML 文件 → ConfigSpec → 校验 → 编译 → Config → Application
```

## 3. 设计决策

### 3.1 配置来源分工

两类配置，边界清晰：

| 来源 | 职责 | 内容 |
|---|---|---|
| 环境变量 | 本地 BootstrapConfig | 监听地址、配置文件路径、shutdown 超时 |
| YAML 文件 | 声明式 ConfigSpec | upstreams、routes、policies |

环境变量留在 bootstrap 层的原因：这些参数因部署环境而异，不应进入业务配置的 checksum。使用 `os.LookupEnv` 严格区分"未设置"和"显式空值"。

### 3.2 旧环境变量迁移

检测已移除的环境变量并返回迁移错误，不静默忽略：

- `GATEWAY_UPSTREAM_URL` → 迁移提示：upstream URL 现在由 YAML 配置
- `GATEWAY_REQUEST_TIMEOUT` → 迁移提示：request timeout 现在由 YAML policies 配置

### 3.3 YAML 库

使用 `gopkg.in/yaml.v3`，原因：

- 标准生态最成熟的 YAML 库；
- 支持 `KnownFields(true)` 严格模式；
- 支持 `yaml.Node` 位置信息；
- 是本项目第一个第三方依赖；
- 固定版本，提交 `go.sum`。

### 3.4 配置模型结构

```yaml
# configs/gateway.yaml
api_version: v1

upstreams:
  - id: mock-service
    endpoints:
      - id: mock-1
        url: http://127.0.0.1:18080
        weight: 100

routes:
  - id: default
    path: /
    upstream: mock-service

policies:
  request_timeout: 3s
  rate: 0
  burst: 0
```

```go
// ConfigSpec 是 YAML 文件的顶层结构。
type ConfigSpec struct {
    APIVersion string         `yaml:"api_version"`
    Upstreams  []UpstreamSpec  `yaml:"upstreams"`
    Routes     []RouteSpec     `yaml:"routes"`
    Policies   PolicySpec      `yaml:"policies"`
}

// UpstreamSpec 定义一组可负载均衡的 endpoint。
// Phase 2 只接受单个 endpoint；Phase 4 解除限制。
type UpstreamSpec struct {
    ID        string         `yaml:"id"`
    Endpoints []EndpointSpec `yaml:"endpoints"`
}

// EndpointSpec 定义一个具体的服务地址。
type EndpointSpec struct {
    ID     string `yaml:"id"`
    URL    string `yaml:"url"`
    Weight int    `yaml:"weight"`
}

// RouteSpec 定义路由规则。
// Phase 2 只接受单条 catch-all route；Phase 3 解除限制。
type RouteSpec struct {
    ID           string `yaml:"id"`
    Host         string `yaml:"host"`
    Path         string `yaml:"path"`
    Method       string `yaml:"method"`
    Upstream     string `yaml:"upstream"`
    Priority     int    `yaml:"priority"`
    PreserveHost bool   `yaml:"preserve_host"`
}

// PolicySpec 定义全局策略。
// Phase 2 只解析和校验，限流执行延后到 Phase 9A。
type PolicySpec struct {
    RequestTimeout string `yaml:"request_timeout"`
    Rate           int    `yaml:"rate"`
    Burst          int    `yaml:"burst"`
}
```

### 3.5 编译后的 Config

保留现有 `Config` 结构体驱动 Application，接口不变。编译过程从 `ConfigSpec` + 环境变量生成 `Config`：

```go
type Config struct {
    PublicAddr      string        // 来自环境变量
    AdminAddr       string        // 来自环境变量
    UpstreamURL     *url.URL      // 来自 YAML（Phase 2 单 endpoint 的 URL）
    RequestTimeout  time.Duration // 来自 YAML（policies.request_timeout）
    ShutdownTimeout time.Duration // 来自环境变量
}
```

同时保留校验后的完整 `ConfigSpec` 引用，为 Phase 3 路由匹配预留。

### 3.6 环境变量

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `GATEWAY_CONFIG_FILE` | `configs/gateway.yaml` | YAML 配置文件路径 |
| `GATEWAY_PUBLIC_ADDR` | `:8080` | public Server 监听地址 |
| `GATEWAY_ADMIN_ADDR` | `:9090` | admin Server 监听地址 |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | Graceful Shutdown 超时 |

### 3.7 校验层次

1. **YAML 语法层**：`KnownFields(true)` 拒绝未知字段；拒绝空文件、`null`、多文档；
2. **结构校验层**：
   - `api_version` 必须为 `"v1"`；
   - upstream ID 非空且唯一；
   - endpoint ID 非空且在 upstream 内唯一；
   - route ID 非空且唯一；
   - endpoint URL 非空；
   - route path 非空；
   - route upstream 非空；
   - weight 为正整数（默认 100）；
   - rate、burst 为非负整数；
   - request_timeout 非空；
3. **语义校验层**：
   - route 引用的 upstream 存在；
   - endpoint URL 是 `http` 或 `https` 绝对 URL；
   - endpoint URL 不包含 userinfo（拒绝 `user:pass@host`）；
   - request_timeout 为正数 duration；
4. **Phase 2 运行约束校验**：
   - 恰好一条 route；
   - route path 为 `/`（catch-all）；
   - route host 和 method 为空；
   - route 引用的 upstream 恰好有一个 endpoint；
   - 不满足时返回明确错误，说明"Phase 2 暂不支持，请等待 Phase 3/4"。

### 3.8 错误格式

两类错误定位精度不同：

- **语法/未知字段错误**：提供文件路径、行号、列号（来自 `yaml.Node`）；
- **语义校验错误**：提供文件路径 + 字段路径（如 `upstreams[0].endpoints[0].url`），不虚假承诺行号。

错误不包含 secret：URL 校验失败时不回显完整原始值；拒绝 userinfo。

错误聚合：一次收集并报告所有问题，不在第一个错误处停止。

示例：
```
配置校验失败:
  configs/gateway.yaml: upstreams[0].endpoints[0].url: URL 协议必须为 http 或 https
  configs/gateway.yaml: routes[1].upstream: 引用的 upstream "user-service" 不存在
  configs/gateway.yaml: policies.request_timeout: 超时值必须为正数
  configs/gateway.yaml: 编译约束: Phase 2 暂不支持多路由，请等待 Phase 3
```

## 4. 预计文件

```text
configs/
└── gateway.yaml                    # 示例配置文件
internal/config/
├── spec.go                         # ConfigSpec, UpstreamSpec, EndpointSpec, RouteSpec, PolicySpec
├── spec_test.go                    # 模型测试
├── loader.go                       # YAML 加载 + KnownFields + 环境变量
├── loader_test.go                  # 加载测试
├── validate.go                     # 结构校验 + 语义校验 + Phase 2 约束
├── validate_test.go                # 表驱动校验测试
├── bootstrap.go                    # 更新：Config 结构和 Load() 适配 YAML
└── bootstrap_test.go               # 更新：适配新配置来源
```

修改文件：

- `cmd/gateway/main.go` — 改为加载 YAML 配置文件；
- `go.mod` / `go.sum` — 添加 `gopkg.in/yaml.v3`；
- `Makefile` — `run-gateway` 目标说明配置文件路径；
- `README.md` — 更新配置说明；
- `docs/05-phase-2-implementation-plan.md` — 本文档。

不修改文件（代理核心保持不变）：

- `internal/bootstrap/application.go`
- `internal/dataplane/proxy/proxy.go`
- `internal/dataplane/transport/transport.go`
- `internal/dataplane/server/server.go`
- `internal/dataplane/response/error.go`

## 5. 实施步骤

### 步骤 1：配置模型与 YAML 加载

- 定义 `ConfigSpec`、`UpstreamSpec`、`EndpointSpec`、`RouteSpec`、`PolicySpec`；
- 实现 `LoadConfig(path)` 读取 YAML 文件并使用 `KnownFields(true)` 解析；
- 拒绝空文件、`null`、多文档；
- 返回 `ConfigSpec` 和包含位置信息的错误。

### 步骤 2：校验

- 实现结构校验（必填、类型、唯一性）；
- 实现语义校验（引用、URL 协议、duration、范围）；
- 实现 Phase 2 运行约束校验；
- 检测旧环境变量并返回迁移错误；
- 错误聚合，一次报告所有问题；
- 表驱动测试覆盖每个校验规则。

### 步骤 3：编译与环境变量

- 实现 `Compile(spec, envOpts)` 将 `ConfigSpec` + 环境变量编译为强类型 `Config`；
- URL 解析为 `*url.URL`；
- request_timeout 解析为 `time.Duration`；
- 环境变量提供监听地址和 shutdown 超时；
- 检测旧环境变量返回迁移错误。

### 步骤 4：接入主入口

- 更新 `cmd/gateway/main.go` 使用新加载流程；
- 更新 `internal/config/bootstrap.go` 的 `Load()` 函数；
- 更新 Makefile run 目标；
- 创建 `configs/gateway.yaml` 示例文件；
- 使用 listener factory 注入验证非法配置不启动 listener。

### 步骤 5：测试与文档

- 补全所有表驱动测试；
- 启动边界测试：非法配置不创建 listener；
- 更新 README 配置说明；
- 运行全部验证命令。

## 6. 验收标准

- [x] 未知 YAML 字段导致启动失败；
- [x] route 引用不存在的 upstream 时失败；
- [x] 非 http/https upstream URL 失败；
- [x] timeout、weight、rate、burst 范围校验有表驱动测试；
- [x] 不合法配置不会启动 public listener（启动边界测试）；
- [x] 配置错误不包含 secret（URL 不回显完整值、拒绝 userinfo）；
- [x] 合法 YAML 配置可驱动代理转发请求；
- [x] 环境变量可设置监听地址和 shutdown 超时；
- [x] 旧环境变量 `GATEWAY_UPSTREAM_URL` 和 `GATEWAY_REQUEST_TIMEOUT` 返回迁移错误；
- [x] Phase 2 运行约束：多路由、多 endpoint、非 catch-all path 返回明确错误；
- [x] `gofmt`、`go vet`、`go test`、`go test -race`、`go build`、Makefile 门禁全部通过；
- [x] 中文注释详细准确；
- [x] 无 Phase 3+ 越界实现。

## 7. 审查契约

Reviewer 使用新鲜上下文，只读检查，并按以下维度报告：

1. Phase 2 范围是否完整且没有越界；
2. YAML KnownFields 严格解析是否正确；
3. 校验是否覆盖所有验收标准（含 rate、burst、Phase 2 约束）；
4. 错误信息是否清晰且不包含 secret；
5. 环境变量与 YAML 的分工是否合理；
6. 配置模型是否面向 Phase 3/4 兼容（upstream/endpoint 分离、route 字段预留）；
7. 现有代理核心是否未被破坏；
8. 启动边界测试是否真实验证非法配置不创建 listener；
9. 测试是否真实验证声明；
10. 中文注释是否详细准确；
11. README、Makefile 和 CI 是否可执行。

blocker/concern 必须修复；纯风格 suggestion 由主智能体判断是否纳入。

## 8. 最终复核命令

```bash
gofmt -w .
gofmt -l .
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./...
make fmt-check vet test race build
```

## 9. 非目标

本轮明确不实现：

- Radix Tree 路由匹配；
- 多 upstream 负载均衡；
- 动态配置、etcd、控制面；
- JWT、限流执行、指标；
- 重试、熔断、服务发现；
- Docker Compose 环境。

## 10. Oracle 第二想法记录

Oracle（gpt-5.6-sol）审查了本计划，提出以下关键反馈，已在计划中修订：

- **Blocker 修复**：PolicySpec 增加 `rate` 和 `burst` 字段及范围校验；
- **Blocker 修复**：明确 Phase 2 运行约束（单 catch-all route + 单 endpoint），不静默忽略多路由；
- **模型调整**：`UpstreamSpec` 包含 `[]EndpointSpec`，weight 放在 endpoint，与 Phase 4 模型一致；
- **模型调整**：`RouteSpec` 增加 `id`、`method`、`priority`、`preserve_host`，为 Phase 3 预留；
- **模型调整**：`ConfigSpec` 增加 `api_version` 顶层字段；
- **错误精度**：语义错误只承诺文件路径 + 字段路径，不虚假承诺行号；
- **迁移检测**：检测旧环境变量并返回迁移错误；
- **启动边界**：使用 listener factory 注入验证非法配置不创建 listener；
- **URL 安全**：拒绝 userinfo，错误不回显完整原始值。
