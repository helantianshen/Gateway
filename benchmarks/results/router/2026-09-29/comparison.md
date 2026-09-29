# Router 性能对照

以下各项为同机、同 Go 工具链、`-cpu=4 -benchtime=100ms -count=3` 三轮中位数。所有规模和场景均保留，不只列出改善项。Host 基准不包含 URL 解析；Method miss 的 after 额外计算 405/Allow，与旧版仅返回 404 工作量不同。

| 场景 | before ns/op | after ns/op | before B/op → after | before allocs/op → after |
|---|---:|---:|---:|---:|
| MatchParallel | 115.3 | 114.1 | 416 → 400 | 4 → 3 |
| HostSelector/hosts_10/exact | 371.6 | 284.7 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10/wildcard | 577.9 | 294.0 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10/fallback | 355.2 | 287.9 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10/miss | 235.9 | 110.7 | 136 → 48 | 5 → 1 |
| HostSelector/hosts_100/exact | 712.2 | 290.6 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_100/wildcard | 2347.0 | 304.5 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_100/fallback | 579.3 | 295.7 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_100/miss | 434.2 | 113.4 | 136 → 48 | 5 → 1 |
| HostSelector/hosts_1000/exact | 3894.0 | 312.5 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_1000/wildcard | 19815.0 | 321.1 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_1000/fallback | 2524.0 | 309.3 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_1000/miss | 2335.0 | 115.3 | 136 → 48 | 5 → 1 |
| HostSelector/hosts_10000/exact | 36570.0 | 303.6 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10000/wildcard | 196568.0 | 304.9 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10000/fallback | 23765.0 | 299.7 | 488 → 400 | 6 → 3 |
| HostSelector/hosts_10000/miss | 23462.0 | 114.8 | 136 → 48 | 5 → 1 |
| RouteMatrix/routes_10/static | 210.4 | 175.9 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10/param | 414.6 | 326.4 | 512 → 400 | 7 → 3 |
| RouteMatrix/routes_10/multi_param | 491.5 | 383.7 | 560 → 400 | 8 → 3 |
| RouteMatrix/routes_10/catch_all | 398.2 | 353.4 | 472 → 408 | 6 → 4 |
| RouteMatrix/routes_10/fan_out | 167.7 | 153.2 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10/depth_5 | 209.3 | 174.9 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10/depth_10 | 239.3 | 211.1 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10/depth_20 | 310.3 | 280.3 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10/post | 339.5 | 282.5 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_10/head_fallback | 346.3 | 291.4 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_10/any_method | 351.7 | 289.2 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_10/method_miss | 236.3 | 466.8 | 128 → 496 | 5 → 6 |
| RouteMatrix/routes_1000/static | 252.1 | 215.3 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_1000/param | 456.7 | 377.4 | 512 → 400 | 7 → 3 |
| RouteMatrix/routes_1000/multi_param | 540.9 | 467.4 | 560 → 400 | 8 → 3 |
| RouteMatrix/routes_1000/catch_all | 436.7 | 403.0 | 472 → 408 | 6 → 4 |
| RouteMatrix/routes_1000/fan_out | 220.5 | 194.4 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_1000/depth_5 | 253.1 | 217.4 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_1000/depth_10 | 284.0 | 250.0 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_1000/depth_20 | 349.7 | 320.6 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_1000/post | 391.9 | 336.5 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_1000/head_fallback | 414.5 | 355.6 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_1000/any_method | 419.4 | 349.5 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_1000/method_miss | 223.4 | 521.2 | 128 → 496 | 5 → 6 |
| RouteMatrix/routes_10000/static | 284.1 | 249.8 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10000/param | 541.5 | 442.2 | 512 → 400 | 7 → 3 |
| RouteMatrix/routes_10000/multi_param | 621.6 | 477.9 | 560 → 400 | 8 → 3 |
| RouteMatrix/routes_10000/catch_all | 507.7 | 445.6 | 472 → 408 | 6 → 4 |
| RouteMatrix/routes_10000/fan_out | 251.2 | 223.4 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10000/depth_5 | 289.4 | 247.2 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10000/depth_10 | 320.6 | 284.2 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10000/depth_20 | 390.6 | 350.4 | 128 → 64 | 3 → 1 |
| RouteMatrix/routes_10000/post | 443.4 | 374.7 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_10000/head_fallback | 496.8 | 390.8 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_10000/any_method | 481.6 | 391.2 | 480 → 400 | 6 → 3 |
| RouteMatrix/routes_10000/method_miss | 238.4 | 586.0 | 128 → 496 | 5 → 6 |
| Match/routes_10/hit | 224.8 | 214.9 | 416 → 400 | 4 → 3 |
| Match/routes_10/miss | 165.1 | 146.6 | 88 → 48 | 3 → 1 |
| Match/routes_1000/hit | 285.6 | 281.7 | 416 → 400 | 4 → 3 |
| Match/routes_1000/miss | 213.0 | 195.9 | 88 → 48 | 3 → 1 |
| Match/routes_10000/hit | 324.6 | 312.1 | 416 → 400 | 4 → 3 |
| Match/routes_10000/miss | 247.5 | 223.2 | 88 → 48 | 3 → 1 |

## 原有热路径的长测复核

使用 `-benchtime=1s -count=3 -cpu=4`；此组没有 DNS Host 解析成本，并发为四个 P 下的摊销 ns/op，不能解释为单请求尾延迟。

| 场景 | before ns/op | after ns/op | before B/op → after | before allocs/op → after |
|---|---:|---:|---:|---:|
| Match/routes_10/hit | 228.8 | 216.5 | 416 → 400 | 4 → 3 |
| Match/routes_10/miss | 166.0 | 146.5 | 88 → 48 | 3 → 1 |
| Match/routes_1000/hit | 285.9 | 275.4 | 416 → 400 | 4 → 3 |
| Match/routes_1000/miss | 212.0 | 191.6 | 88 → 48 | 3 → 1 |
| Match/routes_10000/hit | 326.1 | 311.3 | 416 → 400 | 4 → 3 |
| Match/routes_10000/miss | 243.9 | 221.2 | 88 → 48 | 3 → 1 |
| MatchParallel | 115.8 | 115.5 | 416 → 400 | 4 → 3 |
