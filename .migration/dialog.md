# dialog

2026-10-17 · 引擎迁移（radix → Base UI，golden-pair 不适用：手写组件非 shadcn registry）

## Changed

- `web/src/components/ui/dialog.tsx` — `@radix-ui/react-dialog` → `@base-ui/react/dialog`（根命名空间导入）。API 差异：
  - `DialogPrimitive.Root/Portal/Overlay/Content/Title/Close` → `Dialog.Root/Portal/Backdrop/Popup/Title/Close`（`index.parts.d.ts` 确认）。
  - `Dialog.Backdrop` 替换 `Dialog.Overlay`（Radix 的 Overlay 语义 = Base UI 的 Backdrop）。
  - `Dialog.Popup` 替换 `Dialog.Content`，其余子组件（`ModalHeader/Body/Footer/Hint`）为纯 DOM 包装，未改动。
- `web/package.json` — 删除 `@radix-ui/react-dialog`，新增 `@base-ui/react@1.9.0`（bun.lock 同步）。
- 遗留扫描：`grep -rn "@radix-ui\|radix-ui" web/src` 无结果，radix 依赖已全部移除。

## Left alone

- `web/src/components/ui/{button,tabs,box,table,stat,field,bar}.tsx` — 手写组件，不依赖 radix，仅样式类同步极简主题（去边框化、去掉 Stat 右侧竖条、Kpi 去掉 before 侧条、Tabs 选中项改边框）。
- `web/src/index.css` — 设计令牌重写为中性灰阶 + 蓝色强调（极简主题），`--radius-*` 收紧（control 4px / inner 6px / surface 8px），阴影弱化为 `0 1px 2px`。
- 弹窗消费方（`KeyGate`、`AddAccountDialog`、`TasksDialog`、`VouchersDialog`、`KeysView`）未改导入路径，组件 API 保持向后兼容（`Modal/ModalHeader/ModalTitle/ModalHint/ModalBody/ModalFooter/CloseOnEscape` 签名不变）。

## Behavior changes

- Base UI `Dialog` 默认 `modal=true`（焦点陷阱 + 滚动锁 + 外部指针屏蔽）与 Radix 一致。
- `aria-describedby={undefined}` 保留原用法（无 Description 时不挂 aria-describedby）。
- 无键盘交互差异预期；Base UI 焦点管理与 Radix 均遵循 WAI-ARIA dialog 模式。

## Verify by hand

1. 打开「添加账号」弹窗：Esc 关闭、点击遮罩关闭、Tab 键焦点循环在弹窗内。
2. 弹窗打开时页面滚动锁定，关闭后焦点回到触发按钮。
3. 「创建密钥」「查询券码」「任务详情」各弹窗重复上述检查。
4. 浅色 / 深色主题下弹窗遮罩与卡片边框均正常。
