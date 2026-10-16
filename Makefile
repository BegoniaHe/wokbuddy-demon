# WokBuddy Demon 开发任务入口。
# 依赖：go 1.27+、golangci-lint v2（含 gofumpt / gci formatter）。
# 安装 golangci-lint：
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

GOLANGCI ?= golangci-lint
PKG      ?= ./...

.PHONY: all build test test-short lint fmt fmt-check vet tidy clean ci

# 默认：格式化 + 静态检查 + 测试 + 构建（与 CI 同口径）。
all: fmt lint test build

build:
	go build $(PKG)

# 全量测试（含 scheduler 的时序用例，约 30s）。
test:
	go test $(PKG) -count=1

# 快速测试（跳过耗时/网络相关）。
test-short:
	go test $(PKG) -count=1 -short

# 静态检查（严格规则集见 .golangci.yml）。
lint:
	$(GOLANGCI) run $(PKG)

# 格式化：gofumpt（含 extra.group-params）+ gci（import 分组）。
fmt:
	$(GOLANGCI) fmt $(PKG)

# 仅校验格式（CI 用；有差异则非零退出）。
fmt-check:
	$(GOLANGCI) fmt --diff $(PKG)

vet:
	go vet $(PKG)

tidy:
	go mod tidy

clean:
	go clean -cache -testcache

# CI 本地复现：格式校验 + 静态检查 + 全量测试。
ci: fmt-check lint test
