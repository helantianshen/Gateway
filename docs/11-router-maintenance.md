# Router 核心维护报告（2026-09-29）

本次在现有冻结、segment-oriented compressed Radix Tree 上增量修改，属于维护期的核心模块修复与优化。生产基线为 `5dcfbf1`。用户已有的 `configs/gateway.yaml` 修改保持原样，未提交或推送。

## 修改与原因

1. Host 的全量有序扫描替换为两个不可变 map 和一个 any 组，最多查询三个候选；避免多 Host 数量直接放大每次请求的查询成本。
2. `NormalizeHost` 返回 `(string, *MatchError)`，非法非空 Host 显式返回 400，避免与无 authority 的空 Host、任意 Host 配置混淆。
3. `MatchError` 增加 `MatchErrIllegalHost`、`MatchErrMethodNotAllowed` 与 `AllowedMethods`。Gateway 映射 400/404/405，405 带 Allow；Router 自身不写 HTTP 响应。
4. 正常匹配失败后才探测尚未尝试的方法树，提供 `Router.AllowedMethods` 查询入口；已有 GET 隐含 HEAD，OPTIONS 不自动响应。
5. 明确 decoded path template，拒绝不可达的反斜杠、dot segment、无效 UTF-8；控制字符在配置与请求两侧一致拒绝。补 catch-all、空段、尾斜杠与编码字面量测试。
6. 根据 profile 保留 `Params map[string]string`，使用八槽栈上捕获缓冲与递归 append，超过八个参数时正常扩容；没有 unsafe、Pool、arena 或外部 Router。消除小写 DNS Host 的字符串复制和失败 IP 解析分配。
7. reference 仍在独立外部测试包中线性扫描，独立实现 Host、Path、specificity 和逐对冲突判断。差分比较完整结果、错误与 Allow，并覆盖随机多 Host 和插入顺序。

## 当前请求架构

```text
EscapedPath → 分段安全校验与解码
Host → authority 校验、端口/尾点剥离、ASCII lowercase
     → exactHosts[host]
     → wildcardHosts[首 label 后的 suffix]
     → anyHost
每个候选：显式 Method → HEAD 的 GET fallback → any-method
         → Compressed Radix Tree → MatchResult（独立 Params map）
全部未命中：只探测未尝试的方法树
         → 有可用方法：MethodNotAllowed + AllowedMethods
         → 无可用方法：NoRoute
GatewayHandler：非法 Host/Path → 400；NoRoute → 404；MethodNotAllowed → 405 + Allow
```

高 specificity 的 Host 只有在 Method/Path 匹配成功时才胜出；它仅声明了其他方法时，低一级 Host 的正常成功匹配仍可继续。Allow 汇总三个匹配 Host 层级中当前路径的可用方法，不泄漏无关 Host 的路由。排序采用字典序并去重。

`AllowedMethods(host, segments)` 返回 `(methods []string, any bool, err *MatchError)`。`any=true` 表示该路径接受任意方法，不枚举有限列表；没有路径返回 NoRoute。Match 与该查询均要求调用方先用 ParsePath 验证路径段。请求 Method 沿用既有转大写与 HEAD 回退规则，本次未更改自定义 Method 的大小写契约。

## Host 与 HTTP 边界

- exact 支持既有 IPv4、IPv6、hostname:port、`[IPv6]:port`、裸 IPv6；不把端口用作路由条件。
- 沿用项目 DNS/IP 子集校验：ASCII DNS label、单个尾点、显式端口 1–65535。RFC 的一般 reg-name 语法更宽，不能把项目拒绝 `_` 等字符描述成所有 HTTP Host 的统一语法规则。
- 空请求 Host 保留为“无 authority”，只能命中 any-host；非空非法值返回 IllegalHost。HTTP Server 对报文格式的校验仍在 Router 外部。
- `*.example.com` 只匹配一个 label，例如 `foo.example.com`；不匹配 `example.com` 或 `a.b.example.com`。没有 Host Trie、正则或多层 wildcard。
- 不实现自动 OPTIONS 或 CORS。显式 OPTIONS 与 any-method 路由照常转发；其余 OPTIONS 在路径存在时得到 405/Allow。

核对依据：[RFC 9110 Host/:authority](https://www.rfc-editor.org/rfc/rfc9110.html#section-7.2)、[405 与 Allow](https://www.rfc-editor.org/rfc/rfc9110.html#section-15.5.6)、[RFC 3986 Host 语法](https://www.rfc-editor.org/rfc/rfc3986.html#section-3.2.2)。项目既有兼容规则与协议通用语法分开记录。

## Path 固定语义

| 模板 | 请求路径 | 结果 |
|---|---|---|
| `/files/*path` | `/files` | 命中，path 为 `""` |
| `/files/*path` | `/files/` | 命中，path 为 `""` |
| `/files/*path` | `/files/a` | 命中，path 为 `"a"` |
| `/files/*path` | `/files/a/b` | 命中，path 为 `"a/b"` |
| `/files/*path` | `/files//a/` | 命中，path 为 `"/a/"` |
| `/users` | `/users/` | 不匹配；不重定向 |
| `/a/b` | `/a//b` | 不匹配；不合并斜杠 |
| `/用户/:id` | `/%E7%94%A8%E6%88%B7/42` | id 为 `42` |
| `/100%` | `/100%25` | 字面 `%` 匹配 |
| `/a?b#c` | `/a%3Fb%23c` | `?/#` 属于解码后的路径字符，不是 query/fragment |
| `/literal%2F` | `/literal%252F` | 字面 `%2F` 匹配，不二次解码 |
| `/%E7%94%A8%E6%88%B7` | `/%E7%94%A8%E6%88%B7` | 不匹配；模板中的百分号序列不是 Unicode 编码语法 |

配置模板继续拒绝重复斜杠和静态尾空段；请求侧保留这些空段并允许 catch-all 捕获。精确 `/files` 与 `/files/*path` 共存时，`/files` 由精确路由胜出，`/files/` 由 catch-all 接收。参数只匹配非空单段。控制字符指 Unicode Cc；不会笼统拒绝所有 Unicode。

## 测量方法与结果

同机 Linux amd64、AMD Ryzen 7 7735H，Go `1.27.1-X:nodwarf5`，`-cpu=4`，三轮中位数。没有固定 CPU 频率或独占机器；小幅 ns/op 变化不据此宣称统计显著。所有矩阵计时排除 Compile、测试数据生成与 ParsePath，只包含 Match（含 Host 归一化），不是端到端网关吞吐。

矩阵覆盖 routes 10/1000/10000；static、2/3 params、catch-all、高 fan-out、5/10/20 层深路径；Host 10/100/1000/10000 的 exact/wildcard/fallback/miss；GET/POST/HEAD/any/method miss；并行匹配。Host 场景分别配置 N 个 exact 或 wildcard 分组，fallback 额外配置一个 any 组；exact 选择字典序末端以暴露旧扫描成本。

以下为每项 100ms 的三轮中位数：

| 场景 | before ns/op | after ns/op | before B/op → after | before allocs/op → after |
|---|---:|---:|---:|---:|
| HostSelector/hosts_10000/exact | 36570.0 | 303.6 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10000/wildcard | 196568.0 | 304.9 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10000/fallback | 23765.0 | 299.7 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10000/miss | 23462.0 | 114.8 | 136 → 48 | 5 → 1 |
| RouteMatrix/routes_1000/param | 456.7 | 377.4 | 512 → 400 | 7 → 3 |
| RouteMatrix/routes_1000/multi_param | 540.9 | 467.4 | 560 → 400 | 8 → 3 |
| RouteMatrix/routes_10000/method_miss | 238.4 | 586.0 | 128 → 496 | 5 → 6 |

原有空 Host 单参数与并发场景另用每项 1s 复核：

| 场景 | before ns/op | after ns/op | before B/op → after | before allocs/op → after |
|---|---:|---:|---:|---:|
| Match/routes_10000/hit | 326.1 | 311.3 | 416 → 400 | 4 → 3 |
| MatchParallel | 115.8 | 115.5 | 416 → 400 | 4 → 3 |

并发耗时基本持平，不声称明显提速；分配下降稳定可见。Method miss 的 after 需要判定路径存在、构建去重 Allow，工作量高于旧版直接 404；这是新增正确语义的失败路径成本。已有方法树失败后不重复探测，成功请求不进入 Allow 计算。

完整矩阵及较短轮次的波动保留在 [comparison.md](../benchmarks/results/router/2026-09-29/comparison.md)，原始 [before](../benchmarks/results/router/2026-09-29/before.txt)、[after](../benchmarks/results/router/2026-09-29/after.txt)、[长测 before](../benchmarks/results/router/2026-09-29/hot-before.txt)、[长测 after](../benchmarks/results/router/2026-09-29/hot-after.txt) 可直接核对。

## Allocation 证据与选择

三参数 prealloc 长测中位数约 562.6ns、560B、8 alloc；仅替换递归捕获后约 494.9ns、464B、5 alloc。八槽缓冲减少三次参数 slice 分配。Host 小写快路径和跳过 DNS 的 IP 失败解析再减少两次分配，最终相同矩阵为 400B、3 alloc。保留 map 可避免影响 Gateway 和调用方的查找方式；未以零分配为目标。

[内存 profile before](../benchmarks/results/router/2026-09-29/alloc-before.txt) 中 appendParamValue 占分配对象约 41.5%，buildMatchResult 约 36.2%；[after](../benchmarks/results/router/2026-09-29/alloc-after.txt) 中约 99% 分配对象集中在 buildMatchResult。百分比来自抽样 profile，包含 benchmark 的少量准备分配，不等同于逐请求精确占比。CPU profile 和逃逸分析见同目录的 cpu/escape 文件；逃逸分析确认捕获 append 不逃逸。测试覆盖 20 个参数、回溯和结果 map 所有权。

## 分步验证与复现

1. 基线：先补 benchmark 与多 Host reference 差分，Router test/race 通过；保存旧版独立测试二进制后开始改生产代码。
2. Host 索引：Router test/race 通过；阶段 [Host 索引基准](../benchmarks/results/router/2026-09-29/step2-host-index.txt) 显示 10k exact 约 390ns、wildcard 396ns。
3. Host 错误：Router/Gateway test/race 通过；[阶段基准](../benchmarks/results/router/2026-09-29/step3-host-error.txt) 未增加成功匹配分配。
4. 405：全仓库 test、Router/Gateway race 通过；[阶段基准](../benchmarks/results/router/2026-09-29/step4-method-error.txt) 保留原有匹配场景。
5. Path：全仓库 test、Router/config/Gateway race 通过；[阶段基准](../benchmarks/results/router/2026-09-29/step5-path-semantics.txt) 记录参数与方法失败开销。
6. Profile：使用 pprof 与 `-m=2`，记录 [prealloc](../benchmarks/results/router/2026-09-29/prealloc.txt)、CPU、内存与逃逸证据。
7. 参数优化：Router test/race 通过，先比较 [仅参数优化](../benchmarks/results/router/2026-09-29/params-after.txt)，再做完整 after 与热路径长测。

最终验证：全仓库 test/race/vet；Router 随机差分、独立编译冲突、插入顺序、真实 HTTP 400/404/405/HEAD/OPTIONS；四个 fuzz 目标各 10 秒。最终结果与原始执行日志见基准目录及任务状态；持续 fuzz 的通过只描述此次运行时长。

复现命令（在同一工具链执行）：

```bash
go test ./...
go test -race ./...
go vet ./...
go test ./internal/router -run='^$' -bench='Benchmark(Match$|MatchParallel$|HostSelector$|RouteMatrix$)' -benchmem -benchtime=100ms -count=3 -cpu=4
go test ./internal/router -run='^$' -bench='^BenchmarkMatch$|^BenchmarkMatchParallel$' -benchmem -benchtime=1s -count=3 -cpu=4
go test ./internal/router -run='^$' -fuzz='^FuzzRequestRoutingDifferential$' -fuzztime=10s -parallel=4
go test -c -o /tmp/router.test ./internal/router
/tmp/router.test -test.run='^$' -test.bench='BenchmarkRouteMatrix/routes_1000$/multi_param$' -test.benchtime=1s -test.cpu=4 -test.memprofile=/tmp/router.mem -test.cpuprofile=/tmp/router.cpu
go tool pprof -top -alloc_objects /tmp/router.test /tmp/router.mem
go tool pprof -top /tmp/router.test /tmp/router.cpu
go test -run='^$' -gcflags='github.com/helantianshen/gateway/internal/router=-m=2' ./internal/router
```

before 使用基线 `5dcfbf1` 的生产代码加本次 `bench_test.go`；可在临时目录用该基线的 Git archive 解包，再复制本次 benchmark 文件并构建复现。before 全矩阵和原有 BenchmarkMatch 分两次调用收集，采用相同 `100ms/count=3/cpu=4` 参数。profile 二进制位于临时目录，仓库只保存可审查的文本输出。

## 未改变的边界

冻结 Radix 与单模块架构保留；不支持 regex、header/query routing、gRPC/HTTP3 routing、多层 wildcard、自动 slash redirect 或 path clean。Host map 查询不随分组数量线性增长，但 Host 字符扫描和哈希仍与输入长度有关。高 fan-out builder 仍线性查找兄弟节点；本次未优化编译期复杂度。

本机未在项目/CI 的 Go 1.26.5 复测，性能数字只对应上述本地工具链；未运行 audit、完整 gateway 二进制部署或端到端负载测试。此前审查 R1–R5 仍未修复。
