# Gateway

一个仅使用 Go 标准库实现的最小 HTTP 网关，当前范围是 Phase 0 工程骨架与 Phase 1 最小反向代理链路。

## 当前状态

Phase 0/1 已实现，并通过独立只读复审、主智能体最终复核和本地双进程联调：

- Go module 为 `github.com/helantianshen/gateway`，语言版本声明为 `go 1.26.3`；
- `cmd/gateway` 负责配置读取、依赖装配、signal context 和进程生命周期；
- public/admin 两个独立 HTTP Server，提供 `/livez` 和 `/readyz`；
- 通过环境变量配置监听地址、upstream、请求总超时和优雅退出超时；
- 基于 `httputil.ReverseProxy` 的 `Rewrite` 模式反向代理；
- 清理客户端转发头，重新设置 `X-Forwarded-For`、`X-Forwarded-Host` 和 `X-Forwarded-Proto`；
- 应用生命周期内复用带连接池和超时配置的 `http.Transport`；
- 连接失败和 upstream 超时分别返回统一的 502/504 JSON 错误；
- 请求体、escaped path、query、流式响应和客户端取消传播有自动化测试；
- `cmd/mock-service` 提供本地演示 upstream；
- Makefile 提供格式化、静态检查、测试、race、构建和本地运行目标；
- 独立 reviewer 最终结论为“无 blocker/concern/suggestion”，普通测试、race、重复稳定性测试和真实 SIGTERM 优雅退出演示均通过。

当前未实现的路由树、动态配置、负载均衡、重试、熔断、认证、限流、指标和分布式配置不属于本阶段能力，也不在本 README 中作为已完成能力声明。

## 快速开始

```bash
# 终端 1：启动 mock-service（默认监听 :18080）
make run-mock

# 终端 2：启动 gateway（默认 public :8080，admin :9090）
make run-gateway

curl -i 'http://127.0.0.1:8080/hello?name=gateway'
curl -i http://127.0.0.1:8080/echo -d 'hello body'
curl -i http://127.0.0.1:9090/livez
curl -i http://127.0.0.1:9090/readyz
```

## 配置项

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `GATEWAY_PUBLIC_ADDR` | `:8080` | public Server 监听地址 |
| `GATEWAY_ADMIN_ADDR` | `:9090` | admin Server 监听地址 |
| `GATEWAY_UPSTREAM_URL` | `http://127.0.0.1:18080` | 反向代理目标地址 |
| `GATEWAY_REQUEST_TIMEOUT` | `3s` | 单次代理请求总超时 |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | Graceful Shutdown 超时 |

配置使用环境变量读取，不使用 YAML 或外部配置中心。upstream 必须是带 host 的 `http` 或 `https` 绝对 URL；时长配置必须为正值。

## 项目文件

- [Phase 0/1 实施计划](docs/04-phase-0-1-implementation-plan.md)
- [贡献说明](CONTRIBUTING.md)
- [MIT License](LICENSE)
