from pathlib import Path
import json,statistics as st
p=Path(__file__).parent;r=p/'results'
m=json.loads((r/'micro-summary.json').read_text());e=json.loads((r/'e2e-summary.json').read_text());d={x['name']:x for x in m}
assert len(m)==68 and all(x['rounds']==8 for x in m)
assert len(e)==6 and all(x['rps']['rounds']==6 for x in e)
raw=[json.loads(l) for l in (r/'e2e.jsonl').read_text().splitlines()]
assert len(raw)==72
out=['# 路由匹配优化独立压测报告','', '测试时间：2026-09-29 夜间至 2026-09-30（Asia/Shanghai）。比较 `5dcfbf1` → `eee0fce`。这是本次重新构建、重新采样的数据，不是历史报告摘录。','',
'## 结论','',
'Host 索引优化在大量虚拟主机场景下有明确的路由层收益；参数分配也稳定下降。整条 HTTP 链路的收益受配置规模影响，不能将微基准的倍数套用于网关吞吐。小配置应结合下表的波动与区间判断；方法不匹配的路径因新增 405/Allow 语义而增加成本。','',
'## 真实 HTTP 进程压测','',
'每场景每版本 6 轮，每轮预热 2 秒、采样 10 秒，AB/BA 交替。网关 4 个物理核心，客户端和上游各使用另外 2 个物理核心；均匀循环访问 Host。以下为各轮中位数，RPS 是固定并发下观测到的成功请求吞吐，不是最大容量。','',
'| 场景 | 并发 | before RPS | after RPS | 吞吐变化 | before P99 ms | after P99 ms | 网关 CPU µs/请求 before → after |',
'|---|---:|---:|---:|---:|---:|---:|---:|']
for x in e:
 s=x['scenario'];c=s.split('-c')[-1];a=x['rps'];q=x['p99_ms'];cpu=x['gateway_cpu_us_per_request']
 out.append(f"| {s} | {c} | {a['before']:,.0f} | {a['after']:,.0f} | {a['change_pct']:+.1f}% | {q['before']:.3f} | {q['after']:.3f} | {cpu['before']:.2f} → {cpu['after']:.2f} |")
out += ['',f"共 {len(raw)} 个正式采样窗口、{sum(x['requests'] for x in raw):,} 个成功 HTTP 请求、{sum(x['errors'] for x in raw)} 个错误。每个响应均检查 200 与完整固定响应体；预热及直接上游对照不包含在此请求总量中。",'',
'### 吞吐重复性','',
'| 场景 | before RPS min–max | after RPS min–max | 配对吞吐 after/before 的 95% bootstrap 区间 |',
'|---|---:|---:|---:|']
for x in e:
 a=x['rps'];lo,hi=a['paired_ratio_ci95'];out.append(f"| {x['scenario']} | {a['before_range'][0]:,.0f}–{a['before_range'][1]:,.0f} | {a['after_range'][0]:,.0f}–{a['after_range'][1]:,.0f} | {lo:.3f}–{hi:.3f} |")
out += ['', '区间基于 6 对样本的配对比率中位数，20000 次重采样，仅作探索性估计，不作生产 SLA 或多重比较后的显著性保证。区间跨 1 的场景不宣称可靠提速。P99 为每轮经验 P99 的中位数，属于 closed-loop 模型，未测开放到达率下的排队尾延迟。','',
'## Router 微基准','',
'每项 300ms × 8 轮，AB/BA 交替。相同测试文件已核对 SHA-256。只测 Match（含 Host 归一化），不含 ParsePath、HTTP 或上游。均匀 Host 用例在计时前逐一核验 RouteID 与参数。','',
'| 场景 | before ns/op | after ns/op | 耗时降低 | 速度比 before/after | B/op | allocs/op |',
'|---|---:|---:|---:|---:|---:|---:|']
for name in [f'BenchmarkUniformHost/hosts_{n}/{kind}-4' for n in [10,1000,10000] for kind in ['exact','wildcard']]+['BenchmarkRouteMatrix/routes_10000/multi_param-4','BenchmarkMatch/routes_10000/hit-4','BenchmarkMatchParallel-4']:
 x=d[name];out.append(f"| {name} | {x['before']:,.2f} | {x['after']:,.2f} | {-x['change_pct']:.1f}% | {x['before']/x['after']:.2f}× | {x['before_B']:g} → {x['after_B']:g} | {x['before_alloc']:g} → {x['after_alloc']:g} |")
out += ['', '均匀访问 10,000 个 Host 的结果消除了只挑旧版扫描末端的偏差。新版仍会受 Host 长度、哈希与缓存局部性影响；不能写成任何输入均为固定耗时。微基准中的 B/op 是每次操作分配字节，不是进程常驻内存。','',
'### 代价与没有证实的收益','']
x=d['BenchmarkRouteMatrix/routes_10000/method_miss-4'];out.append(f"- Method miss：{x['before']:.2f} → {x['after']:.2f} ns/op，耗时增加 {x['change_pct']:.1f}%，分配 {x['before_B']:g} → {x['after_B']:g} B/op，{x['before_alloc']:g} → {x['after_alloc']:g} allocs/op。旧版直接失败，新版识别 405 并生成 Allow；这不是同工作量的纯性能回归，但确实是用户会承担的新成本。")
x=d['BenchmarkCompile/routes_10000-4'];out.append(f"- 编译 10,000 条路径：{x['before']/1e6:.2f} → {x['after']/1e6:.2f} ms/op（{x['change_pct']:+.1f}%），配对区间跨 1；未证实启动编译提速。这不是 10,000 个 Host 的编译用例，也不是完整进程启动时间。")
out += ['', '## 环境、验证与适用范围','',
'- 本机 Linux/CachyOS x86_64，Ryzen 7 7735H；Go 1.27.1-X:nodwarf5。项目/CI Go 1.26.5 未复测。源码依赖与工具链两版本一致。',
'- 两版均通过 go test ./...、Router/Gateway 的 go test -race，正式网关和微基准二进制构建成功。未改生产代码。',
'- 保留正式中间件、指标和 JSON 日志序列化，但日志输出到 /dev/null；未包括日志存储/采集成本。',
'- 指标路由预算为 1000：超过阈值后路由标签聚合为 _other。因此只能用同规模前后数据计算收益，不应跨规模绝对 QPS 推断路由规模本身的影响。',
'- 单机 loopback、HTTP/1.1 keep-alive、固定 17 字节上游响应、一个 endpoint，无 TLS、远程网络、真实业务耗时或慢上游。',
'- performance governor、boost 开启，未锁频。桌面后台应用保留，CPU 亲和性不等同独占核心；SMT 兄弟线程、温度与频率仍可干扰。',
'- 没有测最大持续容量、长时间稳定性、生产负载权重、固定到达率 SLA，不能据此承诺生产 QPS。',
'- 两份代码在 /tmp 中从本地 Git 仓库 clone 并还原至固定提交；原工作区的未提交修改不参与测量，也未被覆盖。',
'', '## 原始证据与复现','',
'- [完整方法与复现命令](METHOD.md)',
'- [68 项微基准全部汇总及区间](results/micro-summary.md)',
'- [端到端全部逐轮 JSONL](results/e2e.jsonl)',
'- [端到端汇总 JSON，包含 P50/P95/P99 与各项范围](results/e2e-summary.json)',
'- [源码提交与构建产物 SHA-256](results/provenance.json)',
'- [环境快照](results/environment.txt)',
'- [进程 CPU/RSS 采样](results/process-samples.jsonl)',
'- 微基准原始输出：results/micro-before-0.txt 至 micro-before-7.txt，以及对应 after 文件',
'- 测试与 race 原始输出：results/before-test.txt、after-test.txt、before-race.txt、after-race.txt',
'- prepare.py、run_micro.py、load.go、run_e2e.py、summarize.py、report.py 为本次运行脚本',
]
if (r/'upstream-direct.jsonl').exists():
 direct=[json.loads(l) for l in (r/'upstream-direct.jsonl').read_text().splitlines()]
 out += ['',f"直接上游 C=32 对照（{len(direct)} 轮 × 10 秒）：RPS 中位数 {st.median(x['rps'] for x in direct):,.0f}，范围 {min(x['rps'] for x in direct):,.0f}–{max(x['rps'] for x in direct):,.0f}，错误 {sum(x['errors'] for x in direct)}。它用于评估本机客户端/上游余量，不计入网关收益。原始数据见 results/upstream-direct.jsonl。"]
(p/'REPORT.md').write_text('\n'.join(out)+'\n')
