# 独立路由优化压测：方法与复现

比较本地仓库已提交的 `5dcfbf1f0d7e9171b5364ad526dd74e65cf364a9` 与 `eee0fceede4dedc6dc4187c5b3eecac2bc9788de`。两份代码使用 git clone --no-hardlinks 从本地完整仓库复制并 detached checkout，没有使用含未提交修改的工作树，也没有将旧报告作为本次数据。完整提交和构建产物 SHA-256 见 results/provenance.json。

## 测量范围

- Router：相同的新版 bench_test.go 加 uniform_bench_test.go 放到两个版本，仅增加测试，不替换生产实现；68 项，每项 300ms，8 轮，按 AB/BA 交替；同一时刻只运行一个被测进程
- 微基准包括 10/1000/10000 条路径、不同参数数目、catch-all、深路径、方法、miss、并发以及 Compile；Host 10/100/1000/10000 末端 exact/wildcard/fallback/miss；额外覆盖 10/1000/10000 Host 均匀访问
- UniformHost 每个 Host 的 RouteID、参数结果在计时前逐一检查；计时只包含 Match，包含 Host 归一化，不含编译/ParsePath/HTTP
- 编译基准单列启动成本；每种方法的固定重复次数由 testing.B 自动校准
- E2E：真实正式 cmd/gateway 二进制 → loopback HTTP 上游；只有一个 endpoint，返回固定的 `gateway-bench-ok\n`（17 字节），不含业务处理/TLS/远程网络
- 指标路由预算为 1000，路由数超过预算后聚合为 _other；只比较同规模前后，跨规模绝对吞吐存在指标基数差异
- 正式中间件、Prometheus 指标和 JSON access log 生成保留；stdout 重定向 /dev/null，因此不包含真实日志磁盘/采集器成本
- C=32 增加单条静态 any-host 对照，并测 10、1000、10000 exact Host 和 10000 wildcard Host；另以 C=1 测 10000 exact Host
- 除单条静态 any-host 对照使用 GET /users/42，其他路由 GET /users/:id；原子序号按 Host 数量取余，使每轮发起请求以确定的均匀循环遍历所有 Host，避免只挑旧实现最差末端
- 每场景每版本 6 次独立启动；2s HTTP 预热，10s 采样，AB/BA 交替；10s 到期后等待在途请求完成，RPS 分母使用真实完整耗时
- 每条响应检查状态码和响应体，读取完 body 后关闭以复用连接；统计成功请求数、错误数、P50/P95/P99；超时为 3s
- 延迟从 client.Do 前到 body 读取关闭后，不含构造请求和等待发送；属于 closed-loop 固定并发模型，有 coordinated omission 局限，不能用于声称固定到达率的 SLA
- 网关 CPU 时间读取 /proc/PID/stat 的 utime+stime 增量，除以成功请求数；包括整个网关进程及 GC/中间件/代理成本，内核计时粒度会引入误差
- 直接上游 C=32 重复 3 次用于检查客户端/上游吞吐余量；这不是 gateway 性能数据

## 环境与干扰控制

Linux x86_64 本机，AMD Ryzen 7 7735H，8 核 16 线程；Go 1.27.1-X:nodwarf5，项目/CI 1.26.5 未复测。使用同一编译器、源码依赖与构建方式。performance governor、boost 开启、未锁频。未停止桌面背景应用，不是独占机器。

网关和 Router 基准 GOMAXPROCS=4，taskset CPU 4,6,8,10；客户端 GOMAXPROCS=2，CPU 0,2；上游 GOMAXPROCS=2，CPU 12,14。这些是不同物理核心的一条硬件线程，另一条 SMT 线程以及整机资源仍可能被后台程序使用。没有修改系统级配置。

## 统计口径

完整原始样本保留；主要展示各轮中位数、min–max、after/before 比率。耗时下降百分比 = (1-after/before)×100%；吞吐增幅 = (after/before-1)×100%；不能混用“耗时降低百分比”与“速度倍数”。

同一轮的 before/after 组成配对。以配对比率的中位数为统计量，随机种子 20260929，20000 次有放回 bootstrap，报告 2.5%/97.5% 分位区间。样本量小，区间是探索性估计；没有进行多重比较校正，不能把每一项区间当成生产环境统计保证。报告的中位数之比与配对比率中位数不是同一个估计量，区间专门对应后者。

P99 是每轮成功请求的经验分位数，汇总列为各轮 P99 的中位数，不能冒充所有请求合并后的 P99。错误率另列，错误请求不计入成功延迟分位数。

## 复现

将 `_harness/` 中的脚本复制到新临时目录；在该临时目录根目录放入 prepare.py、run_micro.py、load.go、run_e2e.py、summarize.py。以下命令使用 bash，并将路径调整为当前机器路径：

```bash
run_dir=$(mktemp -d /tmp/gateway-routing-bench-XXXXXX)
git clone --no-hardlinks /home/helan/own/gateway "$run_dir/before"
git -C "$run_dir/before" checkout --detach 5dcfbf1f0d7e9171b5364ad526dd74e65cf364a9
git clone --no-hardlinks /home/helan/own/gateway "$run_dir/after"
git -C "$run_dir/after" checkout --detach eee0fceede4dedc6dc4187c5b3eecac2bc9788de
mkdir "$run_dir/results"
cp "$run_dir/after/internal/router/bench_test.go" "$run_dir/before/internal/router/bench_test.go"
# 将交付的五个脚本复制到 "$run_dir" 后运行
python3 "$run_dir/prepare.py"
python3 "$run_dir/run_micro.py"
python3 "$run_dir/run_e2e.py"
python3 "$run_dir/summarize.py"
```

先核对本机 CPU 拓扑并修改 taskset 参数。两个压测脚本顺序执行；不要同时运行，以免相互干扰。run_e2e.py 使用本机动态空闲端口，并在结束时终止它创建的子进程。每次使用新目录，避免向已有 JSONL 追加重复样本。

归档中的两个 Go 文件已执行 gofmt；相邻的 `.go.raw.txt` 保存实测时的原稿，与原始 provenance 哈希对应。格式化副本的哈希另列于 `archived_formatted_source_sha256`，两者只存在格式差异。
归档微基准文本仅清理 CPU 名称行的尾随空格；所有测量数字保持原样。
