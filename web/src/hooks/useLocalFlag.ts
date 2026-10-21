// useLocalFlag.ts 本机持久化的布尔开关（localStorage）。
//
// 用途：面板里「不需要进配置、只想记住我这台机器上的偏好」的开关（如积分到期卡片
// 是否折叠）。与服务端配置无关，换设备不跟随。
import { useCallback, useState } from 'react';

// readLocalFlag 读取布尔开关；未设置/非 "1"/"0" → 返回默认值。
export function readLocalFlag(key: string, defaultValue: boolean): boolean {
  try {
    const raw = localStorage.getItem(key);
    if (raw === '1') return true;
    if (raw === '0') return false;
  } catch {
    // localStorage 不可用：回落默认值（不阻塞渲染）。
  }
  return defaultValue;
}

// writeLocalFlag 写入布尔开关（不可用时静默降级）。
export function writeLocalFlag(key: string, value: boolean): void {
  try {
    localStorage.setItem(key, value ? '1' : '0');
  } catch {
    // 忽略：偏好类状态写不进不影响功能。
  }
}

// useLocalFlag 返回 [当前值, 设值/翻转函数]。value 省略即翻转到反面。
export function useLocalFlag(key: string, defaultValue: boolean): [boolean, (next?: boolean) => void] {
  const [value, setValue] = useState<boolean>(() => readLocalFlag(key, defaultValue));

  const update = useCallback(
    (next?: boolean) => {
      setValue((prev) => {
        const target = typeof next === 'boolean' ? next : !prev;
        writeLocalFlag(key, target);
        return target;
      });
    },
    [key],
  );

  return [value, update];
}
