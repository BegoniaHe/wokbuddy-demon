// format.mjs — 基于 token 的可读排版，供 format_tree / rename_scope 共用。
// 只在安全位置插换行（`;` `{` `}` 之后、超宽时括号内的 `,` 之后），换行点全部避开
// ASI 敏感位置（`return`/`throw` 之后永不换行）。字面量回填原文切片，
// 所以模板串 / 正则 / 转义不会被改动——这一点由调用方的 token 级比对兜底。
import { tokenize, TOKEN } from './jslex.mjs';

// 这些关键字后面跟的是块/表达式，与上一 token 之间留空格更可读
const BLOCK_KEYWORDS = new Set(['var', 'let', 'const', 'function', 'class', 'return', 'typeof', 'new']);
// `}` 之后接这些关键字时不换行（`}else{` / `}catch(e){`）
const JOIN_AFTER_BRACE = new Set(['else', 'catch', 'finally', 'while']);
// `}` 之后接这些标点时既不加空格也不换行（`});` / `},` / `})`）
const JOIN_PUNCT = new Set([',', ';', ')', ']', '.', ':', '=']);

/**
 * @param {string} text 原始（压缩）源码
 * @param {{indentUnit?: string, maxWidth?: number}} [opts]
 * @returns {string} 排版后的源码
 */
export function formatByTokens(text, opts = {}) {
  const indentUnit = opts.indentUnit ?? '  ';
  const maxWidth = opts.maxWidth ?? 120;
  const tokens = tokenize(text).filter(t => t.t !== TOKEN.COMMENT);
  const lines = [];
  let line = '';
  let depth = 0;
  let forDepth = -1;
  let prev = null;

  const write = s => {
    line += s;
  };
  const space = () => {
    if (line && !line.endsWith(' ')) line += ' ';
  };
  const newline = () => {
    lines.push(line.replace(/[ \t]+$/, ''));
    line = indentUnit.repeat(Math.max(depth, 0));
  };
  const flushLine = () => {
    if (line.trim()) newline();
  };

  for (let i = 0; i < tokens.length; i++) {
    const tok = tokens[i];
    const v = tok.v;

    if (tok.t === TOKEN.PUNCT) {
      if (v === '{' || v === '[' || v === '(') {
        write(v);
        depth++;
        if (v === '{') newline();
        else if (v === '(' && prev && prev.t === TOKEN.NAME && (prev.v === 'for' || prev.v === 'while')) forDepth = depth;
        prev = tok;
        continue;
      }
      if (v === '}' || v === ']' || v === ')') {
        depth = Math.max(depth - 1, 0);
        if (v !== ')') flushLine();
        // 缩进只留给生成的空白；收括号前把这一行的缩进按新深度重算
        if (!line.trim()) line = indentUnit.repeat(Math.max(depth, 0));
        write(v);
        if (forDepth > depth) forDepth = -1;
        if (v === '}') {
          const nx = tokens[i + 1];
          const joinKeyword = nx && nx.t === TOKEN.NAME && JOIN_AFTER_BRACE.has(nx.v);
          const joinPunct = nx && nx.t === TOKEN.PUNCT && JOIN_PUNCT.has(nx.v);
          if (joinKeyword) space();
          else if (!joinPunct) newline();
        }
        prev = tok;
        continue;
      }
      if (v === ';') {
        write(v);
        if (forDepth === -1) newline();
        prev = tok;
        continue;
      }
      if (v === ',') {
        write(v);
        if (depth > 0 && forDepth === -1 && line.length > maxWidth) newline();
        else space();
        prev = tok;
        continue;
      }
      write(v);
      prev = tok;
      continue;
    }

    // 名字/字面量相邻必须空格，否则 `return e` 会粘成 `returne`
    const prevNeedsSpace = prev && (prev.t === TOKEN.NAME || prev.t === TOKEN.NUM)
      && (tok.t === TOKEN.NAME || tok.t === TOKEN.NUM || tok.t === TOKEN.STR
        || tok.t === TOKEN.TPL || tok.t === TOKEN.REGEX);
    const afterBlockKeyword = prev && prev.t === TOKEN.NAME && BLOCK_KEYWORDS.has(prev.v);
    if (prevNeedsSpace || afterBlockKeyword) space();

    write(tok.t === TOKEN.STR || tok.t === TOKEN.TPL || tok.t === TOKEN.REGEX
      ? text.slice(tok.start, tok.end)
      : v);
    prev = tok;
  }
  if (line.trim()) lines.push(line.replace(/[ \t]+$/, ''));
  return lines.join('\n') + '\n';
}

/**
 * token 级比对：排版前后非注释 token 的类型与值必须完全一致。
 * strict=false 只比类型序列——第三方重排版器（terser）会规范化引号与转义，
 * 逐字比对必然失败，那种情况只能保证「结构没被破坏」。
 */
export function verifyTokens(before, after, { strict = true } = {}) {
  const a = tokenize(before).filter(t => t.t !== TOKEN.COMMENT);
  const b = tokenize(after).filter(t => t.t !== TOKEN.COMMENT);
  if (a.length !== b.length) return { ok: false, reason: `token count ${a.length} -> ${b.length}` };
  for (let i = 0; i < a.length; i++) {
    if (a[i].t !== b[i].t) return { ok: false, reason: `token ${i} type ${a[i].t} -> ${b[i].t}` };
    if (strict && a[i].v !== b[i].v) {
      return { ok: false, reason: `token ${i} value mismatch: ${JSON.stringify(a[i].v.slice(0, 40))}` };
    }
  }
  return { ok: true };
}
