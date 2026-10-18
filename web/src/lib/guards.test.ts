import { describe, expect, it } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

// 结构性前端护栏：这些约定类型系统表达不了，但可以用源码扫描钉住。
// 运行目录是 web/（见 package.json 的 test 脚本）。

const SRC = join(process.cwd(), 'src');

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...walk(path));
    else if (/\.(ts|tsx)$/.test(name)) out.push(path);
  }
  return out;
}

function tagSnippets(src: string, tag: string): string[] {
  const out: string[] = [];
  const needle = '<' + tag;
  let at = 0;
  while ((at = src.indexOf(needle, at)) >= 0) {
    let depth = 0;
    let quote = '';
    let end = -1;
    for (let i = at + needle.length; i < src.length; i++) {
      const ch = src[i];
      if (quote) {
        if (ch === quote && src[i - 1] !== '\\') quote = '';
        continue;
      }
      if (ch === '"' || ch === "'") quote = ch;
      else if (ch === '{') depth++;
      else if (ch === '}') depth--;
      else if (ch === '>' && depth === 0) {
        end = i;
        break;
      }
    }
    if (end < 0) break;
    out.push(src.slice(at, end + 1));
    at = end + 1;
  }
  return out;
}

describe('前端结构护栏', () => {
  it('不再使用原生 confirm / alert / prompt', () => {
    const bad: string[] = [];
    for (const path of walk(SRC)) {
      if (path.endsWith('ConfirmDialog.tsx')) continue; // 只含用法说明，不调用原生弹窗
      const src = readFileSync(path, 'utf8');
      for (const match of src.matchAll(/\b(?:confirm|alert|prompt)\(/g)) {
        const before = src.slice(Math.max(0, match.index! - 8), match.index);
        if (before.endsWith('await ')) continue; // useConfirm 的 Promise 形态
        bad.push(`${path}: ${match[0]}`);
      }
    }
    expect(bad).toEqual([]);
  });

  it('SelectTrigger 与 Switch 都有可读名称', () => {
    const missing: string[] = [];
    for (const path of walk(SRC)) {
      if (path.includes(join('components', 'ui'))) continue; // 第三方原语
      const src = readFileSync(path, 'utf8');
      for (const tag of ['SelectTrigger', 'Switch']) {
        for (const snippet of tagSnippets(src, tag)) {
          if (!/aria-label|aria-labelledby/.test(snippet)) {
            missing.push(`${path}: ${snippet.replace(/\s+/g, ' ').slice(0, 100)}`);
          }
        }
      }
    }
    expect(missing).toEqual([]);
  });

  it('添加账号弹窗的轮询处理标签页可见性', () => {
    const src = readFileSync(join(SRC, 'components', 'AddAccountDialog.tsx'), 'utf8');
    expect(src).toContain('visibilitychange');
    expect(src).toContain('document.hidden');
    expect(src).toContain('setInterval');
  });
});
