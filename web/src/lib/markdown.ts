// 轻量 Markdown 解析（块级 + 行内），只产出结构化 token，不生成 HTML。
//
// 定位：试聊消息渲染。覆盖聊天里常见的代码块、标题、列表、引用、表格与行内样式；
// 不支持嵌套块与完整 GFM。始终由 React 元素渲染，天然避免 dangerouslySetInnerHTML。

export type InlineToken =
  | { kind: 'text'; text: string }
  | { kind: 'code'; text: string }
  | { kind: 'strong'; text: string }
  | { kind: 'em'; text: string }
  | { kind: 'strike'; text: string }
  | { kind: 'link'; text: string; href: string };

export type Block =
  | { kind: 'code'; lang: string; code: string }
  | { kind: 'heading'; level: number; text: string }
  | { kind: 'list'; ordered: boolean; items: string[] }
  | { kind: 'quote'; text: string }
  | { kind: 'hr' }
  | { kind: 'table'; head: string[]; rows: string[][] }
  | { kind: 'paragraph'; text: string };

const FENCE_OPEN = /^\s*```(\S*)\s*$/;
const FENCE_CLOSE = /^\s*```\s*$/;
const HEADING = /^(#{1,4})\s+(.*)$/;
const HR = /^\s*(-{3,}|\*{3,}|_{3,})\s*$/;
const QUOTE = /^\s*>/;
const UL = /^\s*[-*+]\s+/;
const OL = /^\s*\d+[.)]\s+/;

function isTableSep(line: string): boolean {
  return line.includes('-') && /^[\s|:-]+$/.test(line);
}

function isBlockStart(lines: string[], i: number): boolean {
  const line = lines[i] ?? '';
  return (
    FENCE_OPEN.test(line) ||
    HEADING.test(line) ||
    HR.test(line) ||
    QUOTE.test(line) ||
    UL.test(line) ||
    OL.test(line) ||
    (line.includes('|') && isTableSep(lines[i + 1] ?? ''))
  );
}

function splitRow(line: string): string[] {
  return line
    .trim()
    .replace(/^\|/, '')
    .replace(/\|$/, '')
    .split('|')
    .map((c) => c.trim());
}

export function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n?/g, '\n').split('\n');
  const blocks: Block[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i] ?? '';
    if (!line.trim()) {
      i++;
      continue;
    }

    const fence = FENCE_OPEN.exec(line);
    if (fence) {
      const lang = fence[1] ?? '';
      i++;
      const code: string[] = [];
      while (i < lines.length && !FENCE_CLOSE.test(lines[i] ?? '')) {
        code.push(lines[i] ?? '');
        i++;
      }
      if (i < lines.length) i++;
      blocks.push({ kind: 'code', lang, code: code.join('\n') });
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading) {
      blocks.push({ kind: 'heading', level: (heading[1] ?? '#').length, text: (heading[2] ?? '').trim() });
      i++;
      continue;
    }

    if (HR.test(line)) {
      blocks.push({ kind: 'hr' });
      i++;
      continue;
    }

    if (QUOTE.test(line)) {
      const quote: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i] ?? '')) {
        quote.push((lines[i] ?? '').replace(/^\s*>\s?/, ''));
        i++;
      }
      blocks.push({ kind: 'quote', text: quote.join('\n') });
      continue;
    }

    if (UL.test(line)) {
      const items: string[] = [];
      while (i < lines.length && UL.test(lines[i] ?? '')) {
        items.push((lines[i] ?? '').replace(UL, ''));
        i++;
      }
      blocks.push({ kind: 'list', ordered: false, items });
      continue;
    }

    if (OL.test(line)) {
      const items: string[] = [];
      while (i < lines.length && OL.test(lines[i] ?? '')) {
        items.push((lines[i] ?? '').replace(OL, ''));
        i++;
      }
      blocks.push({ kind: 'list', ordered: true, items });
      continue;
    }

    if (line.includes('|') && isTableSep(lines[i + 1] ?? '')) {
      const head = splitRow(line);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && (lines[i] ?? '').trim() && (lines[i] ?? '').includes('|')) {
        rows.push(splitRow(lines[i] ?? ''));
        i++;
      }
      blocks.push({ kind: 'table', head, rows });
      continue;
    }

    const para: string[] = [];
    while (i < lines.length) {
      const cur = lines[i] ?? '';
      if (!cur.trim()) break;
      if (para.length && isBlockStart(lines, i)) break;
      para.push(cur);
      i++;
    }
    blocks.push({ kind: 'paragraph', text: para.join('\n') });
  }

  return blocks;
}

// isSafeHref 只放行 http(s)：javascript:/data: 等一律降级成纯文本。
export function isSafeHref(href: string): boolean {
  return /^https?:\/\/\S+$/i.test(href);
}

const INLINE =
  /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(__[^_\n]+__)|(\*[^*\n]+\*)|(~~[^~\n]+~~)|(\[[^\]\n]+\]\([^)\s]+\))/g;

// parseInline 解析行内 token；未闭合标记按普通文本保留。
export function parseInline(text: string): InlineToken[] {
  const out: InlineToken[] = [];
  let last = 0;
  INLINE.lastIndex = 0;

  const pushText = (t: string) => {
    if (!t) return;
    const prev = out[out.length - 1];
    if (prev?.kind === 'text') prev.text += t;
    else out.push({ kind: 'text', text: t });
  };

  for (let m = INLINE.exec(text); m; m = INLINE.exec(text)) {
    const at = m.index;
    if (at > last) pushText(text.slice(last, at));
    const tok = m[0];
    if (m[1]) {
      out.push({ kind: 'code', text: tok.slice(1, -1) });
    } else if (m[2] || m[3]) {
      out.push({ kind: 'strong', text: tok.slice(2, -2) });
    } else if (m[4]) {
      out.push({ kind: 'em', text: tok.slice(1, -1) });
    } else if (m[5]) {
      out.push({ kind: 'strike', text: tok.slice(2, -2) });
    } else {
      const link = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(tok);
      if (link && isSafeHref(link[2] ?? '')) {
        out.push({ kind: 'link', text: link[1] ?? '', href: link[2] ?? '' });
      } else {
        pushText(tok);
      }
    }
    last = at + tok.length;
  }

  if (last < text.length) pushText(text.slice(last));
  return out;
}
