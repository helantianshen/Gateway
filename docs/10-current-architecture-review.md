# 当前架构与代码审查（2026-09-27）

## 范围与结论

审查基线为 `63d073dac246eef8890c9c0d443f366a146b0097`，初始工作区干净。阅读生产代码、测试、配置、Makefile、CI 与 docs/01–09。当前实现是 **Phase 5 静态数据面**：单 Go Module、双 HTTP Server、启动时编译路由和 upstream、共享 Transport、全局中间件、zap 与私有 Prometheus Registry。模块职责总体清楚，路由配置和 endpoint 原子运行状态分离；控制面、动态配置、实际限流和主动健康检查尚未实现。

完整模块、依赖、生命周期和开发入口见 [AI 架构导航](../.agent/PROJECT.md)。本次仅修改文档，以下五项缺陷**均未修复**。严重度中 P1 表示优先处理的资源控制问题，P2 表示确定的功能或契约错误；不是 CVSS 评分。

验证环境为本机 Linux amd64，Bash，`go1.27.1-X:nodwarf5`，非容器。仓库 `go.mod` 和 CI 基线为 Go 1.26.5；本次没有在 1.26.5 重跑。涉及标准库行为的发现应在该版本补充复核，不能直接把本次结果当作所有 Go 版本的证明。

## 已复现的缺陷

### R1 · P1：客户端停止上传时，代理超时不能终止请求

- 位置：[Proxy.ServeHTTP](../internal/dataplane/proxy/proxy.go) 第 156–165 行；[Server 配置](../internal/dataplane/server/server.go) 第 63–69 行。
- 触发：客户端声明 `Content-Length: 100`，只发送一个字节后保持连接；upstream 等待 body 完成。代理 timeout 设为 20ms。
- 实测：等待 300ms 仍无响应，探针由自己的 socket deadline 退出。三轮均复现。另一次堆栈采样显示，RoundTrip 在等待写循环结束，写循环阻塞于读取客户端请求体。
- 原因：出站 Context 取消不等于中断入站 socket 的 body Read；Server 只有 ReadHeaderTimeout/IdleTimeout，没有请求体读取 deadline。现有“覆盖整个链路”的注释超出了实际保证。
- 影响：慢上传可使请求、连接和 goroutine 超过配置时间继续占用；本次没有压测或证明生产故障。Body 的大小限制也不能限制一个永远不完成的上传。
- 建议：为客户端 body 读取建立可取消的期限机制，并明确上传、upstream、下行流式响应的预算。不要仅靠统一 WriteTimeout 处理，否则会改变 SSE 等长流语义。回归必须使用真实 TCP 的停滞上传，同时验证正常流式请求与停机。

### R2 · P2：ReverseProxy 默认错误日志绕过受控日志路径

- 位置：[proxy.New](../internal/dataplane/proxy/proxy.go) 第 77–129 行，未设置 `ReverseProxy.ErrorLog`。
- 触发：upstream 返回 chunked body 后给出格式错误的 Trailer 行 `REVIEW_SECRET_SENTINEL`。
- 实测：标准 logger 输出 `httputil: ReverseProxy read error during body copy: malformed MIME header: missing colon: "REVIEW_SECRET_SENTINEL"`，三轮均出现。
- 原因：响应 body 复制错误使用标准库 `ReverseProxy.logf`；配置 ErrorHandler 不能覆盖这条路径。该日志不经过项目 zap 字段约束。
- 影响：上游异常协议内容可原样进入进程日志；若其中包含敏感值，就会泄漏。当前 access log 不记录 body 的约束，不能推导为整个进程不会记录原文。
- 建议：显式配置受控 ErrorLog，输出固定分类或经过审查的字段；不能只把原始文本转交 zap。回归应捕获所有日志出口，覆盖错误 Trailer 和响应复制失败。

### R3 · P2：103 被记录为最终状态，最终响应丢失 Request ID

- 位置：[MarkResponseStarted](../internal/dataplane/requestctx/context.go) 第 159–164 行、[Observe](../internal/dataplane/middleware/observe.go) 第 75–80 行、[ModifyResponse](../internal/dataplane/proxy/proxy.go) 第 118–120 行。
- 触发：upstream 依次发送 `103 Early Hints` 和最终 `502`。
- 实测：客户端最终状态为 502，Observer 记录为 103，最终响应 `X-Request-ID` 为空，三轮一致。
- 原因：首次 WriteHeader 被无条件当作最终响应开始；标准库转发 1xx 后清理 Header map，提前设置的 Request ID 随之被清除，最终响应没有重新注入可信 ID。
- 影响：日志/指标把最终 5xx 错分为 1xx，响应关联 ID 丢失。Recovery 依赖同一 ResponseStarted 状态，1xx 后 panic 的处理也需要补充验证。
- 建议：区分临时状态与最终响应，单独处理 101 升级；在最终响应写出时保证可信 Request ID。覆盖 100/103 后 2xx、5xx 和 panic。

### R4 · P2：Connection token 可移除转发的 Request ID

- 位置：[RequestID 中间件](../internal/dataplane/middleware/middleware.go) 第 80–97 行、[Rewrite](../internal/dataplane/proxy/proxy.go) 第 88–114 行。
- 触发：请求同时包含 `Connection: X-Request-ID` 和合法 `X-Request-ID: review-id`。
- 实测：响应 ID 是 `review-id`，upstream 收到的 ID 为空，三轮一致。
- 原因：中间件在进入 ReverseProxy 前写入 ID；标准库先按 Connection token 清除逐跳字段，再调用 Rewrite，而 Rewrite 未从可信 RequestContext 恢复该字段。
- 影响：客户端可破坏 gateway 与 upstream 的关联链，不符合文档的 ID 全链路透传契约；这不是已证明的认证绕过。
- 建议：在 Rewrite 中从已验证的请求上下文注入 ID；不要恢复客户端任意指定的逐跳字段。覆盖大小写和多个 Connection token。

### R5 · P2：YAML merge 中显式非法 weight 被默认值覆盖

- 位置：[applyEndpointWeightDefaults](../internal/config/loader.go) 第 94–117 行。
- 触发：一个完整有效配置中的 endpoint 使用 `<<: &base {weight: 0}`，自身没有直接的 weight key。
- 实测：LoadConfig 后 weight 变成 100，Validate 返回 nil，三轮一致。
- 原因：YAML 解码器支持 merge，但默认值补齐逻辑只检查 AST 的直接 key，把合并得到的显式值当成“省略”。
- 影响：非法值未被拒绝，配置含义被静默改变。普通 RR 当前不读取 weight，因此不是当前流量比例错误的证明。
- 建议：默认值判断应理解 merge/alias，或在 schema 层明确拒绝这些语法。两种方式存在兼容性取舍，修复时需先确定配置契约。覆盖省略值、直接值、继承的零/负数/正数与 alias。

## 结构和测试上的不足

以下与已复现缺陷分开，不把后续阶段规划当成实现错误：

1. **Host 分组线性搜索，编译构建存在高扇出成本。** 当前请求匹配遍历排序的 Host 分组；Radix builder 查找同级边也是线性的。大量不同 Host 或高扇出输入需要专门测量；现有同 Host 基准不能证明这些负载下的性能。没有本次性能回归证据，不建议仅凭复杂度立即重构。
2. **Fuzz 与差分覆盖仍有盲区。** `FuzzParsePathPattern` 面向配置模式；`FuzzCompileAndMatch` 没有覆盖真实请求 `EscapedPath → ParsePath` 的完整边界。差分主要比较 RouteID/PathTemplate，未全面比较 Params、UpstreamID、PreserveHost。应优先补完整结果和原始编码路径的测试。
3. **协议测试不能完全替代真实连接。** 现有测试通过，但上述 1xx、异常 Trailer、停止上传均漏检；同步读取 body 的自定义 RoundTripper 不能代表真实 Transport 的取消行为。
4. **静态配置边界需要保持明确。** config.Validate 与 bootstrap 装配会分别编译路由；Config 仍保留 Spec 指针，并不是全应用不可变快照。当前没有动态发布路径，未来热更新必须处理快照和资源生命周期，不能只增加一个原子指针。
5. **部署与能力边界。** admin 默认监听全部接口且无认证，`/readyz` 不证明 upstream 可用；入口未提供 TLS，X-Forwarded-* 没有可信前置代理策略。rate/burst 可配置但没有实际限流，endpoint 初始健康且没有生产健康更新任务。部署必须自行限制 admin 可达范围；不能把这些占位当作安全和可用性保障。
6. **质量门禁范围有限。** CI 执行 verify/audit，但没有持续 fuzz、性能门禁或完整生产进程场景门禁。历史 benchmark 与联调记录有参考价值，但不是当前变更的自动验收结果。

## 验证与复现证据

| 检查 | 本次结果 | 说明 |
|---|---|---|
| `make verify` | 通过 | diff、格式、vet、现有单测、race、build |
| `go mod verify` | 通过 | 模块缓存完整性；不是漏洞扫描 |
| 五项临时专项探针，`go test -v -count=3 ./.agent` | 五项均失败，三轮一致 | 断言期望正确行为，失败即上述缺陷证据 |
| Markdown 本地链接、文档差异 | 通过 | 最终文档检查；不改变业务代码 |
| Go 1.26.5、audit、持续 fuzz、benchmark、完整进程 E2E | 未执行 | 不复用历史结果冒充本次通过 |

完整探针、运行方法和实际输出保存在 [复现证据](../.agent/tasks/2026-09-27-review-evidence.md)。探针仅使用本地临时配置、loopback HTTP/TCP 和模拟上游，使用项目的 Server/Transport/中间件/Proxy；不代表已启动完整 gateway 二进制。临时 `.go` 文件已移除，避免把文档任务变成测试代码变更。

建议修复顺序：R1 → R2 → R3/R4 → R5，每项独立回归；随后补请求路径 fuzz 和多 Host 基准。修复提交应更新本文状态并保留原始证据，不直接删除发现。
