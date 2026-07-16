# Phase 0–5 全量回溯审查报告

> 状态：已完成，确定问题均已修复
>
> 审查日期：2026-07-16（CST）
>
> 基线提交：`7b251f7`（Phase 4）；审查对象包含未提交的完整 Phase 5 工作树
>
> 当前工具链：`go1.26.5 linux/amd64`

## 1. 审查范围

本次不是只复核 Phase 5 diff，而是从进程入口向下回溯 Phase 0–5：

- 工程与供应链：Go module、Makefile、GitHub Actions、静态检查、漏洞扫描；
- 配置与生命周期：严格 YAML、Bootstrap、双 Server、共享 Transport、Graceful Shutdown；
- 代理契约：Rewrite、Host、Forwarded Header、502/504、SSE、取消和大 Body；
- 路由：Host/Method/Path 规范、冲突检测、Radix/reference 差分、fuzz 和性能；
- Upstream：endpoint 编译、健康过滤、Round Robin、active request 和共享连接池；
- Phase 5：RequestContext、中间件顺序、Recovery、Guard、日志、私有 Metrics 和空 route policy chain；
- 文档、示例配置、真实三进程联调和阶段边界。

JWT、限流执行、OTel SDK、etcd 动态快照、主动健康、retry、熔断和 SWRR 仍属于后续阶段，不以“缺少功能”计为本次问题。

## 2. 已修复发现

| ID | 严重度 | 发现 | 修复与证据 |
|---|---|---|---|
| SEC-01 | 高 | `go1.26.3` 命中 3 个可达标准库漏洞；间接 `x/sys v0.35.0` 另有 Windows 漏洞 | Go/module/CI 升至 `1.26.5`，x/sys 升至 `v0.44.0`；复跑 govulncheck 为 0 可达、0 package、0 module 漏洞 |
| LIFE-01 | 中 | Graceful Shutdown 超时后只关闭 idle upstream 连接，活跃客户端连接和请求 Context 可在 `Run` 返回后存活 | Shutdown 出错后强制 `Server.Close`；`Run` 返回前收齐两个 Serve 结果；真实 HTTP 生命周期测试证明连接关闭、请求 Context 取消 |
| ROUTE-01 | 中 | `foo_bar.example.com`、`bad-.example.com` 等非法 label 可命中 `*.example.com` | 请求 Host 增加无分配的 DNS label 扫描；非法 Host 归一化为空，只允许 any-host route；Radix 和独立 reference matcher 同步测试 |
| OBS-01 | 中 | 响应前抛出 `http.ErrAbortHandler` 时 Observe 会把未写响应补成 200 | 仅在 Handler 正常返回时补隐式 200；中断路径保留 status `0/_none`；access log、Snapshot 和 Prometheus 均有回归测试 |
| BAL-01 | 低 | Round Robin 在 `uint64` 回绕点可能连续选择同一 endpoint | 扫描起点先取模，溢出时按候选范围重基准；覆盖全健康和跳过不健康的 MaxUint64 边界，热路径仍 0 alloc |
| MOCK-01 | 低 | mock `/echo` 无界 ReadAll；slow/stream timer 生命周期可改进；无 Flusher 时先写 200；非正 delay 被接受 | 改为流式 io.Copy、可停止 Timer/单 Ticker、先验证 Flusher、拒绝非正 delay，并增加 ReadHeaderTimeout 和 handler 集成测试 |
| ENG-01 | 低 | `make run` 声明但无目标；Router benchmark 管道会吞失败；CI 的干净工作树 diff check 无效 | 补 run/verify/audit；benchmark 显式保留退出码并清理尾空格；CI 增加最小权限、并发取消、module verify、全仓 whitespace、staticcheck、govulncheck 和超时 |
| DOC-01 | 低 | 技术选型、阶段状态、Router 路径、YAML 模块、日志采样和 etcd 版本前提已漂移；两个测试 helper 未使用 | 文档与实际依赖/行为同步，Phase 0–5 checklist 完成；删除无效 helper，staticcheck 复扫无输出 |

三名独立只读审查者分别检查基础代理/生命周期、Router/Upstream、Phase 5 可观测链；其四项有效发现对应 LIFE-01、ROUTE-01、BAL-01、OBS-01。修复后由独立 claim verifier 逐项核对，四项均为 Verified。

## 3. 跨阶段契约复核

- Phase 0–1：共享 Transport、`ReverseProxy.Rewrite`、502/504、SSE flush、取消传播和双 Server 停机语义保持不变；超时停机现在额外强制收口。
- Phase 2：KnownFields、单 YAML 文档、引用/URL/duration/weight 校验、错误脱敏和“不合法配置不监听”保持通过。
- Phase 3：Host > Method > Path > priority、HEAD fallback、EscapedPath、编码斜杠/dot segment 拒绝、插入顺序独立和冲突拒绝保持通过。
- Phase 4：Route -> CompiledUpstream -> healthy RR -> fixed endpoint Proxy 链不变；全不健康仍为 503；状态原子与不可变配置边界不变。
- Phase 5：public 顺序仍为 RequestContext -> Request ID -> Trace -> Observe -> Recovery -> Guard -> Gateway；route policy 位于匹配后、endpoint 选择前；admin 不计入 public Metrics。
- 隐私：日志和 Metrics 不记录 Authorization、Cookie、JWT、body、query value、原始 path、IP、用户 ID或底层敏感错误；upstream 伪造的 X-Request-ID 仍被剥离。

## 4. 验证结果

### 正确性与工程门禁

- 全量普通测试、race、vet、build、format、module verify 和 diff/whitespace 检查通过；
- staticcheck `v0.7.0` 无发现；govulncheck `v1.6.0` 无漏洞；
- 全仓 `-shuffle=on -count=20` 通过；关键修复包 `-race -count=10` 通过；8 个无真实 TCP listener 的核心包连续 100 轮通过；
- 曾尝试把 11 个大量创建 `httptest.Server` 的包并行执行 100 轮；本机临时端口范围仅 `60500–60800`，随机在 `net.Listen(:0)` 处因 `EADDRINUSE` 中止，无业务断言或 race 失败，因此不把该次运行计为通过，也不把环境端口耗尽误报为产品缺陷；
- Linux amd64、Windows amd64、Darwin arm64 交叉编译通过；
- 最终修复后 Router 三组 Go 1.26.5 fuzz 复跑约 53.5 万、39.2 万、49.3 万次，无 crash。

### 覆盖率

全仓语句覆盖率 `86.7%`。除只负责 OS signal/os.Exit 的 `cmd/gateway` 外，各运行时包为 `82.2%–100%`；mock-service 从 `5.0%` 提升到 `82.3%`。

### 当前性能基线

- 10,000 routes：hit `465–486 ns/op`，miss `303–306 ns/op`；
- Round Robin 全健康：`5.0–5.7 ns/op`；完整 Upstream 选择：`7.5–8.2 ns/op`；均为 0 alloc；
- 100 endpoint 稀疏健康完整选择：`241–262 ns/op`，0 alloc；
- Phase 5 production JSON logger + Metrics：`4.0–4.1 µs/op`、`2456 B/op`、`22 allocs/op`。

原始结果位于 `benchmarks/results/router/`、`benchmarks/results/balancer/` 和 `benchmarks/results/observability/`。

### 真实进程联调

Go 1.26.5 下重新启动三个 mock 和 production Gateway：

- 六次选择为 `mock-1,mock-2,mock-3,mock-1,mock-2,mock-3`；
- 固定 request ID 恰好一条完成日志，敏感哨兵无泄露；
- 非法 Host `foo_bar.example.com` 选择 any-host `hello`，未命中 wildcard route；
- SSE 返回两个事件，私有 Metrics 无原始 query/path；
- SIGTERM 退出码为 0。

详细证据见 `benchmarks/results/observability/integration.txt`。

## 5. 保留边界

以下是已知且有明确后续阶段归属的边界，不是本次未修复缺陷：

- `config.Validate` 与 `bootstrap.New` 仍各编译一次 Router；Phase 7 统一为单次 ConfigSnapshot 编译；
- config version 在静态 Phase 5 固定为 1；动态版本属于 Phase 7；
- endpoint weight 仅校验，普通 RR 不读取权重；SWRR 属于扩展阶段；
- 主动健康、retry、熔断、JWT/限流执行和真实 OTel 均未提前实现；
- CI 尚未运行真实多进程 integration/image build，按路线图在 Phase 11 固化；当前证据由本地真实进程运行保存；
- `cmd/gateway` 不做业务单测，装配和生命周期由 `internal/bootstrap` 的真实 listener/HTTP 测试覆盖。
