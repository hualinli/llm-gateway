#!/usr/bin/env bash
#
# t10-lab-common.sh — T10 故障/安全/恢复验收共用的隔离实验台（source 使用，不单独执行）。
#
# 提供的约定（与 T09 `scripts/two-gateway-experiment.py` 的隔离方式一致，但用 bash 实现）：
#   * 依赖只用 docker：PostgreSQL 16.15 + Redis 7.4.11，tmpfs，随机空端口，容器名带 run id，
#     绝不触碰日常 demo 栈（docker-compose.yml）或 `make test-up` 的 llm-gateway-tests 项目；
#   * 业务进程（control / console / gateway / metering / upstream-mock）都由本脚本 spawn，
#     日志落在证据目录的 logs/ 下，pid 记录在 $T10_PIDS，trap 退出时全部回收；
#   * 二进制构建到临时目录（不进仓库、不进证据目录）；
#   * 所有随机端口都先 bind(0) 探测，避免与并行任务抢端口。
#
# 依赖：docker、curl、python3、go、pg_dump/pg_restore（后者在容器内执行）。
set -uo pipefail

T10_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
T10_ROOT="$(cd "$T10_COMMON_DIR/.." && pwd)"
T10_PG_IMAGE="${T10_PG_IMAGE:-postgres:16.15}"
T10_REDIS_IMAGE="${T10_REDIS_IMAGE:-redis:7.4.11}"

# 运行期状态
T10_RUN_ID="${T10_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
T10_EVIDENCE_DIR=""
T10_BUILD_DIR=""
T10_PIDS=()
T10_CONTAINERS=()
T10_LOG_DIR=""
T10_SECRETS_DIR=""
T10_PROFILE_TAG="t10"

t10_log()  { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
t10_warn() { printf '[%s] WARNING: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
t10_die()  { printf '[%s] ERROR: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; exit 2; }

# t10_require <cmd>... — 缺任何依赖立即退出 2（用法/环境错误，不是产品失败）。
t10_require() {
  local missing=()
  local cmd
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    t10_die "缺少依赖: ${missing[*]}"
  fi
}

# t10_free_port — 打印一个当前空闲的 127.0.0.1 TCP 端口。
t10_free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

t10_now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# t10_prepare_lab <evidence-dir> — 建立证据目录、日志/秘密/构建子目录。
t10_prepare_lab() {
  T10_EVIDENCE_DIR="$1"
  mkdir -p "$T10_EVIDENCE_DIR"
  T10_EVIDENCE_DIR="$(cd "$T10_EVIDENCE_DIR" && pwd)"
  T10_LOG_DIR="$T10_EVIDENCE_DIR/logs"
  mkdir -p "$T10_LOG_DIR"
  T10_BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/t10lab-build.XXXXXX")"
  T10_SECRETS_DIR="$(mktemp -d "${TMPDIR:-/tmp}/t10lab-secrets.XXXXXX")"
  chmod 700 "$T10_SECRETS_DIR"
}

# t10_build <binary>... — 构建 ./cmd/<binary> 到 $T10_BUILD_DIR。
t10_build() {
  local name
  for name in "$@"; do
    (cd "$T10_ROOT" && go build -o "$T10_BUILD_DIR/$name" "./cmd/$name") \
      || t10_die "go build ./cmd/$name 失败"
  done
}

t10_bin() { printf '%s/%s' "$T10_BUILD_DIR" "$1"; }

# t10_start_container <name> <image> <args...>
t10_start_container() {
  local name="$1" image="$2"; shift 2
  # 不用 --rm：演练需要 docker stop/start 同一个容器（--rm 会在 stop 时把容器删掉）。
  docker run -d --name "$name" "$@" "$image" >/dev/null \
    || t10_die "docker run $name 失败"
  T10_CONTAINERS+=("$name")
}

t10_start_pg() {
  local name="$1" port="$2" db="${3:-gateway_test}"
  t10_start_container "$name" "$T10_PG_IMAGE" \
    -p "127.0.0.1:$port:5432" \
    -e "POSTGRES_DB=$db" -e "POSTGRES_USER=gateway_test" -e "POSTGRES_PASSWORD=test_only_local" \
    --tmpfs /var/lib/postgresql/data
  t10_wait_pg "$name"
}

t10_start_redis() {
  local name="$1" port="$2"
  # 无持久化：Redis 在本实验里只是传输与限流状态，清空必须是安全的。
  # redis-server 的参数必须写在镜像名之后，因此这里不用 t10_start_container。
  # 不用 --rm：Redis 停机/恢复演练要能 docker start 同一个容器。
  docker run -d --name "$name" -p "127.0.0.1:$port:6379" --tmpfs /data \
    "$T10_REDIS_IMAGE" redis-server --save '' --appendonly no >/dev/null \
    || t10_die "docker run $name 失败"
  T10_CONTAINERS+=("$name")
  t10_wait_tcp "$port" 30 "redis $name"
}

t10_wait_pg() {
  local name="$1" i
  for i in $(seq 1 120); do
    docker exec "$name" pg_isready -U gateway_test -d gateway_test >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  t10_die "postgres $name 未就绪"
}

t10_wait_tcp() {
  local port="$1" timeout="${2:-30}" what="${3:-tcp $1}"
  python3 - "$port" "$timeout" "$what" <<'PY' || t10_die "$what 未在超时内监听"
import socket, sys, time
port, timeout, what = int(sys.argv[1]), float(sys.argv[2]), sys.argv[3]
deadline = time.time() + timeout
while time.time() < deadline:
    s = socket.socket()
    s.settimeout(0.5)
    try:
        s.connect(("127.0.0.1", port)); s.close(); sys.exit(0)
    except OSError:
        time.sleep(0.2)
    finally:
        try: s.close()
        except OSError: pass
print("timeout waiting for %s" % what, file=sys.stderr)
sys.exit(1)
PY
}

# t10_wait_http <url> <timeout-seconds> [expected-status]
t10_wait_http() {
  local url="$1" timeout="$2" expect="${3:-200}" i code
  local deadline=$(( $(date +%s) + timeout ))
  while [[ $(date +%s) -lt $deadline ]]; do
    code="$(curl -s -o /dev/null -m 3 -w '%{http_code}' "$url" 2>/dev/null)"
    [[ "$code" == "$expect" ]] && return 0
    sleep 0.3
  done
  t10_die "等待 $url 返回 $expect 超时（最后=${code}）"
}

# t10_psql <container> <sql> — 服务端执行 SQL，返回 -At 输出。
t10_psql() {
  local container="$1" sql="$2"
  docker exec -i "$container" psql -v ON_ERROR_STOP=1 -U gateway_test -d gateway_test -At -c "$sql"
}

t10_psql_db() {
  local container="$1" db="$2" sql="$3"
  docker exec -i "$container" psql -v ON_ERROR_STOP=1 -U gateway_test -d "$db" -At -c "$sql"
}

t10_redis_cli() {
  local container="$1"; shift
  docker exec -i "$container" redis-cli "$@"
}

# t10_spawn <name> <logfile> <env assignment>... -- <command> [args...]
# 结果放在全局 T10_LAST_PID（**不要**用 $(t10_spawn ...)：命令替换会在子 shell 里
# 执行，父 shell 的 T10_PIDS 就收不到 pid，teardown 会漏杀进程）。
t10_spawn() {
  local name="$1" logfile="$2"; shift 2
  local envs=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
  [[ "${1:-}" == "--" ]] && shift
  : > "$logfile"
  env "${envs[@]}" "$@" >>"$logfile" 2>&1 &
  T10_LAST_PID=$!
  disown "$T10_LAST_PID" 2>/dev/null || true
  T10_PIDS+=("$T10_LAST_PID")
  printf '%s %s\n' "$T10_LAST_PID" "$name" >> "$T10_LOG_DIR/pids.txt"
  return 0
}

t10_stop_pid() {
  local pid="$1" sig="${2:-TERM}"
  [[ -n "$pid" ]] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill "-$sig" "$pid" 2>/dev/null || true
  local i
  for i in $(seq 1 60); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -9 "$pid" 2>/dev/null || true
}

# t10_teardown — 停掉所有本脚本（以及 python 驱动）spawn 的进程并删除本脚本创建的容器。
t10_teardown() {
  local pid
  for pid in "${T10_PIDS[@]:-}"; do
    [[ -n "$pid" ]] && t10_stop_pid "$pid" TERM
  done
  # 驱动重启过进程，它的 pid 记录在 pids.txt 里，这里一并回收。
  if [[ -n "$T10_LOG_DIR" && -r "$T10_LOG_DIR/pids.txt" ]]; then
    while read -r pid _name; do
      [[ -n "$pid" ]] && t10_stop_pid "$pid" TERM
    done < "$T10_LOG_DIR/pids.txt"
  fi
  local name
  for name in "${T10_CONTAINERS[@]:-}"; do
    [[ -n "$name" ]] && docker rm -f "$name" >/dev/null 2>&1
  done
  [[ -n "$T10_BUILD_DIR" ]] && rm -rf "$T10_BUILD_DIR"
}

# t10_install_traps — 让脚本无论成功失败都回收资源。
t10_install_traps() {
  trap 't10_teardown' EXIT
}

# t10_write_env <file> — 记录非机密的运行元数据。
t10_write_env() {
  local file="$1"; shift
  {
    printf 'run_id=%s\n' "$T10_RUN_ID"
    printf 'started_at=%s\n' "$(t10_now_utc)"
    printf 'root=%s\n' "$T10_ROOT"
    printf 'git_head=%s\n' "$(cd "$T10_ROOT" && git rev-parse HEAD 2>/dev/null || echo unknown)"
    printf 'git_dirty=%s\n' "$(cd "$T10_ROOT" && git status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
    local line
    for line in "$@"; do printf '%s\n' "$line"; done
  } > "$file"
}

t10_random_secret() { python3 -c 'import secrets; print(secrets.token_urlsafe(24))'; }
