// export_map.mjs — 每个模块的「可读 API 面」，跨版本比对的主键。
// 用法（仓库根执行）：
//   node export_map.mjs --target "global/<ver>" [--out e.json] [--dirs main,preload]
//   node export_map.mjs --target "global/<ver>" --module main/transport.js
// 为什么这层最值钱：压缩只改本地标识符，exports 上的可读名（createConsumerInvoke、
// IPC_EVENT_CHANNEL…）不参与混淆，所以两版本同名模块的 API 面可直接 diff；
// mangled 名反而每版都变，不能当 join key。
// 事实边界：只记录静态可见的出口形态；动态挂载（exports[k]=v、defineProperties 循环）
// 无法静态恢复，落在 unresolved 里如实报数，不当成「没有导出」。
import path from 'node:path';
import { parseArgs, list, resolveTarget, writeJson, fmtBytes, fail } from './lib/util.mjs';
import { buildExportMap } from './lib/exportmap.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const dirs = list(args.dirs, ['main', 'preload', 'tencent-docs']);
const out = args.out;

function run(sel) {
  const { asar } = resolveTarget(root, sel, asarName);
  const { modules, stats } = buildExportMap(asar, dirs);
  // 只保留有信息量的字段，产物要能直接 diff 与 jq
  const compact = {};
  for (const [rel, m] of Object.entries(modules)) {
    const readable = Object.fromEntries(
      Object.entries(m.readable).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => [k, v.mangled]),
    );
    compact[rel] = {
      bytes: m.bytes,
      out: m.requires.filter(s => s.startsWith('.')).length,
      exports: m.ownExports.length,
      readableNames: Object.keys(readable).length,
      readable,
      facadeOf: Object.values(m.aliases),
      dangling: m.dangling.length ? m.dangling : undefined,
    };
  }
  return { target: sel, stats, modules: compact };
}

if (args.module) {
  const sel = String(args.target || fail('--module needs --target'));
  const data = run(sel);
  const key = String(args.module).replace(/^\.?\//, '');
  const found = data.modules[key];
  if (!found) fail(`module not in map: ${key}`);
  console.log(JSON.stringify({ target: sel, module: key, ...found }, null, 1));
  process.exit(0);
}

if (!args.target && !args.compare) fail('need --target or --compare');
const data = args.compare
  ? (() => {
    const [a, b] = list(args.compare);
    if (!a || !b) fail('--compare needs "<left>,<right>"');
    return { left: a, right: b, dirs, leftMap: run(a), rightMap: run(b) };
  })()
  : run(String(args.target));

if (out) writeJson(out, data);
else console.log(JSON.stringify(data, null, 1));

const s = data.stats || data.leftMap?.stats;
if (s) {
  console.error(`${data.target || `${data.left} vs ${data.right}`}: ${s.modules} modules, ${s.facades} facades, ${s.readablePairs} readable names, ${s.dangling} dangling`);
}

// 兜底：让 `--dirs` 的空值不会静默产出空表
if (!data.stats && !data.leftMap) fail('no data produced');
if (data.stats && data.stats.modules === 0) fail(`no modules matched dirs: ${dirs.join(',')} under ${path.join(root, String(args.target), asarName)}`);
