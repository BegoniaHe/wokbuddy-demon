#!/usr/bin/env bash
# make_dev.sh — 后台托管 wokbuddy-demon 开发进程（run / stop / status / logs / clean）。
#
# 生命周期以 pid 文件（.make/server.pid）为唯一事实源：stop / status / clean
# 只针对 `make run` 启动的那个进程，与其监听端口无关。监听地址由服务端
# config.json 的 listen（或 WB2A_LISTEN 环境变量）决定，脚本不臆测端口。
set -euo pipefail

if (($# != 1)); then
  echo "Usage: scripts/make_dev.sh <run|stop|status|logs|clean>" >&2
  exit 2
fi

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
run_dir="$repo_root/.make"
pid_file="$run_dir/server.pid"
stdout_log="$repo_root/server_run.log"
stderr_log="$repo_root/server_run.err.log"
bin_path="$repo_root/wokbuddy-demon"
config_path="${CONFIG:-config.json}"
warmup_seconds="${WARMUP:-1}"

remove_if_exists() {
  if [[ -e "$1" || -L "$1" ]]; then
    rm -rf -- "$1"
  fi
}

# pid_is_ours 校验 pid 确为脚本拉起的二进制，避免 pid 复用误杀无关进程。
pid_is_ours() {
  local pid="$1"
  kill -0 "$pid" 2>/dev/null || return 1
  local cmd
  cmd="$(ps -o command= -p "$pid" 2>/dev/null || true)"
  [[ "$cmd" == *"$bin_path"* || "$cmd" == *"$(basename -- "$bin_path")"* ]]
}

stop_pid() {
  local pid="$1"
  pid_is_ours "$pid" || return 0

  local pgid
  pgid="$(ps -o pgid= -p "$pid" 2>/dev/null | tr -d '[:space:]' || true)"
  if [[ "$pgid" == "$pid" ]]; then
    kill -TERM -- "-$pid" 2>/dev/null || true
  else
    kill -TERM "$pid" 2>/dev/null || true
  fi

  for _ in {1..20}; do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done

  if [[ "$pgid" == "$pid" ]]; then
    kill -KILL -- "-$pid" 2>/dev/null || true
  else
    kill -KILL "$pid" 2>/dev/null || true
  fi
}

stop_from_pid_file() {
  local file="$1"
  [[ -f "$file" ]] || return 0
  local pid
  pid="$(<"$file")"
  if [[ "$pid" =~ ^[0-9]+$ ]]; then
    stop_pid "$pid"
  fi
  rm -f -- "$file"
}

# listen_port 取实际监听端口，仅用于面板 URL 与 /healthz 探测：
# WB2A_LISTEN 覆盖 > config.json listen > 7863。
listen_port() {
  local addr=""
  if [[ -n "${WB2A_LISTEN:-}" ]]; then
    addr="$WB2A_LISTEN"
  elif [[ -f "$config_path" ]] && command -v python3 >/dev/null 2>&1; then
    addr="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("listen") or "")' "$config_path" 2>/dev/null || true)"
  fi
  if [[ "$addr" == *:* ]]; then
    printf '%s' "${addr##*:}"
  elif [[ "$addr" =~ ^[0-9]+$ ]]; then
    printf '%s' "$addr"
  else
    printf '7863'
  fi
}

url_is_available() {
  curl --fail --silent --show-error --max-time 5 "$1" >/dev/null 2>&1
}

is_running() {
  [[ -f "$pid_file" ]] || return 1
  local pid
  pid="$(<"$pid_file")"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  kill -0 "$pid" 2>/dev/null
}

case "$1" in
  run)
    mkdir -p "$run_dir"
    echo "==> [run] building $bin_path"
    (cd "$repo_root" && go build -o "$bin_path" ./cmd/server)

    stop_from_pid_file "$pid_file"

    : >"$stdout_log"
    : >"$stderr_log"

    (
      cd "$repo_root"
      if command -v setsid >/dev/null 2>&1; then
        exec setsid "$bin_path" -config "$config_path"
      fi
      exec "$bin_path" -config "$config_path"
    ) >>"$stdout_log" 2>>"$stderr_log" &
    local_pid=$!
    printf '%s\n' "$local_pid" >"$pid_file"

    sleep "$warmup_seconds"

    if kill -0 "$local_pid" 2>/dev/null; then
      echo "==> [run] wokbuddy-demon 已在后台启动 (pid $local_pid)"
      echo "    面板: http://127.0.0.1:$(listen_port)/panel/"
      echo "    日志: $stdout_log / $stderr_log"
      echo "    停止: make stop    状态: make status    跟踪: make logs"
    else
      echo "==> [run] 启动失败，最近日志：" >&2
      tail -n 20 "$stderr_log" >&2 || true
      rm -f -- "$pid_file"
      exit 1
    fi
    ;;
  stop)
    stop_from_pid_file "$pid_file"
    echo "==> [stop] wokbuddy-demon 已停止"
    ;;
  status)
    if is_running; then
      state="running (pid $(<"$pid_file"))"
    else
      state="stopped"
    fi
    port="$(listen_port)"
    if url_is_available "http://127.0.0.1:${port}/healthz"; then
      health="up"
    else
      health="down"
    fi
    printf 'Server : %s\n' "$state"
    printf 'Health : http://127.0.0.1:%s/healthz -> %s\n' "$port" "$health"
    ;;
  logs)
    touch "$stdout_log" "$stderr_log"
    tail -n 50 -f "$stdout_log" "$stderr_log"
    ;;
  clean)
    stop_from_pid_file "$pid_file"
    remove_if_exists "$run_dir"
    remove_if_exists "$stdout_log"
    remove_if_exists "$stderr_log"
    remove_if_exists "$bin_path"
    ;;
  *)
    echo "Unsupported action: $1" >&2
    exit 2
    ;;
esac
