# WokBuddy Demon 开发任务入口。
# 依赖：go 1.27+、golangci-lint v2
# 安装 golangci-lint：
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

GOLANGCI ?= golangci-lint
PKG      ?= ./...
BIN      ?= wokbuddy-demon
# 开发运行参数（可覆盖：make run PORT=7863 CONFIG=config.json）。
PORT     ?= 7863
CONFIG   ?= config.json
DEV      := bash scripts/make_dev.sh
# 构建标识：编译时经 -ldflags 注入 server 二进制（滚动更新、无版本号）。
# 可覆盖：make build-all VERSION=nightly-20261016-abc1234
VERSION  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.appVersion=$(VERSION)

.PHONY: all build build-all build-tools run dev restart stop status logs config \
	test test-short test-race bench cover lint fmt fmt-check vet tidy clean ci \
	docker-build docker-up docker-down docker-logs help

# 默认：格式化 + 静态检查 + 测试 + 构建
all: fmt lint test build

help:
	@echo "常用目标："
	@echo "  make run          构建并在后台启动服务"
	@echo "  make dev          前台运行"
	@echo "  make stop         停止后台服务"
	@echo "  make restart      重启后台服务"
	@echo "  make status       查看运行状态与 /healthz"
	@echo "  make logs         跟踪服务日志"
	@echo "  make config       由 config.example.json 生成 config.json（已存在则跳过）"
	@echo "  make build        构建 server 二进制"
	@echo "  make build-all    构建全部二进制（server/signin/login/credit）"
	@echo "  make test         全量测试      make test-short  快速测试"
	@echo "  make lint/fmt     静态检查 / 格式化"
	@echo "  make docker-up    构建并后台启动 Docker 服务"

# ---------------------------------------------------------------------------
# 构建
# ---------------------------------------------------------------------------

build:
	go build $(PKG)

# 一次编译全部二进制（与 Dockerfile 同口径）。
build-all: build-tools

build-tools:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/server
	go build -trimpath -ldflags="-s -w" -o signin_bin ./cmd/signin
	go build -trimpath -ldflags="-s -w" -o login ./cmd/login
	go build -trimpath -ldflags="-s -w" -o credit ./cmd/credit

# ---------------------------------------------------------------------------
# 运行（后台托管，见 scripts/make_dev.sh）
# ---------------------------------------------------------------------------

# 后台启动：构建 → 拉起进程 → 写 pid/日志 → 立即返回终端。
run:
	@PORT=$(PORT) CONFIG=$(CONFIG) $(DEV) run

# 前台运行：Ctrl-C 退出，适合开发调试。
dev:
	go run ./cmd/server -config $(CONFIG)

restart:
	@PORT=$(PORT) $(DEV) stop
	@$(MAKE) --no-print-directory run

stop:
	@PORT=$(PORT) $(DEV) stop

status:
	@PORT=$(PORT) $(DEV) status

logs:
	@$(DEV) logs

# 由样例生成配置（保留已有 config.json，避免覆盖真实密钥）。
config:
	@if [ -f $(CONFIG) ]; then \
		echo "$(CONFIG) 已存在，跳过"; \
	else \
		cp config.example.json $(CONFIG); \
		echo "已生成 $(CONFIG)，请按需修改 api_key 等字段"; \
	fi

# ---------------------------------------------------------------------------
# 测试与质量
# ---------------------------------------------------------------------------

# 全量测试（含 scheduler 的时序用例，约 30s）。
test:
	go test $(PKG) -count=1

# 快速测试（跳过耗时/网络相关）。
test-short:
	go test $(PKG) -count=1 -short

# 竞态检测（较慢，CI 之外手工跑）。
test-race:
	go test $(PKG) -count=1 -race

bench:
	go test $(PKG) -run '^$$' -bench . -benchmem

cover:
	go test $(PKG) -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out

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
	@PORT=$(PORT) $(DEV) clean

# CI 本地复现：格式校验 + 静态检查 + 全量测试。
ci: fmt-check lint test

# ---------------------------------------------------------------------------
# Docker
# ---------------------------------------------------------------------------

docker-build:
	docker compose build

docker-up: config
	docker compose up -d --build

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f
