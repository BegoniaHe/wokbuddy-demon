#!/usr/bin/env node
// runtime_probe.mjs 官方 WorkBuddy 客户端运行时探针（opt-in）。
//
// 目标：在用户明确授权、使用测试账号的前提下，隔离采集运行时证据：
//   - Electron renderer 的 CDP Network 请求（白名单头，URL 去 query）
//   - Chromium NetLog 中的端点 URL（默认采集后删除原始文件）
//   - 客户端日志中的 chat/report/qimei 事件（只提取字段，不复制正文）
//   - 可选 tcpdump 抓包（提权只走 pkexec，向用户弹出 polkit 授权）
//
// 隔离规则：
//   - 所有产物只写 --out（默认 .tmp/workbuddy-runtime/<realm>-<时间>），.tmp 已 gitignore
//   - 默认不复制 auth 文件、不读取凭证、不记录 Authorization/Cookie/X-Userinfo
//   - 默认不发消息；--send 必须同时给 --confirm-spend
//   - 默认使用现有 WINEPREFIX；--wineprefix 可指定隔离 prefix
//   - --isolated-user-data 用临时 --user-data-dir（未登录，只验证启动/公开端点）
//
// 不依赖第三方包；Node >= 22（全局 WebSocket、fetch）。

import { spawn } from 'node:child_process';
import { closeSync, createReadStream, existsSync, mkdirSync, openSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { createInterface } from 'node:readline';
import { createServer } from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// 从脚本位置向上找仓库根，保证从任意 cwd 运行时 .tmp/ 仍指向仓库内的路径。
function findRepoRoot() {
  let dir = path.dirname(fileURLToPath(import.meta.url));
  for (let i = 0; i < 8; i += 1) {
    if (existsSync(path.join(dir, 'go.mod')) || existsSync(path.join(dir, '.git'))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return process.cwd();
}

const REPO_ROOT = findRepoRoot();

const HELP = `用法：
  node scripts/runtime_probe.mjs --realm cn|global [选项]

选项：
  --realm <cn|global>        必填；选择 .tmp/workbuddy/<realm> 下的包
  --version <ver>            指定版本目录；缺省取最新
  --package-dir <dir>        直接指定 win32-app 目录（内含 WorkBuddy.exe）
  --out <dir>                产物目录；默认 .tmp/workbuddy-runtime/<realm>-<ts>
  --wineprefix <dir>         指定 WINEPREFIX；缺省沿用当前环境或 ~/.wine
  --isolated-user-data       使用临时 --user-data-dir（不读取现有登录态）
  --capture                  通过 pkexec 启动 tcpdump（会弹出提权授权）
  --capture-seconds <n>      抓包时长，默认 30
  --send <text>              通过 CDP 发送一条消息（需 --confirm-spend）
  --confirm-spend            确认发送会消耗测试账号额度
  --reload                   采集期间重载 renderer（会触发启动请求）
  --duration <n>             CDP 采集时长，默认 15 秒
  --keep-raw                 保留 netlog.json 与 runtime.log（可能含敏感元数据）
  --dry-run                  只打印将执行的命令与路径，不启动客户端
  --help                     显示本帮助

边界：
  - 不主动登录、默认不发消息；需要 chat 证据时由用户显式 --send + --confirm-spend
  - 提权仅使用 pkexec；脚本不读取、不保存密码，不调用 sudo
  - 原始 NetLog / stdout / pcap 可能含敏感网络元数据，默认不提交、不复制到仓库
`;

const argv = process.argv.slice(2);
if (argv.includes('--help') || argv.length === 0) {
  process.stdout.write(HELP);
  process.exit(0);
}

function opt(name, fallback) {
  const i = argv.indexOf(name);
  if (i < 0) return fallback;
  const value = argv[i + 1];
  if (!value || value.startsWith('--')) throw new Error(`${name} 需要参数`);
  return value;
}

function flag(name) {
  return argv.includes(name);
}

function toWin(p) {
  return 'Z:' + path.resolve(p).replace(/\//g, '\\');
}

function shellQuote(s) {
  return `'${String(s).replace(/'/g, `'\\''`)}'`;
}

function stamp() {
  const d = new Date();
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

function findPackage(realm, version) {
  if (argv.includes('--package-dir')) {
    return path.resolve(opt('--package-dir'));
  }
  const base = path.join(REPO_ROOT, '.tmp', 'workbuddy', realm);
  if (!existsSync(base)) throw new Error(`找不到 ${base}，先运行 download_latest.mjs 并解包`);
  if (version) return path.join(base, version, 'win32-app');
  const dirs = readdirSync(base)
    .map((name) => ({ name, mtime: statSync(path.join(base, name)).mtimeMs }))
    .sort((a, b) => b.mtime - a.mtime);
  for (const d of dirs) {
    const candidate = path.join(base, d.name, 'win32-app');
    if (existsSync(path.join(candidate, 'WorkBuddy.exe'))) return candidate;
  }
  throw new Error(`${base} 下没有 win32-app/WorkBuddy.exe`);
}

function freePort(start = 9222) {
  return new Promise((resolve, reject) => {
    let p = start;
    const tryNext = () => {
      if (p > 9250) {
        reject(new Error('9222-9250 无空闲端口'));
        return;
      }
      const srv = createServer();
      srv.once('error', () => {
        p += 1;
        setImmediate(tryNext);
      });
      srv.once('listening', () => {
        const port = p;
        srv.close(() => resolve(port));
      });
      srv.listen(p, '127.0.0.1');
    };
    tryNext();
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitForCdp(port, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/json/version`, { signal: AbortSignal.timeout(1000) });
      if (r.ok) return true;
    } catch {}
    await sleep(500);
  }
  return false;
}

const ALLOW_HEADER = /^(user-agent|origin|referer|accept|accept-language|accept-encoding|content-type|x-[a-z0-9-]+)$/i;
const DENY_HEADER = /authorization|cookie|token|refresh|userinfo|api-key|secret|user-id/i;

function safeUrl(raw) {
  try {
    const u = new URL(raw);
    return u.origin + u.pathname;
  } catch {
    return String(raw).split('?')[0];
  }
}

function isTargetUrl(u) {
  return /(workbuddy|codebuddy)\.(cn|ai|com)/i.test(u) || /copilot\.tencent\.com|tencent\.sso/i.test(u);
}

const SENSITIVE_ID_HEADER = /^(x-(conversation|request|root|parent)|x-b3-|traceparent$|b3$)/i;

function pickHeaders(headers) {
  const out = {};
  for (const [k, v] of Object.entries(headers || {})) {
    if (!ALLOW_HEADER.test(k) || DENY_HEADER.test(k)) continue;
    // 会话/请求/追踪 ID 只保留存在性与格式长度，避免把可关联账号的标识写进摘要。
    if (SENSITIVE_ID_HEADER.test(k)) {
      const s = Array.isArray(v) ? v.join(', ') : String(v);
      out[k] = `<redacted:${s.length}>`;
      continue;
    }
    out[k] = Array.isArray(v) ? v.join(', ') : String(v);
  }
  return out;
}

async function cdpJson(port, route) {
  const r = await fetch(`http://127.0.0.1:${port}${route}`, { signal: AbortSignal.timeout(3000) });
  return r.json();
}

async function collectCdp(port, requestsPath, { reload, duration, sendText }) {
  const targets = await cdpJson(port, '/json/list');
  const page = targets.find((t) => t.type === 'page');
  if (!page) throw new Error('CDP 中没有 page target');
  if (typeof WebSocket !== 'function') throw new Error('当前 Node 缺少全局 WebSocket，请使用 Node >= 22');

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  const lines = [];
  let seq = 0;
  const pending = new Map();

  const send = (method, params = {}) => {
    const id = ++seq;
    return new Promise((resolve) => {
      pending.set(id, resolve);
      ws.send(JSON.stringify({ id, method, params }));
    });
  };

  ws.addEventListener('message', (event) => {
    let msg;
    try {
      msg = JSON.parse(event.data);
    } catch {
      return;
    }
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
      return;
    }
    if (msg.method === 'Network.requestWillBeSent') {
      const p = msg.params;
      if (!isTargetUrl(p.request.url)) return;
      lines.push(JSON.stringify({
        method: p.request.method,
        url: safeUrl(p.request.url),
        type: p.type,
        headers: pickHeaders(p.request.headers),
      }));
    }
  });

  await new Promise((resolve, reject) => {
    ws.addEventListener('open', resolve, { once: true });
    ws.addEventListener('error', () => reject(new Error('CDP WebSocket 连接失败')), { once: true });
  });

  await send('Network.enable');
  await send('Page.enable');
  if (reload) await send('Page.reload', { ignoreCache: false });

  if (sendText) {
    await sleep(1500);
    const focus = await send('Runtime.evaluate', {
      expression: `document.querySelector('[contenteditable="true"]')?.focus(); !!document.querySelector('[contenteditable="true"]')`,
      returnByValue: true,
      userGesture: true,
    });
    if (!focus.result?.result?.value) {
      ws.close();
      throw new Error('页面上没有可编辑输入框（未登录或界面未就绪？）');
    }
    await send('Input.insertText', { text: sendText });
    await sleep(300);
    const clicked = await send('Runtime.evaluate', {
      expression: `(()=>{const b=[...document.querySelectorAll('button')].find(b=>(b.getAttribute('aria-label')||'')==='发送');if(!b)return 'no-send';b.click();return 'clicked'})()`,
      returnByValue: true,
      userGesture: true,
    });
    if (clicked.result?.result?.value !== 'clicked') {
      ws.close();
      throw new Error('找不到发送按钮（界面语言/布局变化？）');
    }
  }

  await sleep(duration * 1000);
  ws.close();
  writeFileSync(requestsPath, lines.join('\n') + (lines.length ? '\n' : ''));
  return lines.length;
}

function extractFromLine(line) {
  const out = {};
  const url = line.match(/url=(https?:\/\/[^\s,]+)/i) || line.match(/(https?:\/\/[^\s,"]+chat\/completions[^\s,"]*)/i);
  if (url) out.url = safeUrl(url[1]);
  const model = line.match(/model=([^,\s]+)/);
  if (model) out.model = model[1];
  const agent = line.match(/agent=([^,\s]+)/);
  if (agent) out.agent = agent[1];
  if (/QimeiFork/.test(line)) out.event = 'QimeiFork';
  if (/QimeiUpdate/.test(line)) out.event = 'QimeiUpdate';
  return out;
}

function recentLogFiles(prefix, sinceMs, limit = 30) {
  const users = path.join(prefix, 'drive_c', 'users');
  if (!existsSync(users)) return [];
  const files = [];
  for (const user of readdirSync(users)) {
    const logs = path.join(users, user, '.workbuddy', 'logs');
    if (!existsSync(logs)) continue;
    walk(logs, files);
  }
  return files
    .map((p) => {
      try {
        return { p, mtime: statSync(p).mtimeMs };
      } catch {
        return null;
      }
    })
    .filter((f) => f && f.mtime >= sinceMs)
    .sort((a, b) => b.mtime - a.mtime)
    .slice(0, limit)
    .map((f) => f.p);
}

function walk(dir, out) {
  for (const name of readdirSync(dir)) {
    const p = path.join(dir, name);
    let st;
    try {
      st = statSync(p);
    } catch {
      continue;
    }
    if (st.isDirectory()) {
      if (/sandbox|Crashpad|perf|editor_sdk/i.test(name)) continue;
      walk(p, out);
    } else if (name.endsWith('.log')) {
      out.push(p);
    }
  }
}

async function scanClientLogs(files, maxLines = 200000) {
  const found = new Map();
  for (const file of files) {
    let n = 0;
    const rl = createInterface({ input: createReadStream(file, 'utf8'), crlfDelay: Infinity });
    for await (const line of rl) {
      if (++n > maxLines) break;
      if (!/chat\/completions|\/v2\/report|QimeiFork|QimeiUpdate/i.test(line)) continue;
      const hit = extractFromLine(line);
      if (!Object.keys(hit).length) continue;
      const key = JSON.stringify(hit);
      if (!found.has(key)) found.set(key, { file: path.basename(file), ...hit });
      if (found.size >= 50) {
        rl.close();
        return [...found.values()];
      }
    }
  }
  return [...found.values()];
}

async function netlogUrls(file) {
  if (!existsSync(file)) return [];
  const urls = new Set();
  const rl = createInterface({ input: createReadStream(file, 'utf8'), crlfDelay: Infinity });
  for await (const line of rl) {
    for (const m of line.matchAll(/"url":"(https?:[^"]+)"/g)) {
      if (!isTargetUrl(m[1])) continue;
      urls.add(safeUrl(m[1]));
      if (urls.size >= 100) {
        rl.close();
        return [...urls];
      }
    }
  }
  return [...urls];
}

function startCapture(outDir, seconds) {
  const pcap = path.join(outDir, 'traffic.pcap');
  const uid = process.getuid?.();
  const gid = process.getgid?.();
  if (uid == null || gid == null) throw new Error('仅支持 Unix 环境下的 pkexec 提权');
  const script = [
    `timeout ${Number(seconds)} tcpdump -i any -n -s 0 -w ${shellQuote(pcap)} 'port 443 or port 53 or port 80'`,
    `rc=$?`,
    `if [ -f ${shellQuote(pcap)} ]; then chown ${uid}:${gid} ${shellQuote(pcap)}; chmod 600 ${shellQuote(pcap)}; fi`,
    `exit $rc`,
  ].join('; ');
  const child = spawn('pkexec', ['sh', '-c', script], {
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, DISPLAY: process.env.DISPLAY, XAUTHORITY: process.env.XAUTHORITY },
  });
  return { child, pcap };
}

async function main() {
  const realm = opt('--realm', '');
  if (realm !== 'cn' && realm !== 'global') throw new Error('--realm 必须是 cn 或 global');
  const packageDir = findPackage(realm, opt('--version', ''));
  const exe = path.join(packageDir, 'WorkBuddy.exe');
  if (!existsSync(exe)) throw new Error(`找不到 ${exe}`);

  const outDir = path.resolve(opt('--out', path.join(REPO_ROOT, '.tmp', 'workbuddy-runtime', `${realm}-${stamp()}`)));

  const prefix = path.resolve(opt('--wineprefix', process.env.WINEPREFIX || path.join(os.homedir(), '.wine')));
  const isolatedUserData = flag('--isolated-user-data');
  const userDataDir = isolatedUserData ? path.join(outDir, 'user-data') : '';
  const netlog = path.join(outDir, 'netlog.json');
  const runtimeLog = path.join(outDir, 'runtime.log');
  const requestsPath = path.join(outDir, 'requests.jsonl');
  const summaryPath = path.join(outDir, 'summary.json');
  const sendText = opt('--send', '');
  const duration = Number(opt('--duration', '15'));
  const captureSeconds = Number(opt('--capture-seconds', '30'));
  const keepRaw = flag('--keep-raw');

  if (!Number.isFinite(duration) || duration <= 0) throw new Error('--duration 必须是正数');
  if (!Number.isFinite(captureSeconds) || captureSeconds <= 0) {
    throw new Error('--capture-seconds 必须是正数');
  }
  if (sendText && !flag('--confirm-spend')) {
    throw new Error('--send 会消耗测试账号额度，必须同时加 --confirm-spend');
  }
  if (sendText && isolatedUserData) {
    throw new Error('--send 需要已登录界面；--isolated-user-data 下无法发送');
  }

  const port = await freePort();
  const args = [
    `--remote-debugging-port=${port}`,
    '--no-sandbox',
    '--disable-gpu',
    '--disable-dev-shm-usage',
    `--log-net-log=${toWin(netlog)}`,
    '--net-log-capture-mode=Default',
  ];
  if (isolatedUserData) args.push(`--user-data-dir=${toWin(userDataDir)}`);

  const env = {
    ...process.env,
    WINEDEBUG: '-all',
    WINEPREFIX: prefix,
  };

  if (flag('--dry-run')) {
    console.log(JSON.stringify({
      packageDir, exe, outDir, prefix, isolatedUserData, userDataDir,
      command: ['wine', exe, ...args], capture: flag('--capture'), send: !!sendText,
    }, null, 2));
    return;
  }

  console.log(`[probe] package=${packageDir}`);
  console.log(`[probe] out=${outDir}`);
  console.log(`[probe] WINEPREFIX=${prefix}${isolatedUserData ? ' (isolated user-data)' : ''}`);
  console.log(`[probe] 边界：不主动登录、默认不发消息；提权仅 pkexec`);

  mkdirSync(outDir, { recursive: true });
  const logFd = openSync(runtimeLog, 'w');
  const startedAt = Date.now();

  let capture = null;
  if (flag('--capture')) {
    console.log(`[probe] 通过 pkexec 请求抓包授权（${captureSeconds}s）`);
    capture = startCapture(outDir, captureSeconds);
  }

  const warnings = [];
  const child = spawn('wine', [exe, ...args], {
    cwd: packageDir,
    env,
    detached: true,
    stdio: ['ignore', logFd, logFd],
  });
  const childFailed = new Promise((resolve) => child.once('error', resolve));
  childFailed.then(() => warnings.push('客户端启动失败（wine 不可用或 WorkBuddy.exe 无法执行）'));

  const stop = async () => {
    try { process.kill(-child.pid, 'SIGTERM'); } catch {}
    await sleep(2500);
    try { process.kill(-child.pid, 'SIGKILL'); } catch {}
  };

  const onSignal = async () => {
    await stop();
    process.exit(130);
  };
  process.once('SIGINT', onSignal);
  process.once('SIGTERM', onSignal);

  let cdpCount = 0;
  try {
    const cdpReady = await Promise.race([waitForCdp(port, 60000), childFailed.then(() => false)]);
    if (!cdpReady) {
      warnings.push('CDP 未就绪（客户端可能因 Wine/登录态失败）');
    } else {
      cdpCount = await collectCdp(port, requestsPath, {
        reload: flag('--reload'),
        duration,
        sendText,
      });
    }
  } catch (err) {
    warnings.push(`CDP 采集失败：${err.message}`);
  } finally {
    await stop();
  }

  let captureStatus = 'skipped';
  if (capture) {
    const code = await new Promise((resolve) => {
      capture.child.once('close', (c) => resolve(c));
      capture.child.once('error', () => resolve(-1));
    });
    // timeout 在达到指定时长后以 124 退出，这正是抓包正常结束的路径。
    if (code === 0 || code === 124) captureStatus = 'ok';
    else if (code === 126 || code === 127) captureStatus = `pkexec-denied-or-missing(${code})`;
    else captureStatus = `failed(${code})`;
    if (captureStatus !== 'ok') warnings.push(`抓包未完成：${captureStatus}（pkexec 授权被拒或 tcpdump 不可用）`);
  }

  const netlogList = await netlogUrls(netlog);
  const logFiles = recentLogFiles(prefix, startedAt - 15 * 60 * 1000);
  const clientHits = await scanClientLogs(logFiles);

  const summary = {
    generatedAt: new Date().toISOString(),
    realm,
    packageDir,
    outDir,
    isolatedUserData,
    sentMessage: !!sendText,
    cdpRequests: cdpCount,
    netlogUrls: netlogList,
    clientLogMatches: clientHits,
    capture: { status: captureStatus, pcap: captureStatus === 'ok' ? capture.pcap : null },
    warnings,
  };
  writeFileSync(summaryPath, JSON.stringify(summary, null, 2) + '\n');

  closeSync(logFd);
  if (!keepRaw) {
    rmSync(netlog, { force: true });
    rmSync(runtimeLog, { force: true });
    if (isolatedUserData) rmSync(userDataDir, { recursive: true, force: true });
  }

  console.log(`[probe] requests=${cdpCount} netlog=${netlogList.length} clientLog=${clientHits.length}`);
  if (warnings.length) console.log(`[probe] warnings: ${warnings.join(' | ')}`);
  console.log(`[probe] summary=${summaryPath}`);
  if (!keepRaw) console.log('[probe] 原始 NetLog/stdout 已删除；--keep-raw 可保留（含敏感元数据）');
}

main().catch((err) => {
  console.error(`[probe] ${err.message}`);
  process.exit(1);
});
