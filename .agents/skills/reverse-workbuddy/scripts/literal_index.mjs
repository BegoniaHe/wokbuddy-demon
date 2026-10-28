// literal_index.mjs — 全树字符串字面量倒排索引，跨版本比对的第二把主键。
// 用法（仓库根执行）：
//   node literal_index.mjs --target "global/<ver>" [--dirs main,preload] [--out l.json]
//   node literal_index.mjs --compare "cn/<a>,global/<b>" [--only-added] [--out d.json]
//   node literal_index.mjs --target "..." --match "^X-"          # 查询模式
// 与 protocol_sweep.mjs 的分工：那个只认端点 / X-* 头族 / UA；这个给全量字面量，
// i18n key、配置键、模型名、deep-link scheme、错误码、MIME、事件名都从这一份出。
// 归一规则：模板串里的 `${…}` 一律替换成 `${}`，否则每版都变、无法比对。
// 事实边界：字面量存在 != 该字面量会被使用；死分支里的字面量同样会被索引。
import path from 'node:path';
import {
  parseArgs, list, walkFiles, readText, resolveTarget, writeJson, sortObject, fmtBytes, fail,
} from './lib/util.mjs';
import { lex, TOKEN } from './lib/jslex.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const dirs = list(args.dirs, ['main', 'preload', 'cli', 'tencent-docs']);
const minLen = Number(args['min-len'] ?? 4);
const maxLen = Number(args['max-len'] ?? 300);
const maxFiles = Number(args['max-files'] ?? 8);
const jsExt = /\.(js|mjs|cjs)$/;
// 资源/分包文件名里的哈希：`contexts-ZnvOifDf.js` -> `contexts-<hash>.js`
const ASSET_HASH = /-[A-Za-z0-9_-]{8}\.(js|mjs|cjs|css|png|svg|woff2?)$/;

const TAGGERS = [
  ['endpoint', /^\/(v[0-9]|console|activity|plugin|billing|auth|agenttool)\//],
  ['url', /^https?:\/\//],
  ['header', /^X-[A-Z][A-Za-z0-9-]*$/i],
  ['mime', /^[a-z]+\/[a-z0-9.+-]+$/i],
  ['scheme', /^[a-z][a-z0-9+.-]*:\/\//i],
  ['i18n', /^[a-zA-Z][\w.-]*\.[\w.-]+$/],
];

function tag(value) {
  const tags = [];
  for (const [name, re] of TAGGERS) if (re.test(value)) tags.push(name);
  return tags;
}

function normalize(raw) {
  let v = raw;
  // 模板串插值归一：`/v2/${id}/x` 与 `/v2/${uid}/x` 视为同一形态
  v = v.replace(/\$\{[^}]*\}/g, '${}');
  if (args['keep-asset-hash'] !== true) v = v.replace(ASSET_HASH, '-<hash>.$1');
  return v;
}

/** 收集一棵树的字面量：value -> { count, files: {rel: count} }。 */
function collect(sel) {
  const { asar } = resolveTarget(root, sel, asarName);
  const index = new Map();
  const stats = { files: 0, bytes: 0, literals: 0, skipped: 0 };
  const wanted = rel => dirs.some(d => rel === d || rel.startsWith(d + '/'));
  for (const file of walkFiles(asar)) {
    const rel = path.relative(asar, file).split(path.sep).join('/');
    if (!jsExt.test(rel) || !wanted(rel)) continue;
    const text = readText(file);
    if (text === null) continue;
    stats.files++;
    stats.bytes += text.length;
    lex(text, tok => {
      if (tok.t !== TOKEN.STR && tok.t !== TOKEN.TPL) return;
      const raw = tok.v;
      if (raw.length < minLen || raw.length > maxLen) return;
      if (raw.includes('\n')) {
        stats.skipped++;
        return;
      }
      const value = normalize(raw);
      stats.literals++;
      let entry = index.get(value);
      if (!entry) {
        entry = { count: 0, files: {} };
        index.set(value, entry);
      }
      entry.count++;
      if (!entry.files[rel]) entry.files[rel] = 0;
      entry.files[rel]++;
    });
  }
  return { target: sel, stats, index };
}

/** 只留前 N 个来源文件，产物不至于因为某个到处复用的短串而爆掉。 */
function compact(entry) {
  const files = Object.entries(entry.files).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  return {
    count: entry.count,
    files: Object.fromEntries(files.slice(0, maxFiles)),
    moreFiles: Math.max(0, files.length - maxFiles),
  };
}

function emit(sel) {
  const { stats, index } = collect(sel);
  const out = {};
  for (const [value, entry] of [...index.entries()].sort(([a], [b]) => a.localeCompare(b))) {
    const tags = tag(value);
    const row = compact(entry);
    if (tags.length) row.tags = tags;
    out[value] = row;
  }
  return {
    target: sel,
    stats: { ...stats, unique: index.size },
    literals: out,
  };
}

// 查询模式：不落盘，直接打印命中项
if (args.match) {
  const sel = String(args.target || fail('--match needs --target'));
  const re = new RegExp(String(args.match));
  const data = emit(sel);
  const hits = Object.entries(data.literals).filter(([v]) => re.test(v));
  console.log(JSON.stringify({ target: sel, match: args.match, hits: hits.length, literals: Object.fromEntries(hits) }, null, 1));
  process.exit(0);
}

let data;
if (args.compare) {
  const [a, b] = list(args.compare);
  if (!a || !b) fail('--compare needs "<left>,<right>"');
  const left = emit(a);
  const right = emit(b);
  const added = [];
  const removed = [];
  const changed = [];
  for (const [value, row] of Object.entries(right.literals)) {
    const l = left.literals[value];
    if (!l) added.push({ value, count: row.count, files: Object.keys(row.files) });
    else if (l.count !== row.count) changed.push({ value, count: [l.count, row.count] });
  }
  for (const [value, row] of Object.entries(left.literals)) {
    if (!(value in right.literals)) removed.push({ value, count: row.count, files: Object.keys(row.files) });
  }
  // --only-added：只留新增项，省掉 removed/changed 的噪音（本来就在头部注释里承诺过）
  const onlyAdded = args['only-added'] === true;
  data = {
    left: a,
    right: b,
    dirs,
    summary: {
      leftUnique: left.stats.unique,
      rightUnique: right.stats.unique,
      added: added.length,
      removed: onlyAdded ? 0 : removed.length,
      changed: onlyAdded ? 0 : changed.length,
    },
    added,
    removed: onlyAdded ? [] : removed,
    changed: onlyAdded ? [] : changed,
  };
} else if (args.target) {
  data = emit(String(args.target));
} else {
  fail('need --target, --compare or --match');
}

if (args.out) writeJson(args.out, data);
else console.log(JSON.stringify(data, null, 1));

if (data.summary) {
  const s = data.summary;
  console.error(`literals ${s.leftUnique} -> ${s.rightUnique}: +${s.added} / -${s.removed} / ~${s.changed}`);
} else {
  console.error(`${data.target}: ${data.stats.files} files (${fmtBytes(data.stats.bytes)}), ${data.stats.literals} literals, ${data.stats.unique} unique`);
}
