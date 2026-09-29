# 贡献指南

感谢你对 Gateway 项目的关注！

## 开发环境

- `go.mod` 要求 Go 1.26.8，CI 从 `go.mod` 读取该版本；没有 `toolchain` 指令。先用 `go version` 核实本机，不把历史记录当成当前环境。
- 当前为 Phase 5；直接依赖包括 yaml.v3、httpsnoop、zap 和 prometheus/client_golang，确切版本见 go.mod。
- 开发前阅读 [当前架构](.agent/PROJECT.md)、[当前审查](docs/10-current-architecture-review.md)及适用阶段计划。目标架构中的未来接口不是当前实现。
- 示例运行需要三个 mock-service；命令与配置见 README。测试本身不依赖 etcd、Redis 或外部数据库。

## 开发流程

1. Fork 仓库并从 `main` 分支创建特性分支；
2. 确保代码通过全部检查：

   ```bash
   make ci
   ```

3. 提交 Pull Request 到 `main` 分支。

`make ci` 顺序运行模块完整性、`make verify` 和 `make audit`，覆盖 CI 的全部检查。复现 CI 时使用 `GOTOOLCHAIN=go1.26.8 make ci`，不会修改系统默认 Go 版本。

`make verify` 包含 diff、格式、vet、单测、race 和 build；`make audit` 另行执行 staticcheck 与 govulncheck，可能需要下载工具和访问漏洞库。`go mod verify` 仅验证模块缓存完整性。

修改协议或流式行为时，补充真实 TCP/HTTP 测试；路由变化应比较完整匹配结果。benchmark/fuzz 命令见 Makefile，部分命令会覆盖仓库中的历史结果，执行前确认输出位置。性能和安全结论必须注明工具链、命令与本次证据。

修改架构、依赖、配置 schema 或错误契约时，同步 README 和 `.agent/PROJECT.md`；历史报告保留原始时间与证据，通过新报告说明后续变化。

## 代码规范

- 每个 package 须有中文包注释，说明职责与非职责；
- 每个导出类型、函数、方法和常量须有中文 Go Doc 注释；
- 注释应解释设计意图、边界条件和并发语义，不做逐字翻译；
- 进程生命周期、Context 取消、Graceful Shutdown、错误通道等关键逻辑须有详细中文注释；
- 测试中的特殊场景须有中文注释说明风险与预期。

## 提交信息

建议使用简洁明了的提交信息，遵循 Conventional Commits 格式，例如：

```
feat(proxy): 实现 ReverseProxy Rewrite 头清理
fix(shutdown): 修复 Shutdown 超时未关闭监听器的问题
docs(readme): 更新 Phase 1 完成状态
```
