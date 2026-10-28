# 机械整理工具链：跨版本通用、可复现的静态整理脚本

这组脚本只做**机械整理**：不猜语义、不做 LLM 重命名、不试图还原 TypeScript 类型。
目标是「同一份脚本对任意版本跑出可比的结构化事实」，让跨版本差异落到可核对的表格上，
而不是让人去读压缩单行文件。

## Locked

- 输入只读：只扫 `.tmp/workbuddy/<realm>/<ver>/app-asar/`，不改原始解包树。
- 产物只落 `.tmp/`（默认 `.tmp/workbuddy-diff/`、`.tmp/readable*/`、`.tmp/split/`），不进版本库。
- **零常驻依赖**：`scripts/lib/*.mjs` 全部自包含。需要第三方分析器时只临时装到
  `.tmp/tooling/`，不写 `package.json`（与 REA 的处理方式一致）：
  `npm i --prefix .tmp/tooling acorn eslint-scope terser --no-audit --no-fund`
- join key 只能是**语义键**：相对路径、可读导出名、字面量值。
  mangled 名、renderer 的 hash 文件名、`esm2.js` 这类数字后缀都会随版本漂移，不能当键。

## 脚本一览

| 脚本 | 做什么 | 跨版本为什么稳定 |
| --- | --- | --- |
| `asar_manifest.mjs` | 每个文件的 `{kind, bytes, lines, sha256}`；`--compare` 出 added/removed/changed | 按相对路径 join；同名文件实测 170/174 |
| `module_graph.mjs` | `require()` / `import` 边、入出度 hub、外部包、悬空引用；`--who-requires` / `--closure` | specifier 是字符串字面量，按路径归一 |
| `export_map.mjs` | `module -> {可读导出名: mangled}`；含 facade 两跳解析 | **可读导出名不参与混淆**，天然可比 |
| `public_surface_diff.mjs` | 上面两份的四类差异：模块增删 / API 增删 / mangled 变化 / API 搬家 | 同上 |
| `literal_index.mjs` | 全量字符串字面量倒排索引 + 标签（endpoint/url/header/mime/scheme/i18n） | 模板串插值归一成 `${}` |
| `format_tree.mjs` | 压缩单行 → 可读排版，`--verify` 做 token 级回校 | 与版本无关，纯形态 |
| `rename_scope.mjs` | 确定性 α 重命名 `e,t,n…` → `v1..vN`（按定义顺序） | 同模块内稳定；跨模块无意义 |
| `split_inline_modules.mjs` | 拆大聚合文件里的内联模块工厂，带字面量线索 | 工厂名从 `rolldown-runtime.js` 机械推导 |

## 推荐顺序

```bash
# 0) 清单与依赖图（秒级，先看有没有结构变化）
node .agents/skills/reverse-workbuddy/scripts/asar_manifest.mjs \
  --compare "cn/<verA>,global/<verB>" --out .tmp/workbuddy-diff/manifest.json
node .agents/skills/reverse-workbuddy/scripts/module_graph.mjs \
  --target "global/<ver>" --out .tmp/workbuddy-diff/graph.json

# 1) API 面差异（回答「哪个模块的哪个导出变了」）
node .agents/skills/reverse-workbuddy/scripts/public_surface_diff.mjs \
  --compare "cn/<verA>,global/<verB>" --out .tmp/workbuddy-diff/surface.json

# 2) 字面量差异（回答「哪个端点/头/配置键变了」）
node .agents/skills/reverse-workbuddy/scripts/literal_index.mjs \
  --compare "cn/<verA>,global/<verB>" --out .tmp/workbuddy-diff/literals.json

# 3) 需要阅读实现时，再格式化 / 重命名 / 拆块
node .agents/skills/reverse-workbuddy/scripts/format_tree.mjs \
  --target "global/<ver>" --dirs main,preload --verify
node .agents/skills/reverse-workbuddy/scripts/rename_scope.mjs \
  --target "global/<ver>" --dirs main --out-dir .tmp/readable-renamed
node .agents/skills/reverse-workbuddy/scripts/split_inline_modules.mjs \
  --target "global/<ver>" --min-bytes 300000
```

## 实测基线（global 5.8.4）

| 指标 | 数值 |
| --- | --- |
| asar 树 | 13864 文件 / 581MB（`node_modules` 占 355MB，通常直接排除） |
| `main/` | 176 js / 17MB，平均 3.1KB/行，`__esmMin` 包装 0 处（已是「一文件一模块」） |
| `main/` 边 | 721 条内部 require 边 |
| 可读导出名 | 182 模块 / 45 个 facade / 137 条可读名（`transport.js` 单模块 31 条） |
| 字面量 | `main/` 96544 个 / 42031 唯一 |
| 格式化 | 181 文件 17MB→21MB，5793 行→517007 行，token 回校 0 失败 |
| 重命名 | 181 文件 / 195534 个标识符，AST 结构校验 0 失败 |
| 拆块 | 13 个 >300KB 文件 → 2279 块，字节区间无重叠 |

## 事实边界（写进报告前必须区分）

- 字面量/lit 索引只证明「字面量存在」，不证明「会被执行」；tree-shaking 后的死分支同样被索引。
- `export_map` 只覆盖静态可见出口；`exports[k]=v` 动态挂载无法恢复，脚本会如实报 `dangling` 数。
- `rename_scope` 的 `vN` 只在单模块内稳定；新增一个变量会让其后编号整体平移。
- `split_inline_modules` 切的是语法块不是原模块，块内仍引用外层作用域变量。
- `format_tree --engine terser` 会规范化引号/转义/分号，此时 `--verify` 退化为「acorn 可解析」；
  `--engine lex`（默认）才会做逐 token 比对。

## Verify

- 同一树跑两遍，产物字节一致（脚本无时间戳、键序固定）。
- `format_tree --verify` 失败数应为 0；非 0 时该文件不落盘，且进程退出码为 1。
- `public_surface_diff` 里 `mangledChanged` 应远小于 `readableAdded + readableRemoved`；
  若反过来，说明导出名本身在变，需回头核对是否版本跨度过大（例如 5.8.x → 5.9.x 会有模块拆包）。
- `asar_manifest --compare` 抽 3 个 `same` 文件用 `shasum` 复核。
