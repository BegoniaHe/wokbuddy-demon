import { useCallback, useEffect, useState } from 'react';

// useHashRoute：以 location.hash 驱动的极简路由。
// 用 hash 而非 history，服务端无需 SPA fallback，且与旧面板行为一致。
export function useHashRoute<T extends string>(
  views: readonly T[],
  fallback: T,
): [T, (v: T) => void] {
  const read = useCallback((): T => {
    const h = (location.hash || '').slice(1);
    return (views as readonly string[]).includes(h) ? (h as T) : fallback;
  }, [views, fallback]);

  const [view, setView] = useState<T>(read);

  useEffect(() => {
    const on = () => setView(read());
    window.addEventListener('hashchange', on);
    return () => window.removeEventListener('hashchange', on);
  }, [read]);

  const go = useCallback((v: T) => {
    location.hash = v;
    setView(v);
  }, []);

  return [view, go];
}
