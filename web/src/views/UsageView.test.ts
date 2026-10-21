// @vitest-environment jsdom
// UsageView 时序图的 hook 顺序回归（issue #202）：数据从「有」变「无」时，早返回会让
// hook 数量前后不一致，React 直接抛 "Rendered fewer hooks than expected" 并白屏。
// 用 createElement 而非 JSX：vitest 的 include 只收 *.test.ts。
import { createElement } from 'react';
import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { UsageChart } from './UsageView';
import type { SeriesPoint } from '@/lib/usage';

describe('UsageChart 空数据切换', () => {
  it('有数据 → 空数据 的重渲染不抛错', () => {
    const series: SeriesPoint[] = [
      { t: '2026-10-21 10', prompt_tokens: 10, completion_tokens: 5, total_tokens: 15, requests: 1 },
    ];

    const view = render(createElement(UsageChart, { series }));
    expect(() => view.rerender(createElement(UsageChart, { series: [] }))).not.toThrow();
    expect(view.getByText(/暂无用量数据/)).toBeTruthy();
  });
});
