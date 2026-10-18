# WokBuddy Demon 开发任务入口。
# 依赖：go 1.27+、golangci-lint v2
# 安装 golangci-lint：
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

GOLANGCI ?= golangci-lint
PKG      ?= ./...
BIN      ?= wokbuddy-demon
# 开发运行参数（可覆盖：make run CONFIG=config.json）。
# 监听端口由 config.json 的 listen（或 WB2A_LISTEN 环境变量）决定；
# run/stop/status 以 pid 文件为生命周期事实源，不依赖端口。
CONFIG   ?= config.json
DEV      := bash scripts/make_dev.sh
# 构建标识：编译时经 -ldflags 注入 server 二进制（滚动更新、无版本号）。
# 可覆盖：make build-all VERSION=nightly-20261016-abc1234
VERSION  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.appVersion=$(VERSION)

.PHONY: all build build-all build-tools run dev restart stop status logs config \
	test test-short test-race bench cover lint fmt fmt-check vet escape tidy clean ci \
	web web-install web-dev web-check web-check-dist \
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
	@echo "  make escape       逃逸分析（-gcflags=-m）"
	@echo "  make web          构建前端（web/ → internal/panel/dist，需 Bun）"
	@echo "  make web-dev      前端开发服务器   make web-check  前端类型检查 + 单测"
	@echo "  make docker-up    构建并后台启动 Docker 服务"

# ---------------------------------------------------------------------------
# 构建
# ---------------------------------------------------------------------------

build:
	go build $(PKG)

# 前端工程（web/，Rsbuild + React + Tailwind）：产物输出到 internal/panel/dist，
# 随 go:embed 打进二进制。dist 已提交进仓库，故 `go build` 本身无需 node/bun；
# 仅改动前端源码时才需重新构建。依赖 Bun（https://bun.sh）。
BUN ?= bun

web:
	cd web && $(BUN) install --frozen-lockfile && $(BUN) run build

web-install:
	cd web && $(BUN) install

web-dev:
	cd web && $(BUN) run dev

web-check:
	cd web && $(BUN) run typecheck && $(BUN) run test

# 校验提交的 dist 与 web/ 源码一致（CI 用：重新构建后要求零 diff）。
web-check-dist: web
	@git diff --exit-code -- internal/panel/dist || \
		(echo "internal/panel/dist 与 web/ 源码不一致，请运行 'make web' 后提交产物"; exit 1)


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
	@CONFIG=$(CONFIG) $(DEV) run

# 前台运行：Ctrl-C 退出，适合开发调试。
dev:
	go run ./cmd/server -config $(CONFIG)

restart:
	@CONFIG=$(CONFIG) $(DEV) stop
	@$(MAKE) --no-print-directory run

stop:
	@CONFIG=$(CONFIG) $(DEV) stop

status:
	@CONFIG=$(CONFIG) $(DEV) status

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

# 编译器逃逸分析：查看变量是否逃逸到堆及原因；需要更详细的内联/逃逸链时用 -gcflags='-m -m'。
escape:
	go build -gcflags='-m' $(PKG) 2>&1

tidy:
	go mod tidy

clean:
	go clean -cache -testcache
	@CONFIG=$(CONFIG) $(DEV) clean

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
