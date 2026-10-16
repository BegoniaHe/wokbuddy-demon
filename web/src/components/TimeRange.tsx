import {
  TRANGE_PRESETS,
  trangeMidnight,
  type TimeRangeState,
} from '@/lib/timeRange';
import { dtLocalParse, dtLocalValue } from '@/lib/format';
import { Input, Select } from '@/components/ui/field';
import { cn } from '@/lib/utils';

// TimeRange：预设下拉 + 「自定义」时展开的起止时间。起止颠倒就地标红（不静默纠正）。
export function TimeRange({
  value,
  onChange,
}: {
  value: TimeRangeState;
  onChange: (s: TimeRangeState) => void;
}) {
  const custom = value.preset === 'custom';
  const bad = !!(value.from && value.to && value.from > value.to);

  const setPreset = (preset: string) => {
    const next: TimeRangeState = { ...value, preset };
    if (preset === 'custom' && (!next.from || !next.to)) {
      next.from = trangeMidnight();
      next.to = new Date();
    }
    onChange(next);
  };

  const readCustom = (which: 'from' | 'to', raw: string) => {
    onChange({ ...value, [which]: dtLocalParse(raw) });
  };

  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <Select
        size="sm"
        aria-label="时间范围"
        value={value.preset}
        onChange={(e) => setPreset(e.target.value)}
      >
        {TRANGE_PRESETS.map(([v, label]) => (
          <option key={v} value={v}>
            {label}
          </option>
        ))}
      </Select>
      {custom ? (
        <span className="inline-flex items-center gap-1.5">
          <Input
            type="datetime-local"
            size="sm"
            aria-label="起始时间"
            className={cn('font-mono', bad && 'border-bad bg-bad-soft')}
            value={value.from ? dtLocalValue(value.from) : ''}
            onChange={(e) => readCustom('from', e.target.value)}
          />
          <span className="text-xs text-ink-3">→</span>
          <Input
            type="datetime-local"
            size="sm"
            aria-label="结束时间"
            className={cn('font-mono', bad && 'border-bad bg-bad-soft')}
            value={value.to ? dtLocalValue(value.to) : ''}
            onChange={(e) => readCustom('to', e.target.value)}
          />
        </span>
      ) : null}
    </span>
  );
}
