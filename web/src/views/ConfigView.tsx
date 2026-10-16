import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api, LS_KEY, postJSON } from '@/lib/api';
import {
  CFG_FIELDS,
  collectConfig,
  dig,
  durationBad,
  DURATION_FIELDS,
  DURATION_TIP,
  type FieldValue,
} from '@/lib/config';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
import { Field, Input, Select, Switch } from '@/components/ui/field';
import { Box, BoxBody, BoxHeader, BoxNote, BoxTitle, Grow } from '@/components/ui/box';
import { cn } from '@/lib/utils';

export function ConfigView() {
  const qc = useQueryClient();
  const [values, setValues] = useState<Record<string, FieldValue>>({});
  const [path, setPath] = useState('');
  const [note, setNote] = useState('');
  const [showKey, setShowKey] = useState(false);
  const [saving, setSaving] = useState(false);

  const load = async () => {
    try {
      const d = await api<{ path?: string; config?: unknown }>('config');
      setPath(d.path || '');
      const cfg = d.config;
      const next: Record<string, FieldValue> = {};
      for (const f of CFG_FIELDS) {
        const v = dig(cfg, f.path);
        if (f.kind === 'checkbox') next[f.name] = !!v;
        else if (Array.isArray(v)) next[f.name] = v.join(', ');
        else next[f.name] = v == null ? '' : String(v);
      }
      setValues(next);
      setNote('');
    } catch (e) {
      toast('读取配置失败：' + (e as Error).message, 'err');
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const set = (name: string, v: FieldValue) => setValues((prev) => ({ ...prev, [name]: v }));

  const badFields = [...DURATION_FIELDS].filter((n) => {
    const v = values[n];
    return typeof v === 'string' && durationBad(v);
  });

  const save = async () => {
    if (badFields.length) {
      toast('「' + badFields[0] + '」' + DURATION_TIP, 'err');
      return;
    }
    setSaving(true);
    try {
      const r = await postJSON<{ restart_required?: string[] }>('config', collectConfig(values));
      const n = (r.restart_required || []).length;
      toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
      const k = String(values.api_key ?? '').trim();
      if (k) localStorage.setItem(LS_KEY, k);
      await load();
      await qc.invalidateQueries({ queryKey: ['overview'] });
    } catch (e) {
      toast('保存失败：' + (e as Error).message, 'err');
    } finally {
      setSaving(false);
    }
  };

  const text = (name: string, props: React.ComponentProps<typeof Input> = {}) => (
    <Input
      value={String(values[name] ?? '')}
      onChange={(e) => set(name, e.target.value)}
      className={cn(DURATION_FIELDS.has(name) && durationBad(String(values[name] ?? '')) && 'border-bad bg-bad-soft')}
      title={DURATION_FIELDS.has(name) && durationBad(String(values[name] ?? '')) ? DURATION_TIP : undefined}
      {...props}
    />
  );
  const num = (name: string, props: React.ComponentProps<typeof Input> = {}) =>
    text(name, { type: 'number', ...props });
  const check = (name: string, label: React.ReactNode) => (
    <Switch label={label} checked={!!values[name]} onChange={(v) => set(name, v)} />
  );

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <Box>
        <BoxHeader>
          <BoxTitle>服务</BoxTitle>
          <Grow />
          <BoxNote>{path}</BoxNote>
        </BoxHeader>
        <BoxBody>
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field label="监听地址" hint="改动需重启进程">
              {text('listen', { placeholder: ':7863' })}
            </Field>
            <Field label="API 密钥" hint="立即生效（含面板自身）">
              <div className="flex items-center gap-2">
                {text('api_key', { type: showKey ? 'text' : 'password', placeholder: '留空 = 不鉴权' })}
                <Button size="xs" onClick={() => setShowKey((s) => !s)}>
                  {showKey ? '隐藏' : '显示'}
                </Button>
              </div>
            </Field>
          </div>
          <Field label="单账号明细默认展示条数" hint="积分构成页按最早到期展示，其余未用完包聚合">
            {num('package_detail_limit', { min: 1, placeholder: '5' })}
          </Field>
        </BoxBody>
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>定时任务</BoxTitle>
        </BoxHeader>
        <BoxBody>
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>
              {check('checkin_enabled', '自动签到')}
              <Field label="签到时点（小时，逗号分隔）">{text('checkin_hours', { placeholder: '9, 21' })}</Field>
            </div>
            <div>
              {check('keepalive_enabled', 'Token 保活')}
              <Field label="保活时点（小时，逗号分隔）">{text('keepalive_hours', { placeholder: '22' })}</Field>
            </div>
          </div>
          <Hr />
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>
              {check('travel_enabled', '猫猫旅行')}
              <Field label="旅行时点（小时，逗号分隔）" hint="一趟派出 + 一趟领奖闭环">
                {text('travel_hours', { placeholder: '9, 21' })}
              </Field>
            </div>
            <div>
              {check('activity_enabled', '活跃上报')}
              <Field label="上报时点（小时，逗号分隔）" hint="点亮连登 + 解锁领养前置">
                {text('activity_hours', { placeholder: '10' })}
              </Field>
            </div>
          </div>
          <Hr />
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>{check('balance_refresh_enabled', '后台刷新余额')}</div>
            <Field label="刷新间隔（分钟）">{num('balance_refresh_minutes', { min: 1, placeholder: '5' })}</Field>
          </div>
          <Hr />
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>{check('growth_enabled', '成长任务自动执行')}</div>
            <Field label="执行时点（小时，逗号分隔）" hint="每日到点自动「扫描 + 执行全部待办」（Sequential 任务链零点解锁后自动推进，建议零点后）">
              {text('growth_hours', { placeholder: '1' })}
            </Field>
          </div>
          <Hr />
          {check('include_disabled_in_tasks', '保号任务覆盖已禁用账号')}
          <div className="text-2xs text-ink-3">
            禁用只关选号：打开后已禁用的账号仍会签到 / 活跃上报 / 保活 / 刷新余额（依旧不参与选号）。适合「一次只放开一个账号、用禁用做流量开关」的轮换养号用法，默认关闭。
            <br />
            若只想让<b>单个</b>账号临时退出选号但保留保号，直接用账号行的「<b>暂停选号</b>」按钮（账号级，无需打开本开关）。
          </div>
        </BoxBody>
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>账号池与流量治理</BoxTitle>
        </BoxHeader>
        <BoxBody>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field label="单账号最大在途" hint="0 = 不限制">{num('max_in_flight', { min: 0, placeholder: '3' })}</Field>
            <Field label="国际版在途上限" hint="global 域风控更紧，默认 2">{num('max_in_flight_global', { min: 1, placeholder: '2' })}</Field>
            <Field label="连续失败熔断阈值">{num('breaker_threshold', { min: 1, placeholder: '3' })}</Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field label="软限流冷却基数">{text('soft_rate', { placeholder: '600s' })}</Field>
            <Field label="软冷却退避上限">{text('soft_rate_max', { placeholder: '2h' })}</Field>
            <Field label="熔断基础时长">{text('breaker_cooldown', { placeholder: '30m' })}</Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field label="熔断退避上限">{text('breaker_cooldown_max', { placeholder: '6h' })}</Field>
            <Field label="连败降权阈值" hint="未知错误连败 N 次临时出池">{num('degrade_threshold', { min: 1, placeholder: '5' })}</Field>
            <Field label="连败降权时长">{text('degrade_cooldown', { placeholder: '10m' })}</Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field label="连败降权上限">{text('degrade_cooldown_max', { placeholder: '2h' })}</Field>
            <Field label="闲置补偿 / 小时">{num('idle_weight_per_hour', { step: 0.1, placeholder: '0.5' })}</Field>
            <Field label="闲置补偿上限">{num('idle_weight_max', { step: 0.1, placeholder: '5' })}</Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field label="成本探索窗口" hint="垄断破除：免费层垄断时定期搭车探索未知号；0 关停">
              {text('cost_explore_interval', { placeholder: '30m' })}
            </Field>
            <Field label="积分保底" hint="余额低于此值不再接实测收费模型（保住免费模型可用）；0 关闭">
              {num('credit_floor', { min: 0, placeholder: '100' })}
            </Field>
            <Field label="快过期路由窗口" hint="窗口内有快过期批次的账号选号权重 ×3（不排序、与金额无关）；0 关闭">
              {text('expiring_soon', { placeholder: '168h' })}
            </Field>
            <Field label="会话粘性 TTL" hint="改动需重启进程">{text('ttl', { placeholder: '30m' })}</Field>
          </div>
          {check('prefer_expiring', '快过期积分优先')}
        </BoxBody>
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>上游与高级</BoxTitle>
        </BoxHeader>
        <BoxBody>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field label="短请求超时" hint="需重启">{num('timeout_seconds', { min: 1, placeholder: '120' })}</Field>
            <Field label="聊天首字节超时" hint="需重启">{num('header_timeout_seconds', { min: 1, placeholder: '120' })}</Field>
            <Field label="流空闲超时" hint="需重启">{num('idle_timeout_seconds', { min: 1, placeholder: '300' })}</Field>
          </div>
          <Hr />
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field label="出站 User-Agent" hint="影响官网积分记录「使用端」显示；需重启">
              {text('user_agent', { placeholder: '留空 = CLI/2.63.2 CodeBuddy/2.63.2' })}
            </Field>
          </div>
          <Hr />
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field label="系统提示词模式" hint="需重启">
              <Select value={String(values.prompt_mode ?? '')} onChange={(e) => set('prompt_mode', e.target.value)}>
                <option value="custom">custom — 网关自有提示词（避免指纹误报）</option>
                <option value="append">append — 客户端 system 后插网关提示词（并用）</option>
                <option value="passthrough">passthrough — 透传客户端原始 system</option>
              </Select>
            </Field>
            <Field label="提示词文件路径" hint="需重启">
              {text('prompt_file', { placeholder: '留空 = 内置默认提示词' })}
            </Field>
          </div>
          <Hr />
          {check('sanitize_blacklist_fingerprints', '出站请求指纹脱敏')}
          {check('session_sticky_enabled', '会话粘性路由')}
          <div className="mt-2.5 text-2xs text-ink-3">
            Upstash Redis 镜像、凭证目录与状态文件路径需手工编辑配置文件（判为重启项）。
          </div>
        </BoxBody>
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>日志</BoxTitle>
        </BoxHeader>
        <BoxBody>
          {check('request_client_info', '记录调用来源（客户端 IP / User-Agent）')}
          <div className="text-2xs text-ink-3">
            开启后，「运行日志」页的请求记录会显示每次调用来自哪个 IP、用什么客户端；关闭则归档与面板都不再出现来源信息。改动即时生效，无需重启。
          </div>
          <div className="mt-2 text-2xs text-ink-3">
            归档开关、保留天数与容量上限（<code>logging.request_archive_*</code>）需手工编辑配置文件。
          </div>
        </BoxBody>
      </Box>

      <div className="sticky bottom-0 flex items-center gap-3 border-t border-line-soft bg-surface-2 px-4 py-3">
        <Grow />
        {badFields.length ? <span className="text-xs text-warn">{DURATION_TIP}</span> : null}
        <span className="text-xs text-warn">{note}</span>
        <Button disabled={saving} onClick={() => void load()}>
          放弃修改
        </Button>
        <Button type="submit" variant="primary" disabled={saving}>
          {saving ? '保存中…' : '保存配置'}
        </Button>
      </div>
    </form>
  );
}

function Hr() {
  return <div className="my-1 mb-4 h-px bg-line-soft" />;
}
