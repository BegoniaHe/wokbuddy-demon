// protocol_sweep.mjs — 协议面粗扫：从解包的 app-asar 树提取
//   product.json 事实 / 端点路径字面量 / X-* 头族 / UA 模板（变量插值归一）
// 用法（仓库根执行）：
//   node protocol_sweep.mjs --root .tmp/workbuddy [--asar app-asar]
//     [--out <file.json>] [--compare "<realm/verA>,<realm/verB>"]
// 无 --compare：逐 realm/version 全量汇总（含计数）；有则输出端点集合差
//   added=right 独有 / removed=left 独有 / changed=同名计数不同。
// 匹配是字面量扫描：模板串（`/v2/${x}/y`）只报告能匹配的前缀事实，
// 调用上下文另用 rg 追；结论层级按 SKILL.md「事实分层」标注。
import fs from 'node:fs';
import path from 'node:path';

const args = process.argv.slice(2);
function opt(name, dflt) {
  const i = args.indexOf('--' + name);
  if (i === -1) return dflt;
  return args[i + 1];
}

const root = opt('root', '.tmp/workbuddy');
const sar = opt('asar', 'app-asar'); // 每个版本目录下的 ASAR 树名
const out = opt('out');
const compare = opt('compare');

// 只扫含协议面字面量的目录；preload/renderer 全量，i18n 语言包噪音大单独计数
const SCAN_DIRS = ['main', 'cli', 'renderer', 'preload'];
// 出站端点：/v1|v2|v3/...、/console/...、/activity/...、/plugin/...、/billing/...、
// /auth/...、/agenttool/...；模板串（`${base}/v2/...`）由下一条正则补扫。
const EP_RE = /(?:^|[^\w./-])(["'`])(\/(?:v[0-9]|console|activity|plugin|billing|auth|agenttool)\/[A-Za-z0-9/_.$-]*)\1/g;
// 模板串端点：`${chatBase}/v2/images/generations` 这类字面量前缀
const EP_TPL_RE = /`\$\{[^}]+\}(\/(?:v[0-9]|agenttool)\/[A-Za-z0-9/_.$-]*)/g;
const HDR_RE = /["'`](X-[A-Z][A-Za-z0-9-]*)["'`]/g;
// UA 形态只记「模板痕迹」（变量插值一律归一成 ${…}），真实版本号不在 JS 里
const UA_RE = /\b((?:WorkBuddy|CLI)\/(\$?\{[^}]*\}|[A-Za-z0-9._-]{1,24}))/g;
const NORM_UA = s => s.replace(/\$\{[^}]*\}/g, '${…}');

function* walk(dir) {
  let entries;
  try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
  for (const e of entries) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) yield* walk(p);
    else if (e.isFile() && (e.name.endsWith('.js') || e.name.endsWith('.mjs') || e.name.endsWith('.cjs'))) yield p;
  }
}

function bump(map, key) { map[key] = (map[key] || 0) + 1; }

function sweep(target) {
  const res = { product: null, endpoints: {}, headers: {}, ua: {}, files: 0 };
  const pj = path.join(target, sar, 'cli', 'product.json');
  if (fs.existsSync(pj)) {
    try {
      const d = JSON.parse(fs.readFileSync(pj, 'utf8'));
      const auth = d.authentication || {};
      res.product = {
        productName: d.productName ?? null,
        endpoint: d.endpoint ?? null,
        platform: d.platform ?? null,
        authId: auth.id ?? null,
        authType: auth.type ?? null,
        officialEndpoints: d.officialEndpoints ?? null,
      };
    } catch (e) { res.product = { error: String(e && e.message || e) }; }
  }
  for (const sub of SCAN_DIRS) {
    for (const file of walk(path.join(target, sar, sub))) {
      res.files++;
      let text;
      try { text = fs.readFileSync(file, 'utf8'); } catch { continue; }
      for (const m of text.matchAll(EP_RE)) bump(res.endpoints, m[2]);
      for (const m of text.matchAll(EP_TPL_RE)) bump(res.endpoints, m[1]);
      if (sub !== 'renderer') for (const m of text.matchAll(HDR_RE)) bump(res.headers, m[1]);
      for (const m of text.matchAll(UA_RE)) bump(res.ua, NORM_UA(m[1]));
    }
  }
  return res;
}

function sorted(obj) {
  return Object.fromEntries(Object.keys(obj).sort().map(k => [k, obj[k]]));
}

function select(sel) {
  const p = path.join(root, sel);
  if (!fs.existsSync(p)) throw new Error(`target not found: ${p}`);
  return sweep(p);
}

let data;
if (compare) {
  const [a, b] = compare.split(',').map(s => s.trim());
  const left = select(a), right = select(b);
  const keys = new Set([...Object.keys(left.endpoints), ...Object.keys(right.endpoints)]);
  const added = [], removed = [], changed = [];
  for (const k of keys) {
    if (!(k in left.endpoints)) added.push(k);
    else if (!(k in right.endpoints)) removed.push(k);
    else if (left.endpoints[k] !== right.endpoints[k]) changed.push([k, left.endpoints[k], right.endpoints[k]]);
  }
  data = { left: a, right: b, added, removed, changed };
} else {
  let realms = [];
  try {
    realms = fs.readdirSync(root, { withFileTypes: true })
      .filter(d => d.isDirectory())
      .map(d => d.name);
  } catch { throw new Error(`root not found: ${root}`); }
  data = {};
  for (const realm of realms.sort()) {
    const versions = fs.readdirSync(path.join(root, realm), { withFileTypes: true })
      .filter(d => d.isDirectory()).map(d => d.name).sort();
    for (const v of versions) {
      data[realm + '/' + v] = (() => { const r = select(realm + '/' + v); r.endpoints = sorted(r.endpoints); r.headers = sorted(r.headers); r.ua = sorted(r.ua); return r; })();
    }
  }
}

const json = JSON.stringify(data, null, 1);
if (out) { fs.writeFileSync(out, json + '\n'); console.error(`written: ${out}`); }
console.log(json);
