#!/usr/bin/env python3
"""用网关 auths/*.json 的凭证登录 wine 里的官方 WorkBuddy 客户端。

原理（逆向结论，证据见 .tmp/workbuddy/<realm>/<ver>/app-asar 源码）：
  客户端登录态落盘为 <数据目录>/Data/Public/auth/workbuddy-desktop.info
  （main/file-authentication-storage.js:getAuthSavePath + cli/product.json 的
  authentication.id）。敏感字段在磁盘上是 `$wbEncrypted` 信封，但解码路径
  main/credential-protection.js:decodeString 对普通字符串原样放行（这是旧明文
  会话的迁移通道），客户端读到明文后会自己用 at-rest key 重新加密写回。
  因此注入不需要客户端密钥。

用法：
  python3 scripts/wine_login.py --auth auths/<uid>.json    # 注入后启动客户端
  python3 scripts/wine_login.py --auth auths/<uid>.json --dry-run   # 只打印计划，不落盘
  python3 scripts/wine_login.py --auth auths/<uid>.json --no-launch # 只落盘，不启动
  python3 scripts/wine_login.py                             # 不注入，直接启动

约束：
  - 写入前会备份现有会话文件，并清掉 `.logged-out` 标记；
  - 客户端疑似在运行时默认拒绝写入（--force 覆盖）；
  - 输出不含 token，账号标识做掩码。
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

AUTH_ID = "workbuddy-desktop"
BASE_BY_REALM = {
    "cn": "https://www.workbuddy.cn",
    "global": "https://www.workbuddy.ai",
}
DEFAULT_APP_PARENT = "CodeBuddyExtension"
UA_TEMPLATE = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) WorkBuddy/{ver} Chrome/130.0.0.0 Electron/32.0.0.0 Safari/537.36"
)


def die(msg: str) -> "None":
    print(f"错误: {msg}", file=sys.stderr)
    raise SystemExit(1)


def info(msg: str) -> None:
    print(msg, flush=True)


def mask(s: str, keep: int = 3) -> str:
    s = s or ""
    return s[:keep] + "*" * max(0, min(6, len(s) - keep))


def read_auth(path: Path) -> dict:
    try:
        doc = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        die(f"找不到凭证文件: {path}")
    except json.JSONDecodeError as exc:
        die(f"凭证文件不是合法 JSON: {path}: {exc}")

    auth = doc.get("auth") or {}
    account = doc.get("account") or {}
    for key in ("accessToken", "refreshToken"):
        if not isinstance(auth.get(key), str) or not auth[key].strip():
            die(f"凭证缺少 auth.{key}")
    if not isinstance(account.get("uid"), str) or not account["uid"]:
        die("凭证缺少 account.uid")

    realm = (auth.get("realm") or "").strip().lower()
    if realm not in BASE_BY_REALM:
        die(f"未知 realm: {realm!r}（只支持 cn / global）")

    return {"raw": doc, "auth": auth, "account": account, "realm": realm}


def jwt_claims(token: str) -> dict:
    parts = token.split(".")
    if len(parts) < 2:
        return {}
    payload = parts[1] + "=" * (-len(parts[1]) % 4)
    try:
        return json.loads(base64.urlsafe_b64decode(payload))
    except Exception:
        return {}


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="用 auths/*.json 的凭证登录 wine 内的官方 WorkBuddy 客户端",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--auth", type=Path, help="auths/<uid>.json 路径；给出则先注入会话")
    p.add_argument("--prefix", type=Path, help="WINEPREFIX（默认取 $WINEPREFIX 或 ~/.wine）")
    p.add_argument("--exe", type=Path, help="客户端 exe；默认在 .tmp/workbuddy/<realm> 下找最新的")
    p.add_argument("--realm", choices=sorted(BASE_BY_REALM), help="覆盖凭证里的 realm（选 exe 用）")
    p.add_argument("--base", help="覆盖上游 base（默认按 realm：workbuddy.cn / workbuddy.ai）")
    p.add_argument("--dry-run", action="store_true", help="只打印计划，不写文件、不启动")
    p.add_argument("--no-launch", action="store_true", help="只注入，不启动客户端")
    p.add_argument("--force", action="store_true", help="客户端在运行也照样写入")
    p.add_argument("--print-json", action="store_true", help="打印将要写入的会话 JSON（token 打码）")
    return p.parse_args(argv)


def repo_root() -> Path:
    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "go.mod").is_file():
            return parent
    return here.parent


def pick_prefix(arg: Path | None) -> Path:
    prefix = arg or os.environ.get("WINEPREFIX") or Path.home() / ".wine"
    prefix = Path(prefix).expanduser().resolve()
    if not (prefix / "drive_c").is_dir():
        die(f"不是有效的 WINEPREFIX: {prefix}")
    return prefix


def find_session_file(prefix: Path) -> Path:
    users = Path(prefix, "drive_c", "users")
    hits = sorted(users.glob(f"*/AppData/Local/*/Data/Public/auth/{AUTH_ID}.info"))
    if len(hits) > 1:
        listing = "\n  ".join(str(p) for p in hits)
        die(f"找到多个会话文件，请用 --prefix 指定:\n  {listing}")
    if hits:
        return hits[0]

    # 客户端还没写过 auth 目录时，按当前构建的目录名回落（cn 包为 CodeBuddyExtension）。
    owners = [p for p in users.iterdir() if p.is_dir() and p.name not in ("Public", "Default")]
    if not owners:
        die(f"{users} 下找不到 wine 用户目录")
    owner = owners[0]
    return owner / "AppData/Local" / DEFAULT_APP_PARENT / "Data/Public/auth" / f"{AUTH_ID}.info"


def detect_exe(root: Path, realm: str) -> Path:
    base = root / ".tmp/workbuddy" / realm
    candidates = sorted(base.glob("*/payload/WorkBuddy*.exe"), key=lambda p: p.stat().st_mtime)
    candidates = [p for p in candidates if p.name.lower() not in ("repairapp.exe",)]
    if not candidates:
        die(f"{base} 下找不到 WorkBuddy*.exe，请用 --exe 指定")
    return candidates[-1]


def client_running() -> bool:
    try:
        out = subprocess.run(
            ["pgrep", "-af", r"WorkBuddy(AI)?\.exe"],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    lines = [ln for ln in out.stdout.splitlines() if "wb-client.py" not in ln]
    return bool(lines)


def client_version(exe: Path) -> str:
    # exe 父目录的上一层目录名形如 5.9.3.42398134-d4692421
    for part in exe.parts[::-1]:
        m = part.split("-")[0]
        if m and m[0].isdigit() and m.count(".") >= 2:
            return ".".join(m.split(".")[:3])
    return "5.9.0"


def fetch_accounts(base: str, token: str, uid: str, ua: str) -> dict:
    url = f"{base}/v2/plugin/accounts"
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"Bearer {token}",
            "X-User-Id": uid,
            "X-Domain": base.split("://", 1)[-1],
            "Accept": "application/json",
            "User-Agent": ua,
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=25) as resp:
            body = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        die(f"{url} 返回 HTTP {exc.code}（token 可能已失效）")
    except (urllib.error.URLError, TimeoutError) as exc:
        die(f"请求 {url} 失败: {exc}")
    except json.JSONDecodeError:
        die(f"{url} 响应不是 JSON")

    if body.get("code") not in (0, None):
        die(f"{url} 返回业务错误 code={body.get('code')} msg={body.get('msg')}")
    accounts = ((body.get("data") or {}).get("accounts")) or []
    for entry in accounts:
        if entry.get("uid") == uid:
            return entry
    die(f"{url} 返回的账号列表里没有 uid={mask(uid)}")


def build_session(cred: dict, claims: dict, account: dict, domain: str) -> dict:
    auth = cred["auth"]
    access, refresh = auth["accessToken"], auth["refreshToken"]
    at = jwt_claims(access)
    rt = jwt_claims(refresh)

    iat = int(at.get("iat") or auth.get("expiresAt") or 0)
    exp = int(at.get("exp") or auth.get("expiresAt") or 0)
    rt_exp = int(rt.get("exp") or 0)

    return {
        "auth": {
            "accessToken": access,
            "expiresIn": max(0, exp - iat),
            "refreshExpiresIn": max(0, rt_exp - iat),
            "refreshToken": refresh,
            "tokenType": "Bearer",
            "notBeforePolicy": 0,
            "sessionState": at.get("sid") or "",
            "scope": at.get("scope") or "openid profile offline_access email",
            "domain": domain,
            "lastRefreshTime": iat * 1000,
            "expiresAt": exp * 1000,
            "refreshExpiresAt": rt_exp * 1000,
        },
        "account": account,
        "accounts": [account],
        "allAccounts": [account],
    }


def redacted(doc: dict) -> dict:
    out = json.loads(json.dumps(doc))
    for key in ("accessToken", "refreshToken"):
        val = out["auth"].get(key) or ""
        out["auth"][key] = f"<{len(val)} chars>"
    out["auth"]["sessionState"] = "<sid>"
    for acct in [out.get("account")] + list(out.get("accounts") or []) + list(out.get("allAccounts") or []):
        if not isinstance(acct, dict):
            continue
        if isinstance(acct.get("nickname"), str):
            acct["nickname"] = mask(acct["nickname"])
        for key in ("uid", "uin", "oneidAccountId"):
            if isinstance(acct.get(key), str) and acct[key]:
                acct[key] = mask(acct[key])
        if isinstance(acct.get("phoneNumber"), str) and acct["phoneNumber"]:
            acct["phoneNumber"] = mask(acct["phoneNumber"])
    return out


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    root = repo_root()
    prefix = pick_prefix(args.prefix)

    cred = None
    realm = args.realm
    if args.auth:
        cred = read_auth(args.auth.expanduser().resolve())
        realm = realm or cred["realm"]
    realm = realm or "cn"

    exe = args.exe.expanduser().resolve() if args.exe else detect_exe(root, realm)
    if not exe.is_file():
        die(f"客户端 exe 不存在: {exe}")

    session_path = find_session_file(prefix)
    info(f"客户端: {exe}")
    info(f"WINEPREFIX: {prefix}")
    info(f"会话文件: {session_path}")

    doc = None
    if cred:
        base = (args.base or BASE_BY_REALM[realm]).rstrip("/")
        domain = base.split("://", 1)[-1]
        ua = UA_TEMPLATE.format(ver=client_version(exe))
        claims = jwt_claims(cred["auth"]["accessToken"])
        uid = cred["account"]["uid"]

        sub = claims.get("sub")
        if sub and sub != uid:
            die(f"凭证自相矛盾：account.uid={mask(uid)} 与 token sub={mask(sub)} 不一致")
        if uid != sub:
            info("提示: token 无 sub，按 account.uid 注入")

        exp = int(claims.get("exp") or cred["auth"].get("expiresAt") or 0)
        if exp and exp < time.time():
            when = time.strftime("%Y-%m-%d %H:%M:%SZ", time.gmtime(exp))
            info(f"警告: accessToken 已于 {when} 过期，客户端启动后会自行 refresh")

        account = fetch_accounts(base, cred["auth"]["accessToken"], uid, ua)
        doc = build_session(cred, claims, account, domain)
        info(f"注入账号: nickname={mask(account.get('nickname') or '')} uid={mask(uid)} realm={realm}")

        if args.print_json:
            print(json.dumps(redacted(doc), ensure_ascii=False, indent=2))

        if args.dry_run:
            info("dry-run：未写入、未启动")
            return 0

        if client_running() and not args.force:
            die("检测到客户端进程在运行；请先退出，或加 --force（会触发客户端热加载）")

        payload = json.dumps(doc, ensure_ascii=False, indent=2)
        session_path.parent.mkdir(parents=True, exist_ok=True)
        if session_path.exists():
            backup = session_path.with_name(f"{session_path.name}.bak-{time.strftime('%Y%m%d-%H%M%S')}")
            shutil.copy2(session_path, backup)
            info(f"已备份旧会话: {backup}")
            backup.chmod(0o600)

        marker = session_path.with_name(f"{session_path.name}.logged-out")
        if marker.exists():
            marker.unlink()
            info("已清除旧登出标记")

        tmp = session_path.with_name(f".{session_path.name}.tmp-{os.getpid()}")
        tmp.write_text(payload, encoding="utf-8")
        os.chmod(tmp, 0o600)
        os.replace(tmp, session_path)
        info(f"已写入会话（明文 token，客户端启动后会自动重新加密）")

    if args.no_launch:
        info("--no-launch：不启动客户端")
        return 0

    if not shutil.which("wine"):
        die("PATH 里找不到 wine")

    env = dict(os.environ, WINEPREFIX=str(prefix))
    info(f"启动: wine {exe.name}")
    return subprocess.call(["wine", str(exe)], cwd=str(exe.parent), env=env)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
