// format_tree.mjs — 把压缩成单行的模块展开成可读排版，产物只落 .tmp/readable/。
// 用法（仓库根执行）：
//   node format_tree.mjs --target "global/<ver>" [--dirs main,preload] [--verify]
//   node format_tree.mjs --target "global/<ver>" --engine terser   # 需要 terser
// 引擎：
//   lex    （默认）本仓库自带的分词器排版，零依赖、可离线、模板串不会被切碎
//   terser 走 terser 的 beautify，排版更漂亮；缺模块时按提示临时装到 .tmp/tooling
// 只插换行/空格，不删分号、不改 token 顺序；`--verify` 会重分词比对 token 序列，
// 不一致就把该文件判失败并跳过，避免「看起来格式化好了、语义变了」。
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import {
  parseArgs, list, walkFiles, readText, resolveTarget, writeJson,
  countLines, fmtBytes, fail,
} from './lib/util.mjs';
import { formatByTokens, verifyTokens } from './lib/format.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const dirs = list(args.dirs, ['main', 'preload']);
const engine = String(args.engine || 'lex');
const outRoot = String(args['out-dir'] || '.tmp/readable');
const jsExt = /\.(js|mjs|cjs)$/;

/** terser beautify：模块缺省时报错并给出临时安装命令，不进 package.json。 */
function loadTerser() {
  const require = createRequire(import.meta.url);
  const tools = String(args.tools || '.tmp/tooling');
  for (const candidate of ['terser', path.resolve(tools, 'node_modules/terser')]) {
    try {
      return require(candidate);
    } catch {
      // 换下一个候选
    }
  }
  return fail(`terser not found. run: npm i --prefix ${tools} terser --no-audit --no-fund`);
}

/** 可选语法校验：有 acorn 就解析一遍，没有就如实标 skipped。 */
function loadAcorn() {
  const require = createRequire(import.meta.url);
  const tools = String(args.tools || '.tmp/tooling');
  for (const candidate of ['acorn', path.resolve(tools, 'node_modules/acorn')]) {
    try {
      return require(candidate);
    } catch {
      // 换下一个候选
    }
  }
  return null;
}

const terser = engine === 'terser' ? loadTerser() : null;
const acorn = args.verify && terser ? loadAcorn() : null;
const sel = String(args.target || fail('need --target'));
const { asar } = resolveTarget(root, sel, asarName);
const [realm, ver] = sel.split('/');
const destRoot = path.join(outRoot, realm, ver);
const report = { target: sel, engine, outDir: destRoot, files: [], failed: [] };
let totalIn = 0;
let totalOut = 0;

for (const file of walkFiles(asar)) {
  const rel = path.relative(asar, file).split(path.sep).join('/');
  if (!jsExt.test(rel) || !dirs.some(d => rel.startsWith(d + '/'))) continue;
  const text = readText(file);
  if (text === null) continue;

  let formatted;
  if (terser) {
    const res = await terser.minify(text, { compress: false, mangle: false, format: { beautify: true, comments: false } });
    if (res.error) {
      report.failed.push({ file: rel, reason: String(res.error.message || res.error).slice(0, 200) });
      continue;
    }
    formatted = res.code.endsWith('\n') ? res.code : res.code + '\n';
  } else {
    formatted = formatByTokens(text, { indentUnit: args.tabs ? '\t' : '  ', maxWidth: Number(args['max-width'] ?? 120) });
  }

  if (args.verify) {
    // lex 引擎是自己排版，必须逐字一致；terser 会规范化引号/转义/分号，逐字比对
    // 必然失败，退化成「能被 acorn 解析」，没有 acorn 就如实标 skipped。
    let v;
    if (!terser) {
      v = verifyTokens(text, formatted);
    } else if (acorn) {
      try {
        acorn.parse(formatted, {
          ecmaVersion: 2024,
          sourceType: rel.startsWith('cli/') ? 'module' : 'script',
          allowHashBang: true,
        });
        v = { ok: true };
      } catch (e) {
        v = { ok: false, reason: 'parse error: ' + String(e.message).slice(0, 120) };
      }
    } else {
      v = { ok: true, skipped: true };
      report.verifySkipped = (report.verifySkipped || 0) + 1;
    }
    if (!v.ok) {
      report.failed.push({ file: rel, reason: v.reason });
      continue;
    }
  }

  const dest = path.join(destRoot, rel);
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.writeFileSync(dest, formatted);
  totalIn += text.length;
  totalOut += formatted.length;
  report.files.push({ file: rel, bytes: [text.length, formatted.length], lines: [countLines(text), countLines(formatted)] });
}

report.summary = {
  files: report.files.length,
  failed: report.failed.length,
  verifySkipped: report.verifySkipped || 0,
  bytesIn: totalIn,
  bytesOut: totalOut,
  linesBefore: report.files.reduce((a, f) => a + f.lines[0], 0),
  linesAfter: report.files.reduce((a, f) => a + f.lines[1], 0),
};
writeJson(path.join(destRoot, '_format-manifest.json'), report);

const s = report.summary;
console.error(`${sel} [${engine}]: ${s.files} files, ${fmtBytes(s.bytesIn)} -> ${fmtBytes(s.bytesOut)}, lines ${s.linesBefore} -> ${s.linesAfter}, failed ${s.failed}${s.verifySkipped ? `, verify skipped ${s.verifySkipped}` : ''}`);
// 校验失败意味着有文件没落盘，退出码要能让调用方（和 CI）看见
if (s.failed > 0) process.exitCode = 1;
