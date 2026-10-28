// 智能体配置的模型清单选择（纯函数，便于测试）。
//
// 模型 id 来自 /panel/api/models，id 本身就是调用时要填的完整 model 值
// （带 cn: / global: 前缀），因此这里只做域过滤与去重截断，不改写内容。

import type { ModelEntry } from './models';

// 与后端 agentcfg.maxModelList 同口径：超出截断（不报错），UI 负责提示。
export const MAX_IMPORT_MODELS = 200;

export type ModelRealmFilter = 'all' | 'cn' | 'global';

// modelRealm 从 id 前缀推断域：显式 global: 才算国际版，其余（含裸名）按国内版。
export function modelRealm(id: string): 'cn' | 'global' {
  return id.trim().toLowerCase().startsWith('global:') ? 'global' : 'cn';
}

// selectModelIDs 按域过滤、trim、去重（保序）、上限截断，返回要写入客户端的 id 列表。
export function selectModelIDs(
  entries: ModelEntry[] | undefined,
  filter: ModelRealmFilter,
  limit: number = MAX_IMPORT_MODELS,
): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const entry of entries || []) {
    const id = String(entry?.id || '').trim();
    if (!id || seen.has(id)) continue;
    if (filter !== 'all' && modelRealm(id) !== filter) continue;
    seen.add(id);
    out.push(id);
    if (out.length >= limit) break;
  }
  return out;
}

// countByRealm 统计两域条目数，用于「将写入 N 个模型（CN x · Global y）」提示。
export function countByRealm(ids: string[]): { cn: number; global: number } {
  const out = { cn: 0, global: 0 };
  for (const id of ids) out[modelRealm(id)] += 1;
  return out;
}
