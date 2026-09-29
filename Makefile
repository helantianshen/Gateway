.PHONY: fmt fmt-check check-diff vet staticcheck vuln audit test race build verify ci run run-mock run-mock-1 run-mock-2 run-mock-3 run-gateway bench bench-balancer bench-observability fuzz fuzz-path fuzz-conflict fuzz-match

FUZZTIME ?= 15s
STATICCHECK_VERSION ?= v0.7.0
GOVULNCHECK_VERSION ?= v1.6.0

# gofmt 格式化所有 Go 源码
fmt:
	gofmt -w .

# 检查格式化但不修改文件，CI 使用
fmt-check:
	@diff=$$(gofmt -l .); \
	if [ -n "$$diff" ]; then \
		echo "以下文件需要 gofmt:"; \
		echo "$$diff"; \
		exit 1; \
	fi

# 检查当前 diff，并扫描已跟踪文本中的尾随空格
check-diff:
	git diff --check
	@trailing=$$(git grep -nI -E '[[:blank:]]+$$' -- . ':!go.sum' || true); \
	if [ -n "$$trailing" ]; then \
		echo "以下已跟踪文件包含尾随空格:"; \
		echo "$$trailing"; \
		exit 1; \
	fi

# 静态检查
vet:
	go vet ./...

# 运行固定版本 staticcheck（需要下载工具时会使用 Go module cache）
staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

# 扫描标准库与依赖中的可达漏洞
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# 完整静态/安全审查；与不下载额外工具的 verify 分开
audit: staticcheck vuln

# 运行全部测试
test:
	go test ./...

# 运行竞态检测
race:
	go test -race ./...

# 编译全部包
build:
	go build ./...

# 不下载额外审查工具的本地质量检查
verify: check-diff fmt-check vet test race build

# 与 CI 相同的检查范围，包含模块完整性与安全审查
ci:
	go version
	go mod verify
	$(MAKE) verify audit

# 默认运行 gateway
run: run-gateway

# 运行 gateway 进程；可通过 GATEWAY_CONFIG_FILE 覆盖默认配置文件 configs/gateway.yaml
run-gateway:
	@echo "使用配置文件: $${GATEWAY_CONFIG_FILE:-configs/gateway.yaml}"
	go run ./cmd/gateway

# 运行默认 mock-service；多 endpoint 演示需在三个终端分别运行 1/2/3。
run-mock: run-mock-1

run-mock-1:
	go run ./cmd/mock-service -addr :18080 -id mock-1

run-mock-2:
	go run ./cmd/mock-service -addr :18081 -id mock-2

run-mock-3:
	go run ./cmd/mock-service -addr :18082 -id mock-3

# 运行 router benchmark，结果保存到 benchmarks/results/router/
bench:
	@mkdir -p benchmarks/results/router
	@output=benchmarks/results/router/bench.txt; \
	go test -bench=. -benchmem -count=3 -run=^$$ ./internal/router/ >$$output 2>&1; \
	status=$$?; sed -i 's/[[:blank:]]\+$$//' $$output; cat $$output; \
	echo "Go version: $$(go version)" >> $$output; \
	echo "Date: $$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> $$output; \
	echo "Command: go test -bench=. -benchmem -count=3 -run=^$$ ./internal/router/" >> $$output; exit $$status

# 运行 Round Robin benchmark，结果保存到 benchmarks/results/balancer/
bench-balancer:
	@mkdir -p benchmarks/results/balancer
	@output=benchmarks/results/balancer/bench.txt; \
	go test -run=^$$ -bench='Benchmark(RoundRobin|CompiledUpstreamSelect)' -benchmem -count=3 \
		./internal/dataplane/balancer/ ./internal/dataplane/upstream/ >$$output 2>&1; \
	status=$$?; sed -i 's/[[:blank:]]\+$$//' $$output; cat $$output; \
	echo "Go version: $$(go version)" >> $$output; \
	echo "Date: $$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> $$output; \
	echo "Command: go test -run=^$$ -bench='Benchmark(RoundRobin|CompiledUpstreamSelect)' -benchmem -count=3 ./internal/dataplane/balancer/ ./internal/dataplane/upstream/" >> $$output; exit $$status

# 运行 Phase 5 全局中间件 benchmark，结果保存到 benchmarks/results/observability/
bench-observability:
	@mkdir -p benchmarks/results/observability
	@output=benchmarks/results/observability/bench.txt; \
	go test -run=^$$ -bench=BenchmarkHTTPMiddleware -benchmem -count=3 \
		./internal/dataplane/middleware/ >$$output 2>&1; \
	status=$$?; sed -i 's/[[:blank:]]\+$$//' $$output; cat $$output; \
	echo "Go version: $$(go version)" >> $$output; \
	echo "Date: $$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> $$output; \
	echo "Command: go test -run=^$$ -bench=BenchmarkHTTPMiddleware -benchmem -count=3 ./internal/dataplane/middleware/" >> $$output; exit $$status

# 运行路径模式解析 fuzz 测试并保存原始输出
fuzz-path:
	@mkdir -p benchmarks/results/router
	@output=benchmarks/results/router/fuzz-path.txt; \
	go test -fuzz=FuzzParsePathPattern -fuzztime=$(FUZZTIME) ./internal/router/ >$$output 2>&1; \
	status=$$?; cat $$output; echo "Go version: $$(go version)" >> $$output; \
	echo "Command: go test -fuzz=FuzzParsePathPattern -fuzztime=$(FUZZTIME) ./internal/router/" >> $$output; exit $$status

# 运行冲突检测对称性 fuzz 测试并保存原始输出
fuzz-conflict:
	@mkdir -p benchmarks/results/router
	@output=benchmarks/results/router/fuzz-conflict.txt; \
	go test -fuzz=FuzzConflictDetection -fuzztime=$(FUZZTIME) ./internal/router/ >$$output 2>&1; \
	status=$$?; cat $$output; echo "Go version: $$(go version)" >> $$output; \
	echo "Command: go test -fuzz=FuzzConflictDetection -fuzztime=$(FUZZTIME) ./internal/router/" >> $$output; exit $$status

# 运行编译与匹配 fuzz 测试并保存原始输出
fuzz-match:
	@mkdir -p benchmarks/results/router
	@output=benchmarks/results/router/fuzz-match.txt; \
	go test -fuzz=FuzzCompileAndMatch -fuzztime=$(FUZZTIME) ./internal/router/ >$$output 2>&1; \
	status=$$?; cat $$output; echo "Go version: $$(go version)" >> $$output; \
	echo "Command: go test -fuzz=FuzzCompileAndMatch -fuzztime=$(FUZZTIME) ./internal/router/" >> $$output; exit $$status

# 运行全部 fuzz 测试（各 15 秒）
fuzz: fuzz-path fuzz-conflict fuzz-match
