# API 网关开发路线与验收计划

> 状态说明（2026-09-27）：Phase 0–5 的完成勾选表示历史阶段验收，不能推导为无缺陷或生产就绪。本次新增五项未修复发现，见 [当前审查](10-current-architecture-review.md)；当前实现见 [AI 架构导航](../.agent/PROJECT.md)。后续阶段仍为规划。

> 建议节奏：每周 8–12 小时，约 12–16 周完成可投递核心版；完整扩展路线更现实的周期是 20–30 周。
> 核心方法：每个阶段都形成可运行的纵向切片，不允许长期停留在“只有目录和接口”的状态。

## 1. 版本范围

### MVP（Phase 0–5）

目标：证明网关主链路正确。

包含：

- 单 Go Module 和工程质量基线；
- `net/http` 数据面；
- 静态 YAML 配置；
- 自研路由语义与 Radix Tree；
- ReverseProxy；
- Round Robin；
- 基础中间件、日志和 Prometheus；
- 单元测试、race、fuzz 和初始 benchmark。

### 可投递核心版（约 12–16 周）

目标：形成最有企业意义的动态配置闭环，而不是一次接入全部组件。

在 MVP 上新增：

- Gin 控制面；
- etcd 单调 `config_version`、CAS、Watch、Last Known Good 和复制式回滚；
- 不可变 `ConfigSnapshot` 与独立 `RuntimeRegistry`；
- 主动健康检查；
- JWT 或本地限流至少完成一个策略闭环；时间允许再完成另一个；
- 核心 Compose、E2E、故障脚本和 Vegeta 对照报告。

Phase 7 完成后发布一个“动态配置核心版”中途里程碑；即使扩展功能延期，也已有可投递成果。

### 完整扩展版（完整路线约 20–30 周）

在核心版稳定后增加：

- Smooth Weighted Round Robin；
- Redis 跨实例共享额度和 fail-open/fail-closed；
- OpenTelemetry Trace、Collector 和 Jaeger；
- 被动健康、更多 Dashboard、k6 复杂场景和完整故障矩阵。

### v1.1（可选增强）

候选：

- timeout budget 下的幂等重试；
- 熔断和半开探测；
- least-connections 或一致性 Hash；
- JWKS 自动刷新；
- WebSocket/SSE 专项验证；
- pprof 与更细致的性能优化。

v1.1 不应在核心配置闭环、测试和压测尚未完成时提前开发。

## 2. 里程碑总览

| Phase | 主题 | 建议耗时 | 版本归属 | 可演示结果 |
|---:|---|---:|---|---|
| 0 | 工程骨架与质量基线 | 2–3 天 | MVP（已完成） | gateway 进程可构建、测试、优雅退出 |
| 1 | 最小反向代理链路 | 4–6 天 | MVP（已完成） | 一个请求可稳定代理到 mock upstream，代理契约被测试固定 |
| 2 | 配置模型与严格 YAML | 4–6 天 | MVP（已完成） | YAML 可校验并驱动代理，不合法配置拒绝启动 |
| 3 | 路由语义与 Radix Tree | 1–2 周 | MVP（已完成） | host/method/path 规范、冲突检测、fuzz、benchmark |
| 4 | Upstream 与 Round Robin | 1 周 | MVP（已完成） | 多实例 RR；注入健康状态后正确选择 |
| 5 | 中间件、日志与指标 | 1 周 | MVP（已完成） | 请求链有统一错误、结构化日志和 Prometheus 指标 |
| 6 | Gin 控制面与 etcd 发布 | 1–2 周 | 核心版 | validate、CAS publish、config version、复制式 rollback |
| 7 | Watch、ConfigSnapshot 与 LKG | 1–2 周 | 核心版 | 多次发布不中断请求，失败保留 Last Known Good |
| 8 | 主动健康检查与摘除 | 1 周 | 核心版 | upstream 故障自动摘除和恢复 |
| 9A | JWT / 本地限流策略切片 | 1 周 | 核心版 | 至少一个策略闭环；两者可独立验收 |
| 9B | Redis 分布式限流 | 1 周 | 扩展版 | 多实例共享额度，故障策略可验证 |
| 10 | OpenTelemetry Trace | 1 周 | 扩展版 | 完整 Trace；Phase 1 代理契约继续通过回归 |
| 11 | Compose、E2E、故障与压测 | 1–2 周 | 核心/扩展 profiles | 一条命令启动对应演示，生成可复现报告 |
| 12 | 文档、Release 与简历收尾 | 3–5 天 | 对应发布版 | README、架构图、Demo、Release 和证据索引完成 |

## 3. 分阶段计划

## Phase 0：工程骨架与质量基线

### 目标

建立最小但可靠的工程底座，不提前创建所有业务抽象。

### 工作内容

- 初始化 Git 与单 Go Module；
- 在 `go.mod` 中记录 Go toolchain 基线；Phase 0–5 安全审查后升级到 1.26.5；
- 只创建 `cmd/gateway`；`cmd/control-plane` 延后到 Phase 6，避免长期维护无业务价值的空壳进程；
- 实现统一启动、信号处理和 graceful shutdown 骨架；
- 创建最小 Makefile：`fmt`、`vet`、`test`、`race`、`build`；
- 添加 CI；
- 添加 `.gitignore`、LICENSE、基础贡献约定；
- 确定配置、日志和错误的命名规范。

### 验收标准

- [x] `go build ./...` 成功；
- [x] `go test ./...` 成功；
- [x] `go test -race ./...` 成功；
- [x] gateway 进程收到 SIGTERM 后在超时内退出；
- [x] `cmd/*` 只负责依赖装配，不包含业务逻辑；
- [x] CI 在干净环境可复现执行。

### 暂不做

- 不创建大批空 interface；
- 不引入 etcd、Redis、OTel；
- 不写完整 Compose。

---

## Phase 1：最小反向代理链路

### 目标

用最少代码完成 `Client -> Gateway -> mock upstream`。

### 工作内容

- 使用 `httptest` 或 `cmd/mock-service` 提供 upstream；
- 创建 Data Plane HTTP Server；
- 使用 `httputil.ReverseProxy.Rewrite`；
- 创建共享 `http.Transport`，设置基础连接超时和连接池参数；
- 实现 `ErrorHandler`；
- 处理客户端取消、上游连接失败和超时；
- 添加 `/livez` 和 `/readyz`。

### 验收标准

- [x] GET/POST 的 method、escaped path、query 和 body 正确转发；
- [x] Hop-by-hop headers、重复转发头和客户端伪造的 `X-Forwarded-*` 有明确清理测试；
- [x] 大请求体采用流式转发或受控限制，不为通用重试无上限缓存；
- [x] SSE/流式响应按预期 flush；upstream 在已写响应头后断连时不尝试改写为新的 502；
- [x] 客户端取消后 upstream context 被取消；
- [x] upstream 连接失败返回标准 502；
- [x] upstream 超时返回标准 504；
- [x] 多次请求复用 Transport，而不是每请求新建连接池；
- [x] `httptest` 覆盖成功、断连、慢响应、异常响应和响应头后断连；
- [x] 本阶段形成固定代理契约；Phase 10 只增加 Trace，不重新定义基础超时和错误语义；受限重试留到 v1.1。

### 演示点

```bash
curl http://localhost:8080/hello
```

展示 upstream 收到的 path、headers 和 trace/request ID 占位字段。

---

## Phase 2：配置模型与严格 YAML

### 目标

让网关由类型安全的静态配置驱动。

### 工作内容

- 定义 `ConfigSpec`、`RouteSpec`、`UpstreamSpec`、`PolicySpec`；
- YAML 使用 `KnownFields(true)`；
- 支持环境变量覆盖启动连接信息；
- 实现结构校验和语义校验；
- URL、duration、weight 等在启动阶段解析成强类型；
- 输出清晰、可定位的配置错误。

### 验收标准

- [x] 未知 YAML 字段导致失败；
- [x] route 引用不存在 upstream 时失败；
- [x] 非 http/https upstream URL 失败；
- [x] timeout、weight、rate 范围校验有表驱动测试；
- [x] 不合法配置不会启动 public listener；
- [x] 配置错误不包含 secret。

---

## Phase 3：路由语义与不可变 Radix Tree

### 目标

完成项目最核心的算法模块，并证明其正确性而不是只展示“写过 Trie”。

### 工作内容

- 先按架构文档 6.4 固化路由规范表：Host 大小写/端口/尾点、HEAD 回退、EscapedPath、编码斜杠、dot segment、重复斜杠、尾斜杠和 upstream Host；
- 支持 static、`:param`、`*catch-all`；
- 实现固定优先级；
- 在编译阶段检测歧义和冲突；
- 创建不可变节点结构；
- 请求侧只读匹配；
- 建立参考线性 matcher；
- 与参考 matcher 做随机差分测试；
- 对路径解析和冲突检测做 fuzz；
- 对 10/1000/10000 条路由分别做 hit/miss benchmark，并记录编译和并行匹配；
- 高 fan-out static 子边采用二分定位，压缩 prefix 比较不得在请求热路径 `strings.Split`；
- GatewayHandler 把 `MatchResult` 写入 Context，供 Phase 5 日志/指标消费；
- 可选与 `httprouter`、ServeMux 对照。

### 验收标准

- [x] 相同输入在 Radix 与参考 matcher 中结果一致；
- [x] 路由结果不依赖配置插入顺序；
- [x] 冲突配置在发布/启动阶段被拒绝；
- [x] 路由规范表中的每个边界输入都有唯一期望结果和表驱动测试；
- [x] reference matcher 不复用 Radix 的解析、冲突或比较函数；
- [x] `go test -race` 无问题；
- [x] fuzz 在约定时长内无 crash；
- [x] benchmark 区分 hit/miss，记录 `ns/op`、`B/op`、`allocs/op`；
- [x] 10,000 路由 miss 不得退化为逐 static 子边线性扫描或每路由一次分配；
- [x] GatewayHandler 专项测试覆盖 400/404、HEAD fallback、多 upstream、preserveHost 和路由 Context；
- [x] benchmark 原始结果和提交前优化基线保存到 `benchmarks/results/router/`。

### 面试准备

必须能解释：

- 为什么配置更新不直接修改现有树；
- 为什么不能依赖插入顺序；
- 冲突检测与请求匹配为什么要分开；
- 复杂度、内存占用和参数提取方式。

### Phase 3 提交前复盘对后续阶段的约束

- **Phase 4**：从“每个 upstream 预建两个 preserveHost Proxy”收敛为只构建路由实际引用的 `{upstreamID, preserveHost}` 组合；多 endpoint 状态不得写入不可变 Router。
- **Phase 5**：直接消费 GatewayHandler 写入 Context 的 `RouteID`/`UpstreamID`，日志和指标不得重新匹配或使用原始 path 作为 label。
- **Phase 6/7**：消除 `config.Validate` 与 `bootstrap.New` 的重复 Router 编译，统一产出不可变 `ConfigSnapshot`，通过原子指针整体替换，禁止就地修改现有树或 Proxy map。
- **Phase 11**：补充真实 HTTP 并发压测、直连 upstream 对照和 100/1,000/10,000 routes 的 p50/p95/p99；微基准不能替代 E2E 代理压测。

---

## Phase 4：Upstream 与 Round Robin

### 目标

把单 upstream URL 扩展为可健康选择的 endpoint pool。

### 工作内容

- 定义不可变 `CompiledUpstream` 与独立可变 `EndpointState`；
- 实现 Round Robin；
- 扩展路线再实现 Smooth Weighted Round Robin，并明确 weight 变化时状态重置；
- active request 使用原子计数；
- 无健康节点时快速失败；
- 为相同 endpoint ID 保留稳定身份；
- 为算法添加确定性测试和并发测试。

### 验收标准

- [x] RR 使用确定性序列和固定样本验证均匀分布；
- [x] 通过测试注入 health state 后，unhealthy endpoint 不被选择；
- [x] 注入全部 unhealthy 状态时返回 503；
- [x] 本阶段明确不实施 SWRR，weight 继续校验但普通 RR 不读取；
- [x] 并发选择无 data race；
- [x] 测试不依赖真实时间和随机睡眠；
- [x] 基准记录 1/10/100 endpoint、稀疏健康集合和并行选择开销。

### 实施结果（2026-07-15）

- `CompiledUpstream`、`CompiledEndpoint` 与独立原子 `EndpointState` 已接入数据面；
- Round Robin 使用单调 `atomic.Uint64` + CAS，健康集合内确定性轮转；
- 全不健康返回 `503 NO_HEALTHY_UPSTREAM`，不执行 RoundTrip；
- endpoint active request 覆盖普通、阻塞和取消路径；
- Gateway Context 同时提供 Route Match 与实际 Endpoint Selection；
- 真实三实例序列验证为 `mock-1, mock-2, mock-3, mock-1, mock-2, mock-3`；
- 本机纯 RR 健康选择约 `4.4–5.7 ns/op`，完整 Upstream 选择约 `7.2–8.1 ns/op`；100 节点最坏扫描约 `260 ns/op`，均为 `0 allocs/op`；
- 原始结果保存于 `benchmarks/results/balancer/bench.txt`。

---

## Phase 5：中间件、日志与 Prometheus

### 目标

形成一致的请求上下文、错误分类和基础可运维能力。

### 工作内容

- 全局中间件：Recovery、Request ID、Trace 占位、Access Log、Metrics、Header/Body Guard；
- 路由策略链编译接口；
- 标准错误 JSON；
- zap 字段规范和敏感信息脱敏；
- 项目私有 Prometheus Registry；
- 请求、延迟、并发、upstream、配置版本指标；
- 防止高基数 label。

### 验收标准

- [x] 任意失败路径都有 request ID；
- [x] 每请求最多一条主要访问完成日志；
- [x] Authorization、Cookie、JWT 和 body 不进入默认日志；
- [x] `/metrics` 可被 Prometheus 抓取；
- [x] route 指标使用 `route_id/path_template`，不使用原始 path；
- [x] Registry 可在测试中重复创建，不发生重复注册 panic；
- [x] 本阶段已存在的 404/502/503/504 使用统一错误结构；429 在 Phase 9A 加入；
- [x] `route_id` 指标受 route series 预算控制，超过阈值时聚合为 `_other`。

### 实施结果（2026-07-16）

- RequestContext、Request ID、Trace 占位、Observe、Recovery 和流式 Header/Body Guard 已接入 public/admin 链；
- route policy chain 在启动期编译，Phase 5 使用空策略链；
- zap production JSON logger 禁用采样，每请求最多一条 `request completed`；
- 私有 Prometheus Registry 暴露 request/route/upstream/endpoint/config 指标；
- Gateway 生成的 400/404/413/431/500/502/503/504 使用统一 ErrorBody 和 request ID；客户端取消内部记录为 499；
- ResponseWriter 与 request body 包装保留可选流式接口，SSE、取消和响应开始后中断契约通过回归；
- 回溯修复后覆盖率：全仓 `86.7%`；RequestContext `85.5%`、Middleware `82.6%`、Policy `100%`、Observability `93.8%`；
- production JSON logger + Metrics 基线约 `4.0–4.1 µs/op`、`2456 B/op`、`22 allocs/op`（Go 1.26.5）；
- 三实例真实进程联调验证 RR、request ID、脱敏日志、template-only label、SSE、metrics 和 SIGTERM；
- 原始证据保存在 `benchmarks/results/observability/`。

### MVP 完成检查点

Phase 0–5 已满足 `v0.1.0` 准备条件；tag 需在独立发布确认后创建：

- 静态配置可运行；
- 路由、负载均衡、代理和基础可观测闭环完整；
- 已有 benchmark 和自动化测试；
- 可以开始投递实习，但尚不能宣称动态配置或分布式治理完成。

---

## Phase 6：Gin 控制面与 etcd 版本发布

### 目标

完成配置管理 API 和原子发布。

### 工作内容

- 创建 `cmd/control-plane` 并使用 Gin 实现管理 API；
- 添加 Admin Token 中间件；
- 实现 validate、get、put、config version list、rollback；
- 明确区分 `config_version`、`etcd_mod_revision` 和 checksum；
- 配置规范化为 JSON 并计算 checksum；
- etcd 按 `/versions/{config_version}` 保存完整配置；
- 使用 CAS 比较 `expected_config_version` 和 `/current` 的值/ModRevision；
- 回滚通过复制历史内容创建新的单调版本，并记录 `rolled_back_from`；
- 默认只保留最近 50 个非 protected 历史版本；
- 写入管理审计日志；
- 使用 testcontainers-go 测试真实 etcd，并提供最小 Compose smoke test。

### 验收标准

- [ ] validate 不写 etcd；
- [ ] 合法 publish 产生单调递增的新 `config_version`；
- [ ] 过期 `expected_config_version` 返回 409；
- [ ] 并发发布只有一个成功；
- [ ] 从历史版本 M 回滚时创建 N+1，内容等于 M、版本不倒退，并记录来源；
- [ ] Watch/CAS 代码不把业务版本误当 etcd MVCC revision；
- [ ] 清理任务不删除 current 或 protected 版本；
- [ ] etcd 请求均带 deadline；
- [ ] client 在退出时关闭，无 goroutine 泄漏；
- [ ] OpenAPI 与实际接口一致；
- [ ] `docker compose --profile core` 下可执行一次 validate/publish/get smoke test。

---

## Phase 7：Watch、ConfigSnapshot 与 Last Known Good

### 目标

完成项目最有企业价值的配置收敛闭环。

### 工作内容

- 启动时读取 `/current`、`config_version` 与其 `etcd_mod_revision`；
- 数据面二次验证；
- `ConfigSpec -> ConfigSnapshot` 编译；
- 独立维护 `RuntimeRegistry`：endpoint、balancer、Transport、健康任务和本地 limiter；
- 使用 `atomic.Pointer[ConfigSnapshot]` 原子切换；
- Watch 从 `etcd_mod_revision + 1` 开始；
- 处理断线、重连和 compaction 后的全量 resync；
- 编译失败保留旧配置快照；
- 数据面实例通过 lease 上报 `config_version`、checksum、`source_etcd_revision`、状态和错误；
- 只在 endpoint 身份和 transport fingerprint 未变化时复用安全的运行时状态。

### 验收标准

- [ ] 请求侧从不访问 etcd；
- [ ] 压力请求期间连续发布配置无 panic、无 data race；
- [ ] 一个请求始终使用同一 `ConfigSnapshot`；健康和 active request 等运行时状态可独立变化；
- [ ] 非法 `config_version` 不替换 Last Known Good；
- [ ] etcd 停止后已有路由继续工作；
- [ ] etcd 恢复后数据面可重新收敛；
- [ ] Watch compacted 后可全量 resync，并从新的 MVCC revision 继续；
- [ ] 控制面可查看每个实例的 applied config version 和 source etcd revision；
- [ ] 长请求进行中删除 upstream/切换配置，旧请求仍完成；引用归零后 Transport/健康任务最终被清理；
- [ ] 记录配置发布到实例生效的收敛耗时；
- [ ] testcontainers 集成测试和 core Compose smoke test 均覆盖 publish -> watch -> apply。

### 关键故障演示

1. 持续请求 `/api/user/profile`；
2. 发布新增/删除 endpoint 或新路由的配置版本；
3. 请求不中断，路由或 endpoint 集合按新版本变化；
4. 发布非法配置；
5. 数据面拒绝切换，旧配置继续服务；
6. 回滚生成新的单调 `config_version`，并观察实例 ACK 收敛。

### 中途发布点

完成本阶段后发布“动态配置核心版”里程碑：即使后续 JWT、Redis 或 Trace 延期，项目仍具备可投递的配置发布、原子切换、LKG 和回滚闭环。

---

## Phase 8：主动健康检查与故障摘除

### 目标

让网关在 upstream 故障时自动摘除并恢复节点。

### 工作内容

- 主动健康检查调度；
- 连续失败/成功阈值；
- 健康状态指标；
- 被动失败统计和 half-open 探测移入扩展版/v1.1；
- 避免每个路由重复检查同一 endpoint；
- 进程退出时停止后台任务。

### 验收标准

- [ ] mock upstream 停止后，在 `failureThreshold × interval + timeout + 1 × interval` 的上界内被摘除；
- [ ] 其他健康节点继续提供服务；
- [ ] upstream 恢复后经过成功阈值重新加入；
- [ ] 全部失败时快速 503，不持续重试；
- [ ] 健康检查 goroutine 数量有界；
- [ ] goleak 和 race 测试通过；
- [ ] 配置删除 upstream 后相应任务和 idle connection 被清理；
- [ ] core Compose smoke test 可自动执行停止/恢复 upstream 的场景。

---

## Phase 9A：JWT / 本地限流策略切片

### 目标

将 JWT 与本地限流拆成两个可独立交付的小切片。核心发布门槛是至少完成一个闭环；时间允许再完成另一个。

### 工作内容

- **9A-1 JWT**：RS256 验证；
- 限制 algorithms、issuer、audience、expiration；
- identity/claims 写入请求上下文；
- 核心版只做身份认证，不承诺通用授权和 403；
- **9A-2 本地限流**：route/upstream Token Bucket；
- per-client limiter 使用有界 LRU/TTL 回收，避免内存 DoS；
- 429 + `Retry-After`；
- 认证和限流决策的 zap/Prometheus 字段。

### 验收标准

- [ ] 非法签名、过期、错误 issuer/audience 均返回 401；
- [ ] 不允许 token 自行选择任意算法；
- [ ] 不在日志中输出 token；
- [ ] 单实例本地限流符合 rate/burst，并用固定时钟验证；
- [ ] 429 使用统一错误结构，并在可计算时返回 `Retry-After`；
- [ ] 创建大量不同 client key 后 limiter 数量仍受预算和 TTL 限制；
- [ ] 已选择 JWT 时，core Compose smoke test 覆盖 200/401；
- [ ] 已选择本地限流时，core Compose smoke test 覆盖 200/429；
- [ ] 两个子切片互不依赖，任一未完成都不会留下半接入代码。

---

## Phase 9B：Redis 分布式限流（扩展版）

### 目标

在本地保护之上增加多 gateway 实例共享额度，并明确依赖故障语义。

### 工作内容

- 在统一 `RateLimiter` 接口下增加 Redis 实现；
- 使用原子 Lua/GCRA 或 Token Bucket，不使用分布式锁拼接限流；
- 维度支持 route + subject/api-key/IP；
- key TTL、长度、基数和 hot-key 保护；
- route 级 `fail_open` / `fail_closed`；
- Redis 调用使用短独立 timeout；
- Prometheus 指标；Phase 10 再接入 Trace instrumentation。

### 验收标准

- [ ] 两个 gateway 实例共享 Redis 总额度；
- [ ] Redis 原子性在并发 testcontainers 测试中成立；
- [ ] Redis 故障时按路由执行明确的 fail-open/closed；
- [ ] fail-open 时仍有本地保护；
- [ ] Redis key 自动过期，没有无界增长；
- [ ] extended Compose smoke test 自动覆盖 Redis 停止和恢复。

---

## Phase 10：OpenTelemetry Trace（扩展版）

### 目标

补齐入口、策略和出口 Trace，同时保持 Phase 1 已固定的代理契约不变。

### 工作内容

- 初始化 Resource、TracerProvider、OTLP/gRPC exporter；
- 控制面接 `otelgin`；
- 数据面入口和出口接 `otelhttp`；
- 添加 route/auth/limit/select 等内部 span；
- 传播 W3C Trace Context；
- 将 trace ID 写入 zap；
- 在不改变 timeout、cancel、streaming 和错误分类语义的前提下完成 instrumentation；
- OTel Collector 不可用时非阻塞降级；
- 受限重试移入 v1.1，除非核心版本所有验收已经完成。

### 验收标准

- [ ] Jaeger 中可看到 Client -> Gateway -> Upstream 完整链路；
- [ ] 日志中的 trace ID 可定位 Trace；
- [ ] exporter/Collector 故障不增加网关业务 5xx，且请求线程不等待 exporter；
- [ ] Phase 1 的 cancel、502/504、streaming、header 和响应头后断连测试全部继续通过；
- [ ] Redis 已启用时，其 span 不暴露限流 key、subject 或凭据；
- [ ] extended Compose smoke test 可自动查询一条完整 Trace。

---

## Phase 11：Docker Compose、E2E、故障注入与压测

### 目标

把代码变成“一条命令可演示、结果可复现”的项目。

### 工作内容

Compose 按能力拆分 profile：

```text
core:
  gateway-1, gateway-2, control-plane, etcd,
  user-service-1, user-service-2, prometheus

extended:
  redis, order-service

observability:
  grafana, otel-collector, jaeger
```

- 多阶段 Dockerfile；
- 非 root 用户；
- 健康检查；
- 固定镜像 tag/digest；
- Compose profiles：`core`、`extended`、`observability`；
- E2E：发布配置后请求通过；
- Fault：core 必测 etcd/upstream；扩展阶段再测 Redis/Collector；
- Vegeta 是核心压测工具；k6 只用于扩展复杂场景；
- 与直连 upstream 对照；
- 保存测试环境信息和原始结果。

### 验收标准

- [ ] `docker compose --profile core up -d` 可启动可投递核心环境；扩展 profile 按需叠加；
- [ ] 所有依赖有健康检查；
- [ ] E2E 脚本无需手工修改配置；
- [ ] 停止 control-plane 不影响已有转发；
- [ ] 停止 etcd 不影响 Last Known Good；
- [ ] 停止一个 upstream 后流量转移；
- [ ] 若启用扩展 profile，Redis 和 Collector 故障符合设计降级；
- [ ] 压测报告包含 p50/p95/p99、吞吐、错误率、CPU、内存、GC、goroutine；
- [ ] 报告同时包含直连和经过 gateway 的数据；
- [ ] 配置更新期间单独做持续流量测试。

### 压测矩阵

| 场景 | 目的 |
|---|---|
| 直连 upstream | 建立环境基线 |
| Gateway 无策略 | 测纯路由和代理增量开销 |
| JWT | 测签名校验成本 |
| 本地限流 | 测本地策略开销 |
| Redis 限流 | 测一次远程状态调用影响 |
| 100 / 1,000 / 10,000 routes | 测路由规模影响 |
| 慢 upstream | 观察 timeout、并发和连接池 |
| upstream 失败 | 观察错误分类和健康摘除 |
| 发布配置时持续压测 | 证明热更新无中断和无 data race |

禁止只给出“单机 XX 万 QPS”而没有硬件、参数、直连基线和延迟分位数。

---

## Phase 12：文档、Release 与简历收尾

### 目标

把工程结果整理为面试官能快速理解、又经得住深挖的项目材料。

### 工作内容

- 完善 README：问题、架构、Quick Start、演示命令；
- 生成架构图、请求时序图、配置发布时序图；
- 添加配置示例和 OpenAPI；
- 整理 benchmark 与 load-test 报告；
- 编写故障场景手册；
- Phase 7 发布动态配置中途版本；完成所选核心范围后发布 `v1.0.0`，扩展能力使用后续 minor 版本；
- 录制 3–5 分钟 Demo；
- 准备简历表述和面试追问答案。

### 验收标准

- [ ] 新用户按 README 可在 10 分钟内启动并完成一次发布；
- [ ] 文档明确哪些是自研、哪些使用成熟库；
- [ ] 所有性能数字可追溯到脚本、环境和原始结果；
- [ ] 至少有一个配置失败、一个依赖故障和一个热更新演示；
- [ ] Release 使用固定依赖和镜像；
- [ ] 项目不存在“README 声称已实现、代码实际未实现”的能力。

## 4. 验收证据矩阵

关键能力完成前必须把“执行命令、固定场景、数字阈值、结果文件”同时锁定。以下为初始标准；首次本机基线后可调整一次，但必须在结果文件中记录原因，不能在测试失败后随意放宽。

| 能力 | 计划命令 | 固定场景与初始阈值 | 结果文件 |
|---|---|---|---|
| 路由 fuzz | `make fuzz` | 路径、冲突对称性、编译匹配各 15s 无 crash；CI/nightly 后续提升至 60s/10min | `benchmarks/results/router/fuzz-*.txt` |
| 路由 benchmark | `make bench` | 10/1,000/10,000 routes hit/miss + compile + parallel；记录 ns/op、B/op、allocs/op；后续回归不劣化超过锁定阈值 | `benchmarks/results/router/bench*.txt` |
| RR/SWRR | `go test ./internal/dataplane/balancer -count=100` | RR 验证确定性序列；若有 SWRR，10,000 次选择的比例误差初始不超过 ±2% | `benchmarks/results/balancer/` |
| 热更新 | `make e2e-config-churn` | 2 gateway、200 RPS、5min、20 次合法发布和 5 次非法发布；网关引入 5xx 为 0 | `tests/e2e/results/config/` |
| 实例收敛 | `make e2e-convergence` | 两实例 ACK 同一 config version；本地 Compose 初始 p95 ≤2s | `tests/e2e/results/convergence/` |
| 健康摘除 | `make fault-upstream` | 摘除上界 ≤ `failureThreshold × interval + timeout + interval`；恢复遵守 success threshold | `tests/fault/results/upstream/` |
| 生命周期泄漏 | `make test-lifecycle` | Watch/reload/健康任务启动停止 100 次，race + goleak 通过 | `tests/integration/results/lifecycle/` |
| Transport 旧请求 | `make e2e-transport-retire` | 长请求进行中删除 upstream，旧请求完成；引用归零后无遗留任务/idle pool | `tests/e2e/results/transport/` |
| Collector 故障 | `make fault-collector` | 若启用 Trace，200 RPS 持续 5min，停 Collector 不增加业务 5xx | `tests/fault/results/otel/` |
| 代理增量开销 | `make loadtest-proxy` | 同机同参数比较直连/Gateway；报告 p50/p95/p99、吞吐、CPU、内存、GC | `benchmarks/results/proxy/` |

## 5. 测试金字塔

### 5.1 单元测试

重点模块：

- 路由匹配与冲突；
- Config validation/compiler；
- RR；若扩展实现 SWRR，再加入其状态机测试；
- timeout budget；
- error mapping；
- JWT claims；
- 限流 key；
- Header Rewrite。

### 5.2 Fuzz 与差分测试

- Radix matcher vs reference matcher；
- path 参数和 URL 编码；
- YAML decode/validate；
- upstream URL rewrite；
- Header 清理；
- route conflict detector。

### 5.3 集成测试

- etcd CAS 并发更新；
- Watch reconnect/compaction；
- 扩展版 Redis Lua 原子限流；
- control-plane publish -> data-plane apply；
- instance config version/source etcd revision ACK；
- graceful shutdown 与 goroutine cleanup。

### 5.4 E2E 与故障测试

- 完整 Compose 流程；
- upstream 停止/恢复；
- etcd 停止/恢复；
- 扩展 profile 下 Redis 停止；
- observability profile 下 OTel Collector 停止；
- 压力下配置发布与回滚。

### 5.5 每个 Pull Request 的最低门槛

```bash
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
```

耗时较长的 integration/E2E 可在独立 CI Job 执行，但合并到主分支前必须通过。

## 6. Demo 脚本

### 核心 Demo（不超过 5 分钟）

1. `docker compose --profile core up -d` 启动环境；
2. 调用控制面发布 user-service 路由；
3. 请求 `/api/user/profile`，展示 Round Robin endpoint 分布；
4. 持续请求期间发布新增/删除 endpoint 的新 `config_version`，展示请求不中断和两个实例 ACK；
5. 停止一个 user-service，展示主动健康摘除和恢复；
6. 发布非法配置，展示数据面保留 Last Known Good；
7. 回滚历史内容并生成新的单调版本；
8. 快速展示 Prometheus 中的请求、配置版本和健康指标。

### 扩展 Demo（按已实现能力选择）

- 使用/缺失 JWT 展示 200/401，快速请求展示 429；
- 启用两个 gateway 与 Redis，展示共享额度及 fail-open/fail-closed；
- 展示 SWRR 权重变化；
- 在 Grafana/Jaeger 查看指标和 Trace；
- 停止 Collector，证明业务请求不受影响。

主 Demo 只证明核心闭环；扩展能力不与核心流程抢占 5 分钟展示时间，也不需要额外管理前端。

## 7. 面试会重点追问的问题

开发过程中必须为以下问题准备可基于代码回答的答案：

1. 为什么控制面和数据面分开？
2. etcd 为什么不放在请求热路径？
3. 如何保证请求始终使用同一 ConfigSnapshot，同时允许健康状态变化？
4. Watch 断线和 compaction 怎么处理？
5. `config_version`、`etcd_mod_revision` 和 checksum 有什么区别？多个实例如何衡量收敛？
6. 路由冲突为什么在发布阶段处理？
7. 自研 Radix Tree 如何证明正确？
8. ReverseProxy 的 Rewrite、Transport、连接复用如何设计？
9. 为什么默认不重试 POST？
10. Redis 限流失败时为什么有 fail-open/closed？
11. 限流 key 如何避免内存或 Redis key 爆炸？
12. 为什么 Prometheus 不能用原始 path、user ID 作 label？
13. upstream 全挂时为什么应该快速失败？
14. 配置变更后旧 Transport 如何等待 in-flight 请求并安全释放？
15. 性能数字如何与直连基线比较？
16. 在 race、fuzz、故障注入中发现过什么问题，如何修复？

## 8. 简历表达建议

只有实现和数据都完成后，才可写类似内容：

核心版完成后可写：

> 使用 Go `net/http` 与 `httputil.ReverseProxy` 实现控制面/数据面分离的 API 网关；设计单调配置版本模型，通过 etcd CAS + Watch 发布配置，数据面将配置编译为不可变 `ConfigSnapshot` 并使用 `atomic.Pointer` 原子切换，在配置更新和 etcd 短时故障期间保持已有流量可用。

> 自研带冲突检测的 Host/Method/Path Radix 路由与 Round Robin，使用路由规范表、独立 reference matcher、fuzz、race 和 benchmark 验证正确性及并发安全；压测对比直连 upstream，记录网关增量延迟、吞吐和资源开销。

> 实现主动健康检查、JWT、本地限流、zap 和 Prometheus，并通过 Docker Compose 复现配置失败、节点摘除、Last Known Good 和复制式回滚场景。

只有扩展能力和证据都完成后，才追加 SWRR、Redis 分布式限流、被动健康或 OpenTelemetry 等描述。

不要写：

- “百万 QPS”但没有报告；
- “高可用”但没有故障测试；
- “零停机”但没有持续流量下的配置切换验证；
- “企业级”但没有配置版本、回滚、监控和降级语义。

## 9. 时间不足时的裁剪顺序

优先保留：

1. ReverseProxy 正确性；
2. 配置发布/热更新/回滚；
3. 路由语义与测试；
4. Round Robin 与主动健康检查；
5. zap 与 Prometheus；
6. 核心 Compose、故障和 Vegeta 对照压测；
7. JWT 与本地限流。

优先删除：

1. Web 管理后台；
2. 插件系统；
3. Kubernetes；
4. gRPC/HTTP3；
5. 多种熔断/重试算法；
6. SWRR 之外的多种负载均衡算法；
7. 复杂 RBAC 和多租户；
8. Redis、OpenTelemetry、Grafana 和 k6（核心版时间不足时）。

一个完成度高、数据可信的窄项目，比一个功能列表很长但无法解释和验证的项目更有求职价值。
