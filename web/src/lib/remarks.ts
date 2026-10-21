// remarks.ts 账号本机备注（只存 localStorage，不改 auth 文件、不进 state）。
//
// 键用 uid 前 8 位（与界面显示的短 uid 同口径）：上游 uid 很长，全量键会让
// localStorage 体积翻倍，且 8 位在单机账号规模下碰撞概率可忽略。
// 解析失败一律静默回落空表——备注是纯装饰信息，坏数据不该让面板报错。
const LS_REMARKS = 'wb2api.remarks.v1';

// REMARKS_KEY_PREFIX_LEN 备注键取自 uid 的前 N 位。
const REMARKS_KEY_PREFIX_LEN = 8;

export type RemarkMap = Record<string, string>;

// remarkKey 由 uid 派生备注键；空 uid 返回空串（调用方据此跳过）。
export function remarkKey(uid: unknown): string {
  return String(uid || '').slice(0, REMARKS_KEY_PREFIX_LEN);
}

// readRemarks 读取全部备注；无数据/解析失败/非对象 → 空表。
export function readRemarks(): RemarkMap {
  try {
    const raw = localStorage.getItem(LS_REMARKS);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const out: RemarkMap = {};
    for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
      const text = typeof value === 'string' ? value.trim() : '';
      if (key && text) out[key] = text;
    }
    return out;
  } catch {
    return {};
  }
}

// writeRemarks 整体写入（空字符串条目在写入前已被过滤）。
export function writeRemarks(map: RemarkMap): void {
  try {
    localStorage.setItem(LS_REMARKS, JSON.stringify(map));
  } catch {
    // localStorage 不可用（隐私模式/配额满）：备注属装饰信息，静默降级。
  }
}

// setRemark 设置单账号备注；空文本等价于清除。
export function setRemark(uid: unknown, text: string): RemarkMap {
  const key = remarkKey(uid);
  const map = readRemarks();
  const value = text.trim();
  if (!key) return map;
  if (value) map[key] = value;
  else delete map[key];
  writeRemarks(map);
  return map;
}

// clearRemark 清除单账号备注。
export function clearRemark(uid: unknown): RemarkMap {
  return setRemark(uid, '');
}

// remarkName 返回展示名与「原标题」幽灵文本：
//   - 有备注 → display=备注，ghost=原标题（浅色保留，避免彻底丢失真实昵称）；
//   - 无备注 → display=原标题，ghost 为空。
export function remarkName(uid: unknown, name: unknown): { display: string; ghost: string } {
  const original = String(name || remarkKey(uid) || '—');
  const key = remarkKey(uid);
  const remark = key ? readRemarks()[key] : '';
  if (!remark) return { display: original, ghost: '' };
  return { display: remark, ghost: original };
}
