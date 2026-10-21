// 积分包与到期聚合（首页到期提醒 / 积分构成共用）。

export interface Package {
  remain?: number;
  end_time?: string;
  [k: string]: unknown;
}

// expBatches 把某账号的包聚合成「到期日 → 该日作废积分」升序列表。
// 只统计 remain>0 且有到期时间的包。
export function expBatches(packs: Package[] | undefined): { date: string; remain: number }[] {
  const byDay = new Map<string, number>();
  for (const p of packs || []) {
    const r = Number(p.remain || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (r <= 0 || !t) continue;
    byDay.set(t, (byDay.get(t) || 0) + r);
  }
  return [...byDay.entries()]
    .map(([date, remain]) => ({ date, remain }))
    .sort((a, b) => (a.date < b.date ? -1 : 1));
}

export function expDaysLeft(dateStr: string, today: Date): number {
  return Math.round((new Date(dateStr + 'T00:00:00').getTime() - today.getTime()) / 86400000);
}

// ExpiryBucket 上游快照里的逐日到期批次（与「检查」明细同口径的按日聚合）。
export interface ExpiryBucket {
  date?: string;
  expire_at?: string;
  remain?: number;
}

// expBucketsFromSnapshot 把快照批次转成与 expBatches 同签名的列表。
// 口径一致才能保证「快照」与「检查」对同一账号给出完全相同的批次（issue #200）；
// 快照缺失（旧 state / 未查询过）时返回空列表，调用方回落既有「最早一批」渲染。
export function expBucketsFromSnapshot(buckets: ExpiryBucket[] | undefined): { date: string; remain: number }[] {
  // 上游已按日聚合，这里再按 date 合并一次：旧数据/混合来源出现同日多条时，
  // 输出仍与 expBatches 完全一致（测试钉住这条 parity）。
  const byDay = new Map<string, number>();
  for (const bucket of buckets || []) {
    const date = String(bucket.date || '').slice(0, 10);
    const remain = Number(bucket.remain || 0);
    if (!date || remain <= 0) continue;
    byDay.set(date, (byDay.get(date) || 0) + remain);
  }
  return [...byDay.entries()].map(([date, remain]) => ({ date, remain })).sort((a, b) => (a.date < b.date ? -1 : 1));
}
