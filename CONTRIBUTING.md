# 贡献指南

感谢你对 Gateway 项目的关注！

## 开发环境

- Go 1.26.3（toolchain 固定）
- Phase 0–1 仅使用 Go 标准库，不引入任何第三方依赖

## 开发流程

1. Fork 仓库并从 `main` 分支创建特性分支；
2. 确保代码通过全部检查：

   ```bash
   make fmt-check vet test race build
   ```

3. 提交 Pull Request 到 `main` 分支。

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
