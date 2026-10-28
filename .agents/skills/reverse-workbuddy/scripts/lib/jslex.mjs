// jslex.mjs — 依赖自由的 JS 分词器，只做「切词」，不做语法分析。
// 为什么需要它：asar 里的主进程/渲染层是 Rolldown 压成的单行文件（平均 3KB/行），
// 纯正则会把这些东西当成真代码而误判：
//   - 模板串里的 `${base}/v2/x`            -> 端点扫描想看见它，平衡括号扫描必须跳过它
//   - 字符串里的 "require('./x')"          -> 不是模块边
//   - 注释里的 /* ... */ 与 /正则/g          -> 不是标点
// 覆盖：行/块注释、单双引号串、模板串（含 ${} 嵌套）、正则字面量、标识符、数字、标点。
// 不覆盖：\uXXXX 形式的标识符转义、HTML 注释；遇到就按标点降级，不抛错。
export const TOKEN = {
  NAME: 'name',
  NUM: 'num',
  STR: 'str',
  TPL: 'tpl',
  REGEX: 'regex',
  PUNCT: 'punct',
  COMMENT: 'comment',
};

// 可以紧跟在 `/` 之前、且此时 `/` 一定是正则起始的关键字
const REGEX_KEYWORDS = new Set([
  'return', 'typeof', 'instanceof', 'in', 'of', 'new', 'delete', 'void', 'do',
  'else', 'yield', 'await', 'case', 'throw',
]);
// 同上，但形态是标点
const REGEX_PUNCT = new Set([
  '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%',
  '<', '>', '~', '^', '/',
]);

const RE_WS = /\s+/y;
const RE_NAME = /[A-Za-z0-9_$]+/y;
// 前导点的小数（`.5`）也要吃进来，否则 `.` 之后会被当成标点、`5` 再进数字分支
const RE_NUM = /(?:[0-9][0-9a-zA-Z_.]*|\.[0-9][0-9a-zA-Z_.]*)/y;

// sticky 正则匹配失败会把 lastIndex 归零，直接用会让主循环跳回 0（死循环）。
// 统一走这个包装：失败返回 -1，调用方降级成标点。
function stickyEnd(re, src, i) {
  re.lastIndex = i;
  const m = re.exec(src);
  return m === null ? -1 : re.lastIndex;
}

function isNameStart(c) {
  return (c >= 97 && c <= 122) || (c >= 65 && c <= 90) || c === 95 || c === 36;
}

function isDigit(c) {
  return c >= 48 && c <= 57;
}

// `)` 之后既可能是除法也可能是正则：只有闭合的是控制语句括号（`if(...)` / `for(...)`）
// 时才是正则。压缩产物里 `for(let[n,s]of e)/^[a-z]/i.test(n)&&…` 很常见，
// 不区分这一点就会把正则拆成三个 `/` 标点，排版后直接变成语法错误。
const CONTROL_KEYWORDS = new Set(['if', 'for', 'while', 'with', 'switch', 'catch']);

function isRegexStart(prev, afterCondParen = false) {
  if (prev === null) return true;
  if (prev.t === TOKEN.NAME) return REGEX_KEYWORDS.has(prev.v);
  if (prev.t === TOKEN.PUNCT) return prev.v === ')' ? afterCondParen : REGEX_PUNCT.has(prev.v);
  return false;
}

// 从引号位置起跳过一个普通字符串，返回闭合引号之后的下标
function skipQuoted(src, i, quote) {
  for (let j = i + 1; j < src.length; j++) {
    const c = src.charCodeAt(j);
    if (c === 92) {
      j++;
      continue;
    }
    if (c === quote) return j + 1;
    if (c === 10) return j; // 未闭合：按行尾截断，不吞掉后续文件
  }
  return src.length;
}

// `${` 之后的表达式：跳过到配对的 `}`，内部继续识别串/模板/注释/正则
function skipTemplateExpr(src, i) {
  let depth = 1;
  let j = i;
  let prev = null;
  while (j < src.length) {
    const c = src.charCodeAt(j);
    if (c === 39 || c === 34) {
      j = skipQuoted(src, j, c);
      prev = { t: TOKEN.STR };
      continue;
    }
    if (c === 96) {
      j = skipTemplate(src, j);
      prev = { t: TOKEN.TPL };
      continue;
    }
    if (c === 47 && src.charCodeAt(j + 1) === 47) {
      const nl = src.indexOf('\n', j + 2);
      j = nl === -1 ? src.length : nl + 1;
      continue;
    }
    if (c === 47 && src.charCodeAt(j + 1) === 42) {
      const close = src.indexOf('*/', j + 2);
      j = close === -1 ? src.length : close + 2;
      continue;
    }
    if (c === 47 && isRegexStart(prev)) {
      j = skipRegex(src, j);
      prev = { t: TOKEN.REGEX };
      continue;
    }
    if (c === 123) depth++;
    else if (c === 125) {
      depth--;
      if (depth === 0) return j + 1;
    }
    if (!(c === 32 || c === 9 || c === 10 || c === 13)) {
      // prev 必须带上 v：正则/除法的判定靠「上一个标点是什么」，漏掉 v 会把
      // 正则的闭合斜杠当成新的正则起始，进而吞掉后面的模板串（已实测复现）
      RE_NAME.lastIndex = j;
      prev = isNameStart(c) && RE_NAME.test(src) ? { t: TOKEN.NAME, v: src[j] } : { t: TOKEN.PUNCT, v: src[j] };
    }
    j++;
  }
  return src.length;
}

// 模板串：i 指向反引号，返回闭合反引号之后的下标
function skipTemplate(src, i) {
  let j = i + 1;
  while (j < src.length) {
    const c = src.charCodeAt(j);
    if (c === 92) {
      j += 2;
      continue;
    }
    if (c === 96) return j + 1;
    if (c === 36 && src.charCodeAt(j + 1) === 123) {
      j = skipTemplateExpr(src, j + 2);
      continue;
    }
    j++;
  }
  return src.length;
}

// 正则字面量：i 指向 `/`，返回 flags 之后的下标
function skipRegex(src, i) {
  let j = i + 1;
  let inClass = false;
  while (j < src.length) {
    const c = src.charCodeAt(j);
    if (c === 92) {
      j += 2;
      continue;
    }
    if (c === 10) return j; // 未闭合：不当正则处理，交由上层按除法兜底
    if (c === 91) inClass = true;
    else if (c === 93) inClass = false;
    else if (c === 47 && !inClass) {
      j++;
      while (j < src.length && isNameStart(src.charCodeAt(j))) j++;
      return j;
    }
    j++;
  }
  return src.length;
}

/**
 * 核心分词循环。`onToken` 逐词回调，避免大文件把整棵 token 数组留在内存里。
 * token: { t: TOKEN.*, v: string, start: number, end: number, nl: boolean }
 *   `nl` 表示该 token 之前是否有换行（含中间的注释）。排版器靠它保住 ASI 边界：
 *   换行在 JS 里是有语义的（`return\n{x}` 与 `return {x}` 不同），不能随手吞掉。
 * 字符串/模板串的 `v` 是不含定界符的原文（模板串里的 ${} 保留原样）。
 */
export function lex(src, onToken) {
  let i = 0;
  let prev = null;
  let prevEnd = 0;
  // 括号栈只记「这个 `(` 是不是控制语句的括号」，用来判定 `)` 之后的 `/` 是正则还是除法
  const parens = [];
  let afterCondParen = false;
  const emit = tok => {
    tok.nl = tok.start > prevEnd && src.slice(prevEnd, tok.start).includes('\n');
    onToken(tok);
    prevEnd = tok.end;
    if (tok.t === TOKEN.PUNCT) {
      if (tok.v === '(') parens.push(Boolean(prev && prev.t === TOKEN.NAME && CONTROL_KEYWORDS.has(prev.v)));
      else if (tok.v === ')') afterCondParen = parens.pop() === true;
    }
    if (tok.t !== TOKEN.COMMENT) prev = tok;
  };
  while (i < src.length) {
    const c = src.charCodeAt(i);
    // 空白
    if (c === 32 || c === 9 || c === 10 || c === 13 || c === 12 || c === 11) {
      const end = stickyEnd(RE_WS, src, i);
      i = end === -1 ? i + 1 : end;
      continue;
    }
    // 注释
    if (c === 47 && src.charCodeAt(i + 1) === 47) {
      const nl = src.indexOf('\n', i + 2);
      const end = nl === -1 ? src.length : nl;
      emit({ t: TOKEN.COMMENT, v: src.slice(i + 2, end), start: i, end });
      i = end;
      continue;
    }
    if (c === 47 && src.charCodeAt(i + 1) === 42) {
      const close = src.indexOf('*/', i + 2);
      const end = close === -1 ? src.length : close + 2;
      emit({ t: TOKEN.COMMENT, v: src.slice(i + 2, close === -1 ? src.length : close), start: i, end });
      i = end;
      continue;
    }
    // 字符串
    if (c === 39 || c === 34) {
      const end = skipQuoted(src, i, c);
      emit({ t: TOKEN.STR, v: src.slice(i + 1, src.charCodeAt(end - 1) === c ? end - 1 : end), start: i, end });
      i = end;
      continue;
    }
    // 模板串
    if (c === 96) {
      const end = skipTemplate(src, i);
      emit({ t: TOKEN.TPL, v: src.slice(i + 1, src.charCodeAt(end - 1) === 96 ? end - 1 : end), start: i, end });
      i = end;
      continue;
    }
    // 正则
    if (c === 47 && isRegexStart(prev, afterCondParen)) {
      const end = skipRegex(src, i);
      if (end <= src.length && end > i + 1) {
        emit({ t: TOKEN.REGEX, v: src.slice(i + 1, end), start: i, end });
        i = end;
        continue;
      }
    }
    // 标识符 / 数字
    if (isNameStart(c)) {
      const end = stickyEnd(RE_NAME, src, i);
      if (end !== -1) {
        emit({ t: TOKEN.NAME, v: src.slice(i, end), start: i, end });
        i = end;
        continue;
      }
    }
    if (isDigit(c) || (c === 46 && isDigit(src.charCodeAt(i + 1)))) {
      const end = stickyEnd(RE_NUM, src, i);
      if (end !== -1) {
        emit({ t: TOKEN.NUM, v: src.slice(i, end), start: i, end });
        i = end;
        continue;
      }
    }
    // 标点：逐字符，够用且不会误伤 `?.` / `...` / `=>` 的识别
    emit({ t: TOKEN.PUNCT, v: src[i], start: i, end: i + 1 });
    i++;
  }
}

/** 便于小文件/查询场景：直接返回 token 数组。 */
export function tokenize(src) {
  const out = [];
  lex(src, tok => out.push(tok));
  return out;
}

/** 去掉注释后的有效 token（查询时几乎都要这个）。 */
export function significant(tokens) {
  return tokens.filter(t => t.t !== TOKEN.COMMENT);
}

/** 判断 token 序列在 `i` 处是否为给定形态：['name:require','punct:(','str'] 之类。 */
export function matchesAt(tokens, i, pattern) {
  if (i + pattern.length > tokens.length) return false;
  for (let k = 0; k < pattern.length; k++) {
    const tok = tokens[i + k];
    if (!tok) return false;
    const spec = pattern[k];
    if (spec === '*') continue;
    if (spec.includes(':')) {
      const [type, value] = spec.split(':');
      if (tok.t !== type) return false;
      if (value !== undefined && tok.v !== value) return false;
    } else if (tok.t === TOKEN.PUNCT || tok.t === TOKEN.NAME) {
      if (tok.v !== spec) return false;
    } else return false;
  }
  return true;
}
