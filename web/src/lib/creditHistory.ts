// creditHistory.ts 面板「积分历史」的纯格式化逻辑（与视图分离，便于单测）。

export interface CreditHistoryEntry {
  time?: string;
  uid?: string;
  nickname?: string;
  delta?: number;
  remain?: number;
  note?: string;
}

// chName 账本行的展示名：昵称优先，其次账号短 uid（账号被移除时昵称为空）。
export function chName(entry: CreditHistoryEntry | undefined): string {
  const nickname = String(entry?.nickname || '').trim();
  if (nickname) return nickname;
  const uid = String(entry?.uid || '');
  return uid ? uid.slice(0, 8) : '—';
}

// chDelta 变动量展示：+N / -N / 0（带符号，便于一眼看出方向）。
export function chDelta(delta: unknown): string {
  const n = Number(delta || 0);
  if (!Number.isFinite(n) || n === 0) return '0';
  return (n > 0 ? '+' : '') + String(Math.round(n));
}

// chTone 变动方向对应的语义色（增=ok、减=warn）。
export function chTone(delta: unknown): 'ok' | 'warn' | 'mute' {
  const n = Number(delta || 0);
  if (!Number.isFinite(n) || n === 0) return 'mute';
  return n > 0 ? 'ok' : 'warn';
}

// chTime 流水时间（本地 HH:MM:SS，与面板其它时间列同口径）；解析失败回空串。
export function chTime(time: unknown): string {
  const raw = String(time || '');
  if (!raw) return '';
  const ms = Date.parse(raw);
  if (!Number.isFinite(ms)) return '';
  return new Date(ms).toLocaleTimeString('zh-CN', { hour12: false });
}
