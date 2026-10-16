// 时间范围控件逻辑（用量 / 请求记录共用）。纯函数，便于测试。
//
// 区间一律由前端算好再发：
//  - 「今天」必须是浏览器本地时区 00:00 起（服务端时区未必一致）。
//  - 「自定义」是用户挑的具体时刻。
// 滚动预设（近 N 小时/天）保留 hours 参数：服务端按整点对齐，前端自减会错桶。

export interface TimeRangeState {
  preset: string;
  from: Date | null;
  to: Date | null;
}

export const TRANGE_PRESETS: [string, string][] = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];

export const TRANGE_DEFAULT = '72';

export function trangeMidnight(): Date {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

export function defaultTimeRangeState(preset: string = TRANGE_DEFAULT): TimeRangeState {
  return { preset, from: trangeMidnight(), to: new Date() };
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours；今天/自定义发 from/to
//   rolling=false → 一律发 from/to
// 「全部历史」滚动端点须显式 hours=0（后端缺省是 72h）。
export function trangeQuery(st: TimeRangeState, rolling: boolean): URLSearchParams {
  const q = new URLSearchParams();
  const sec = (d: Date) => String(Math.floor(d.getTime() / 1000));
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) {
    q.set('hours', st.preset);
    return q;
  }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

export function trangeLabel(st: TimeRangeState): string {
  const found = TRANGE_PRESETS.find((p) => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = (d: Date | null) =>
    d
      ? d.getMonth() +
        1 +
        '-' +
        String(d.getDate()).padStart(2, '0') +
        ' ' +
        String(d.getHours()).padStart(2, '0') +
        ':' +
        String(d.getMinutes()).padStart(2, '0')
      : '…';
  return f(st.from) + ' → ' + f(st.to);
}
