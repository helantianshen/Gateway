# Gateway

一个用 Go 标准库实现的 API 网关，基于 `net/http` 与 `httputil.ReverseProxy` 构建数据面，支持双 HTTP Server 生命周期、优雅停机、连接池复用和统一错误处理。

> 当前版本：**Phase 0/1** — 工程骨架与最小反向代理链路

## 架构

```text
客户端
  │
  ▼
┌─────────────────────────────────────────┐
│  Gateway                                │
│                                         │
│  public Server (:8080)                  │
│  ├─ httputil.ReverseProxy (Rewrite)     │
│  ├─ hop-by-hop 头清理                   │
│  ├─ X-Forwarded-* 重建                  │
│  ├─ 请求总超时 (Context)                 │
│  └─ 502 / 504 错误分类                  │
│         │                               │
│         ▼                               │
│  共享 http.Transport                    │
│  ├─ 连接池 (MaxConnsPerHost)            │
│  ├─ TLS / HTTP2 协商                     │
│  └─ CloseIdleConnections (停机时)       │
│         │                               │
│  admin Server (:9090)                   │
│  ├─ GET/HEAD /livez                     │
│  └─ GET/HEAD /readyz                    │
└─────────────────────────────────────────┘
  │
  ▼
upstream 服务
```

## 快速开始

```bash
# 终端 1：启动 mock-service（默认监听 :18080）
make run-mock

# 终端 2：启动 gateway（默认 public :8080，admin :9090）
make run-gateway
```

验证：

```bash
# 业务请求经网关转发到 mock-service
curl -i 'http://127.0.0.1:8080/hello?name=gateway'
curl -i http://127.0.0.1:8080/echo -d 'hello body'

# SSE 流式响应
curl -N 'http://127.0.0.1:8080/stream?count=3'

# upstream 超时返回 504
curl -i 'http://127.0.0.1:8080/slow?delay=5s'

# 健康端点
curl -i http://127.0.0.1:9090/livez
curl -i http://127.0.0.1:9090/readyz
```

## 配置

通过环境变量配置，不使用 YAML 或外部配置中心：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `GATEWAY_PUBLIC_ADDR` | `:8080` | public Server 监听地址 |
| `GATEWAY_ADMIN_ADDR` | `:9090` | admin Server 监听地址 |
| `GATEWAY_UPSTREAM_URL` | `http://127.0.0.1:18080` | 反向代理目标地址 |
| `GATEWAY_REQUEST_TIMEOUT` | `3s` | 单次代理请求总超时 |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | Graceful Shutdown 超时 |

- upstream 必须是带 host 的 `http` 或 `https` 绝对 URL
- 时长配置必须为正值（如 `3s`、`500ms`、`1m`）

## 项目结构

```text
.
├── cmd/
│   ├── gateway/            # 网关进程入口
│   └── mock-service/       # 本地演示 upstream
├── internal/
│   ├── bootstrap/          # 应用生命周期：双 Server、Run、Graceful Shutdown
│   ├── config/             # 环境变量配置加载与校验
│   └── dataplane/
│       ├── proxy/          # httputil.ReverseProxy 封装
│       ├── server/         # HTTP Server 与健康端点
│       ├── transport/      # 共享 http.Transport
│       └── response/       # 统一 JSON 错误响应
├── docs/
│   ├── 01-technology-selection.md   # 技术选型
│   ├── 02-architecture-design.md    # 架构设计
│   ├── 03-development-roadmap.md    # 完整开发路线
│   └── 04-phase-0-1-implementation-plan.md  # Phase 0/1 实施计划
├── .github/workflows/ci.yml          # GitHub Actions CI
├── Makefile                          # fmt / vet / test / race / build
├── go.mod                            # github.com/helantianshen/gateway
├── LICENSE                           # MIT
└── CONTRIBUTING.md
```

## 核心设计

### 双 HTTP Server

public Server 接收业务流量并转发到 upstream；admin Server 独立提供 `/livez` 和 `/readyz` 健康端点。两者使用不同的监听器，可以独立关闭或暴露到不同网络。

### 共享 Transport

应用启动时创建一个 `http.Transport`，在整个生命周期内被所有代理请求复用。配置了连接池上限、Dial 超时、TLS 握手超时、响应头超时和空闲连接回收。停机时调用 `CloseIdleConnections` 释放出站连接池。

### ReverseProxy Rewrite 模式

使用 Go 1.20+ 的 `Rewrite` + `ProxyRequest.SetURL`，不使用已废弃的 `Director`。标准库在调用 `Rewrite` 前自动移除 hop-by-hop 头和客户端伪造的 `X-Forwarded-*`，`Rewrite` 中重新设置干净的转发头。

### 错误分类

| 场景 | 状态码 | 说明 |
|---|---|---|
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

# 一键全部检查
make fmt-check vet test race build
```

## 技术约束

本阶段严格只使用 Go 标准库，不引入任何第三方依赖：

```bash
$ go list -m all
github.com/helantianshen/gateway
```

## 开发路线

| Phase | 主题 | 状态 |
|---:|---|---|
| 0 | 工程骨架与质量基线 | ✅ 完成 |
| 1 | 最小反向代理链路 | ✅ 完成 |
| 2 | 配置模型与严格 YAML | 计划中 |
| 3 | 路由语义与 Radix Tree | — |
| 4 | Upstream 与 Round Robin | — |
| 5 | 中间件、日志与 Prometheus | — |
| 6 | Gin 控制面与 etcd 发布 | — |
| 7 | Watch、ConfigSnapshot 与 LKG | — |

完整路线见 [开发路线图](docs/03-development-roadmap.md)。

## License

[MIT](LICENSE)
