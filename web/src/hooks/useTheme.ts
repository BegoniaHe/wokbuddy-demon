import { useCallback, useEffect, useState } from 'react';
import { LS_THEME } from '@/lib/api';

export type Theme = 'auto' | 'light' | 'dark';
export type EffTheme = 'light' | 'dark';

export function effTheme(theme: Theme): EffTheme {
  if (theme === 'auto') {
    return matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  }
  return theme;
}

function readStored(): Theme {
  const t = localStorage.getItem(LS_THEME);
  return t === 'light' || t === 'dark' || t === 'auto' ? t : 'auto';
}

// useTheme：两态翻转（浅/深），首次访问跟随系统；点击总是切换可见外观。
export function useTheme() {
  const [theme, setTheme] = useState<Theme>(readStored);
  const effective = effTheme(theme);

  useEffect(() => {
    document.documentElement.dataset.theme = effTheme(theme);
  }, [theme]);

  useEffect(() => {
    const onChange = () => {
      if (theme === 'auto') document.documentElement.dataset.theme = effTheme('auto');
    };
    const mq = matchMedia('(prefers-color-scheme: light)');
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, [theme]);

  const toggle = useCallback(() => {
    setTheme((prev) => {
      const next: Theme = effTheme(prev) === 'light' ? 'dark' : 'light';
      localStorage.setItem(LS_THEME, next);
      return next;
    });
  }, []);

  return { theme, effective, toggle };
}
