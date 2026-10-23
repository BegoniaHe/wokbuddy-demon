import { Fragment, useMemo, useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { parseBlocks, parseInline, type Block, type InlineToken } from '@/lib/markdown';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

function Inline({ text }: { text: string }) {
  const lines = text.split('\n');
  return (
    <>
      {lines.map((line, i) => (
        <Fragment key={i}>
          {i > 0 ? <br /> : null}
          {parseInline(line).map((t, j) => renderInline(t, j))}
        </Fragment>
      ))}
    </>
  );
}

function renderInline(t: InlineToken, key: number) {
  switch (t.kind) {
    case 'text':
      return <Fragment key={key}>{t.text}</Fragment>;
    case 'code':
      return (
        <code key={key} className="rounded bg-secondary px-1 py-0.5 font-mono text-[0.85em]">
          {t.text}
        </code>
      );
    case 'strong':
      return (
        <strong key={key} className="font-semibold">
          {t.text}
        </strong>
      );
    case 'em':
      return <em key={key}>{t.text}</em>;
    case 'strike':
      return <del key={key}>{t.text}</del>;
    case 'link':
      return (
        <a
          key={key}
          href={t.href}
          target="_blank"
          rel="noreferrer noopener"
          className="text-primary underline underline-offset-2 hover:text-primary-hover">
          {t.text}
        </a>
      );
  }
}

function CodeBlock({ lang, code }: { lang: string; code: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      // 剪贴板不可用（非安全上下文/权限拒绝）：不提示，避免误导。
    }
  };

  return (
    <div className="overflow-hidden rounded-lg border border-border bg-secondary/60">
      <div className="flex items-center justify-between gap-2 border-b border-border/70 px-2.5 py-1">
        <span className="truncate font-mono text-3xs text-muted-foreground">{lang || 'text'}</span>
        <Button
          variant="ghost"
          size="xs"
          className="h-5 px-1.5 text-3xs text-muted-foreground"
          onClick={() => void copy()}
          aria-label="复制代码">
          {copied ? <Check /> : <Copy />}
          {copied ? '已复制' : '复制'}
        </Button>
      </div>
      <pre className="overflow-x-auto p-3 font-mono text-xs leading-relaxed">
        <code>{code}</code>
      </pre>
    </div>
  );
}

function BlockView({ block }: { block: Block }) {
  switch (block.kind) {
    case 'paragraph':
      return (
        <p>
          <Inline text={block.text} />
        </p>
      );
    case 'heading': {
      const size =
        block.level <= 1 ? 'text-lg' : block.level === 2 ? 'text-base' : 'text-sm';
      return (
        <p className={cn('font-semibold tracking-tight', size)}>
          <Inline text={block.text} />
        </p>
      );
    }
    case 'list': {
      const Tag = block.ordered ? 'ol' : 'ul';
      return (
        <Tag className={cn('pl-5 [&>li+li]:mt-1', block.ordered ? 'list-decimal' : 'list-disc')}>
          {block.items.map((item, i) => (
            <li key={i}>
              <Inline text={item} />
            </li>
          ))}
        </Tag>
      );
    }
    case 'quote':
      return (
        <blockquote className="border-l-2 border-border pl-3 text-muted-foreground">
          <Inline text={block.text} />
        </blockquote>
      );
    case 'hr':
      return <hr className="border-border" />;
    case 'code':
      return <CodeBlock lang={block.lang} code={block.code} />;
    case 'table':
      return (
        <div className="overflow-x-auto rounded-lg border border-border">
          <table className="w-full border-collapse text-xs">
            <thead className="bg-secondary/70">
              <tr>
                {block.head.map((cell, i) => (
                  <th key={i} className="border-b border-border px-2.5 py-1.5 text-left font-medium">
                    <Inline text={cell} />
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {block.rows.map((row, i) => (
                <tr key={i} className="border-b border-border/60 last:border-0">
                  {row.map((cell, j) => (
                    <td key={j} className="px-2.5 py-1.5 align-top">
                      <Inline text={cell} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
  }
}

// Markdown 仅服务于试聊消息：用 React 元素渲染，绝不注入 HTML。
export function Markdown({ text }: { text: string }) {
  const blocks = useMemo(() => parseBlocks(text), [text]);
  return (
    <div className="flex flex-col gap-2.5 break-words text-base leading-relaxed">
      {blocks.map((b, i) => (
        <BlockView key={i} block={b} />
      ))}
    </div>
  );
}
