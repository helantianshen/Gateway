.PHONY: fmt fmt-check vet test race build run run-mock run-mock-1 run-mock-2 run-mock-3 run-gateway bench bench-balancer fuzz fuzz-path fuzz-conflict fuzz-match

FUZZTIME ?= 15s

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

# 静态检查
vet:
	go vet ./...

# 运行全部测试
test:
	go test ./...

# 运行竞态检测
race:
	go test -race ./...

# 编译全部包
build:
	go build ./...

# 运行 gateway 进程；可通过 GATEWAY_CONFIG_FILE 覆盖默认配置文件 configs/gateway.yaml
run-gateway:
	@echo "使用配置文件: $${GATEWAY_CONFIG_FILE:-configs/gateway.yaml}"
	go run ./cmd/gateway

# 运行默认 mock-service；Phase 4 多 endpoint 演示需在三个终端分别运行 1/2/3。
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
	go test -bench=. -benchmem -count=3 -run=^$$ ./internal/router/ | tee benchmarks/results/router/bench.txt
	@echo "Go version: $$(go version)" >> benchmarks/results/router/bench.txt
	@echo "Date: $$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> benchmarks/results/router/bench.txt
	@echo "Command: go test -bench=. -benchmem -count=3 -run=^$$ ./internal/router/" >> benchmarks/results/router/bench.txt

# 运行 Round Robin benchmark，结果保存到 benchmarks/results/balancer/
bench-balancer:
	@mkdir -p benchmarks/results/balancer
	@output=benchmarks/results/balancer/bench.txt; \
	go test -run=^$$ -bench='Benchmark(RoundRobin|CompiledUpstreamSelect)' -benchmem -count=3 \
		./internal/dataplane/balancer/ ./internal/dataplane/upstream/ >$$output 2>&1; \
	status=$$?; cat $$output; echo "Go version: $$(go version)" >> $$output; \
	echo "Date: $$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> $$output; \
	echo "Command: go test -run=^$$ -bench='Benchmark(RoundRobin|CompiledUpstreamSelect)' -benchmem -count=3 ./internal/dataplane/balancer/ ./internal/dataplane/upstream/" >> $$output; exit $$status

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
