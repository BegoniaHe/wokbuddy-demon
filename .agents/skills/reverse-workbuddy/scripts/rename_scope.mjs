// rename_scope.mjs — 确定性的 α 重命名：把 `e,t,n,r…` 按「定义出现顺序」改写成 v1..vN。
// 用法（仓库根执行）：
//   npm i --prefix .tmp/tooling acorn eslint-scope --no-audit --no-fund
//   node rename_scope.mjs --target "global/<ver>" [--dirs main,preload] [--out-dir .tmp/readable-renamed]
// 为什么值得做：Rolldown 的字母分配会随代码增删整体重排，直接 diff 两个版本的压缩
// 产物几乎全是噪音；按定义顺序重编号后，diff 才能落到真正的改动上。
// 边界（务必按事实读）：
//   - 只在单个模块内稳定；跨模块同名 v3 没有共同含义。
//   - 定义顺序变化（新增一个变量）会让其后所有编号平移，diff 仍会红一片。
//   - 只重命名作用域绑定，不动属性名/字符串/exports 键，所以导出面完全不变。
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import {
  parseArgs, list, walkFiles, readText, resolveTarget, writeJson,
  countLines, fmtBytes, fail,
} from './lib/util.mjs';
import { formatByTokens } from './lib/format.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const dirs = list(args.dirs, ['main', 'preload']);
const outRoot = String(args['out-dir'] || '.tmp/readable-renamed');
const tools = String(args.tools || '.tmp/tooling');
const jsExt = /\.(js|mjs|cjs)$/;

/** 从 .tmp/tooling 这类临时目录加载分析器；不进 package.json。 */
function loadTool(name) {
  const require = createRequire(import.meta.url);
  for (const candidate of [name, path.resolve(tools, 'node_modules/' + name)]) {
    try {
      return require(candidate);
    } catch {
      // 换下一个候选
    }
  }
  return fail(`missing ${name}. run: npm i --prefix ${tools} acorn eslint-scope --no-audit --no-fund`);
}

const acorn = loadTool('acorn');
const eslintScope = loadTool('eslint-scope');

const PARSE_OPTS = { ecmaVersion: 2024, ranges: true, allowHashBang: true, allowAwaitOutsideFunction: true };

function parse(text) {
  for (const sourceType of ['script', 'module']) {
    try {
      return { ast: acorn.parse(text, { ...PARSE_OPTS, sourceType }), sourceType };
    } catch {
      // 换下一个 sourceType
    }
  }
  return null;
}

/**
 * 结构签名：把所有「绑定型标识符」的名字抹成 ID，但保留成员名与属性键。
 * 用它校验「除绑定名之外 AST 完全没变」——比 token 比对更贴近语义，
 * 也能发现「不小心把 obj.e 改成了 obj.v1」这种真正的错误。
 */
function structuralSignature(ast) {
  const norm = (node, parent, key) => {
    if (Array.isArray(node)) return node.map(n => norm(n, parent, key));
    // 字面量里的 BigInt / RegExp 不能直接进 JSON.stringify
    if (typeof node === 'bigint') return `bigint:${node}`;
    if (node instanceof RegExp) return `regex:${node.source}/${node.flags}`;
    if (!node || typeof node !== 'object') return node;
    if (node.type === 'Identifier') {
      const isMemberName = parent && parent.type === 'MemberExpression' && key === 'property' && !parent.computed;
      const isKeyName = parent && key === 'key' && !parent.computed
        && ['Property', 'MethodDefinition', 'PropertyDefinition'].includes(parent.type);
      return isMemberName || isKeyName ? node.name : 'ID';
    }
    const out = {};
    for (const k of Object.keys(node)) {
      if (k === 'start' || k === 'end' || k === 'loc' || k === 'range' || k === 'raw') continue;
      out[k] = k === 'shorthand' ? false : norm(node[k], node, k);
    }
    return out;
  };
  return JSON.stringify(norm(ast, null, null));
}

/** 不参与重命名的名字：全局对象与常见宿主注入。 */
const RESERVED = new Set([
  'module', 'exports', 'require', 'process', 'global', 'globalThis', 'Buffer', 'console',
  'setTimeout', 'setInterval', 'clearTimeout', 'clearInterval', 'setImmediate',
  '__dirname', '__filename', 'URL', 'URLSearchParams', 'TextEncoder', 'TextDecoder',
  'Promise', 'Object', 'Array', 'String', 'Number', 'Boolean', 'Symbol', 'JSON', 'Math',
  'Error', 'TypeError', 'Proxy', 'Reflect', 'WeakMap', 'WeakSet', 'Map', 'Set', 'Date',
  'RegExp', 'Function', 'arguments', 'undefined', 'NaN', 'Infinity',
]);

/** 遍历 AST，收集 shorthand 属性的标识符位置（重命名时必须展开成 `k: v`）。 */
function collectShorthand(ast) {
  const spots = new Map();
  const visit = node => {
    if (!node || typeof node !== 'object') return;
    if (Array.isArray(node)) {
      for (const n of node) visit(n);
      return;
    }
    if (node.type === 'Property' && node.shorthand) {
      let id = node.value;
      if (id && id.type === 'AssignmentPattern') id = id.left;
      if (id && id.type === 'Identifier' && id.range) spots.set(id.range[0], id.name);
    }
    for (const key of Object.keys(node)) {
      if (key === 'range' || key === 'loc') continue;
      const child = node[key];
      if (child && typeof child === 'object') visit(child);
    }
  };
  visit(ast);
  return spots;
}

/**
 * 按「作用域顺序 + 变量定义顺序」重编号。返回 { edits, renamed, remaining }。
 * edits 是按 range 从后往前排好的替换表。
 */
function buildRenames(text, ast, sourceType) {
  const manager = eslintScope.analyze(ast, {
    ecmaVersion: 2024,
    sourceType,
    optimistic: true,
    ignoreEval: true,
    impliedStrict: sourceType === 'module',
  });
  const shorthand = collectShorthand(ast);
  const edits = [];
  const renamed = [];
  let counter = 0;

  for (const scope of manager.scopes) {
    for (const variable of scope.variables) {
      if (RESERVED.has(variable.name)) continue;
      if (!/^[A-Za-z_$][A-Za-z0-9_$]{0,1}$/.test(variable.name)) continue;
      const newName = `v${++counter}`;
      renamed.push([variable.name, newName]);
      const apply = (id) => {
        if (!id || !id.range) return;
        const [start, end] = id.range;
        if (shorthand.has(start)) edits.push({ start, end, text: `${shorthand.get(start)}: ${newName}` });
        else edits.push({ start, end, text: newName });
      };
      for (const def of variable.identifiers) apply(def);
      for (const ref of variable.references) apply(ref.identifier);
    }
  }

  edits.sort((a, b) => b.start - a.start);
  // 同一个 range 只能被替换一次（定义与第一个引用可能指向同一处）
  const deduped = [];
  let lastStart = Infinity;
  for (const e of edits) {
    if (e.start === lastStart) continue;
    deduped.push(e);
    lastStart = e.start;
  }
  return { edits: deduped, renamed };
}

function applyEdits(text, edits) {
  let out = '';
  let cursor = 0;
  for (const e of [...edits].sort((a, b) => a.start - b.start)) {
    out += text.slice(cursor, e.start) + e.text;
    cursor = e.end;
  }
  return out + text.slice(cursor);
}

const sel = String(args.target || fail('need --target'));
const { asar } = resolveTarget(root, sel, asarName);
const [realm, ver] = sel.split('/');
const destRoot = path.join(outRoot, realm, ver);
const report = { target: sel, outDir: destRoot, files: [], failed: [] };
let totalIn = 0;
let totalOut = 0;

for (const file of walkFiles(asar)) {
  const rel = path.relative(asar, file).split(path.sep).join('/');
  if (!jsExt.test(rel) || !dirs.some(d => rel.startsWith(d + '/'))) continue;
  if (args.only && rel !== String(args.only).replace(/^\.?\//, '')) continue;
  const text = readText(file);
  if (text === null) continue;

  const parsed = parse(text);
  if (!parsed) {
    report.failed.push({ file: rel, reason: 'parse failed' });
    continue;
  }
  let result;
  try {
    result = buildRenames(text, parsed.ast, parsed.sourceType);
  } catch (e) {
    report.failed.push({ file: rel, reason: `scope analysis: ${String(e.message).slice(0, 160)}` });
    continue;
  }

  const rewritten = applyEdits(text, result.edits);
  // 校验 1：重命名后仍能被同一 sourceType 解析
  try {
    acorn.parse(rewritten, { ...PARSE_OPTS, sourceType: parsed.sourceType });
  } catch (e) {
    report.failed.push({ file: rel, reason: `reparse: ${String(e.message).slice(0, 160)}` });
    continue;
  }
  // 校验 2：除绑定名之外 AST 必须完全一致（属性键/成员名/字面量都不许变）
  const after = parse(rewritten);
  if (!after) {
    report.failed.push({ file: rel, reason: 'reparse failed' });
    continue;
  }
  if (structuralSignature(parsed.ast) !== structuralSignature(after.ast)) {
    report.failed.push({ file: rel, reason: 'structure changed beyond binding names' });
    continue;
  }

  const formatted = formatByTokens(rewritten, { indentUnit: args.tabs ? '\t' : '  ' });
  const dest = path.join(destRoot, rel);
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.writeFileSync(dest, formatted);
  totalIn += text.length;
  totalOut += formatted.length;
  report.files.push({ file: rel, renamed: result.renamed.length, bytes: [text.length, formatted.length], lines: countLines(formatted) });
}

report.summary = {
  files: report.files.length,
  failed: report.failed.length,
  renames: report.files.reduce((a, f) => a + f.renamed, 0),
  bytesIn: totalIn,
  bytesOut: totalOut,
};
writeJson(path.join(destRoot, '_rename-manifest.json'), report);

const s = report.summary;
console.error(`${sel}: ${s.files} files, ${s.renames} identifiers renamed, ${fmtBytes(s.bytesIn)} -> ${fmtBytes(s.bytesOut)}, failed ${s.failed}`);
if (s.failed > 0) process.exitCode = 1;
