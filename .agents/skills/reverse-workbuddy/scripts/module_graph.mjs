// module_graph.mjs — 模块依赖图：静态抽 `require()` / `import` 边，供跨版本定位改动传播面。
// 用法（仓库根执行）：
//   node module_graph.mjs --target "global/<ver>" [--out g.json]
//   node module_graph.mjs --compare "cn/<verA>,global/<verB>" [--out diff.json]
//   node module_graph.mjs --target "global/<ver>" --who-requires main/server.js
//   node module_graph.mjs --target "global/<ver>" --closure main/index.js [--depth 3]
// 事实边界：只记录「文件里出现了这条 specifier」，不推断运行时是否真的加载该分支。
// 边只在字符串字面量上用分词器识别，所以注释/普通字符串里出现的 require( 不会被误记。
import path from 'node:path';
import {
  parseArgs, list, walkFiles, readText, resolveTarget, listTargets,
  sortObject, writeJson, fmtBytes, fail,
} from './lib/util.mjs';
import { tokenize, significant, TOKEN } from './lib/jslex.mjs';

const args = parseArgs();
const root = args.root || '.tmp/workbuddy';
const asarName = args.asar || 'app-asar';
const out = args.out;
const JS_EXT = /\.(js|mjs|cjs)$/;

/** 从 token 流里抽 specifier；返回 [{ spec, kind }]。 */
function scanSpecifiers(text) {
  const tokens = significant(tokenize(text));
  const found = [];
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.t !== TOKEN.NAME) continue;
    const next = tokens[i + 1];
    // require("<spec>") / import("<spec>")
    if ((t.v === 'require' || t.v === 'import') && next && next.v === '(') {
      const spec = tokens[i + 2];
      const close = tokens[i + 3];
      if (spec && spec.t === TOKEN.STR && close && close.v === ')') {
        found.push({ spec: spec.v, kind: t.v === 'require' ? 'require' : 'dynamic-import' });
      }
      continue;
    }
    // 副作用 import"<spec>"（无 from 的极简形态）
    if (t.v === 'import' && next && next.t === TOKEN.STR) {
      found.push({ spec: next.v, kind: 'side-effect-import' });
      continue;
    }
  }
  // ESM 的 `... from "<spec>"`：import/export 两个方向都要，统一按 from 记账
  for (let i = 1; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.t !== TOKEN.STR) continue;
    const prev = tokens[i - 1];
    if (prev && prev.t === TOKEN.NAME && prev.v === 'from') {
      found.push({ spec: t.v, kind: 'esm-from' });
    }
  }
  return found;
}

/** 把 specifier 解析成树内相对路径；非相对路径（npm 包）记为 external。 */
function resolveSpec(fromRel, spec) {
  if (!spec.startsWith('.')) return { external: true, pkg: packageOf(spec) };
  const baseDir = path.posix.dirname(fromRel);
  return { external: false, resolved: path.posix.normalize(path.posix.join(baseDir, spec)) };
}

const RESOLVE_EXTS = ['', '.js', '.mjs', '.cjs', '.json', '.node'];

/** 压缩产物里 specifier 常省略扩展名，按候选扩展名对已存在的文件做一次收口。 */
function resolveExisting(resolved, fileSet) {
  for (const ext of RESOLVE_EXTS) {
    if (fileSet.has(resolved + ext)) return resolved + ext;
  }
  for (const ext of RESOLVE_EXTS) {
    const idx = `${resolved}/index${ext}`;
    if (ext && fileSet.has(idx)) return idx;
  }
  return null;
}

function packageOf(spec) {
  const parts = spec.split('/');
  return spec.startsWith('@') ? parts.slice(0, 2).join('/') : parts[0];
}

/** 扫一个版本目录，产出节点/边/外部依赖/悬空引用。 */
function scan(sel) {
  const { asar } = resolveTarget(root, sel, asarName);
  const nodes = {};
  const edgeCount = new Map();
  const external = {};
  const unresolved = new Set();
  const subtrees = {};
  const fileSet = new Set();
  let scanned = 0;
  let bytes = 0;

  for (const file of walkFiles(asar)) {
    fileSet.add(path.relative(asar, file).split(path.sep).join('/'));
  }

  for (const rel of fileSet) {
    if (!JS_EXT.test(rel)) continue;
    const text = readText(path.join(asar, rel));
    if (text === null) continue;
    scanned++;
    bytes += text.length;
    const subtree = rel.includes('/') ? rel.slice(0, rel.indexOf('/')) : '(root)';
    const st = (subtrees[subtree] ||= { files: 0, bytes: 0, edges: 0 });
    st.files++;
    st.bytes += text.length;
    const node = (nodes[rel] ||= { bytes: text.length, out: 0, in: 0, external: 0 });
    // 同一文件内同一 specifier 出现 N 次只记一条边，但保留次数便于看是否被复用
    const perTarget = new Map();
    for (const { spec, kind } of scanSpecifiers(text)) {
      const r = resolveSpec(rel, spec);
      if (r.external) {
        external[r.pkg] = (external[r.pkg] || 0) + 1;
        node.external++;
        continue;
      }
      const target = resolveExisting(r.resolved, fileSet);
      if (!target) {
        unresolved.add(r.resolved);
        continue;
      }
      if (!nodes[target]) nodes[target] = { bytes: null, out: 0, in: 0, external: 0, unread: true };
      perTarget.set(`${rel}\u0000${target}\u0000${kind}`, (perTarget.get(`${rel}\u0000${target}\u0000${kind}`) || 0) + 1);
    }
    for (const [key, count] of perTarget) {
      const [from, to] = key.split('\u0000');
      edgeCount.set(key, count);
      node.out++;
      nodes[to].in++;
      st.edges++;
    }
  }

  const edges = [...edgeCount.entries()]
    .map(([key, count]) => {
      const [from, to, kind] = key.split('\u0000');
      return { from, to, kind, count };
    })
    .sort((a, b) => (a.from === b.from ? a.to.localeCompare(b.to) : a.from.localeCompare(b.from)));

  const missing = [...unresolved].sort();
  const ranking = Object.entries(nodes)
    .filter(([, n]) => !n.unread)
    .sort((a, b) => b[1].in - a[1].in || b[1].bytes - a[1].bytes);

  return {
    target: sel,
    stats: {
      files: scanned,
      bytes,
      edges: edges.length,
      nodes: Object.keys(nodes).length,
      externalSpecifiers: Object.keys(external).length,
      missingTargets: missing.length,
      subtrees: sortObject(subtrees),
    },
    hubs: ranking.slice(0, Number(args.hubs || 25)).map(([p, n]) => ({ path: p, in: n.in, out: n.out, bytes: n.bytes })),
    orphans: Object.entries(nodes)
      .filter(([, n]) => !n.unread && n.in === 0 && n.out === 0).map(([p]) => p).sort(),
    missing,
    external: sortObject(external),
    nodes: sortObject(nodes),
    edges,
  };
}

if (args['who-requires']) {
  const sel = String(args.target || listTargets(root).at(-1));
  const goal = String(args['who-requires']).replace(/^\.?\//, '');
  const g = scan(sel);
  const hits = g.edges.filter(e => e.to === goal).map(e => ({ from: e.from, kind: e.kind, count: e.count }));
  console.log(JSON.stringify({ target: sel, module: goal, requiredBy: hits }, null, 1));
  process.exit(0);
}

if (args.closure) {
  const sel = String(args.target || listTargets(root).at(-1));
  const start = String(args.closure).replace(/^\.?\//, '');
  const depth = Number(args.depth || 2);
  const g = scan(sel);
  const seen = new Map([[start, 0]]);
  let frontier = [start];
  for (let d = 1; d <= depth; d++) {
    const next = [];
    for (const mod of frontier) {
      for (const e of g.edges) {
        if (e.from === mod && !seen.has(e.to)) {
          seen.set(e.to, d);
          next.push(e.to);
        }
      }
    }
    frontier = next;
    if (frontier.length === 0) break;
  }
  const modules = [...seen.entries()].sort((a, b) => a[1] - b[1] || a[0].localeCompare(b[0]));
  console.log(JSON.stringify({ target: sel, start, depth, modules: modules.map(([p, d]) => ({ path: p, depth: d })) }, null, 1));
  process.exit(0);
}

let data;
if (args.compare) {
  const [a, b] = list(args.compare);
  if (!a || !b) fail('--compare needs "<left>,<right>"');
  const left = scan(a);
  const right = scan(b);
  const edgeKey = e => `${e.from} -> ${e.to} (${e.kind})`;
  const lset = new Map(left.edges.map(e => [edgeKey(e), e.count]));
  const rset = new Map(right.edges.map(e => [edgeKey(e), e.count]));
  const added = [...rset.keys()].filter(k => !lset.has(k)).sort();
  const removed = [...lset.keys()].filter(k => !rset.has(k)).sort();
  data = {
    left: a, right: b,
    stats: { leftEdges: left.edges.length, rightEdges: right.edges.length, added: added.length, removed: removed.length },
    modulesAdded: Object.keys(right.nodes).filter(p => !left.nodes[p]).sort(),
    modulesRemoved: Object.keys(left.nodes).filter(p => !right.nodes[p]).sort(),
    edgesAdded: added,
    edgesRemoved: removed,
    edgeCountChanged: [...rset.entries()]
      .filter(([k, v]) => lset.has(k) && lset.get(k) !== v)
      .map(([k, v]) => ({ edge: k, count: [lset.get(k), v] })),
  };
} else if (args.target) {
  data = scan(String(args.target));
} else {
  data = { root, targets: listTargets(root).map(scan) };
}

if (out) writeJson(out, data);
else console.log(JSON.stringify(data, null, 1));

if (data.stats) {
  const s = data.stats;
  console.error(s.files === undefined
    ? `${data.left} vs ${data.right}: edges ${s.leftEdges} -> ${s.rightEdges}, added ${s.added}, removed ${s.removed}`
    : `${data.target}: ${s.files} js files (${fmtBytes(s.bytes)}), ${s.edges} edges, ${s.externalSpecifiers} external pkgs, ${s.missingTargets} missing targets`);
}
