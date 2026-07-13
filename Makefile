.PHONY: fmt fmt-check vet test race build run run-mock run-gateway

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

# 运行 gateway 进程
run-gateway:
	go run ./cmd/gateway

# 运行 mock-service 进程，用于演示和测试反向代理
run-mock:
	go run ./cmd/mock-service
