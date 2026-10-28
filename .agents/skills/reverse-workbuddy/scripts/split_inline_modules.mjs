// split_inline_modules.mjs — 把大聚合文件里内联的模块工厂拆成独立小块，便于逐个阅读。
// 用法（仓库根执行）：
//   node split_inline_modules.mjs --target "global/<ver>" [--min-bytes 300000] [--out-dir .tmp/split]
//   node split_inline_modules.mjs --target "global/<ver>" --files main/dist.js,main/module-base.js
// 只识别 Rolldown 的运行时工厂调用（`<runtimeAlias>.<factory>((()=>{…}))`），因为
// 那是唯一可靠的「模块边界」标记：实测 main/dist.js 有 105 处、module-base.js 66 处，
// 而 server.js / tar.js 是单体模块，切不动也不该切。
// 事实边界：切出来的是「语法块」而不是「原模块」；块内仍共用外层作用域变量，
// 单独阅读时需要配合原文件的行号定位。
import fs from 'node:fs';
import path from 'node:path';
import {
  parseArgs, list, walkFiles, readText, resolveTarget, writeJson, fmtBytes, fail, countLines,
} from './lib/util.mjs';
import { tokenize, significant, TOKEN } from './lib/jslex.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const dirs = list(args.dirs, ['main', 'preload']);
const outRoot = String(args['out-dir'] || '.tmp/split');
const minBytes = Number(args['min-bytes'] ?? 300000);
const jsExt = /\.(js|mjs|cjs)$/;

function matchParen(tokens, open) {
  let depth = 0;
  for (let i = open; i < tokens.length; i++) {
    if (tokens[i].t !== TOKEN.PUNCT) continue;
    const v = tokens[i].v;
    if (v === '(' || v === '[' || v === '{') depth++;
    else if (v === ')' || v === ']' || v === '}') {
      depth--;
      if (depth === 0) return i;
    }
  }
  return -1;
}

/** 找出绑定到 rolldown-runtime 的别名变量（`e=require("./rolldown-runtime.js")`）。 */
function runtimeAliases(tokens) {
  const aliases = new Set();
  for (let i = 2; i < tokens.length - 3; i++) {
    if (tokens[i].v !== 'require' || tokens[i + 1].v !== '(') continue;
    if (tokens[i + 2].t !== TOKEN.STR || !tokens[i + 2].v.includes('rolldown-runtime')) continue;
    const alias = tokens[i - 2];
    if (alias.t === TOKEN.NAME && tokens[i - 1].v === '=') aliases.add(alias.v);
  }
  return aliases;
}

/** 抽一个块里的前几个字符串字面量，作为「这块大概是干什么的」线索。 */
function hints(tokens, from, to, max = 4) {
  const out = [];
  for (let i = from; i <= to && out.length < max; i++) {
    const t = tokens[i];
    if (t.t !== TOKEN.STR && t.t !== TOKEN.TPL) continue;
    if (t.v.length < 6 || t.v.length > 80) continue;
    if (/^\.\.?\//.test(t.v)) continue;
    out.push(t.v.slice(0, 80));
  }
  return out;
}

/** 从 rolldown-runtime.js 里读出「模块工厂」的导出名，不写死 n/t。 */
function factoryNames(asar) {
  const runtimeRel = 'main/rolldown-runtime.js';
  const text = readText(path.join(asar, runtimeRel));
  const fallback = new Set(['n', 't']);
  if (text === null) return fallback;
  const tokens = significant(tokenize(text));
  const found = new Set();
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i].v !== 'defineProperty' || tokens[i + 2]?.v !== 'exports' || tokens[i + 4]?.t !== TOKEN.STR) continue;
    const close = matchParen(tokens, i + 1);
    if (close === -1) continue;
    let returned = null;
    for (let k = i + 5; k < close; k++) if (tokens[k].v === 'return' && tokens[k + 1]?.t === TOKEN.NAME) returned = tokens[k + 1].v;
    if (returned === '__esmMin' || returned === '__commonJSMin') found.add(tokens[i + 4].v);
  }
  return found.size ? found : fallback;
}

function splitFile(asar, rel, text, destDir, factories) {
  const tokens = significant(tokenize(text));
  const aliases = runtimeAliases(tokens);
  if (aliases.size === 0) return { file: rel, chunks: [], reason: 'no rolldown-runtime alias' };

  const chunks = [];
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.t !== TOKEN.NAME || !aliases.has(t.v)) continue;
    if (tokens[i + 1]?.v !== '.' || tokens[i + 2]?.t !== TOKEN.NAME || tokens[i + 3]?.v !== '(') continue;
    // 模块工厂在压缩产物里一定出现在赋值右侧：`var x,y=e.n((()=>{…}))`。
    // 卡住这一点，可以排掉「局部变量恰好也叫 e」造成的假块（实测能把误判压掉一半）
    if (tokens[i - 1]?.v !== '=') continue;
    const open = i + 3;
    // 只认模块工厂：`alias.n((()=>{…}))` / `alias.t((function(){…}))`
    // alias 是压缩后的短名，函数参数也常叫 e，所以必须同时卡住工厂名与 thunk 形态
    if (!factories.has(tokens[i + 2].v)) continue;
    const argStart = tokens[i + 4];
    if (!argStart) continue;
    const isThunk = argStart.v === '(' || argStart.v === 'function' || argStart.v === 'async';
    if (!isThunk) continue;
    const close = matchParen(tokens, open);
    if (close === -1) continue;
    const start = t.start;
    const end = tokens[close].end;
    const index = chunks.length + 1;
    const name = `chunk-${String(index).padStart(3, '0')}.js`;
    const head = hints(tokens, i, close);
    const body = text.slice(start, end);
    const header = [
      `// ${rel} 字节 [${start}, ${end})  factory=${t.v}.${tokens[i + 2].v}`,
      head.length ? `// 线索: ${head.join(' | ')}` : '// 线索: (块内没有可辨认的字面量)',
      '',
    ].join('\n');
    fs.writeFileSync(path.join(destDir, name), header + body + '\n');
    chunks.push({ name, start, end, factory: `${t.v}.${tokens[i + 2].v}`, bytes: end - start, hints: head });
    if (close > i) i = close;
  }
  return { file: rel, chunks };
}

const sel = String(args.target || fail('need --target'));
const { asar } = resolveTarget(root, sel, asarName);
const [realm, ver] = sel.split('/');
const destRoot = path.join(outRoot, realm, ver);
const only = list(args.files, []);
const factories = factoryNames(asar);
if (factories.size) console.error(`module factories: ${[...factories].join(',')}`);
const report = { target: sel, outDir: destRoot, minBytes, files: [] };
let totalChunks = 0;
let totalBytes = 0;

for (const file of walkFiles(asar)) {
  const rel = path.relative(asar, file).split(path.sep).join('/');
  if (!jsExt.test(rel) || !dirs.some(d => rel.startsWith(d + '/'))) continue;
  if (only.length && !only.includes(rel)) continue;
  const stat = fs.statSync(file);
  if (!only.length && stat.size < minBytes) continue;
  const text = readText(file);
  if (text === null) continue;
  const destDir = path.join(destRoot, path.dirname(rel).replace(/[^A-Za-z0-9/_-]/g, '_'), path.basename(rel, '.js'));
  // 先清空该文件的块目录：重跑时块数变少会留下上一轮的 chunk-NNN.js，
  // 产物就不再「同一棵树跑两遍字节一致」。
  fs.rmSync(destDir, { recursive: true, force: true });
  fs.mkdirSync(destDir, { recursive: true });
  const result = splitFile(asar, rel, text, destDir, factories);
  report.files.push({ ...result, bytes: text.length, lines: countLines(text), outDir: path.relative(outRoot, destDir) });
  totalChunks += result.chunks.length;
  totalBytes += text.length;
}

report.summary = { files: report.files.length, chunks: totalChunks, bytes: totalBytes };
writeJson(path.join(destRoot, '_split-manifest.json'), report);

const s = report.summary;
console.error(`${sel}: ${s.files} files (${fmtBytes(s.bytes)}) -> ${s.chunks} chunks`);
const empty = report.files.filter(f => (f.chunks?.length ?? 0) === 0);
if (empty.length) console.error(`文件未拆出块（单体模块或没有运行时工厂）：${empty.map(f => f.file).join(', ')}`);
