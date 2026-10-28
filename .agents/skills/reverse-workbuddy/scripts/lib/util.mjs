// util.mjs — 机械整理脚本的公共工具：参数解析、目录遍历、哈希、JSON 落盘。
// 约定（与 scripts/ 下既有脚本一致）：零 npm 依赖、只读输入树、产物只落 .tmp/。
// 所有 CLI 都走 `--key value` / `--flag` 形态，未知参数不报错（脚本自取自用）。
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';

/** 解析 `--key value` / `--flag`，位置参数落到 `_`。 */
export function parseArgs(argv = process.argv.slice(2)) {
  const out = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) {
      out._.push(a);
      continue;
    }
    const key = a.slice(2);
    const next = argv[i + 1];
    if (next === undefined || next.startsWith('--')) out[key] = true;
    else {
      out[key] = next;
      i++;
    }
  }
  return out;
}

/** 逗号分隔的多值参数 -> 数组；缺省给默认值。 */
export function list(value, dflt = []) {
  if (value === undefined || value === true) return dflt.slice();
  return String(value).split(',').map(s => s.trim()).filter(Boolean);
}

/** 递归遍历目录下的文件（跳过符号链接与 .DS_Store）。 */
export function* walkFiles(dir) {
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return;
  }
  for (const e of entries) {
    if (e.name === '.DS_Store') continue;
    const p = path.join(dir, e.name);
    if (e.isDirectory()) yield* walkFiles(p);
    else if (e.isFile()) yield p;
  }
}

export function sha256Hex(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex');
}

/** 读文本；读不了（权限/链接）返回 null，由调用方计 missing。 */
export function readText(file) {
  try {
    return fs.readFileSync(file, 'utf8');
  } catch {
    return null;
  }
}

/** 统计换行数；单行压缩文件返回 1。 */
export function countLines(text) {
  if (text.length === 0) return 0;
  let n = 1;
  for (let i = text.indexOf('\n'); i !== -1; i = text.indexOf('\n', i + 1)) n++;
  return n;
}

/** 稳定的键序（顶层对象按键名排序），让产物可直接 diff。 */
export function sortObject(obj) {
  return Object.fromEntries(Object.keys(obj).sort().map(k => [k, obj[k]]));
}

/** 写 JSON 到文件（父目录自动建），同时回显一句到 stderr。 */
export function writeJson(file, data) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, JSON.stringify(data, null, 1) + '\n');
  console.error(`written: ${file}`);
}

/** 读 JSON；不存在或坏掉时抛错（调用方决定是否吞）。 */
export function readJson(file) {
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

/** `.tmp/workbuddy` + `cn/<ver>` -> 绝对路径；不存在直接报错，避免静默空跑。 */
export function resolveTarget(root, sel, asarName = 'app-asar') {
  const dir = path.join(root, sel);
  const asar = path.join(dir, asarName);
  if (!fs.existsSync(asar)) throw new Error(`asar tree not found: ${asar}`);
  return { dir, asar };
}

/** 枚举 root 下的 `<realm>/<ver>` 组合，按名字排序。 */
export function listTargets(root) {
  const out = [];
  for (const realm of fs.readdirSync(root, { withFileTypes: true })) {
    if (!realm.isDirectory()) continue;
    for (const ver of fs.readdirSync(path.join(root, realm.name), { withFileTypes: true })) {
      if (!ver.isDirectory()) continue;
      out.push(`${realm.name}/${ver.name}`);
    }
  }
  return out.sort();
}

/** `cn/<ver>` 这类选择器 -> 报告里用的短标签。 */
export function targetLabel(sel) {
  return sel.replace(/\/app-asar(-[a-z0-9-]+)?$/, '');
}

const KIND_BY_EXT = new Map([
  ['.js', 'js'], ['.mjs', 'js'], ['.cjs', 'js'], ['.jsx', 'js'],
  ['.ts', 'ts'], ['.tsx', 'ts'], ['.mts', 'ts'], ['.cts', 'ts'],
  ['.json', 'json'], ['.css', 'css'], ['.html', 'html'], ['.svg', 'svg'],
  ['.node', 'native'], ['.dll', 'native'], ['.so', 'native'], ['.dylib', 'native'],
  ['.wasm', 'wasm'], ['.map', 'map'],
]);

/** 按扩展名归一成粗粒度类型，未知类型统一 `asset`。 */
export function classify(relpath) {
  const ext = path.extname(relpath).toLowerCase();
  return KIND_BY_EXT.get(ext) || 'asset';
}

/** 千分位可读字节数，仅用于 stderr 摘要。 */
export function fmtBytes(n) {
  const units = ['B', 'KB', 'MB', 'GB'];
  let v = n;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v < 10 && u > 0 ? v.toFixed(1) : Math.round(v)}${units[u]}`;
}

/** 统一失败出口：打印原因后退出 2，避免半截产物。 */
export function fail(msg) {
  console.error(`error: ${msg}`);
  process.exit(2);
}

/** 数组按 key 函数分组。 */
export function groupBy(items, keyOf) {
  const map = new Map();
  for (const item of items) {
    const k = keyOf(item);
    if (!map.has(k)) map.set(k, []);
    map.get(k).push(item);
  }
  return map;
}
