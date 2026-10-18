import {
  TRANGE_PRESETS,
  trangeMidnight,
  type TimeRangeState,
} from '@/lib/timeRange';
import { dtLocalParse, dtLocalValue } from '@/lib/format';
import {
  Select,
  SelectContent,
  SelectOptions,
  SelectTrigger,
  SelectValue,
  selectOptionItems,
  type SelectOption,
} from '@/components/ui/select';
import { Input } from '@/components/ui/input';

// 时间范围预设（value → 中文标签），同时驱动 items 映射与列表项。
const PRESET_OPTIONS: readonly SelectOption[] = TRANGE_PRESETS.map(([value, label]) => ({
  value,
  label,
}));

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

  const setPreset = (preset: string | null) => {
    if (preset == null) return;
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

  const presetLabel = TRANGE_PRESETS.find((p) => p[0] === value.preset)?.[1] ?? value.preset;

  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <Select items={selectOptionItems(PRESET_OPTIONS)} value={value.preset} onValueChange={setPreset}>
        <SelectTrigger size="sm" className="w-28" aria-label="时间范围">
          <SelectValue>{presetLabel}</SelectValue>
        </SelectTrigger>
        <SelectContent>
          <SelectOptions options={PRESET_OPTIONS} />
        </SelectContent>
      </Select>
      {custom ? (
        <span className="inline-flex items-center gap-1.5">
          <Input
            type="datetime-local"
            aria-label="起始时间"
            className={bad ? 'h-7 border-destructive bg-destructive-soft font-mono text-xs' : 'h-7 font-mono text-xs'}
            value={value.from ? dtLocalValue(value.from) : ''}
            onChange={(e) => readCustom('from', e.target.value)}
          />
          <span className="text-xs text-muted-foreground">→</span>
          <Input
            type="datetime-local"
            aria-label="结束时间"
            className={bad ? 'h-7 border-destructive bg-destructive-soft font-mono text-xs' : 'h-7 font-mono text-xs'}
            value={value.to ? dtLocalValue(value.to) : ''}
            onChange={(e) => readCustom('to', e.target.value)}
          />
        </span>
      ) : null}
    </span>
  );
}
