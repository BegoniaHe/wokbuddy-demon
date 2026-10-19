// download_latest.mjs — 查询 WorkBuddy 官方更新接口并下载最新安装包到
//   .tmp/workbuddy/<realm>/<version>/
// 用法（仓库根执行）：
//   node download_latest.mjs [--realm all|cn|global] [--platform <p>|all]
//     [--out-root .tmp/workbuddy] [--force] [--dry-run]
// 默认 realm=all、platform=当前机器；macOS 的接口返回 .zip，这里取同版本 .dmg。
import crypto from 'node:crypto';
import fs from 'node:fs';
import { once } from 'node:events';
import path from 'node:path';

const REALMS = {
  cn: 'https://www.workbuddy.cn',
  global: 'https://www.workbuddy.ai',
};

const PLATFORM_API = {
  'darwin-arm64': 'workbuddy-darwin-arm64',
  'darwin-x64': 'workbuddy-darwin-x64',
  'win32-x64-user': 'workbuddy-win32-x64-user',
  'linux-x64-deb': 'workbuddy-linux-x64-deb',
  'linux-x64-rpm': 'workbuddy-linux-x64-rpm',
  'linux-arm64-deb': 'workbuddy-linux-arm64-deb',
  'linux-arm64-rpm': 'workbuddy-linux-arm64-rpm',
};

const args = process.argv.slice(2);
function opt(name, dflt) {
  const i = args.indexOf('--' + name);
  if (i === -1) return dflt;
  const value = args[i + 1];
  if (value === undefined || value.startsWith('--')) {
    throw new Error(`--${name} 缺少参数值`);
  }
  return value;
}
const has = name => args.includes('--' + name);

const realmArg = opt('realm', 'all');
const realmList = realmArg === 'all' ? Object.keys(REALMS) : realmArg.split(',').map(s => s.trim());
for (const realm of realmList) {
  if (!REALMS[realm]) throw new Error(`未知 realm: ${realm}（可选 all/cn/global）`);
}

function hostPlatform() {
  if (process.platform === 'darwin') return process.arch === 'arm64' ? 'darwin-arm64' : 'darwin-x64';
  if (process.platform === 'win32') return 'win32-x64-user';
  if (process.platform === 'linux') return process.arch === 'arm64' ? 'linux-arm64-deb' : 'linux-x64-deb';
  throw new Error(`无法识别当前平台: ${process.platform}/${process.arch}，请用 --platform 指定`);
}

const platformArg = opt('platform', null) ?? hostPlatform();
const platformList = platformArg === 'all' ? Object.keys(PLATFORM_API) : platformArg.split(',').map(s => s.trim());
for (const platform of platformList) {
  if (!PLATFORM_API[platform]) throw new Error(`未知 platform: ${platform}（可选 ${Object.keys(PLATFORM_API).join('/')} 或 all）`);
}

const outRoot = path.resolve(opt('out-root', '.tmp/workbuddy'));
const dryRun = has('dry-run');
const force = has('force');

function apiUrl(realm, platform) {
  return `${REALMS[realm]}/v2/update?platform=${PLATFORM_API[platform]}`;
}

async function fetchMeta(realm, platform) {
  const url = apiUrl(realm, platform);
  const res = await fetch(url, { headers: { accept: 'application/json' } });
  if (!res.ok) throw new Error(`${url} 返回 HTTP ${res.status}`);
  const data = await res.json();
  if (data.code) throw new Error(`${url} 返回 ${data.code}: ${data.msg || 'unknown error'}`);
  if (!data.url || !data.version) throw new Error(`${url} 缺少 url/version 字段`);
  return data;
}

function pickDownload(meta) {
  const source = meta.url;
  if (!/darwin-/.test(source)) return { url: source, checksum: meta.sha256hash || '' };
  if (/\.dmg$/i.test(source)) return { url: source, checksum: '' };
  if (!/\.zip$/i.test(source)) throw new Error(`macOS 包既不是 .zip 也不是 .dmg: ${source}`);
  return { url: source.replace(/\.zip$/i, '.dmg'), checksum: '' };
}

function versionDirName(meta, platform, fileUrl) {
  const name = path.basename(new URL(fileUrl).pathname).replace(/\.(dmg|zip|exe|deb|rpm)$/i, '');
  const prefix = `WorkBuddy-${platform}-`;
  if (name.startsWith(prefix)) return name.slice(prefix.length);
  return meta.version;
}

async function download(url, dest, expectedSha) {
  const tmp = dest + '.part';
  fs.rmSync(tmp, { force: true });
  const res = await fetch(url);
  if (!res.ok) throw new Error(`${url} 返回 HTTP ${res.status}`);
  const type = res.headers.get('content-type') || '';
  if (type.includes('text/html')) throw new Error(`${url} 返回 HTML 而不是安装包（可能被重定向到登录/错误页）`);
  const total = Number(res.headers.get('content-length') || 0);
  const hash = crypto.createHash('sha256');
  const out = fs.createWriteStream(tmp);

  let received = 0;
  let last = 0;
  try {
    for await (const chunk of res.body) {
      hash.update(chunk);
      received += chunk.length;
      if (!out.write(chunk)) await once(out, 'drain');
      const now = Date.now();
      if (now - last > 500) {
        last = now;
        const pct = total ? `${((received / total) * 100).toFixed(1)}%` : `${(received / 1048576).toFixed(1)}MB`;
        process.stderr.write(`\r  ${path.basename(dest)} ${pct}`);
      }
    }
    out.end();
    await once(out, 'finish');
  } catch (err) {
    out.destroy();
    fs.rmSync(tmp, { force: true });
    throw err;
  }

  const got = hash.digest('hex');
  if (expectedSha && got !== expectedSha) {
    fs.rmSync(tmp, { force: true });
    throw new Error(`sha256 不匹配: got ${got}, want ${expectedSha}`);
  }
  if (total && received !== total) {
    fs.rmSync(tmp, { force: true });
    throw new Error(`下载不完整: ${received}/${total} bytes`);
  }
  fs.renameSync(tmp, dest);
  process.stderr.write('\n');
  return { bytes: received, sha256: got };
}

for (const realm of realmList) {
  for (const platform of platformList) {
    const meta = await fetchMeta(realm, platform);
    const { url, checksum } = pickDownload(meta);
    const version = versionDirName(meta, platform, url);
    const dir = path.join(outRoot, realm, version);
    const dest = path.join(dir, path.basename(new URL(url).pathname));
    const rel = path.relative(process.cwd(), dest);
    console.log(`${realm} ${platform} ${version}\n  ${url}\n  ${rel}`);
    if (dryRun) continue;
    fs.mkdirSync(dir, { recursive: true });
    if (fs.existsSync(dest) && !force) {
      console.log(`  已存在，跳过（--force 覆盖）`);
      continue;
    }
    const info = await download(url, dest, checksum);
    console.log(`  完成 ${(info.bytes / 1048576).toFixed(1)}MB sha256=${info.sha256}`);
  }
}
