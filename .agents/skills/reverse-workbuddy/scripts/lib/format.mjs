// format.mjs — 基于 token 的可读排版，供 format_tree / rename_scope 共用。
// 只在安全位置插换行（`;` `{` `}` 之后、超宽时括号内的 `,` 之后），换行点全部避开
// ASI 敏感位置（`return`/`throw` 之后永不换行）。字面量回填原文切片，
// 所以模板串 / 正则 / 转义不会被改动——这一点由调用方的 token 级比对兜底。
import { tokenize, TOKEN } from './jslex.mjs';

// 这些关键字后面跟的是块/表达式，与上一 token 之间留空格更可读
const BLOCK_KEYWORDS = new Set(['var', 'let', 'const', 'function', 'class', 'return', 'typeof', 'new']);
// `}` 之后接这些关键字时不换行（`}else{` / `}catch(e){`）
const JOIN_AFTER_BRACE = new Set(['else', 'catch', 'finally', 'while']);
// 两个标点紧贴会被词法重新解释的形态（`+ +` -> `++`、`? .` -> `?.`、`/ /` -> 注释）。
// 排版时源里本来相邻的必须保持相邻，本来隔开的必须补空格，否则改了语法。
const FUSED_PUNCT = new Set([
  '++', '--', '+=', '-=', '*=', '/=', '%=', '&=', '|=', '^=', '=>', '<=', '>=',
  '==', '!=', '&&', '||', '??', '?.', '**', '<<', '>>', '//', '/*', '**=',
]);
// `}` 之后接这些标点时既不加空格也不换行（`});` / `},` / `})`）。
// 表达式还能接着算的运算符全部在内：`{a:1} / 2` 折行后会被 ASI 断成两条语句。
const JOIN_PUNCT = new Set([
  ',', ';', ')', ']', '.', ':', '=',
  '(', '[', '+', '-', '*', '/', '%', '?', '&', '|', '^', '<', '>', '!', '~',
]);

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
    if (line.trim() && !line.endsWith(' ')) line += ' ';
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
    // 原文这里的换行有语义（ASI）：`return null` 后换行再 `const` 是两条语句，
    // 抹成空格就变成非法表达式。原样保住换行点，并在 verify 里逐点核对。
    if (tok.nl && line.trim()) newline();

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
      // 相邻标点会粘成另一个 token（`+ +` → `++`、`! =` → `!=`、`/ /` → 注释）。
      // 源里原本就相邻的照抄（`a++` / `===` 必须保持），原本隔着空白的必须补回来，
      // 否则字符级分词器看不出差别、产物却换了语法（`a+ +!o` → `a++!o`）。
      if (prev && prev.t === TOKEN.PUNCT && prev.end !== tok.start) space();
      write(v);
      prev = tok;
      continue;
    }

    // 名字/字面量相邻必须空格，否则 `return e` 会粘成 `returne`
    const prevNeedsSpace = prev && (prev.t === TOKEN.NAME || prev.t === TOKEN.NUM)
      && (tok.t === TOKEN.NAME || tok.t === TOKEN.NUM || tok.t === TOKEN.STR
        || tok.t === TOKEN.TPL || tok.t === TOKEN.REGEX);
    const afterBlockKeyword = prev && prev.t === TOKEN.NAME && BLOCK_KEYWORDS.has(prev.v);
    // `/` 紧跟正则会被读成 `//` 行注释（`a/ /re/` -> `a//re/`），必须补空格
    const beforeRegex = prev && prev.t === TOKEN.PUNCT && prev.v === '/'
      && tok.t === TOKEN.REGEX && prev.end !== tok.start;
    if (prevNeedsSpace || afterBlockKeyword || beforeRegex) space();

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
    // 字符级分词看不见「两个标点被排版粘成一个运算符」，只能比对相邻性：
    // 原文分开的两个标点在排版后紧贴，等于换了个语法运算符，必须判失败。
    if (i > 0 && a[i].t === TOKEN.PUNCT && a[i - 1].t === TOKEN.PUNCT
      && FUSED_PUNCT.has(a[i - 1].v + a[i].v)
      && b[i].start === b[i - 1].end && a[i].start !== a[i - 1].end) {
      return { ok: false, reason: `token ${i}: punctuators fused (${a[i - 1].v} + ${a[i].v})` };
    }
    // ASI 边界：原文此处有换行，排版必须也保留换行，否则语句被粘成一句。
    if (i > 0 && a[i].nl && !after.slice(b[i - 1].end, b[i].start).includes('\n')) {
      return { ok: false, reason: `token ${i}: line break dropped (${a[i - 1].v} | ${a[i].v})` };
    }
  }
  return { ok: true };
}
