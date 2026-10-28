// asar_manifest.mjs — 解包树的文件清单与指纹，是所有跨版本比对的地基。
// 用法（仓库根执行）：
//   node asar_manifest.mjs --target "global/5.8.4.41366630-73952edb" [--out m.json]
//   node asar_manifest.mjs --compare "cn/<verA>,global/<verB>" [--out diff.json]
// 输出每个文件的 { path, kind, bytes, lines, sha256 }；--compare 时按 path join 出
//   added / removed / changed（sha 不同）/ same，其余字段由下游脚本各自计算。
// 事实边界：只描述「树上有什么」，不解释文件内容；不做断言、不猜版本关系。
import fs from 'node:fs';
import path from 'node:path';
import {
  parseArgs, list, walkFiles, sha256Hex, readText, countLines,
  resolveTarget, listTargets, sortObject, writeJson, classify, fmtBytes, fail,
} from './lib/util.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const out = args.out;

/** 单个版本目录：遍历 asar 树，产出扁平文件表。 */
function manifest(sel) {
  const { asar } = resolveTarget(root, sel, asarName);
  const files = {};
  const byKind = {};
  let total = 0;
  for (const file of walkFiles(asar)) {
    const rel = path.relative(asar, file).split(path.sep).join('/');
    const kind = classify(rel);
    const stat = fs.statSync(file);
    const entry = { kind, bytes: stat.size, sha256: sha256Hex(fs.readFileSync(file)) };
    // 只有文本类才统计行数：单行压缩文件的「行数」是判断是否被压缩过的直接证据
    if (kind === 'js' || kind === 'ts' || kind === 'json' || kind === 'css' || kind === 'html') {
      const text = readText(file);
      entry.lines = text === null ? null : countLines(text);
    }
    files[rel] = entry;
    total += stat.size;
    const acc = (byKind[kind] ||= { count: 0, bytes: 0 });
    acc.count++;
    acc.bytes += stat.size;
  }
  return {
    target: sel,
    // 相对 root 而不是 cwd：否则同一棵树在不同目录下跑出不同字节，破坏「产物可复现」
    asar: path.relative(root, asar) || '.',
    totalBytes: total,
    summary: { files: Object.keys(files).length, bytes: total, byKind: sortObject(byKind) },
    files: sortObject(files),
  };
}

/** 两份清单按 path join。 */
function compare(leftSel, rightSel) {
  const left = manifest(leftSel);
  const right = manifest(rightSel);
  const paths = new Set([...Object.keys(left.files), ...Object.keys(right.files)]);
  const added = [];
  const removed = [];
  const changed = [];
  let same = 0;
  for (const p of [...paths].sort()) {
    const l = left.files[p];
    const r = right.files[p];
    if (!l) added.push({ path: p, bytes: r.bytes, kind: r.kind });
    else if (!r) removed.push({ path: p, bytes: l.bytes, kind: l.kind });
    else if (l.sha256 !== r.sha256) {
      changed.push({ path: p, kind: r.kind, bytes: [l.bytes, r.bytes], lines: [l.lines ?? null, r.lines ?? null] });
    } else same++;
  }
  return {
    left: leftSel,
    right: rightSel,
    summary: {
      leftFiles: left.summary.files,
      rightFiles: right.summary.files,
      same,
      added: added.length,
      removed: removed.length,
      changed: changed.length,
      leftBytes: left.totalBytes,
      rightBytes: right.totalBytes,
    },
    added,
    removed,
    changed,
  };
}

let data;
if (args.compare) {
  const [a, b] = list(args.compare);
  if (!a || !b) fail('--compare needs "<left>,<right>"');
  data = compare(a, b);
} else if (args.target) {
  data = manifest(String(args.target));
} else {
  data = { root, targets: listTargets(root).map(manifest) };
}

if (out) writeJson(out, data);
else console.log(JSON.stringify(data, null, 1));

if (data.summary) {
  const s = data.summary;
  const line = s.same === undefined
    ? `${data.target}: ${s.files} files, ${fmtBytes(s.bytes)}`
    : `${data.left} vs ${data.right}: same ${s.same} / added ${s.added} / removed ${s.removed} / changed ${s.changed}`;
  console.error(line);
}
