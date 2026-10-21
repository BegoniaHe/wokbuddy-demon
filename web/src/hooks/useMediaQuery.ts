// useMediaQuery.ts 响应式断点判定（只读 matchMedia，不引入额外依赖）。
//
// 用途：窄屏下 SVG 时序图需要更小的 viewBox 与更少的刻度——固定 1200×200 的
// viewBox 在 360px 屏上被压到约 0.3 倍，刻度与线条不可读（issue #202）。
import { useEffect, useState } from 'react';

// MOBILE_QUERY 窄屏断点：与 Tailwind 的 md（768px）错开一点，覆盖 360–760px 的手机。
export const MOBILE_QUERY = '(max-width: 760px)';

// matchesMedia 同步读取一次（无 matchMedia 环境返回 false，SSR/测试安全）。
export function matchesMedia(query: string): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false;
  return window.matchMedia(query).matches;
}

// useMediaQuery 订阅媒体查询；环境不支持时恒 false。
export function useMediaQuery(query: string): boolean {
  const [matched, setMatched] = useState<boolean>(() => matchesMedia(query));

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return;
    const mql = window.matchMedia(query);
    const onChange = () => setMatched(mql.matches);
    onChange();
    mql.addEventListener('change', onChange);
    return () => mql.removeEventListener('change', onChange);
  }, [query]);

  return matched;
}
