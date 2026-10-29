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

// ExpiryDayTotal 某自然日全站到期合计。
export interface ExpiryDayTotal {
  date: string;
  remain: number;
  accounts: number;
}

// ExpiryRealmTotal 单个 realm 的按日合计（realm 空按 cn）。
export interface ExpiryRealmTotal {
  realm: string;
  rows: ExpiryDayTotal[];
  accounts: number;
}

// realmRank realm 分块顺序：cn 在前、global 次之，其它（上游新增域）垫底。
function realmRank(realm: string): number {
  if (realm === 'cn') return 0;
  if (realm === 'global') return 1;
  return 2;
}

// expTotals 跨账号按自然日合计（口径与 expBatches 一致：只算未过期、remain>0 的批次）。
// 逐账号复用 expBatches，不另写一套归并逻辑；上游已按自然日聚合，因此按 date 字符串
// 分组即为自然日口径，不做任何时区换算。error 账号的 packages 不可信，直接跳过。
export function expTotals(
  accts: { realm?: string; error?: string; packages?: Package[] }[],
  today: Date,
): ExpiryRealmTotal[] {
  const byRealm = new Map<string, { rows: Map<string, ExpiryDayTotal>; accounts: number }>();
  for (const a of accts || []) {
    if (a.error) continue;
    const realm = (a.realm || '').trim().toLowerCase() || 'cn';
    const batches = expBatches(a.packages).filter((b) => expDaysLeft(b.date, today) >= 0);
    if (!batches.length) continue;
    let bucket = byRealm.get(realm);
    if (!bucket) {
      bucket = { rows: new Map(), accounts: 0 };
      byRealm.set(realm, bucket);
    }
    bucket.accounts += 1;
    for (const b of batches) {
      // expBatches 已把账号内同日多批合并，这里每个日期对同一账号只累加一次账号数。
      const row = bucket.rows.get(b.date) || { date: b.date, remain: 0, accounts: 0 };
      row.remain += b.remain;
      row.accounts += 1;
      bucket.rows.set(b.date, row);
    }
  }
  return [...byRealm.entries()]
    .sort((x, y) => realmRank(x[0]) - realmRank(y[0]) || (x[0] < y[0] ? -1 : 1))
    .map(([realm, v]) => ({
      realm,
      accounts: v.accounts,
      rows: [...v.rows.values()].sort((x, y) => (x.date < y.date ? -1 : 1)),
    }));
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
