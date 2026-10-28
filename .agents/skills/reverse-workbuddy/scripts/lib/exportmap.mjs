// exportmap.mjs — 恢复「可读导出名 <-> 内部 mangled 名」的映射。
// 压缩只改本地标识符，exports 上的名字有两类：
//   1) mangled：Object.defineProperty(exports,"t",{…get:()=>本地名})  —— 模块自身出口
//   2) 可读：  exports.createConsumerInvoke = e.d                     —— barrel/facade 文件
//                （e 是本文件里 require("./transport.js") 的别名）
// 把 2) 的目标 alias 解析回 1) 所在模块，就得到「某模块的可读 API 面」。
// 这张表天然跨版本可比：可读名不会被混淆，mangled 名才会。
import path from 'node:path';
import { walkFiles, readText } from './util.mjs';
import { tokenize, significant, TOKEN } from './jslex.mjs';

const JS_EXT = /\.(js|mjs|cjs)$/;
const RESOLVE_EXTS = ['', '.js', '.mjs', '.cjs', '.json', '.node'];

/** 压缩后的本地名形态：1~2 个字符，`e`/`t`/`_a` 这类；反过来就是可读名。 */
export function isMangledName(name) {
  return /^[A-Za-z_$][A-Za-z0-9_$]?$/.test(name);
}

/** 从 `(` 起找到配对的 `)`，返回其下标（找不到返回 -1）。 */
function matchParen(tokens, open) {
  let depth = 0;
  for (let i = open; i < tokens.length; i++) {
    const v = tokens[i].v;
    if (tokens[i].t !== TOKEN.PUNCT) continue;
    if (v === '(' || v === '[' || v === '{') depth++;
    else if (v === ')' || v === ']' || v === '}') {
      depth--;
      if (depth === 0) return i;
    }
  }
  return -1;
}

/**
 * 分析单个模块文件：抽 require 别名 + exports 出口。
 * 返回 { requires, aliases, exports, cjsDefault }
 *   exports: [{ name, kind, local? , ref? : { alias, prop } }]
 *   kind: 'define-get' | 'assign'
 */
export function analyzeModule(text) {
  const tokens = significant(tokenize(text));
  const aliases = {};
  const requires = [];

  for (let i = 2; i < tokens.length - 3; i++) {
    const t = tokens[i];
    if (t.t !== TOKEN.NAME || t.v !== 'require') continue;
    if (tokens[i - 1].v !== '=' || tokens[i + 1].v !== '(') continue;
    const alias = tokens[i - 2];
    const spec = tokens[i + 2];
    if (alias.t !== TOKEN.NAME) continue;
    if (spec.t !== TOKEN.STR || tokens[i + 3].v !== ')') continue;
    aliases[alias.v] = spec.v;
    requires.push({ spec: spec.v, alias: alias.v });
  }

  const exported = [];
  let cjsDefault = null;
  const pushOnce = entry => {
    if (!exported.some(e => e.name === entry.name && e.kind === entry.kind)) exported.push(entry);
  };

  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];

    // Object.defineProperty(exports, "<name>", { …get: () => <local> })
    if (t.t === TOKEN.NAME && t.v === 'defineProperty' && tokens[i - 1]?.v === '.' && tokens[i + 2]?.v === 'exports'
      && tokens[i + 3]?.v === ',' && tokens[i + 4]?.t === TOKEN.STR) {
      const open = i + 1;
      const close = matchParen(tokens, open);
      if (close === -1) continue;
      const name = tokens[i + 4].v;
      let local = null;
      for (let k = i + 5; k < close; k++) {
        if (tokens[k].t === TOKEN.NAME && tokens[k].v === 'return' && tokens[k + 1]?.t === TOKEN.NAME) {
          local = tokens[k + 1].v;
        }
      }
      pushOnce(local ? { name, kind: 'define-get', local } : { name, kind: 'define-get' });
      continue;
    }

    // exports.<name> = <rhs>
    if (t.t === TOKEN.NAME && t.v === 'exports' && tokens[i + 1]?.v === '.' && tokens[i + 2]?.t === TOKEN.NAME
      && tokens[i + 3]?.v === '=' && tokens[i + 4]) {
      const name = tokens[i + 2].v;
      const rhs = tokens[i + 4];
      if (rhs.t === TOKEN.NAME && tokens[i + 5]?.v === '.' && tokens[i + 6]?.t === TOKEN.NAME) {
        pushOnce({ name, kind: 'assign', ref: { alias: rhs.v, prop: tokens[i + 6].v } });
      } else if (rhs.t === TOKEN.NAME) {
        pushOnce({ name, kind: 'assign', local: rhs.v });
      }
      continue;
    }

    // module.exports = …
    if (t.t === TOKEN.NAME && t.v === 'module' && tokens[i + 1]?.v === '.' && tokens[i + 2]?.v === 'exports'
      && tokens[i + 3]?.v === '=') {
      cjsDefault = tokens[i + 4]?.v ?? null;
    }
  }

  return { requires, aliases, exports: exported, cjsDefault };
}

function resolveSpec(fromRel, spec) {
  if (!spec.startsWith('.')) return null;
  return path.posix.normalize(path.posix.join(path.posix.dirname(fromRel), spec));
}

function resolveExisting(resolved, fileSet) {
  for (const ext of RESOLVE_EXTS) if (fileSet.has(resolved + ext)) return resolved + ext;
  for (const ext of RESOLVE_EXTS) {
    if (!ext) continue;
    const idx = `${resolved}/index${ext}`;
    if (fileSet.has(idx)) return idx;
  }
  return null;
}

/**
 * 扫一棵 asar 树，产出每个模块的可读 API 面。
 * opts.dirs: 只扫这些子目录（默认 main/preload/tencent-docs）
 */
export function buildExportMap(asar, dirs = ['main', 'preload', 'tencent-docs']) {
  const fileSet = new Set();
  for (const file of walkFiles(asar)) fileSet.add(path.relative(asar, file).split(path.sep).join('/'));

  const wanted = rel => dirs.some(d => rel.startsWith(d + '/'));
  const analyzed = new Map();
  for (const rel of fileSet) {
    if (!JS_EXT.test(rel) || !wanted(rel)) continue;
    const text = readText(path.join(asar, rel));
    if (text === null) continue;
    analyzed.set(rel, { bytes: text.length, ...analyzeModule(text) });
  }

  const modules = {};
  for (const [rel, info] of analyzed) {
    modules[rel] = {
      bytes: info.bytes,
      requires: info.requires.map(r => r.spec),
      aliases: {},
      ownExports: [],
      readable: {},
      dangling: [],
    };
    for (const [alias, spec] of Object.entries(info.aliases)) {
      const resolved = resolveSpec(rel, spec);
      const target = resolved ? resolveExisting(resolved, fileSet) : null;
      if (target) modules[rel].aliases[alias] = target;
    }
    // 模块自身出口：mangled 名与「就地定义的可读名」分开记；
    // 可读名 + ref 的形态是 facade，留给第二跳挂到目标模块，避免重复计数
    for (const e of info.exports) {
      if (isMangledName(e.name)) {
        modules[rel].ownExports.push({ name: e.name, kind: e.kind, local: e.local ?? null, via: e.ref ? 're-export' : 'local' });
      } else if (!e.ref) {
        modules[rel].readable[e.name] = { mangled: e.local ?? null, via: rel };
      }
    }
  }

  // 第二跳：把 facade 的可读名挂到目标模块上（链式最多跟 4 层）
  const stats = { modules: analyzed.size, facades: 0, readablePairs: 0, dangling: 0, chainDepthMax: 0 };
  for (const [rel, info] of analyzed) {
    let isFacade = false;
    for (const e of info.exports) {
      if (!e.ref) continue;
      const target = modules[rel].aliases[e.ref.alias];
      if (!target || !modules[target]) {
        if (!isMangledName(e.name)) {
          modules[rel].dangling.push({ name: e.name, alias: e.ref.alias, prop: e.ref.prop });
          stats.dangling++;
        }
        continue;
      }
      isFacade = true;
      let cur = target;
      let prop = e.ref.prop;
      let depth = 0;
      // 目标把该 prop 再转手时继续跟，避免把 A->B->C 的可读名记在中间层
      while (depth < 4) {
        const next = analyzed.get(cur)?.exports.find(x => x.name === prop && x.ref);
        if (!next) break;
        const nxt = modules[cur]?.aliases?.[next.ref.alias];
        if (!nxt || !modules[nxt]) break;
        cur = nxt;
        prop = next.ref.prop;
        depth++;
      }
      stats.chainDepthMax = Math.max(stats.chainDepthMax, depth);
      modules[cur].readable[e.name] = { mangled: prop, via: rel };
      stats.readablePairs++;
    }
    if (isFacade) stats.facades++;
  }

  return { modules: Object.fromEntries(Object.entries(modules).sort(([a], [b]) => a.localeCompare(b))), stats };
}
