// public_surface_diff.mjs — 两版本「可读 API 面」的结构化 diff。
// 用法（仓库根执行）：
//   node public_surface_diff.mjs --compare "cn/<verA>,global/<verB>" [--out d.json]
//   node public_surface_diff.mjs --compare "..." --dirs main,preload
// 产出四类结论，全部可机械核对：
//   modulesAdded / modulesRemoved        —— 模块文件增删
//   readableAdded / readableRemoved      —— 同一模块内 API 名增删
//   mangledChanged                       —— 同 API 名换了内部 mangled 名（正常，多为实现变动）
//   moved                                —— 同名 API 从 A 模块搬到 B 模块（要点名的形态变更）
//   danglingGained                       —— facade 指向的模块消失（通常是改名/合并的信号）
// 事实边界：这些都是「静态可见的出口形态」差异；不推断运行时行为变化。
import { parseArgs, list, resolveTarget, writeJson, fail } from './lib/util.mjs';
import { buildExportMap } from './lib/exportmap.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const dirs = list(args.dirs, ['main', 'preload', 'tencent-docs']);
const out = args.out;

const [leftSel, rightSel] = list(args.compare);
if (!leftSel || !rightSel) fail('need --compare "<left>,<right>"');

function load(sel) {
  const { asar } = resolveTarget(root, sel, asarName);
  return buildExportMap(asar, dirs);
}

const left = load(leftSel);
const right = load(rightSel);

const leftModules = new Set(Object.keys(left.modules));
const rightModules = new Set(Object.keys(right.modules));

const modulesAdded = [...rightModules].filter(m => !leftModules.has(m)).sort();
const modulesRemoved = [...leftModules].filter(m => !rightModules.has(m)).sort();
const common = [...leftModules].filter(m => rightModules.has(m)).sort();

const readableAdded = [];
const readableRemoved = [];
const mangledChanged = [];
const moved = [];
const danglingGained = [];

// 全局可读名索引（两侧全量），用来识别「同一 API 换了个模块住」
function flatten(side) {
  const map = new Map();
  for (const [m, info] of Object.entries(side.modules)) {
    for (const [name, entry] of Object.entries(info.readable)) {
      if (!map.has(name)) map.set(name, []);
      map.get(name).push({ module: m, mangled: entry.mangled });
    }
  }
  return map;
}
const flatLeft = flatten(left);
const flatRight = flatten(right);

for (const m of common) {
  const l = left.modules[m].readable;
  const r = right.modules[m].readable;
  for (const name of Object.keys(r)) {
    if (!(name in l)) readableAdded.push({ module: m, name, mangled: r[name].mangled });
    else if (l[name].mangled !== r[name].mangled) {
      mangledChanged.push({ module: m, name, mangled: [l[name].mangled, r[name].mangled] });
    }
  }
  for (const name of Object.keys(l)) {
    if (!(name in r)) readableRemoved.push({ module: m, name, mangled: l[name].mangled });
  }
  const ld = new Set((left.modules[m].dangling || []).map(d => d.name));
  for (const d of right.modules[m].dangling || []) {
    if (!ld.has(d.name)) danglingGained.push({ module: m, name: d.name, alias: d.alias, prop: d.prop });
  }
}

// 同一个 API 名在两版都能找到，但承载它的模块变了 => moved。
// 只有「两侧各只出现在一个模块」时才认定迁移；多个同名副本（打包进来的第三方库）
// 落到 movedAmbiguous，避免把 vendor 复制品的增删读成搬家。
const movedAmbiguous = [];
for (const [name, lhits] of flatLeft) {
  const rhits = flatRight.get(name) || [];
  if (lhits.length === 0 || rhits.length === 0) continue;
  const lmods = new Set(lhits.map(h => h.module));
  const rmods = new Set(rhits.map(h => h.module));
  const shared = [...lmods].filter(m => rmods.has(m));
  if (shared.length > 0) continue;
  if (lmods.size === 1 && rmods.size === 1) moved.push({ name, from: [...lmods][0], to: [...rmods][0] });
  else movedAmbiguous.push({ name, from: [...lmods].sort(), to: [...rmods].sort() });
}

const data = {
  left: leftSel,
  right: rightSel,
  dirs,
  summary: {
    modules: [leftModules.size, rightModules.size],
    modulesAdded: modulesAdded.length,
    modulesRemoved: modulesRemoved.length,
    readableAdded: readableAdded.length,
    readableRemoved: readableRemoved.length,
    mangledChanged: mangledChanged.length,
    moved: moved.length,
    movedAmbiguous: movedAmbiguous.length,
    danglingGained: danglingGained.length,
  },
  modulesAdded,
  modulesRemoved,
  readableAdded,
  readableRemoved,
  mangledChanged,
  moved,
  movedAmbiguous,
  danglingGained,
};

if (out) writeJson(out, data);
else console.log(JSON.stringify(data, null, 1));

const s = data.summary;
console.error(
  `${leftSel} -> ${rightSel}: modules ${s.modules[0]}->${s.modules[1]} `
  + `(+${s.modulesAdded}/-${s.modulesRemoved}), readable names +${s.readableAdded}/-${s.readableRemoved}, `
  + `mangled changed ${s.mangledChanged}, moved ${s.moved} (ambiguous ${s.movedAmbiguous}), dangling +${s.danglingGained}`,
);
