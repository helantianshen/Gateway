# Phase 3 实施计划：路由语义与不可变 Radix Tree

> 状态：实施、提交前复盘和最终门禁全部完成
>
> 目标版本：v0.3.0（路由语义与 Radix Tree）
>
> Go Module：`github.com/helantianshen/gateway`
>
> Go Toolchain：`go1.26.3 linux/amd64`
>
> License：MIT

## 1. 目标

实现项目最核心的算法模块：自研不可变压缩 Radix Tree 路由匹配器，支持 static、`:param`、`*catch-all` 三种路径模式，在编译阶段检测冲突，并通过独立参考 matcher 的差分测试证明正确性。

## 2. 范围

### 2.1 本阶段实现

- 固化路由规范表为表驱动测试 Oracle（架构文档 6.4）；
- 实现 Host 匹配（大小写归一化、端口剥离、尾点剥离、通配 `*.example.com`）；
- 实现 Method 匹配（精确 + HEAD 回退 GET + 任意 Method 回退）；
- 实现 Path 匹配（EscapedPath、拒绝编码斜杠、拒绝 dot segment、不自动合并/重定向）；
- 支持 static、`:param`、`*catch-all` 三种路径段模式；
- 实现固定优先级（static > param > catch-all；更长前缀 > 更短前缀）；
- 编译阶段冲突检测（完整 specificity tuple 比较）；
- 创建不可变压缩 Radix Tree（静态边压缩）；
- 请求侧只读匹配，返回路由 ID、参数和 upstream 引用；
- 建立独立参考线性 matcher（收集全部候选，独立 specificity 比较器选出唯一结果）；
- 随机差分测试（Radix vs reference matcher，固定随机种子）；
- Fuzz 测试（路径解析和冲突检测）；
- Benchmark（10/1000/10000 条路由）；
- 将 router 接入数据面（新建 GatewayHandler）。

### 2.2 本阶段不实现

- **多 endpoint 负载均衡（Phase 4）**：Phase 3 仍限制每个 upstream 恰好一个 endpoint；
- 主动健康检查（Phase 8）；
- 动态配置、etcd、控制面（Phase 6+）；
- JWT、限流执行、指标（Phase 5/9+）；
- 重试、熔断、服务发现；
- 路径 rewrite（延后，Phase 3 只保留原路径）。

### 2.3 与 Phase 2 的关系

Phase 2 定义了 `ConfigSpec`、`RouteSpec`、`UpstreamSpec`、`EndpointSpec`，但只接受单 catch-all route + 单 endpoint。Phase 3 解除路由限制：
- 多路由、多 upstream 可用；
- **每个 upstream 仍只有一个 endpoint**（Phase 4 解除）；
- 路由匹配结果决定请求转发到哪个 upstream。

## 3. 路由规范表（测试 Oracle）

以下每个维度必须固化为表驱动测试，Radix 和 reference matcher 共用同一份用例：

| 维度 | v1 语义 | 测试要点 |
|---|---|---|
| Host 大小写 | ASCII lowercase | `API.Example.com` == `api.example.com` |
| Host 端口 | 不作为路由条件 | `api.example.com:8080` 匹配 `api.example.com` |
| Host 尾点 | 去除单个 trailing dot | `api.example.com.` == `api.example.com` |
| 通配 Host | `*.example.com` 匹配单层 label | 匹配 `a.example.com`，不匹配 `example.com` 或 `a.b.example.com` |
| Method | 精确 + HEAD 回退 GET + 任意 Method 回退 | 顺序：显式 HEAD → GET fallback → any Method |
| Query | 不参与路由 | `/api?x=1` 和 `/api?y=2` 匹配同一路由 |
| Path 来源 | `URL.EscapedPath()` | 使用 escaped path 建立稳定边界 |
| 编码斜杠 | 拒绝 `%2F`、`%5C` | 返回 400 |
| 参数解码 | segment 匹配后 PathUnescape | 要求有效 UTF-8 |
| Dot segment | 拒绝 `.`、`..` 及编码变体 | 返回 400 |
| 重复斜杠 | 不自动合并 | `/a//b` 不匹配 `/a/b` |
| 尾斜杠 | 不自动重定向 | `/a` 和 `/a/` 是不同路由 |
| Priority | 同语义层级才比较 | 同 priority 的歧义路由拒绝发布 |
| Upstream Host | 默认用 target host | `preserveHost: true` 时保留原 Host |

## 4. 设计决策

### 4.1 Radix Tree 结构（压缩边）

Oracle 指出：原始计划是 Trie 而非真正的 Radix Tree。必须实现静态边压缩——连续 static-only 边合并为最长公共前缀。

```go
// buildNode 仅用于编译期插入、排序和压缩。
type buildNode struct {
    prefix   string
    segType  segmentType
    children []*buildNode
    routes   []*compiledRoute
}

// node 是 freeze 后仅供请求热路径读取的节点。
type node struct {
    prefix   string
    segType  segmentType
    children []*node
    routes   []compiledRoute // 值拷贝；同模式不同 priority 可共存
}
```

参数名不属于共享结构节点：`/:id` 和 `/:name` 共享 param 边，具体参数名保存在叶子的 `compiledRoute.paramNames`，命中后再按位置绑定。static 子边按首段排序，运行时通过二分查找定位，避免高 fan-out 线性扫描。

构建流程：
1. 使用可变 builder 构建树；
2. 连续 static-only 边压缩为最长公共前缀；
3. 深拷贝并冻结所有节点；
4. 冻结节点不引用可变 `ConfigSpec`、`RouteSpec`、`url.URL` 或可修改的外部 map/slice。

### 4.2 路径模式

路径模式按 `/` 分段，每段可能是：
- **static**：`/users` → 精确匹配
- **param**：`/:id` → 匹配任意非空段，提取为参数
- **catch-all**：`/*path` → 匹配剩余所有路径段（包括空）

**catch-all 边界**：
- `/*path` 匹配 `/files/anything/here`
- `/*path` 也匹配 `/files`（catch-all 匹配剩余空串）
- `/*path` 也匹配 `/files/`（catch-all 匹配一个空段）

**参数名约束**：
- 参数名必须非空、只含 `[a-zA-Z][a-zA-Z0-9_]*`
- 同一路径模式中参数名不可重复
- `/:id` 和 `/:name` 视为同一模式（参数名不影响匹配，只影响提取 key）

### 4.3 冲突检测（完整 specificity 语义）

Oracle 指出：冲突检测不能只看"host/method 相同且 path 重叠"。必须按完整优先级元组判断。

两条路由冲突的条件：
1. Host 模式的匹配集合重叠
2. Method 模式的匹配集合重叠
3. Path 模式的匹配集合重叠
4. **且**完整 specificity tuple 相同：
   - Host specificity（exact > wildcard > any）
   - Method specificity（explicit > any）
   - 逐段 Path specificity（static > param > catchAll）
   - 确定性前缀长度（更长 > 更短）
5. **且** priority 相同

实际实现先按归一化 Host 和 Method 分组；同组内 Host/Method specificity 已相同。Path 使用包含 priority、段类型和带长度前缀 static 值的规范化 key 做 O(n) 检测。param/catch-all 名称不进入 key，因此仍完整覆盖 `/:id` 与 `/:name` 的等价冲突，同时避免 10k 路由启动时的 O(n²) 两两比较。

示例：
- `GET /users` 和 `GET /users` → 冲突（完全相同）
- `GET /users/:id` 和 `GET /users/:name` → 冲突（同模式，参数名不影响）
- `GET /users/:id` 和 `GET /users/profile` → 不冲突（static 胜出）
- `GET /users` priority=1 和 `GET /users` priority=1 → 冲突
- `GET /users` priority=1 和 `GET /users` priority=2 → 不冲突（priority 不同）
- exact host `api.example.com` 和 wildcard `*.example.com` → 不冲突（exact 胜出）

### 4.4 Host 匹配

归一化顺序（Oracle 建议的安全顺序）：
1. 安全拆分 host/port（不能简单按最后一个冒号切分，IPv6 会出错）
2. 去除 hostname 的单个 trailing dot
3. ASCII lowercase

通配 `*.example.com` 匹配规则：
- 前缀 `*.` 必须存在且 `*` 之后有非空内容
- 只匹配单层 label：`a.example.com` → 匹配
- 不匹配 `example.com`（没有前缀 label）
- 不匹配 `a.b.example.com`（多层 label）

配置侧拒绝：
- 非 ASCII host
- 非法端口形式
- 多个尾点
- 非前缀形式的 `*`（如 `example.*`）

### 4.5 Method 匹配

匹配顺序（Oracle 建议的固定顺序）：
1. 显式 Method（如 HEAD /api 有显式 HEAD 路由）
2. GET fallback（HEAD 请求无显式 HEAD 路由时，回退到 GET 路由）
3. 任意 Method（route.Method 为空时匹配所有方法）

**重要**：HEAD 回退到 GET 路由时，upstream 收到的仍是 HEAD 请求（不改写 method）。
其他方法不做自动回退（如 OPTIONS 不自动生成）。

### 4.6 Path 安全检查顺序

Oracle 建议的固定顺序：
1. 取得 `URL.EscapedPath()`
2. 扫描并验证所有 `%XX` 转义是否合法
3. 拒绝解码后为 `/` 或 `\` 的转义（`%2F`、`%5C`、`%2f`、`%5c`）
4. 按字面 `/` 分段
5. 每段 `PathUnescape`
6. 验证 UTF-8 有效性
7. 拒绝解码后的 `.` 和 `..`

### 4.7 Router API（区分 400 和 404）

Oracle 指出：`(MatchResult, bool)` 无法区分 400（非法路径）和 404（无匹配）。

```go
// MatchError 表示路由匹配过程中的错误。
type MatchError struct {
    Code    MatchErrorCode
    Message string
}

type MatchErrorCode int
const (
    MatchErrIllegalPath MatchErrorCode = iota // 路径包含非法编码/dot segment → 400
    MatchErrNoRoute                          // 没有匹配的路由 → 404
)

// ParsePath 先把 http.Request 安全解析为解码后的路径段；非法路径返回 MatchErrIllegalPath。
func ParsePath(req *http.Request) ([]string, *MatchError)

// Match 只接受已校验的路径段；无路由时返回 MatchErrNoRoute。
func (r *Router) Match(host, method string, pathSegments []string) (*MatchResult, *MatchError)
```

GatewayHandler 负责将 MatchError 转换为 HTTP 响应：
- `MatchErrIllegalPath` → 400
- `MatchErrNoRoute` → 404

### 4.8 参考 matcher（独立实现）

Oracle 指出：参考 matcher 不能"返回第一个匹配"，否则引入插入顺序依赖。

参考 matcher 必须：
1. 遍历所有路由，收集全部候选
2. 使用独立实现的 specificity 比较器选出唯一结果
3. 如果多个候选 specificity 相同，返回错误（表示配置有冲突，但编译阶段未检测到）

参考 matcher 放在外部测试包（`internal/router/router_test/` 或 `_test.go` 文件），避免生产代码依赖它，也天然无法访问 Radix 私有 helper。

### 4.9 接入数据面（新建 GatewayHandler）

Oracle 建议：不在 ReverseProxy Rewrite 内执行路由匹配（Rewrite 没有 ResponseWriter，不能产生 400/404）。

分层设计：
```text
GatewayHandler（新建）
├── 路径校验（400）
├── 路由匹配（Router.Match）
├── 404 响应
├── upstream 选择
└── 委托给固定目标 Proxy
    ├── Rewrite（SetURL + 转发头）
    ├── 超时
    ├── 502/504
    └── 流式响应
```

- 为每个 `{upstreamID, preserveHost}` 创建一个固定目标 Proxy
- 所有 Proxy 共享同一个 Transport
- 禁止每请求创建 Proxy
- Proxy 继续负责超时、Rewrite、Header 和 502/504
- GatewayHandler 负责 400/404、参数上下文和 upstream 选择

### 4.10 Config 编译（移除 Phase 2 单 route 假设）

Oracle 指出：当前 `bootstrap.go` 的 `Compile` 仍取 `spec.Routes[0]` 并生成单一 `UpstreamURL`。Phase 3 必须改为生成不可变 upstream target map。

```go
type Config struct {
    PublicAddr      string
    AdminAddr       string
    UpstreamURLs    map[string]*url.URL // upstream ID → endpoint URL
    RequestTimeout  time.Duration
    ShutdownTimeout time.Duration
    Spec            *ConfigSpec
}
```

`Compile` 从 `ConfigSpec` 的所有 upstream 提取 endpoint URL，生成 `UpstreamURLs` map。不再只取第一条 route。

### 4.11 解除 Phase 2 约束

Phase 2 的 `validatePhase2Constraints` 调整：
- **移除**：单 route 限制
- **移除**：path 必须为 `/` 限制
- **移除**：host/method 必须为空限制
- **移除**：priority 必须为 0 限制
- **移除**：preserve_host 必须为 false 限制
- **保留**：每个 upstream 恰好一个 endpoint（Phase 4 解除）
- **新增**：`config.Validate` 调用生产 `router.Compile`，统一校验 Host/Method/Path、参数名、RouteID 唯一性和冲突
- **新增**：upstream 和 route 至少各配置一项，避免启动一个永远返回 404 的空网关

### 4.12 preserveHost 实现

`preserveHost: true` 时：
- Proxy 的 Rewrite 在 `SetURL` 之后设置 `r.Out.Host = r.In.Host`
- 需要测试带端口的原始 Host

## 5. 预计文件

```text
internal/router/
├── spec.go              # 类型定义：MatchResult、MatchError、Router 接口
├── tree.go              # 压缩 Radix Tree 实现（builder → freeze）
├── tree_test.go         # 树结构和匹配测试
├── compile.go           # 路由编译和冲突检测
├── compile_test.go      # 编译和冲突测试
├── path.go              # 路径解析、安全检查
├── path_test.go         # 路径安全测试（路由规范表）
├── host.go              # Host 归一化和匹配
├── host_test.go         # Host 匹配测试
├── method.go            # Method 匹配和 HEAD 回退
├── method_test.go       # Method 匹配测试
├── diff_test.go         # 差分测试（Radix vs reference，外部测试包）
├── reference_test.go    # 独立参考 matcher（测试包内）
├── fuzz_test.go         # 路径、冲突对称性、编译匹配 Fuzz
└── bench_test.go        # hit/miss、并发匹配和编译 Benchmark

internal/dataplane/gateway/
├── handler.go           # ParsePath → Match → Context → 固定 Proxy
└── handler_test.go      # 400/404/502、多 upstream、HEAD、preserveHost
```

修改文件：
- `internal/config/validate.go` — 解除 Phase 2 路由约束，保留单 endpoint
- `internal/config/bootstrap.go` — Compile 生成 upstream target map
- `internal/bootstrap/application.go` — 构建 Router 和 GatewayHandler
- `internal/dataplane/gateway/handler.go` — 新建 GatewayHandler 和路由结果 Context
- `internal/dataplane/proxy/proxy.go` — 构造参数增加 preserveHost，并在 SetURL 后恢复原始 Host
- `README.md` — 更新说明
- `Makefile` — 添加 benchmark 和 fuzz 目标
- `configs/gateway.yaml` — 更新示例为多路由

保持核心行为不变的文件：
- `internal/dataplane/transport/transport.go`
- `internal/dataplane/server/server.go`
- `internal/dataplane/response/error.go`

## 6. 实施步骤

### 步骤 1：路径、Host、Method 语义
- 定义 `MatchResult`、`MatchError`、`Router` 接口
- 实现 `normalizeHost`、路径安全检查
- 实现 Method 匹配顺序
- 表驱动测试覆盖路由规范表全部维度

### 步骤 2：冲突检测
- 定义完整 specificity 语义
- 使用结构规范化 key 实现 O(n) 冲突检测
- 表驱动测试与顺序对称 fuzz 覆盖冲突场景

### 步骤 3：压缩 Radix Tree
- 实现可变 builder
- 实现静态边压缩
- 实现冻结
- 实现只读 `Match`
- 验证不可变性和并发安全

### 步骤 4：参考 matcher 与差分测试
- 实现独立线性 matcher（外部测试包）
- 独立 specificity 比较器
- 随机差分测试（固定种子）

### 步骤 5：Fuzz 与 Benchmark
- Fuzz 路径解析、冲突顺序对称性和编译匹配
- Benchmark 10/1000/10000 条路由的 hit/miss，并记录编译与并行匹配
- 同时保存优化前基线和修正后结果到 `benchmarks/results/router/`

### 步骤 6：接入数据面
- 解除 Phase 2 路由约束（保留单 endpoint）
- 修改 Compile 生成 upstream target map
- 构建 GatewayHandler
- 更新示例配置为多路由
- 端到端集成测试

## 7. 验收标准

- [x] 相同输入在 Radix 与参考 matcher 中结果一致；
- [x] 路由结果不依赖配置插入顺序；
- [x] 冲突配置在发布/启动阶段被拒绝；
- [x] 路由规范表中的每个边界输入都有唯一期望结果和表驱动测试；
- [x] reference matcher 不复用 Radix 的解析、冲突或比较函数；
- [x] Router API 能区分 400（非法路径）和 404（无匹配）；
- [x] `go test -race` 无问题；
- [x] fuzz 在约定时长内无 crash；
- [x] benchmark 区分 hit/miss 并记录 `ns/op`、`B/op`、`allocs/op`，原始结果保存到 `benchmarks/results/router/`；
- [x] 合法多路由配置可驱动代理转发到不同 upstream；
- [x] 未匹配路由返回 404；
- [x] 非法路径（编码斜杠、dot segment）返回 400；
- [x] 每个 upstream 仍只有一个 endpoint（Phase 4 解除）；
- [x] `gofmt`、`go vet`、`go test`、`go test -race`、`go build`、Makefile 门禁全部通过；
- [x] 中文注释详细准确；
- [x] 无 Phase 4+ 越界实现。

## 8. 审查契约

Reviewer 使用新鲜上下文，只读检查，并按以下维度报告：

1. Radix Tree 是否真正不可变（builder → freeze，无外部可变引用），是否有静态边压缩；
2. 路由规范表是否每个维度都有表驱动测试；
3. 冲突规范化 key 是否覆盖所有同 specificity 重叠场景，并保持顺序对称；
4. 参考 matcher 是否真正独立（外部测试包，不复用 Radix 逻辑，收集全部候选而非第一个）；
5. 差分测试是否随机且充分，固定种子可复现；
6. Path 安全检查顺序是否正确（EscapedPath → 验证 %XX → 拒绝解码 / → 分段 → PathUnescape → UTF-8 → 拒绝 .）；
7. Host 匹配是否正确（IPv6 安全拆分、大小写、端口、尾点、通配单层）；
8. Method 匹配顺序是否正确（显式 → GET fallback → any）；
9. Router API 是否区分 400 和 404；
10. GatewayHandler 是否正确委托给固定目标 Proxy，Proxy 是否只增加 preserveHost 构造选项而保持代理契约；
11. 现有代理契约是否保持（502/504、流式、取消、转发头）；
12. 单 endpoint 限制是否保留（Phase 3 不做多 endpoint）；
13. Benchmark 和 fuzz 是否可执行；
14. 中文注释是否详细准确；
15. 无 Phase 4+ 越界实现。

## 9. 最终复核命令

```bash
gofmt -w .
gofmt -l .
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./...
make fmt-check vet test race build
```

## 10. 非目标

本轮明确不实现：

- 多 endpoint 负载均衡（Phase 4）；
- 主动健康检查（Phase 8）；
- 动态配置、etcd、控制面（Phase 6+）；
- JWT、限流执行、指标（Phase 5/9+）；
- 重试、熔断、服务发现；
- 路径 rewrite（延后）；
- Docker Compose 环境。

## 11. Oracle 第二想法记录

Oracle（gpt-5.6-sol）审查了本计划，提出以下关键反馈，已在计划中修订：

- **Blocker 修复**：保留单 endpoint 限制，不提前解除到 Phase 4；
- **Blocker 修复**：参考 matcher 收集全部候选，独立 specificity 比较器选出唯一结果，不返回第一个匹配；
- **Blocker 修复**：Router API 增加 typed error 区分 400（非法路径）和 404（无匹配）；
- **Blocker 修复**：实现真正的压缩 Radix Tree（静态边压缩），而非 Trie；
- **Blocker 修复**：Compile 生成不可变 upstream target map，不再只取第一条 route；
- **建议采纳**：新建 GatewayHandler 而非修改 Proxy Rewrite（Rewrite 无 ResponseWriter）；
- **建议采纳**：冲突检测使用完整 specificity tuple；
- **建议采纳**：Path 安全检查固定顺序；
- **建议采纳**：Host 归一化安全顺序（IPv6 安全拆分）；
- **建议采纳**：Method 匹配固定顺序（显式 → GET fallback → any）；
- **建议采纳**：参考 matcher 放在外部测试包；
- **建议采纳**：Fuzz 固定随机种子，Benchmark 记录环境信息。

## 12. 提交前复盘与计划修正

### 12.1 本轮发现并已修复

| 不足 | 影响 | 修正 |
|---|---|---|
| static 子边逐个扫描，且每次用 `strings.Split` 拆压缩 prefix | 旧 10k miss 约 0.47–0.64ms、160072 B/op、10003 allocs/op | static 首段二分定位；直接扫描压缩字符串；Method 热路径不构造切片；无参数结果不创建 Params map |
| 原 benchmark 的 10/1000/10000 请求路径与生成路由不一致 | 只测到 miss，却被描述为通用 Match 性能 | 拆分 hit/miss，新增 compile 和 `RunParallel` benchmark；旧结果保留为 `bench-before-hardening.txt` |
| 冲突检测对同组路由做 O(n²) 两两比较 | 大路由表启动编译成本过高 | 改为包含 priority、段类型和 static 长度前缀的规范化 key，O(n) 检测；增加冲突顺序对称 fuzz |
| GatewayHandler 没有独立测试，也未传递匹配参数上下文 | 400/404、Proxy 对应关系和参数传递只被间接覆盖 | 新增专项测试；`MatchResult` 写入请求 Context，供后续日志/指标读取 |
| `config.Validate` 未执行路由语法和冲突检查 | 非法路由直到 Application 装配阶段才失败，与计划不符 | Validate 调用生产 `router.Compile`；补 Host/Method/Path/冲突和空 routes/upstreams 测试 |
| Application 在路由编译前创建 Transport，且未防御 nil Spec/缺失 target | 无效直接调用可能 panic 或产生不必要资源 | 先校验和编译，再创建 Transport/listener；缺失依赖返回明确错误 |
| 默认 YAML 指向未由 Quick Start 启动的第二个服务 | README 默认步骤不能完整复现 | 默认配置全部指向单个 mock-service；多 upstream 由 GatewayHandler 集成测试证明 |
| 普通单测会创建 benchmark 结果目录 | 测试运行产生工作树副作用 | 删除目录创建测试，由 `make bench` 独立管理结果目录 |

### 12.2 后续阶段继续改进

1. **Phase 4**：只为路由实际引用的 `{upstreamID, preserveHost}` 组合创建运行时代理；接入多 endpoint 后保持 Router 只返回逻辑 upstream ID。
2. **Phase 5**：消费 Context 中的 `MatchResult` 生成 route 级日志和低基数指标，不在中间件重复匹配。
3. **Phase 6/7**：当前 `config.Validate` 与 `bootstrap.New` 为保证边界安全会各编译一次 Router；届时统一为一次 `ConfigSnapshot` 编译并通过 `atomic.Pointer` 整体替换，禁止原地更新树。
4. **Phase 11**：微基准只证明算法热路径，不代表完整网关吞吐；必须补真实 HTTP、直连 upstream 对照、并发连接和 p50/p95/p99 报告。
5. **持续性能门禁**：以 `benchmarks/results/router/bench.txt` 为 Phase 3 锁定基线；同硬件同 Go 版本下显著回归必须解释，不能通过删除场景规避。

### 12.3 锁定的性能与 Fuzz 证据

环境：Go 1.26.3、linux/amd64、AMD Ryzen 7 7735H，均执行 3 次 benchmark。

| 场景 | Phase 3 最终结果 |
|---|---|
| 10 routes hit / miss | 337–359 ns / 209–252 ns |
| 1,000 routes hit / miss | 423–442 ns / 299–318 ns |
| 10,000 routes hit / miss | 483–589 ns / 338–340 ns |
| 1,000 routes parallel hit | 230–252 ns/op |
| high fan-out hit | 469–556 ns/op，400 B/op，4 allocs/op |
| 10,000 routes compile | 203–255 ms，约 10 MB，180136 allocs/op |

旧基线 `bench-before-hardening.txt` 中 10,000 routes miss 为 472–640 µs、160072 B/op、10003 allocs/op；最终 miss 为约 0.34 µs、88 B/op、3 allocs/op。编译 10,000 路由的约 0.2 秒和 10 MB 分配可接受于 Phase 3 静态启动边界，但已列为 Phase 6/7 单次 Snapshot 编译与增量发布需要持续观察的成本。

Phase 0–5 回溯审查在 Go 1.26.5、加入 PathTemplate 元数据和请求 Host DNS 校验后复跑：10,000 routes hit 为 `465–486 ns/op, 416 B/op, 4 allocs/op`，miss 为 `303–306 ns/op, 88 B/op, 3 allocs/op`，未出现规模退化。

15 秒 Fuzz 原始结果保存在 `fuzz-path.txt`、`fuzz-conflict.txt`、`fuzz-match.txt`，三项目标均无 crash。
