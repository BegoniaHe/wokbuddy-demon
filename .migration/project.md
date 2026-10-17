# panel-refactor（shadcn + Base UI + 极简主题）

2026-10-17，whole-project 破坏性重构（用户明确要求「完整重构、破坏性重构」）。

## Changed

- `web/components.json`（新增）— shadcn 配置：style `base-nova`、Base UI 基础、lucide 图标、`@/` 别名。
- `web/src/index.css` — 重写主题：shadcn 标准 token 命名（primary/background/card/muted/border…）+ 本面板极简配色；明暗由 `<html data-theme>` 切换（自定义 dark 变体）。新增 success/warning 语义色与软底色。
- `web/src/components/ui/` — 删除全部自研组件（box/bar/stat/旧 button/dialog/field/table/tabs），改为 shadcn base 套件：button/card/dialog/field/input/select/separator/switch/table/tabs/label（按引用保留，未用的 alert/badge/empty/popover 等已移除）。
- `web/src/components/ui-kit.tsx`（新增）— 领域小组件：Dot/Tag/Chip/Bar/MixBar/Legend（基于 shadcn token 与 cva）。
- `web/src/lib/toast.ts`（新增）— toast 队列抽离为纯模块（与 UI 解耦）。
- `web/src/components/` — Shell/App/Toaster/KeyGate/AddAccountDialog/TasksDialog/VouchersDialog/TimeRange 全部重写为 shadcn 组合（Card/Dialog/Field/Tabs/Select/Switch）。
- `web/src/views/*` — 8 个视图全部重写（Card 布局 + shadcn Table + 领域 ui-kit），保留全部数据逻辑与 API 调用不变。
- `web/package.json` — 新增 `cn`（shadcn registry 依赖，已改 import 指向 `@/lib/utils`，函数一致）。
- `internal/panel/dist/` — 重新构建产物（`make web`）。

radix 扫描：`grep -rn "radix-ui\|@radix-ui" web/src` = 0 命中（本就无 radix；原 dialog 已迁 Base UI）。

## Left alone

- `web/src/lib/*` — 纯函数模块（format/usage/packages/reqlog/timeRange/config/models/expiry/qr/autoTasks/types/api/toast）逻辑不动，仅 `Toaster` 改为读 `lib/toast`。
- `web/src/hooks/*` — useTheme/useHashRoute 不变。
- Go 侧 `internal/*` — 无改动（面板 API 契约不变）。
- 未装 cmdk/vaul/sonner/input-otp/react-day-picker/recharts（项目从未使用）。

## Behavior changes

- 主题切换按钮从自定义 `Box`/`Dot` 改为 shadcn `Button` + ui-kit `Dot`；视觉等价。
- 「刷新」等按钮 variant 由旧的 `default`（边框样式）语义映射到 shadcn `default`（实心主色），符合 shadcn 约定。
- 表格行 rail 色条、状态 Tag、数值 Chip 由自研组件迁到 ui-kit，class 等价。

## Verify by hand

- 8 个视图渲染、深浅主题切换、创建密钥对话框、Select 下拉、Switch 开关。
- 本地 `make run` + playwright 冒烟通过：账号池/用量/积分构成/模型/密钥/配置/日志/任务中心均无控制台错误，卡片/按钮/表格样式正确，无横向溢出。
- `bun run typecheck`、`bun run test`（56 passed）、`bun run build`、`go build ./...`、`go test ./internal/panel/...` 全绿。
