import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api, LS_KEY, postJSON } from '@/lib/api';
import {
  CFG_FIELD_BY_NAME,
  collectConfig,
  configToValues,
  durationBad,
  DURATION_FIELDS,
  DURATION_TIP,
  type FieldValue,
} from '@/lib/config';
import { toast } from '@/components/Toaster';
import { useConfirm } from '@/components/ConfirmDialog';
import { Button } from '@/components/ui/button';
import { Field, FieldLabel, FieldDescription } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import {
  Select,
  SelectContent,
  SelectOptions,
  SelectTrigger,
  SelectValue,
  selectOptionItems,
  type SelectOption,
} from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import {
  Card,
  CardAction,
  CardContent,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Separator } from '@/components/ui/separator';
import { cn } from '@/lib/utils';

// 系统提示词注入模式。
const PROMPT_MODE_OPTIONS = [
  { value: 'custom', label: 'custom — 网关自有提示词（避免指纹误报）' },
  { value: 'append', label: 'append — 客户端 system 后插网关提示词（并用）' },
  { value: 'passthrough', label: 'passthrough — 透传客户端原始 system' },
] as const satisfies readonly SelectOption[];

export function ConfigView() {
  const qc = useQueryClient();
  const [values, setValues] = useState<Record<string, FieldValue>>({});
  const [path, setPath] = useState('');
  const [note, setNote] = useState('');
  const [showKey, setShowKey] = useState(false);
  const [saving, setSaving] = useState(false);
  const [restoring, setRestoring] = useState(false);
  const [clears, setClears] = useState<Record<string, boolean>>({});
  const { confirm, confirmDialog } = useConfirm();

  const load = async () => {
    try {
      const d = await api<{ path?: string; config?: unknown }>('config');
      setPath(d.path || '');
      setValues(configToValues(d.config));
      setClears({});
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
      const payload = collectConfig(values);
      // 显式清除的敏感字段：用空串覆盖，而不是走「空 = 不修改」。
      for (const [name, on] of Object.entries(clears)) {
        if (!on) continue;
        const f = CFG_FIELD_BY_NAME[name];
        if (f) {
          let o: Record<string, unknown> = payload;
          for (let i = 0; i < f.path.length - 1; i++) {
            const k = f.path[i]!;
            if (typeof o[k] !== 'object' || o[k] === null) o[k] = {};
            o = o[k] as Record<string, unknown>;
          }
          o[f.path[f.path.length - 1]!] = '';
        }
      }
      const r = await postJSON<{ restart_required?: string[] }>('config', payload);
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

  const restore = async () => {
    if (
      !(await confirm({
        title: '恢复上一版配置？',
        description: '用 config.json.1 覆盖当前配置，当前配置不会自动再备份。',
        confirmText: '恢复',
        destructive: true,
      }))
    )
      return;
    setRestoring(true);
    try {
      const r = await postJSON<{ restart_required?: string[] }>('config/restore');
      const n = (r.restart_required || []).length;
      toast(n ? '已恢复上一版，其中 ' + n + ' 项需重启进程生效' : '已恢复上一版并立即生效', 'ok');
      await load();
      await qc.invalidateQueries({ queryKey: ['overview'] });
    } catch (e) {
      toast('恢复失败：' + (e as Error).message, 'err');
    } finally {
      setRestoring(false);
    }
  };

  const text = (name: string, props: React.ComponentProps<typeof Input> = {}) => (
    <Input
      value={String(values[name] ?? '')}
      onChange={(e) => set(name, e.target.value)}
      aria-invalid={DURATION_FIELDS.has(name) && durationBad(String(values[name] ?? ''))}
      className={cn(DURATION_FIELDS.has(name) && durationBad(String(values[name] ?? '')) && 'border-destructive bg-destructive-soft')}
      title={DURATION_FIELDS.has(name) && durationBad(String(values[name] ?? '')) ? DURATION_TIP : undefined}
      {...props}
    />
  );
  const num = (name: string, props: React.ComponentProps<typeof Input> = {}) =>
    text(name, { type: 'number', ...props });
  const check = (name: string, label: React.ReactNode, hint?: string) => (
    <Field orientation="horizontal">
      <FieldLabel className="flex-1">
        <span>{label}</span>
        {hint ? <FieldDescription className="mt-0.5">{hint}</FieldDescription> : null}
      </FieldLabel>
      <Switch
        checked={!!values[name]}
        onCheckedChange={(v) => set(name, v)}
        aria-label={typeof label === 'string' ? label : name}
      />
    </Field>
  );

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <Card className="mb-5">
        <CardHeader className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle>服务</CardTitle>
          <CardAction>
            <span className="truncate text-xs text-muted-foreground">{path}</span>
          </CardAction>
        </CardHeader>
        <CardContent>
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field>
              <FieldLabel>监听地址</FieldLabel>
              {text('listen', { placeholder: ':7863' })}
              <FieldDescription>改动需重启进程</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>API 密钥</FieldLabel>
              <div className="flex items-center gap-2">
                {text('api_key', { type: showKey ? 'text' : 'password', autoComplete: 'new-password', placeholder: '留空 = 不鉴权' })}
                <Button size="xs" onClick={() => setShowKey((s) => !s)}>
                  {showKey ? '隐藏' : '显示'}
                </Button>
              </div>
              <FieldDescription>立即生效（含面板自身）</FieldDescription>
            </Field>
          </div>
          <Field>
            <FieldLabel>单账号明细默认展示条数</FieldLabel>
            {num('package_detail_limit', { min: 1, placeholder: '5' })}
            <FieldDescription>积分构成页按最早到期展示，其余未用完包聚合</FieldDescription>
          </Field>
          <Field>
            <FieldLabel>聊天请求体上限（字节）</FieldLabel>
            {num('max_body_bytes', { min: 0, placeholder: '33554432' })}
            <FieldDescription>覆盖 chat / messages / count_tokens；超限返回 413。0 = 不限制。改动需重启进程</FieldDescription>
          </Field>
        </CardContent>
      </Card>

      <Card className="mb-5">
        <CardHeader>
          <CardTitle>定时任务</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>
              {check('checkin_enabled', '自动签到')}
              <Field>
                <FieldLabel>签到时点（小时，逗号分隔）</FieldLabel>
                {text('checkin_hours', { placeholder: '9, 21' })}
              </Field>
            </div>
            <div>
              {check('keepalive_enabled', 'Token 保活')}
              <Field>
                <FieldLabel>保活时点（小时，逗号分隔）</FieldLabel>
                {text('keepalive_hours', { placeholder: '22' })}
              </Field>
            </div>
          </div>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>
              {check('travel_enabled', '猫猫旅行')}
              <Field>
                <FieldLabel>旅行时点（小时，逗号分隔）</FieldLabel>
                {text('travel_hours', { placeholder: '9, 21' })}
                <FieldDescription>一趟派出 + 一趟领奖闭环</FieldDescription>
              </Field>
            </div>
            <div>
              {check('activity_enabled', '活跃上报')}
              <Field>
                <FieldLabel>上报时点（小时，逗号分隔）</FieldLabel>
                {text('activity_hours', { placeholder: '10' })}
                <FieldDescription>点亮连登 + 解锁领养前置</FieldDescription>
              </Field>
            </div>
          </div>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>{check('balance_refresh_enabled', '后台刷新余额')}</div>
            <Field>
              <FieldLabel>刷新间隔（分钟）</FieldLabel>
              {num('balance_refresh_minutes', { min: 1, placeholder: '5' })}
            </Field>
          </div>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>{check('growth_enabled', '成长任务自动执行')}</div>
            <Field>
              <FieldLabel>执行时点（小时，逗号分隔）</FieldLabel>
              {text('growth_hours', { placeholder: '1' })}
              <FieldDescription>每日到点自动「扫描 + 执行全部待办」（Sequential 任务链零点解锁后自动推进，建议零点后）</FieldDescription>
            </Field>
          </div>
          <Separator className="my-4" />
          {check('include_disabled_in_tasks', '保号任务覆盖已禁用账号')}
          <p className="text-2xs text-muted-foreground">
            禁用只关选号：打开后已禁用的账号仍会签到 / 活跃上报 / 保活 / 刷新余额（依旧不参与选号）。适合「一次只放开一个账号、用禁用做流量开关」的轮换养号用法，默认关闭。
            <br />
            若只想让<b>单个</b>账号临时退出选号但保留保号，直接用账号行的「<b>暂停选号</b>」按钮（账号级，无需打开本开关）。
          </p>
        </CardContent>
      </Card>

      <Card className="mb-5">
        <CardHeader>
          <CardTitle>账号池与流量治理</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field>
              <FieldLabel>单账号最大在途</FieldLabel>
              {num('max_in_flight', { min: 0, placeholder: '3' })}
              <FieldDescription>0 = 不限制</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>国际版在途上限</FieldLabel>
              {num('max_in_flight_global', { min: 1, placeholder: '2' })}
              <FieldDescription>global 域风控更紧，默认 2</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>全局在途上限</FieldLabel>
              {num('max_in_flight_total', { min: 0, placeholder: '0' })}
              <FieldDescription>整个实例的总并发上限；0 = 不限。热生效</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>连续失败熔断阈值</FieldLabel>
              {num('breaker_threshold', { min: 1, placeholder: '3' })}
            </Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field>
              <FieldLabel>软限流冷却基数</FieldLabel>
              {text('soft_rate', { placeholder: '600s' })}
            </Field>
            <Field>
              <FieldLabel>软冷却退避上限</FieldLabel>
              {text('soft_rate_max', { placeholder: '2h' })}
            </Field>
            <Field>
              <FieldLabel>熔断基础时长</FieldLabel>
              {text('breaker_cooldown', { placeholder: '30m' })}
            </Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field>
              <FieldLabel>熔断退避上限</FieldLabel>
              {text('breaker_cooldown_max', { placeholder: '6h' })}
            </Field>
            <Field>
              <FieldLabel>连败降权阈值</FieldLabel>
              {num('degrade_threshold', { min: 1, placeholder: '5' })}
              <FieldDescription>未知错误连败 N 次临时出池</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>连败降权时长</FieldLabel>
              {text('degrade_cooldown', { placeholder: '10m' })}
            </Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field>
              <FieldLabel>连败降权上限</FieldLabel>
              {text('degrade_cooldown_max', { placeholder: '2h' })}
            </Field>
            <Field>
              <FieldLabel>闲置补偿 / 小时</FieldLabel>
              {num('idle_weight_per_hour', { step: 0.1, placeholder: '0.5' })}
            </Field>
            <Field>
              <FieldLabel>闲置补偿上限</FieldLabel>
              {num('idle_weight_max', { step: 0.1, placeholder: '5' })}
            </Field>
          </div>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field>
              <FieldLabel>成本探索窗口</FieldLabel>
              {text('cost_explore_interval', { placeholder: '30m' })}
              <FieldDescription>垄断破除：免费层垄断时定期搭车探索未知号；0 关停</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>积分保底</FieldLabel>
              {num('credit_floor', { min: 0, placeholder: '100' })}
              <FieldDescription>余额低于此值不再接实测收费模型（保住免费模型可用）；0 关闭</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>每日 Token 上限</FieldLabel>
              {num('daily_tokens', { min: 0, placeholder: '0' })}
              <FieldDescription>账号当日总 token 达限后不接单；0 = 关闭。本地 0 点重置</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>每日积分上限</FieldLabel>
              {num('daily_credits', { min: 0, step: 0.1, placeholder: '0' })}
              <FieldDescription>达限后只接免费模型；0 = 关闭</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>按模型每日 Token</FieldLabel>
              {num('model_daily_tokens', { min: 0, placeholder: '0' })}
              <FieldDescription>账号 × 模型当日 token 达限后只禁该模型；0 = 关闭</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>快过期路由窗口</FieldLabel>
              {text('expiring_soon', { placeholder: '168h' })}
              <FieldDescription>窗口内有快过期批次的账号选号权重 ×3（不排序、与金额无关）；0 关闭</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>会话粘性 TTL</FieldLabel>
              {text('ttl', { placeholder: '30m' })}
              <FieldDescription>改动需重启进程</FieldDescription>
            </Field>
          </div>
          {check('prefer_expiring', '快过期积分优先')}
        </CardContent>
      </Card>

      <Card className="mb-5">
        <CardHeader>
          <CardTitle>上游与高级</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid gap-x-4 md:grid-cols-3">
            <Field>
              <FieldLabel>短请求超时</FieldLabel>
              {num('timeout_seconds', { min: 1, placeholder: '120' })}
              <FieldDescription>需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>聊天首字节超时</FieldLabel>
              {num('header_timeout_seconds', { min: 1, placeholder: '1200' })}
              <FieldDescription>需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>流空闲超时</FieldLabel>
              {num('idle_timeout_seconds', { min: 1, placeholder: '1200' })}
              <FieldDescription>需重启</FieldDescription>
            </Field>
          </div>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field>
              <FieldLabel>出站 User-Agent</FieldLabel>
              {text('user_agent', { placeholder: '留空 = 内置三段式 UA' })}
              <FieldDescription>影响官网积分记录「使用端」显示；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>客户端版本</FieldLabel>
              {text('client_version', { placeholder: '留空 = 按 realm 内置默认' })}
              <FieldDescription>UA 的 WorkBuddy/&lt;ver&gt; 段；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>CLI 版本</FieldLabel>
              {text('cli_version', { placeholder: '留空 = 按 realm 内置默认' })}
              <FieldDescription>UA 的 CLI/&lt;ver&gt; 段；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>用量归属名</FieldLabel>
              {text('client_name', { placeholder: '留空 = WorkBuddy / SaaS' })}
              <FieldDescription>X-Product 与 X-IDE-Name/Type 跟随该值；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>设备 Token</FieldLabel>
              <div className="flex items-center gap-2">
                {text('device_token', { type: 'password', placeholder: clears.device_token ? '保存后清除' : '留空 = 不修改' })}
                <Button
                  type="button"
                  size="xs"
                  variant={clears.device_token ? 'destructive' : 'ghost'}
                  onClick={() => setClears((prev) => ({ ...prev, device_token: !prev.device_token }))}
                >
                  {clears.device_token ? '取消清除' : '清除'}
                </Button>
              </div>
              <FieldDescription>已设置时留空保持不变；点「清除」保存后置空；账号级 token 优先；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>设备 Token 文件</FieldLabel>
              {text('device_token_file', { placeholder: '留空 = 不读取文件' })}
              <FieldDescription>桌面端落盘 token 路径，5 分钟读取缓存；需重启</FieldDescription>
            </Field>
          </div>
          {check('passthrough_ip', '向上游透传客户端 IP', '默认关闭；开启前确认反代信任边界，需重启')}
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-3">
            <Field>
              <FieldLabel>单请求最多换号</FieldLabel>
              {num('rotate_max', { min: 1, placeholder: '3' })}
              <FieldDescription>需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>轮换总时长预算</FieldLabel>
              {text('rotate_budget', { placeholder: '0 = 不限' })}
              <FieldDescription>如 90s；与换号次数先到先停；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>触发换号的状态码</FieldLabel>
              {text('rotate_status_codes', { placeholder: '留空 = 按错误分类' })}
              <FieldDescription>逗号分隔，如 429,500,502；需重启</FieldDescription>
            </Field>
          </div>
          <Field>
            <FieldLabel>出站代理列表</FieldLabel>
            {text('proxies', { placeholder: 'http://user:pass@host:port, socks5://host:1080' })}
            <FieldDescription>逗号分隔；非空按健康轮换，失败自动冷却；需重启</FieldDescription>
          </Field>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-3">
            <Field>
              <FieldLabel>国际版路由</FieldLabel>
              {check('global_enabled', '启用 global realm')}
              <FieldDescription>关闭后不路由 global 账号与 global: 模型；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>国际版 Chat Base</FieldLabel>
              {text('global_chat_base', { placeholder: '留空 = 内置默认' })}
              <FieldDescription>需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>国际版 Billing Base</FieldLabel>
              {text('global_billing_base', { placeholder: '留空 = 内置默认' })}
              <FieldDescription>需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>国内版 Chat Base</FieldLabel>
              {text('cn_chat_base', { placeholder: '留空 = 内置默认 + 回落链' })}
              <FieldDescription>非空时只用该 base、不回落官方域；需重启</FieldDescription>
            </Field>
          </div>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field>
              <FieldLabel>Upstash URL</FieldLabel>
              {text('upstash_url', { placeholder: 'rediss://... 或 https://xxx.upstash.io' })}
              <FieldDescription>留空 = 纯内存模式；需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>Upstash Token</FieldLabel>
              {text('upstash_token', { type: 'password', placeholder: '留空 = 不修改' })}
              <FieldDescription>已设置时留空保持不变；需重启</FieldDescription>
            </Field>
          </div>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field>
              <FieldLabel>后台任务模型名单</FieldLabel>
              {text('background_names', { placeholder: '如 gpt-5-background, memory-gen' })}
              <FieldDescription>逗号分隔、大小写不敏感；命中后改写到目标模型。留空 = 关闭</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>后台任务改写目标</FieldLabel>
              {text('background_target', { placeholder: 'deepseek-v4.1-flash' })}
              <FieldDescription>只改 model 字段，其余请求原样透传；热生效</FieldDescription>
            </Field>
          </div>
          <Separator className="my-4" />
          <div className="grid gap-x-5 md:grid-cols-2">
            <Field>
              <FieldLabel>系统提示词模式</FieldLabel>
              <Select
                items={selectOptionItems(PROMPT_MODE_OPTIONS)}
                value={String(values.prompt_mode ?? 'custom')}
                onValueChange={(v) => set('prompt_mode', v ?? 'custom')}>
                <SelectTrigger aria-label="系统提示词模式">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectOptions options={PROMPT_MODE_OPTIONS} />
                </SelectContent>
              </Select>
              <FieldDescription>需重启</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>提示词文件路径</FieldLabel>
              {text('prompt_file', { placeholder: '留空 = 内置默认提示词' })}
              <FieldDescription>需重启</FieldDescription>
            </Field>
          </div>
          <Separator className="my-4" />
          {check('sanitize_blacklist_fingerprints', '出站请求指纹脱敏')}
          {check('multimodal_routing', '多模态按能力路由（不匹配时改选支持该模态的模型；关闭则返回 400）')}
          <div className="grid gap-x-5 md:grid-cols-2">
            <div>
              {check('local_web_tools', 'Responses 本地代跑 web_search / web_fetch', '开启后每个工具轮次都会额外消耗一次上游额度')}
            </div>
            <Field>
              <FieldLabel>本地工具最多轮数</FieldLabel>
              {num('local_web_tool_rounds', { min: 1, max: 8, placeholder: '3' })}
              <FieldDescription>默认 3，封顶 8；仅对 /v1/responses 生效</FieldDescription>
            </Field>
          </div>
          {check('session_sticky_enabled', '会话粘性路由')}
          <p className="mt-2.5 text-2xs text-muted-foreground">
            本卡片字段均在启动期装配，保存后需重启进程；未修改的敏感值（设备 Token / Upstash Token）留空即可保留。
          </p>
        </CardContent>
      </Card>

      <Card className="mb-5">
        <CardHeader>
          <CardTitle>日志</CardTitle>
        </CardHeader>
        <CardContent>
          {check('request_client_info', '记录调用来源（客户端 IP / User-Agent）')}
          <p className="text-2xs text-muted-foreground">
            开启后，「运行日志」页的请求记录会显示每次调用来自哪个 IP、用什么客户端；关闭则归档与面板都不再出现来源信息。改动即时生效，无需重启。
          </p>
          <p className="mt-2 text-2xs text-muted-foreground">
            归档开关、保留天数与容量上限（<code>logging.request_archive_*</code>）需手工编辑配置文件。
          </p>
        </CardContent>
      </Card>

      <div className="sticky bottom-0 flex items-center gap-3 border-t border-border bg-secondary px-4 py-3">
        <span className="flex-1" />
        {badFields.length ? <span className="text-xs text-warning">{DURATION_TIP}</span> : null}
        <span className="text-xs text-warning">{note}</span>
        <Button variant="outline" disabled={saving || restoring} onClick={() => void restore()}>
          {restoring ? '恢复中…' : '恢复上一版'}
        </Button>
        <Button disabled={saving || restoring} onClick={() => void load()}>
          放弃修改
        </Button>
        <Button type="submit" variant="default" disabled={saving || restoring}>
          {saving ? '保存中…' : '保存配置'}
        </Button>
      </div>
      {confirmDialog}
    </form>
  );
}
