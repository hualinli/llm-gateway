#!/usr/bin/env bash
#
# security-matrix.sh — T10 F16 安全负向矩阵 + F10/F12 恢复语义 + 管理面隔离（黑盒，真实 HTTP）。
#
# 本脚本自己拉起**完全隔离**的实验台（自己的 docker 容器/端口/scratch 数据库/
# 临时目录），跑完后全部回收，不触碰日常 demo 栈、`make test-up` 的测试栈或
# 其它并行任务：
#
#   PostgreSQL 16.15 (tmpfs) + Redis 7.4.11 (无持久化)
#   cmd/migrate up → cmd/console bootstrap → cmd/control（AdminService 明文逃生开关）
#   cmd/upstream-mock → cmd/gateway（GATEWAY_ACCESS_GUARD=pg, GATEWAY_BILLING=pg）
#   cmd/metering（receipt dispatcher + settlement consumer）
#   cmd/console ×2（主实例 + 短 session TTL 实例，用于"过期 session"这一条）
#
# 断言都在内嵌的 python3 驱动里（只用 curl + 标准库，不读产品源码、不直接改业务表，
# 只有造压数据与少量 SQL 观测走 psql）。所有 4xx/5xx 都要求 T01A 错误信封，
# 跨组织一律要求与"不存在的组织"逐字节同形，并扫描响应、进程日志和落盘证据中的
# raw credentials；完整驱动配置只在临时 secrets 目录里。
#
# 退出码：0 = 全部硬检查通过（允许显式 SKIP）；1 = 存在 FAIL；2 = 用法/环境错误。
#
# 用法见 --help。
set -uo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
script_name="$(basename "$0")"
# shellcheck source=scripts/t10-lab-common.sh
source "$script_dir/t10-lab-common.sh"

evidence_dir="$T10_ROOT/data/verification/t10-security"
phases="all"
timeout_seconds=20
keep_lab=0
lease_seconds=8
driver_dir=""

security_matrix_teardown() {
  t10_teardown
  if [[ -n "$driver_dir" ]]; then rm -rf -- "$driver_dir"; fi
  if [[ "$keep_lab" != "1" && -n "${T10_SECRETS_DIR:-}" ]]; then
    rm -rf -- "$T10_SECRETS_DIR"
  fi
}

usage() {
  cat <<'USAGE'
用法:
  scripts/security-matrix.sh [选项]

选项:
  --evidence-dir DIR   证据目录（默认 data/verification/t10-security）
  --phases LIST        all（默认）或逗号分隔: security,control,longstream,isolation
  --timeout-seconds N  单个 curl 超时，默认 20
  --lease-seconds N    Gateway 控制面租约（CONTROL_CONFIG_LEASE），默认 8s；
                       调小只是为了让"失联超租约"在验收时间内可观测，语义不变
  --keep               结束后保留实验台（容器与进程），便于人工复现
  -h, --help           显示本帮助

检查项:
  SEC-01..SEC-15   F16：无 token / 错误 scheme / 篡改与过期 session / 篡改邀请 token /
                   跨组织 orgs-users-keys-usage-audit-exceptions-statements 枚举 /
                   角色提权 / 列出的 unsafe 路由缺 CSRF 与错 CSRF / 禁用用户活跃 session /
                   被移出成员的有效 Key / 普通成员自有数据与 Key 边界 / 无平台管理员访问 admin /
                   手动开户、个人账户保留、Key 名称与成员关系归属 / 全局 secret 扫描
  CTRL-01..CTRL-04 F10：desired vs applied、控制面分区后 stale（不谎报 active）、
                   租约超时拒新请求、在途流排水、恢复后收敛、Control 重启不复活已撤销 Key
  REV-01..REV-04   F12：长流期间改价（在途钉住原价）/ 撤销 Key / 移除成员，
                   新请求拒绝、旧流排水、账本不重复
  ISO-01..ISO-04   Gateway SSE 基线与 Console 报表负载下的完成数/错误/p95 观察；
                   Console 重启不打断在途流；按 application_name 观测各自连接池上限
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence-dir) evidence_dir="${2:-}"; shift 2 ;;
    --evidence-dir=*) evidence_dir="${1#*=}"; shift ;;
    --phases) phases="${2:-}"; shift 2 ;;
    --phases=*) phases="${1#*=}"; shift ;;
    --timeout-seconds) timeout_seconds="${2:-}"; shift 2 ;;
    --timeout-seconds=*) timeout_seconds="${1#*=}"; shift ;;
    --lease-seconds) lease_seconds="${2:-}"; shift 2 ;;
    --lease-seconds=*) lease_seconds="${1#*=}"; shift ;;
    --keep) keep_lab=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf '%s: 未知参数 %s\n\n' "$script_name" "$1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$timeout_seconds" in ''|*[!0-9]*) t10_die "--timeout-seconds 必须是非负整数";; esac
case "$lease_seconds" in ''|*[!0-9]*) t10_die "--lease-seconds 必须是非负整数";; esac
case ",$phases," in
  *,all,*|*,security,*|*,control,*|*,longstream,*|*,isolation,*) ;;
  *) t10_die "--phases 只支持 security,control,longstream,isolation,all: $phases" ;;
esac

t10_require docker curl python3 go git shasum
t10_install_traps
trap 'security_matrix_teardown' EXIT

# 每个 run 自己的 docker 项目名/容器名，避免与并行任务（含同 worktree 的失败注入任务）冲突。
prefix="t10sec-$(printf '%s' "$T10_RUN_ID" | tr '[:upper:]' '[:lower:]')"
prefix="${prefix:0:40}"
ports=()
for _ in pg redis control_grpc control_http control_proxy gateway gateway_diag metering console console_short upstream; do
  ports+=("$(t10_free_port)")
done
pg_port="${ports[0]}"; redis_port="${ports[1]}"; control_grpc_port="${ports[2]}"
control_http_port="${ports[3]}"; control_proxy_port="${ports[4]}"; gateway_port="${ports[5]}"
gateway_diag_port="${ports[6]}"; metering_port="${ports[7]}"; console_port="${ports[8]}"
console_short_port="${ports[9]}"; upstream_port="${ports[10]}"

t10_prepare_lab "$evidence_dir"
t10_log "run=$T10_RUN_ID evidence=$T10_EVIDENCE_DIR"
t10_log "ports: pg=$pg_port redis=$redis_port control=$control_grpc_port/$control_http_port gateway=$gateway_port console=$console_port metering=$metering_port upstream=$upstream_port"
t10_write_env "$T10_EVIDENCE_DIR/run.env" \
  "script=$script_name" \
  "script_sha256=$(shasum -a 256 "$script_dir/$script_name" | awk '{print $1}')" \
  "common_sha256=$(shasum -a 256 "$script_dir/t10-lab-common.sh" | awk '{print $1}')" \
  "phases=$phases" \
  "lease_seconds=$lease_seconds" \
  "pg_port=$pg_port" "redis_port=$redis_port" \
  "control_grpc_port=$control_grpc_port" "control_http_port=$control_http_port" \
  "control_proxy_port=$control_proxy_port" \
  "gateway_port=$gateway_port" "metering_port=$metering_port" \
  "console_port=$console_port" "console_short_port=$console_short_port" \
  "upstream_port=$upstream_port"

# ---------------------------------------------------------------------------
# 实验台
# ---------------------------------------------------------------------------
pg_container="${prefix}-pg"
redis_container="${prefix}-redis"
admin_email="admin@${T10_PROFILE_TAG}.test"
admin_password_file="$T10_SECRETS_DIR/admin-password"
fixture_password_file="$T10_SECRETS_DIR/fixture-passwords.json"
control_admin_token="$(t10_random_secret)"
metering_admin_token="$(t10_random_secret)"
dsn="postgres://gateway_test:test_only_local@127.0.0.1:${pg_port}/gateway_test?sslmode=disable"
# 每个组件用不同的 application_name，便于在 pg_stat_activity 里观察 Console 与
# Gateway 各自的连接池使用（ISO-04）。application_name 是纯观测标签，不改变语义。
dsn_gateway="${dsn}&application_name=t10-gateway"
dsn_console="${dsn}&application_name=t10-console"
dsn_console_short="${dsn}&application_name=t10-console-short"
dsn_metering="${dsn}&application_name=t10-metering"
dsn_control="${dsn}&application_name=t10-control"
admin_password="$(t10_random_secret)"
printf '%s' "$admin_password" > "$admin_password_file"
chmod 600 "$admin_password_file"

t10_log "构建二进制（control/gateway/console/metering/upstream-mock/migrate/loadtest）"
t10_build control gateway console metering upstream-mock migrate loadtest || exit 2

t10_log "启动隔离 PostgreSQL + Redis"
t10_start_pg "$pg_container" "$pg_port" || exit 2
t10_start_redis "$redis_container" "$redis_port" || exit 2

t10_log "migrate up"
(cd "$T10_ROOT" && POSTGRES_DSN="$dsn" GATEWAY_PROFILE=demo "$(t10_bin migrate)" up) \
  > "$T10_LOG_DIR/migrate.log" 2>&1 || { t10_die "migrate up 失败（见 logs/migrate.log）"; }
schema_version="$(t10_psql "$pg_container" "SELECT version FROM schema_migrations")"
t10_log "schema version=$schema_version"

t10_log "console bootstrap（平台管理员，口令只在 0600 文件里）"
(cd "$T10_ROOT" && POSTGRES_DSN="$dsn" GATEWAY_PROFILE=demo "$(t10_bin console)" bootstrap \
  --admin-email "$admin_email" --admin-password-file "$admin_password_file") \
  > "$T10_LOG_DIR/bootstrap.log" 2>&1 || { t10_die "console bootstrap 失败"; }

# 上游 mock：普通流很快；慢流用 x_mock_* 请求字段（mock 扩展，网关原样透传）。
t10_spawn upstream "$T10_LOG_DIR/upstream-mock.log" \
  "GATEWAY_PROFILE=demo" -- \
  "$(t10_bin upstream-mock)" -listen "127.0.0.1:${upstream_port}" -name primary \
  -ttft 5ms -token-interval 5ms -tokens 8
upstream_pid="$T10_LAST_PID"
t10_wait_tcp "$upstream_port" 30 "upstream-mock"

# Control：PostgreSQL 事实表 + Console 明文管理通道（仅本地一次性验收环境）。
control_env=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=$dsn_control"
  "REDIS_ADDR=127.0.0.1:${redis_port}"
  "CONTROL_GRPC_ADDR=127.0.0.1:${control_grpc_port}"
  "CONTROL_HTTP_ADDR=127.0.0.1:${control_http_port}"
  "CONTROL_REQUIRE_POSTGRES=true"
  "CONTROL_ADMIN_TOKEN=$control_admin_token"
  "CONTROL_ADMIN_SERVICE_ENABLED=true"
  "CONTROL_CONSOLE_SERVICE_NAME=console"
  "CONTROL_ALLOW_INSECURE_ADMIN_SERVICE=true"
)
t10_spawn control "$T10_LOG_DIR/control.log" "${control_env[@]}" -- "$(t10_bin control)"
control_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${control_http_port}/healthz" 30 200

# 控制面 gRPC 的本地 TCP 代理：CTRL-02 用"切断代理"模拟 Gateway↔Control 网络分区，
# 而 Control 本身仍然活着，因此 /gateways 的 desired-vs-applied 视图在分区期间可读。
cat > "$T10_SECRETS_DIR/tcp_proxy.py" <<'PYPROXY'
#!/usr/bin/env python3
"""最小 TCP 转发器：listen_port -> 127.0.0.1:target_port（仅用于本地故障注入）。"""
import socket, sys, threading

listen_port, target_port = int(sys.argv[1]), int(sys.argv[2])
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", listen_port))
listener.listen(64)
print("proxy %d -> %d" % (listen_port, target_port), flush=True)


def pump(src, dst):
    try:
        while True:
            data = src.recv(65536)
            if not data:
                break
            dst.sendall(data)
    except OSError:
        pass
    finally:
        for sock in (src, dst):
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                sock.close()
            except OSError:
                pass


while True:
    try:
        client, _ = listener.accept()
    except OSError:
        break
    try:
        upstream = socket.create_connection(("127.0.0.1", target_port), timeout=5)
    except OSError:
        client.close()
        continue
    threading.Thread(target=pump, args=(client, upstream), daemon=True).start()
    threading.Thread(target=pump, args=(upstream, client), daemon=True).start()
PYPROXY
t10_spawn control-proxy "$T10_LOG_DIR/control-proxy.log" \
  "GATEWAY_PROFILE=demo" -- \
  python3 "$T10_SECRETS_DIR/tcp_proxy.py" "$control_proxy_port" "$control_grpc_port"
control_proxy_pid="$T10_LAST_PID"
t10_wait_tcp "$control_proxy_port" 15 "control proxy"

# 配置：provider（指向本地 mock）+ route + 价格在 fixtures 阶段由 HTTP 下发。
push_config() {
  curl -s -X POST "http://127.0.0.1:${control_http_port}/config" \
    -H 'Content-Type: application/json' -H "Authorization: Bearer ${control_admin_token}" \
    -d "$1"
}
push_config "$(printf '{"providers":{"primary":{"id":"primary","protocol":"openai-compatible","url":"http://127.0.0.1:%s","model":"t10-model","timeouts":{"attempt_timeout_ms":90000}}},"routes":{"t10-model":["primary"],"default":["primary"]}}' "$upstream_port")" \
  > "$T10_LOG_DIR/config-initial.json" || t10_die "初始 /config 失败"

# Gateway：真实 access guard（PG）+ 真实计费准入（PG）+ Redis fail-closed 限流。
gateway_env=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=$dsn_gateway"
  "REDIS_ADDR=127.0.0.1:${redis_port}"
  "GATEWAY_ADDR=127.0.0.1:${gateway_port}"
  "GATEWAY_DIAGNOSTICS_ADDR=127.0.0.1:${gateway_diag_port}"
  "GATEWAY_INSTANCE_ID=gw-t10"
  "CONTROL_ADDR=127.0.0.1:${control_proxy_port}"
  "GATEWAY_ACCESS_GUARD=pg"
  "GATEWAY_BILLING=pg"
  "GATEWAY_BILLING_MAX_CONNS=8"
  "GATEWAY_ACCESS_GUARD_MAX_CONNS=8"
  "LIMITER_ON_REDIS_ERROR=closed"
  # 网关级令牌桶默认 20/s + burst 40；隔离性实验研究的是"Console 报表负载 vs
  # Gateway SSE"，限流不是变量，因此把它提到不会触发的水平（ISO-01 仍会记录
  # 默认值下 55/s 到达会产生 429 这一事实）。
  "GATEWAY_LIMITER_RATE=1000"
  "GATEWAY_LIMITER_BURST=2000"
  "GATEWAY_MAX_ACTIVE_STREAMS=64"
  "GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG=32"
  "CONTROL_CONFIG_POLL_INTERVAL=1s"
  "CONTROL_CONFIG_LEASE=${lease_seconds}s"
  "CONTROL_STATUS_REPORT_INTERVAL=2s"
  "METERING_SPOOL_DIR=$T10_SECRETS_DIR/spool-gw"
)
t10_spawn gateway "$T10_LOG_DIR/gateway.log" "${gateway_env[@]}" -- "$(t10_bin gateway)"
gateway_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${gateway_port}/healthz" 60 200

# Metering：receipt dispatcher + settlement consumer；redispatch 调到 3s 便于观测恢复。
metering_env=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=$dsn_metering"
  "REDIS_ADDR=127.0.0.1:${redis_port}"
  "METERING_ADDR=127.0.0.1:${metering_port}"
  "METERING_REQUIRE_POSTGRES=true"
  "METERING_SPOOL_DIR=$T10_SECRETS_DIR/spool-metering"
  "METERING_ADMIN_TOKEN=$metering_admin_token"
  "METERING_SETTLEMENT_REDISPATCH=3s"
)
t10_spawn metering "$T10_LOG_DIR/metering.log" "${metering_env[@]}" -- "$(t10_bin metering)"
metering_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${metering_port}/healthz" 60 200

# Console：主实例（30m session），以及短 TTL 实例（2s，只用于"过期 session"一条）。
console_env=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=$dsn_console"
  "CONSOLE_ADDR=127.0.0.1:${console_port}"
  "CONTROL_ADDR=127.0.0.1:${control_grpc_port}"
  "CONTROL_CONSOLE_SERVICE_NAME=console"
  "CONSOLE_PUBLICATION_INTERVAL=2s"
  "CONSOLE_SESSION_TTL=30m"
  # 登录节流不在本矩阵的断言范围（T02/console-accept 覆盖）；这里放宽以免
  # 验收脚本自己把同一账号打成 429。
  "CONSOLE_LOGIN_RATE=60"
  "CONSOLE_LOGIN_BURST=30"
)
t10_spawn console "$T10_LOG_DIR/console.log" "${console_env[@]}" -- "$(t10_bin console)" serve
console_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${console_port}/readyz" 60 200

console_short_env=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=$dsn_console_short"
  "CONSOLE_ADDR=127.0.0.1:${console_short_port}"
  "CONTROL_ADDR=127.0.0.1:${control_grpc_port}"
  "CONTROL_CONSOLE_SERVICE_NAME=console"
  "CONSOLE_PUBLICATION_INTERVAL=2s"
  "CONSOLE_SESSION_TTL=2s"
  "CONSOLE_LOGIN_RATE=60"
  "CONSOLE_LOGIN_BURST=30"
)
t10_spawn console-short "$T10_LOG_DIR/console-short.log" "${console_short_env[@]}" -- "$(t10_bin console)" serve
console_short_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${console_short_port}/readyz" 60 200

t10_log "等待 Gateway 首次上报（/gateways health=active）"
python3 - "$control_http_port" "$control_admin_token" <<'PY' || t10_die "Gateway 未在超时内上报"
import json, sys, time, urllib.request
port, token = sys.argv[1], sys.argv[2]
req = urllib.request.Request("http://127.0.0.1:%s/gateways" % port, headers={"Authorization": "Bearer " + token})
deadline = time.time() + 60
last = None
while time.time() < deadline:
    try:
        with urllib.request.urlopen(req, timeout=3) as resp:
            body = json.load(resp)
        for inst in body.get("instances", []):
            if inst.get("instance_id") == "gw-t10" and inst.get("health") == "active":
                sys.exit(0)
        last = body
    except Exception as err:  # noqa: BLE001
        last = repr(err)
    time.sleep(0.3)
print("timeout: %r" % (last,), file=sys.stderr)
sys.exit(1)
PY

# 驱动配置包含 bearer token、DSN 与重启命令，只放 0700/0600 的临时 secrets 目录。
private_lab_config="$T10_SECRETS_DIR/lab-private.json"
cat > "$private_lab_config" <<JSON
{
  "run_id": "$T10_RUN_ID",
  "root": "$T10_ROOT",
  "evidence_dir": "$T10_EVIDENCE_DIR",
  "secret_dir": "$T10_SECRETS_DIR",
  "bin_dir": "$T10_BUILD_DIR",
  "log_dir": "$T10_LOG_DIR",
  "timeout_seconds": $timeout_seconds,
  "lease_seconds": $lease_seconds,
  "phases": "$phases",
  "admin_email": "$admin_email",
  "admin_password_file": "$admin_password_file",
  "fixture_password_file": "$fixture_password_file",
  "dsn": "$dsn",
  "dsn_by_component": {"gateway": "$dsn_gateway", "console": "$dsn_console",
                       "console_short": "$dsn_console_short", "metering": "$dsn_metering",
                       "control": "$dsn_control"},
  "proxy_script": "$T10_SECRETS_DIR/tcp_proxy.py",
  "containers": {"pg": "$pg_container", "redis": "$redis_container"},
  "ports": {"pg": $pg_port, "redis": $redis_port, "control_grpc": $control_grpc_port,
            "control_http": $control_http_port, "control_proxy": $control_proxy_port,
            "gateway": $gateway_port, "gateway_diag": $gateway_diag_port,
            "metering": $metering_port, "console": $console_port,
            "console_short": $console_short_port, "upstream": $upstream_port},
  "urls": {"console": "http://127.0.0.1:$console_port",
           "console_short": "http://127.0.0.1:$console_short_port",
           "gateway": "http://127.0.0.1:$gateway_port",
           "gateway_diag": "http://127.0.0.1:$gateway_diag_port",
           "control": "http://127.0.0.1:$control_http_port",
           "metering": "http://127.0.0.1:$metering_port",
           "upstream": "http://127.0.0.1:$upstream_port"},
  "tokens": {"control_admin": "$control_admin_token", "metering_admin": "$metering_admin_token"},
  "pids": {"control": "$control_pid", "gateway": "$gateway_pid", "metering": "$metering_pid",
           "console": "$console_pid", "console_short": "$console_short_pid",
           "upstream": "$upstream_pid", "control_proxy": "$control_proxy_pid"},
  "commands": {
    "control": {"argv": ["$(t10_bin control)"], "env": $(python3 -c 'import json,sys;print(json.dumps(dict(a.split("=",1) for a in sys.argv[1:])))' "${control_env[@]}"), "log": "$T10_LOG_DIR/control.log"},
    "console": {"argv": ["$(t10_bin console)", "serve"], "env": $(python3 -c 'import json,sys;print(json.dumps(dict(a.split("=",1) for a in sys.argv[1:])))' "${console_env[@]}"), "log": "$T10_LOG_DIR/console.log"},
    "metering": {"argv": ["$(t10_bin metering)"], "env": $(python3 -c 'import json,sys;print(json.dumps(dict(a.split("=",1) for a in sys.argv[1:])))' "${metering_env[@]}"), "log": "$T10_LOG_DIR/metering.log"},
    "loadtest": {"argv": ["$(t10_bin loadtest)"]}
  }
}
JSON
chmod 600 "$private_lab_config"

# Evidence carries connection metadata only; it never stores credentials, DSNs,
# process environments, or command arguments.
python3 - "$private_lab_config" "$T10_EVIDENCE_DIR/lab.json" <<'PY'
import json
import os
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    config = json.load(handle)
public = {
    "run_id": config["run_id"],
    "root": config["root"],
    "evidence_dir": config["evidence_dir"],
    "timeout_seconds": config["timeout_seconds"],
    "lease_seconds": config["lease_seconds"],
    "phases": config["phases"],
    "admin_email": config["admin_email"],
    "containers": config["containers"],
    "ports": config["ports"],
    "urls": config["urls"],
    "secret_material": "temporary private config; removed after teardown unless --keep is used",
}
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(public, handle, indent=2)
os.chmod(sys.argv[2], 0o600)
PY

# 断言驱动：写在临时目录（它是脚本本体的一部分，不属于证据）。
driver_dir="$(mktemp -d "${TMPDIR:-/tmp}/t10sec-driver.XXXXXX")"
driver_path="$driver_dir/security_matrix_driver.py"
lib_path="$driver_dir/t10_driver_lib.py"
cat > "$lib_path" <<'DRIVER_LIB_EOF'
import argparse
import json
import os
import re
import secrets
import shutil
import signal
import socket
import string
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime, timedelta, timezone
MAX_STORED_BODY = 8192


def utcnow():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def slugify(text):
    return re.sub(r"[^a-z0-9]+", "-", str(text).lower()).strip("-")[:56] or "call"


class CheckFailure(Exception):
    pass


class SkipCheck(Exception):
    pass


class InfraFailure(Exception):
    pass


class Redactor:
    """Masks every secret it knows about, plus the obvious JSON keys."""

    def __init__(self):
        self.secrets = []

    def add(self, value):
        value = "" if value is None else str(value)
        if len(value) >= 6 and value not in self.secrets:
            self.secrets.append(value)
        return value

    @staticmethod
    def _mask(value):
        return "%s…<redacted:%d>" % (value[:4], len(value))

    def text(self, text):
        if text is None:
            return ""
        out = str(text)
        for secret in sorted(self.secrets, key=len, reverse=True):
            out = out.replace(secret, self._mask(secret))
        out = re.sub(
            r'("(?:password|csrf_token|token|session_token|access_token|secret|key_hash)"\s*:\s*")([^"]*)(")',
            lambda m: m.group(1) + (self._mask(m.group(2)) if m.group(2) else "") + m.group(3),
            out,
        )
        out = re.sub(r"(gw_session=)([^;,\s]+)", lambda m: m.group(1) + self._mask(m.group(2)), out)
        out = re.sub(r"(gw_csrf=)([^;,\s]+)", lambda m: m.group(1) + self._mask(m.group(2)), out)
        out = re.sub(r"((?i:X-CSRF-Token):\s*)(\S+)", lambda m: m.group(1) + self._mask(m.group(2)), out)
        out = re.sub(r"((?i:Authorization):\s*Bearer\s+)(\S+)", lambda m: m.group(1) + self._mask(m.group(2)), out)
        return out


RED = Redactor()
LOGFILE = None


def redact(text):
    return RED.text(text)


def set_logfile(path):
    """Point log() at a file; called once per script."""
    global LOGFILE
    LOGFILE = path


def log(line):
    line = redact(line)
    print(line, flush=True)
    if LOGFILE:
        with open(LOGFILE, "a", encoding="utf-8") as handle:
            handle.write(line + "\n")


class Response:
    def __init__(self, method, path, status, headers, body, elapsed):
        self.method = method
        self.path = path
        self.status = status
        self.headers = headers
        self.body = body
        self.elapsed = elapsed
        self.curl_timeout = False

    def header(self, name, default=None):
        for key, value in self.headers:
            if key.lower() == name.lower():
                return value
        return default

    def header_all(self, name):
        return [value for key, value in self.headers if key.lower() == name.lower()]

    def json(self):
        try:
            return json.loads(self.body)
        except Exception:
            return None

    def error(self):
        payload = self.json()
        if isinstance(payload, dict) and isinstance(payload.get("error"), dict):
            return payload["error"]
        return None

    def code(self):
        err = self.error()
        return err.get("code") if err else None

    def message(self):
        err = self.error()
        return err.get("message") if err else None


def parse_set_cookie(value):
    parts = [part.strip() for part in value.split(";")]
    name, _, raw = parts[0].partition("=")
    attrs = {}
    for part in parts[1:]:
        key, _, attr_value = part.partition("=")
        attrs[key.strip().lower()] = attr_value.strip()
    return name.strip().lower(), raw, attrs


def cookies_of(response):
    out = {}
    for value in response.header_all("Set-Cookie"):
        name, raw, _ = parse_set_cookie(value)
        if name:
            out[name] = raw
    return out


class Check:
    def __init__(self, cid, title):
        self.cid = cid
        self.title = title
        self.status = "PASS"
        self.started_at = utcnow()
        self.finished_at = None
        self.notes = []
        self.details = {}
        self.failure = None
        self.skip_reason = None
        self.exchanges = []
        self.sequence = 0

    def note(self, text):
        line = redact(text)
        self.notes.append(line)
        log("    note: " + line)

    def detail(self, key, value):
        self.details[key] = value

    def require(self, condition, message):
        if not condition:
            raise CheckFailure(message)

    def require_status(self, response, status, code=None, label=""):
        where = (" " + label) if label else ""
        if response.status != status:
            raise CheckFailure("%s%s: 期望 HTTP %d，实际 %d code=%s message=%s" % (
                response.method, where, status, response.status, response.code(), response.message()))
        if code is not None and response.code() != code:
            raise CheckFailure("%s%s: 期望 error.code=%s，实际 %s（message=%s）" % (
                response.method, where, code, response.code(), response.message()))
        return response

    def skip(self, reason):
        raise SkipCheck(reason)


CSRF_DEFAULT = object()


class Client:
    def __init__(self, evidence_dir, timeout, scratch_dir=None):
        self.evidence_dir = evidence_dir
        self.timeout = timeout
        self.scratch = tempfile.mkdtemp(prefix="t10sec-http.", dir=scratch_dir)
        self.records = []

    def call(self, check, label, method, path, *, base_url, query=None, body=None, raw_body=None,
             session=None, csrf=CSRF_DEFAULT, extra_headers=None, max_time=None, tolerate_timeout=False):
        check.sequence += 1
        seq = check.sequence
        url = base_url.rstrip("/") + path
        if query:
            url += "?" + query
        timeout = max_time or self.timeout
        request_id = "t10-%s-%02d" % (check.cid, seq)
        headers = {"X-Request-ID": request_id, "Accept": "application/json"}
        if session is not None:
            headers["Cookie"] = "gw_session=%s; gw_csrf=%s" % (session["token"], session.get("csrf", ""))
        token = session.get("csrf") if (csrf is CSRF_DEFAULT and session) else csrf
        if token:
            headers["X-CSRF-Token"] = token
        for key, value in (extra_headers or {}).items():
            headers[key] = value

        payload = body
        if payload is not None and not isinstance(payload, str):
            payload = json.dumps(payload)
        if raw_body is not None:
            payload = raw_body
        if payload is not None:
            headers.setdefault("Content-Type", "application/json")

        header_file = os.path.join(self.scratch, "h.%d" % seq)
        body_file = os.path.join(self.scratch, "b.%d" % seq)
        cmd = ["curl", "--silent", "--show-error", "--max-time", str(timeout),
               "--request", method, "--dump-header", header_file, "--output", body_file,
               "--write-out", "%{http_code} %{time_total}"]
        for key, value in headers.items():
            cmd += ["--header", "%s: %s" % (key, value)]
        stdin = None
        if payload is not None:
            cmd += ["--data-binary", "@-"]
            stdin = payload.encode("utf-8")
        cmd.append(url)

        curl_timeout = False
        try:
            proc = subprocess.run(cmd, input=stdin, stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, timeout=timeout + 15)
        except subprocess.TimeoutExpired:
            raise InfraFailure("curl 超时: %s %s" % (method, url))
        if proc.returncode != 0:
            # rc=28 with a captured body is the documented gateway SSE behavior
            # (body complete, chunked response not terminated); SSE helpers opt in.
            if tolerate_timeout and proc.returncode == 28 and os.path.exists(body_file) and os.path.getsize(body_file) > 0:
                curl_timeout = True
            else:
                raise InfraFailure("curl rc=%d %s %s: %s" % (
                    proc.returncode, method, url, redact(proc.stderr.decode("utf-8", "replace").strip())))

        write_out = proc.stdout.decode("utf-8", "replace").strip().split()
        status = int(write_out[0]) if write_out else 0
        elapsed = float(write_out[1]) if len(write_out) > 1 else 0.0

        headers_list = []
        with open(header_file, "r", encoding="utf-8", errors="replace") as handle:
            for line in handle.read().splitlines():
                if not line.strip() or line.lower().startswith("http/"):
                    continue
                key, _, value = line.partition(":")
                headers_list.append((key.strip(), value.strip()))
        with open(body_file, "rb") as handle:
            body_text = handle.read().decode("utf-8", "replace")

        response = Response(method, path + (("?" + query) if query else ""), status, headers_list, body_text, elapsed)
        response.curl_timeout = curl_timeout
        self._record(check, label, url, headers, payload, response)
        return response

    def _record(self, check, label, url, headers, payload, response):
        seq = check.sequence
        check.exchanges.append({"method": response.method, "path": response.path,
                                "status": response.status, "error_code": response.code(),
                                "label": label, "elapsed": round(response.elapsed, 4)})
        self.records.append({"cid": check.cid, "seq": seq, "label": label, "url": url,
                             "status": response.status, "body": response.body, "path": response.path,
                             "method": response.method})
        stem = "%s-%02d-%s" % (check.cid, seq, slugify(label))
        dump = ["%s %s" % (response.method, url)]
        for key, value in headers.items():
            dump.append("%s: %s" % (key, value))
        dump.append("")
        if payload:
            dump.append(payload)
        dump.append("")
        with open(os.path.join(self.evidence_dir, stem + ".req"), "w", encoding="utf-8") as handle:
            handle.write(redact("\n".join(dump)))
        resp_dump = ["HTTP %d (%.3fs)" % (response.status, response.elapsed)]
        for key, value in response.headers:
            resp_dump.append("%s: %s" % (key, value))
        resp_dump.append("")
        response_body = response.body[:MAX_STORED_BODY]
        # Activation codes are one-time credentials carried as `code`, not
        # `token`; redact this response before it reaches evidence files.
        if response.method == "POST" and response.path.split("?", 1)[0] == "/api/v1/admin/activation-codes":
            try:
                payload = json.loads(response_body)
                if isinstance(payload, dict) and isinstance(payload.get("code"), str):
                    payload["code"] = "<redacted: one-time activation code>"
                    response_body = json.dumps(payload, ensure_ascii=False)
            except (TypeError, ValueError):
                pass
        resp_dump.append(response_body)
        if len(response.body) > MAX_STORED_BODY:
            resp_dump.append("\n…(%d bytes truncated)" % (len(response.body) - MAX_STORED_BODY))
        with open(os.path.join(self.evidence_dir, stem + ".resp"), "w", encoding="utf-8") as handle:
            handle.write(redact("\n".join(resp_dump)))

    def raw_bodies(self):
        return [record["body"] for record in self.records]


# ---------------------------------------------------------------------------
# process / container control
# ---------------------------------------------------------------------------

# Processes the driver started, keyed by (lab identity, name): a Proc built later
# for the same lab/name must find (and be able to stop) what an earlier Proc
# started, instead of assuming the shell wrapper owns it.
_PROC_REGISTRY = {}


class Proc:
    """A restarted-by-the-driver process (control/console/metering/loadtest)."""

    def __init__(self, lab, name):
        self.lab = lab
        self.name = name
        self.key = (id(lab), name)
        self.popen = _PROC_REGISTRY.get(self.key)

    def start(self, env_overrides=None):
        spec = self.lab.cfg["commands"][self.name]
        env = dict(os.environ)
        env.update(spec["env"])
        env.update(env_overrides or {})
        log_path = spec["log"]
        handle = open(log_path, "ab")
        self.popen = subprocess.Popen(spec["argv"], env=env, stdout=handle,
                                      stderr=subprocess.STDOUT, cwd=self.lab.root)
        _PROC_REGISTRY[self.key] = self.popen
        with open(os.path.join(self.lab.log_dir, "pids.txt"), "a") as pids:
            pids.write("%d %s\n" % (self.popen.pid, self.name))
        return self.popen.pid

    def stop(self, sig=signal.SIGTERM, timeout=15):
        if self.popen is None:
            self.popen = _PROC_REGISTRY.get(self.key)
        if self.popen is not None:
            if self.popen.poll() is None:
                self.popen.send_signal(sig)
                try:
                    self.popen.wait(timeout=timeout)
                except subprocess.TimeoutExpired:
                    self.popen.kill()
                    self.popen.wait(timeout=10)
            _PROC_REGISTRY.pop(self.key, None)
            self.popen = None
            return
        # The process was started by the shell wrapper: kill the pid it recorded.
        pid = self.lab.cfg.get("pids", {}).get(self.name)
        if not pid:
            return
        pid = int(pid)
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            return
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return
            time.sleep(0.2)
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    def alive(self):
        return self.popen is not None and self.popen.poll() is None


def ensure_service(lab, name, port, timeout=45):
    """Start a service the driver (or a previous phase) stopped, if it is down."""
    if lab.wait_tcp(port, 1):
        return None
    proc = Proc(lab, name)
    proc.start()
    if not lab.wait_tcp(port, timeout):
        raise InfraFailure("%s 未能在 %ss 内重新监听 %s" % (name, timeout, port))
    return proc


class Lab:
    def __init__(self, cfg):
        self.cfg = cfg
        self.root = cfg["root"]
        self.evidence_dir = cfg["evidence_dir"]
        self.log_dir = cfg["log_dir"]
        self.secret_dir = cfg["secret_dir"]
        # Multi-environment labs (backup/restore drill) keep ports/urls/containers
        # under "a"/"b"; the single-environment lab keeps them at the top level.
        self.ports = cfg.get("ports", {})
        self.urls = cfg.get("urls", {})
        self.containers = cfg.get("containers", {})
        self.docker_config = os.environ.get("DOCKER_CONFIG", "")

    def psql(self, sql, db="gateway_test", container=None):
        proc = subprocess.run(
            ["docker", "exec", "-i", container or self.containers["pg"], "psql", "-v", "ON_ERROR_STOP=1",
             "-U", "gateway_test", "-d", db, "-At", "-c", sql],
            capture_output=True, text=True, timeout=180)
        if proc.returncode != 0:
            raise InfraFailure("psql 失败: %s" % redact(proc.stderr.strip()))
        return proc.stdout.strip()

    def psql_lines(self, sql, db="gateway_test", container=None):
        return [line for line in self.psql(sql, db, container).splitlines() if line.strip()]

    def redis(self, *args, container=None):
        proc = subprocess.run(
            ["docker", "exec", "-i", container or self.containers["redis"], "redis-cli", *args],
            capture_output=True, text=True, timeout=60)
        if proc.returncode != 0:
            raise InfraFailure("redis-cli 失败: %s" % redact(proc.stderr.strip()))
        return proc.stdout.strip()

    def docker(self, args, timeout=600):
        proc = subprocess.run(["docker"] + args, capture_output=True, text=True, timeout=timeout)
        if proc.returncode != 0:
            raise InfraFailure("docker %s 失败: %s" % (" ".join(args[:3]), redact(proc.stderr.strip())))
        return proc

    def redis_raw(self, *args):
        """redis-cli without input echo suppression (used for FLUSHALL)."""
        return self.redis(*args)

    def stop_container(self, name, timeout=60):
        subprocess.run(["docker", "stop", "-t", "1", name], capture_output=True, text=True, timeout=timeout)

    def start_container(self, name):
        proc = subprocess.run(["docker", "start", name], capture_output=True, text=True, timeout=60)
        if proc.returncode != 0:
            raise InfraFailure("docker start %s 失败: %s" % (name, redact(proc.stderr.strip())))
        return proc.stdout.strip()

    def wait_port_closed(self, port, timeout):
        """True once nothing accepts a connection on 127.0.0.1:port."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            sock = socket.socket()
            sock.settimeout(0.4)
            try:
                sock.connect(("127.0.0.1", int(port)))
                time.sleep(0.2)
            except OSError:
                return True
            finally:
                try:
                    sock.close()
                except OSError:
                    pass
        return False

    def wait_tcp(self, port, timeout):
        deadline = time.time() + timeout
        while time.time() < deadline:
            sock = socket.socket()
            sock.settimeout(0.5)
            try:
                sock.connect(("127.0.0.1", int(port)))
                sock.close()
                return True
            except OSError:
                time.sleep(0.2)
            finally:
                try:
                    sock.close()
                except OSError:
                    pass
        return False

    def http_json(self, url, method="GET", payload=None, headers=None, timeout=15):
        cmd = ["curl", "-s", "-m", str(timeout), "-X", method, "-w", "\n%{http_code}"]
        for key, value in (headers or {}).items():
            cmd += ["-H", "%s: %s" % (key, value)]
        if payload is not None:
            cmd += ["-H", "Content-Type: application/json", "--data-binary", json.dumps(payload)]
        cmd.append(url)
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 10)
        out = proc.stdout.rsplit("\n", 1)
        status = int(out[1]) if len(out) > 1 and out[1].strip().isdigit() else 0
        try:
            return status, json.loads(out[0])
        except Exception:
            return status, out[0]

    def control_json(self, path, method="GET", payload=None, timeout=20):
        token = self.cfg["tokens"]["control_admin"]
        return self.http_json(self.urls["control"] + path, method=method, payload=payload,
                              headers={"Authorization": "Bearer " + token}, timeout=timeout)

    def metering_json(self, path, timeout=120):
        token = self.cfg["tokens"]["metering_admin"]
        return self.http_json(self.urls["metering"] + path, headers={"Authorization": "Bearer " + token},
                              timeout=timeout)

    def gateway_metrics(self):
        text = subprocess.run(["curl", "-s", "-m", "10", self.urls["gateway_diag"] + "/metrics"],
                              capture_output=True, text=True, timeout=20).stdout
        out = {}
        for line in text.splitlines():
            if not line or line.startswith("#"):
                continue
            parts = line.split()
            if len(parts) >= 2:
                try:
                    out[parts[0]] = float(parts[1])
                except ValueError:
                    pass
        return out


class EnvLab:
    """One environment (a/b) of a multi-environment lab, seen through the Lab API.

    Fixtures, sse_request and Proc are written against a single lab; EnvLab lets
    the backup/restore drill reuse them for the restored environment without
    teaching every caller about two labs.
    """

    def __init__(self, lab, env):
        self._lab = lab
        self.env = env
        self.cfg = dict(lab.cfg)
        self.cfg["commands"] = env["commands"]
        self.cfg["pids"] = env.get("pids", {})
        self.cfg["admin_password_file"] = lab.cfg["admin_password_file"]
        self.evidence_dir = lab.evidence_dir
        self.log_dir = lab.log_dir
        self.secret_dir = lab.secret_dir
        self.root = lab.root
        self.urls = env["urls"]
        self.ports = env["ports"]
        self.containers = env["containers"]

    def psql(self, sql, db="gateway_test"):
        return self._lab.psql(sql, db, container=self.containers["pg"])

    def psql_lines(self, sql, db="gateway_test"):
        return self._lab.psql_lines(sql, db, container=self.containers["pg"])

    def psql_db(self, db, sql):
        return self._lab.psql(sql, db, container=self.containers["pg"])

    def redis(self, *args):
        return self._lab.redis(*args, container=self.containers["redis"])

    def docker(self, args, timeout=600):
        return self._lab.docker(args, timeout)

    def http_json(self, url, method="GET", payload=None, headers=None, timeout=15):
        return self._lab.http_json(url, method, payload, headers, timeout)

    def control_json(self, path, method="GET", payload=None, timeout=20):
        return self._lab.http_json(self.urls["control"] + path, method=method, payload=payload,
                                   headers={"Authorization": "Bearer " + self.cfg["tokens"]["control_admin"]},
                                   timeout=timeout)

    def metering_json(self, path, timeout=180):
        return self._lab.http_json(self.urls["metering"] + path,
                                   headers={"Authorization": "Bearer " + self.cfg["tokens"]["metering_admin"]},
                                   timeout=timeout)

    def gateway_metrics(self):
        text = subprocess.run(["curl", "-s", "-m", "10", self.urls["gateway_diag"] + "/metrics"],
                              capture_output=True, text=True, timeout=20).stdout
        out = {}
        for line in text.splitlines():
            if not line or line.startswith("#"):
                continue
            parts = line.split()
            if len(parts) >= 2:
                try:
                    out[parts[0]] = float(parts[1])
                except ValueError:
                    pass
        return out

    def wait_tcp(self, port, timeout):
        return self._lab.wait_tcp(port, timeout)

    def wait_port_closed(self, port, timeout):
        return self._lab.wait_port_closed(port, timeout)

    def wait_http(self, url, timeout):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            try:
                status, _ = self.http_json(url, timeout=5)
                last = status
                if status == 200:
                    return True
            except Exception as err:  # noqa: BLE001
                last = repr(err)
            time.sleep(0.3)
        return last

    def stop_container(self, name, timeout=60):
        self._lab.stop_container(name, timeout)

    def start_container(self, name):
        self._lab.start_container(name)


class Actor:
    def __init__(self, name, email, password, user_id, session, platform_admin=False):
        self.name = name
        self.email = email
        self.password = password
        self.user_id = str(user_id)
        self.session = session
        self.platform_admin = platform_admin


class Fixtures:
    """Two enterprises and their actors, created through real HTTP."""

    def __init__(self, lab, client, cid="FIX"):
        self.lab = lab
        self.client = client
        self.cid = cid
        self.actors = {}
        self.orgs = {}
        self.keys = {}
        self.invitations = {}
        self.opening_codes = {}
        self.extra_passwords = []
        self.session_tokens = []
        self.csrf_tokens = []
        self.prices = {}
        self.secrets_path = os.path.join(lab.secret_dir, "fixtures.json")

    def url(self, name):
        return self.lab.urls[name]

    def enable_platform_models(self, check, models):
        """Merge required fixture models into the complete platform catalog."""
        admin = self.actors["platform-admin"]
        path = "/api/v1/admin/models"
        current = self.client.call(check, "read-platform-model-catalog", "GET", path,
                                   base_url=self.url("console"), session=admin.session)
        check.require_status(current, 200, label="读取平台模型目录")
        current_body = current.json()
        check.require(isinstance(current_body, dict), "平台模型目录响应不是对象")
        existing = current_body.get("items")
        check.require(isinstance(existing, list), "平台模型目录响应缺少 items 数组")
        catalog = {}
        for item in existing:
            check.require(isinstance(item, dict) and isinstance(item.get("model"), str) and
                          isinstance(item.get("enabled"), bool),
                          "平台模型目录包含无效条目：%r" % item)
            check.require(item["model"] not in catalog,
                          "平台模型目录包含重复条目：%s" % item["model"])
            catalog[item["model"]] = item["enabled"]
        for model in models:
            catalog[model] = True
        requested = [{"model": model, "enabled": catalog[model]} for model in sorted(catalog)]
        updated = self.client.call(check, "enable-platform-fixture-models", "PUT", path,
                                   base_url=self.url("console"), body={"items": requested},
                                   session=admin.session)
        check.require_status(updated, 200, label="启用平台测试模型并保留现有目录")
        updated_body = updated.json()
        check.require(isinstance(updated_body, dict), "更新平台模型目录响应不是对象")
        items = updated_body.get("items")
        check.require(isinstance(items, list), "更新平台模型目录响应缺少 items 数组")
        updated_catalog = {}
        for item in items:
            check.require(isinstance(item, dict) and isinstance(item.get("model"), str) and
                          isinstance(item.get("enabled"), bool),
                          "更新后的平台模型目录包含无效条目：%r" % item)
            check.require(item["model"] not in updated_catalog,
                          "更新后的平台模型目录包含重复条目：%s" % item["model"])
            updated_catalog[item["model"]] = item["enabled"]
        check.require(updated_catalog == catalog,
                      "更新平台模型目录时有条目被删除或改写：expected=%r actual=%r" %
                      (catalog, updated_catalog))
        enabled = {model for model, is_enabled in updated_catalog.items() if is_enabled}
        check.require(set(models).issubset(enabled),
                      "测试模型未全部启用：requested=%r enabled=%r" % (models, sorted(enabled)))
        check.detail("platform_model_catalog_fixture_models", sorted(models))
        check.detail("platform_model_catalog_preserved_entries", len(existing))

    def require_personal_wallet(self, check, actor, only_personal=False):
        response = self.client.call(check, "personal-account-" + actor.name, "GET", "/api/v1/organizations",
                                    base_url=self.url("console"), session=actor.session)
        check.require_status(response, 200, label="自助注册用户的账户 " + actor.name)
        memberships = (response.json() or {}).get("items", [])
        personal = [item for item in memberships
                    if ((item.get("organization") or {}).get("type") == "personal")]
        check.require(len(personal) == 1,
                      "%s 应保留且仅有一个 personal 账户，实际=%r" % (actor.name, personal))
        if len(personal) != 1:
            return
        check.require(personal[0].get("role") == "owner" and personal[0].get("status") == "active",
                      "%s 的 personal membership 非 active owner：%r" % (actor.name, personal[0]))
        if only_personal:
            check.require(len(memberships) == 1,
                          "%s 注册后不应自动加入 enterprise：%r" % (actor.name, memberships))
        if not actor.user_id.isdigit() or int(actor.user_id) <= 0:
            check.require(False, "%s 注册响应缺少有效 user id：%r" % (actor.name, actor.user_id))
            return
        wallets = self.lab.psql_lines(
            "SELECT count(*) FROM organizations o "
            "JOIN memberships m ON m.org_id=o.id AND m.user_id=o.owner_user_id "
            "AND m.role='owner' AND m.status='active' "
            "JOIN billing_accounts a ON a.org_id=o.id "
            "WHERE o.type='personal' AND o.owner_user_id=%s AND m.user_id=%s"
            % (actor.user_id, actor.user_id))
        check.require(wallets == ["1"],
                      "%s 的 personal 钱包/owner membership 未原子创建：%r" % (actor.name, wallets))

    def login(self, check, actor, console="console", label=None):
        response = self.client.call(check, label or ("login-" + actor.name), "POST", "/api/v1/auth/login",
                                    base_url=self.lab.urls[console],
                                    body={"email": actor.email, "password": actor.password}, csrf=None)
        if response.status == 429:
            raise InfraFailure("登录被节流（429）：%s" % actor.email)
        check.require_status(response, 200, label="login " + actor.email)
        payload = response.json() or {}
        cookies = cookies_of(response)
        token = cookies.get("gw_session")
        check.require(bool(token), "登录响应缺少 gw_session cookie")
        csrf = cookies.get("gw_csrf") or payload.get("csrf_token", "")
        session = {"token": token, "csrf": csrf, "user_id": str((payload.get("user") or {}).get("id", "")),
                   "email": actor.email}
        actor.session = session
        RED.add(token)
        RED.add(csrf)
        self.session_tokens.append(token)
        self.csrf_tokens.append(csrf)
        return session

    def create(self):
        """All fixture traffic runs under one bookkeeping check (no assertions here)."""
        check = Check(self.cid, "fixtures")
        admin_password = open(self.lab.cfg["admin_password_file"]).read().strip()
        RED.add(admin_password)
        admin = Actor("platform-admin", self.lab.cfg["admin_email"], admin_password, "0", None, True)
        self.login(check, admin, label="login-platform-admin")
        self.actors["platform-admin"] = admin
        admin_id = admin.session["user_id"]
        admin.user_id = admin_id
        self.enable_platform_models(check, ("default", "t10-model"))

        tag = self.lab.cfg["run_id"][-6:].lower().replace("-", "")[-6:]
        self.tag = tag

        def register_user(name):
            email = "t10-%s-%s@t10.test" % (tag, name)
            password = secrets.token_urlsafe(18)
            response = self.client.call(check, "register-user-" + name, "POST", "/api/v1/auth/register",
                                        base_url=self.url("console"),
                                        body={"email": email, "password": password}, csrf=None)
            check.require_status(response, 201, label="self-register " + email)
            RED.add(password)
            registration = response.json() or {}
            cookies = cookies_of(response)
            registration_token = cookies.get("gw_session", "")
            registration_csrf = cookies.get("gw_csrf") or registration.get("csrf_token", "")
            check.require(bool(registration_token), "注册响应缺少 gw_session cookie")
            check.require(bool(registration_csrf), "注册响应缺少 CSRF token")
            RED.add(registration_token)
            RED.add(registration_csrf)
            self.session_tokens.append(registration_token)
            self.csrf_tokens.append(registration_csrf)
            user = registration.get("user") or {}
            actor = Actor(name, email, password, str(user.get("id", "")), None, False)
            self.actors[name] = actor
            self.login(check, actor, label="login-registered-" + name)
            self.require_personal_wallet(check, actor, only_personal=True)
            return actor

        # Enterprise A
        a_owner = register_user("a-owner")
        a_admin = register_user("a-admin")
        a_member = register_user("a-member")
        a_member3 = register_user("a-member3")
        a_member2 = register_user("a-member2")     # removed-member-with-live-key case
        a_disabled = register_user("a-disabled")   # disabled-user-with-live-session case
        invitee = register_user("a-invitee")       # invitation tamper case
        register_user("csrf-invitee")              # isolated actor for unsafe-route probes
        # Enterprise B
        b_owner = register_user("b-owner")
        b_member = register_user("b-member")

        # Ordinary users cannot open an enterprise from the user organization list.
        legacy = self.client.call(check, "legacy-self-create-org", "POST", "/api/v1/organizations",
                                  base_url=self.url("console"), body={"name": "t10-%s-forbidden" % tag},
                                  session=a_owner.session)
        check.require_status(legacy, 405, label="普通用户不能 POST /organizations 自助开户")
        denied = self.client.call(check, "non-admin-create-org", "POST", "/api/v1/admin/organizations",
                                  base_url=self.url("console"),
                                  body={"name": "t10-%s-forbidden" % tag, "owner_email": a_owner.email},
                                  session=a_owner.session)
        check.require_status(denied, 403, "forbidden", label="普通用户不能手动开户")
        response = self.client.call(check, "create-org-acme", "POST", "/api/v1/admin/organizations",
                                    base_url=self.url("console"),
                                    body={"name": "t10-%s-acme" % tag, "owner_email": a_owner.email},
                                    session=admin.session)
        check.require_status(response, 201, label="平台管理员按 owner_email 手动创建 org A")
        self.orgs["A"] = str(response.json()["organization"]["id"])
        check.require(response.json().get("role") == "owner",
                      "手动开户的 owner_email 未成为 owner：%r" % response.json())
        # Platform admin is an independent identity, not an implicit member.
        admin_members = self.client.call(check, "platform-admin-not-org-member", "GET",
                                         "/api/v1/orgs/%s/members" % self.orgs["A"],
                                         base_url=self.url("console"), session=admin.session)
        check.require_status(admin_members, 404, "not_found", label="平台管理员未被自动加入企业")
        # The legacy `organizations` assignment is rejected before any user is created.
        assignment = self.client.call(check, "legacy-admin-user-org-assignment", "POST", "/api/v1/admin/users",
                                      base_url=self.url("console"),
                                      body={"email": "t10-%s-assignment@t10.test" % tag,
                                            "password": secrets.token_urlsafe(18),
                                            "organizations": [self.orgs["A"]]}, session=admin.session)
        check.require_status(assignment, 400, "invalid_request",
                             label="admin/users 不再支持 organizations 代建成员")
        self.require_personal_wallet(check, a_owner, only_personal=False)

        # Organization B uses the separate, single-use activation-code path.
        code_response = self.client.call(check, "create-opening-code-beta", "POST", "/api/v1/admin/activation-codes",
                                         base_url=self.url("console"),
                                         body={"enterprise_name": "t10-%s-beta" % tag}, session=admin.session)
        check.require_status(code_response, 201, label="平台管理员签发 enterprise 开户码")
        opening_code = (code_response.json() or {}).get("code", "")
        check.require(bool(opening_code), "开户码响应缺少一次性 code")
        RED.add(opening_code)
        self.opening_codes["B"] = {"code": opening_code, "id": (code_response.json() or {}).get("id", "")}
        response = self.client.call(check, "activate-org-beta", "POST", "/api/v1/organizations/activate",
                                    base_url=self.url("console"), body={"code": opening_code},
                                    session=b_owner.session)
        check.require_status(response, 201, label="b-owner 使用开户码创建 org B")
        self.orgs["B"] = str(response.json()["organization"]["id"])
        check.require(response.json().get("role") == "owner",
                      "开户码创建的 user 未成为 owner：%r" % response.json())
        self.require_personal_wallet(check, b_owner, only_personal=False)
        check.require(self.orgs["A"].isdigit() and self.orgs["B"].isdigit(),
                      "企业开户响应缺少数字组织 ID，无法验证默认模型范围")
        inherited_scopes = self.lab.psql_lines(
            "SELECT count(*) FROM organizations WHERE id IN (%s,%s) AND model_allowlist IS NULL"
            % (int(self.orgs["A"]), int(self.orgs["B"])))
        check.require(inherited_scopes == ["2"],
                      "新建企业应以 NULL 范围继承全局模型目录：%r" % inherited_scopes)
        check.detail("enterprise_model_scope", "A/B inherit platform catalog")

        def invite_and_accept(owner, actor, org, role):
            invitation = {"email": actor.email, "role": role}
            if role == "member":
                invitation["monthly_limit_micro"] = "100000000"
            response = self.client.call(check, "invite-" + actor.name, "POST",
                                        "/api/v1/orgs/%s/invitations" % org, base_url=self.url("console"),
                                        body=invitation, session=owner.session)
            check.require_status(response, 201, label="invite " + actor.name)
            token = (response.json() or {}).get("token", "")
            check.require(bool(token), "邀请响应缺少 token")
            RED.add(token)
            self.login(check, actor)
            accept = self.client.call(check, "accept-" + actor.name, "POST",
                                      "/api/v1/invitations/%s/accept" % token, base_url=self.url("console"),
                                      body={}, session=actor.session)
            check.require_status(accept, 200, label="accept " + actor.name)
            membership = accept.json() or {}
            if role == "member":
                check.require(membership.get("monthly_limit_micro") == "100000000",
                              "普通 member 接受邀请后额度未保留：%r" % membership)
            self.invitations[actor.name] = {"token": token, "email": actor.email, "user_id": actor.user_id}
            return token

        missing_member_cap = self.client.call(
            check, "invite-member-without-cap", "POST", "/api/v1/orgs/%s/invitations" % self.orgs["A"],
            base_url=self.url("console"), body={"email": "t10-%s-no-cap@t10.test" % tag, "role": "member"},
            session=a_owner.session)
        check.require_status(missing_member_cap, 400, label="普通 member 邀请必须显式提交额度")
        invite_and_accept(a_owner, a_admin, self.orgs["A"], "admin")
        invite_and_accept(a_owner, a_member, self.orgs["A"], "member")
        invite_and_accept(a_owner, a_member3, self.orgs["A"], "member")
        invite_and_accept(a_owner, a_member2, self.orgs["A"], "member")
        invite_and_accept(a_owner, a_disabled, self.orgs["A"], "member")
        invite_and_accept(b_owner, b_member, self.orgs["B"], "member")
        # 待接受邀请需要用受邀者自己的 session 去试（否则 401 掩盖了邀请逻辑），
        # 但**不**接受它：SEC-05 要拿它做篡改/未知/错 email 的靶子。
        self.login(check, invitee, label="login-a-invitee")

        # A pending invitation (used by the tamper/negative cases).
        response = self.client.call(check, "invite-pending", "POST", "/api/v1/orgs/%s/invitations" % self.orgs["A"],
                                    base_url=self.url("console"),
                                    body={"email": invitee.email, "role": "member",
                                          "monthly_limit_micro": "100000000"}, session=a_owner.session)
        check.require_status(response, 201, label="invite pending")
        pending_token = (response.json() or {}).get("token", "")
        RED.add(pending_token)
        self.invitations["pending"] = {"token": pending_token, "email": invitee.email,
                                       "user_id": invitee.user_id}

        # Keys: every key is created through the Console → Control AdminService path.
        def create_key(actor, org, name=None, operation_id=None):
            name = name or "t10-%s-key" % actor.name
            body = {"name": name}
            if operation_id:
                body["operation_id"] = operation_id
            response = self.client.call(check, "create-key-" + actor.name, "POST",
                                        "/api/v1/orgs/%s/keys" % org, base_url=self.url("console"),
                                        body=body, session=actor.session)
            check.require_status(response, 201, label="create key for " + actor.name)
            payload = response.json() or {}
            key = payload.get("secret", "")
            check.require(bool(key), "创建 Key 响应缺少 secret")
            RED.add(key)
            metadata = payload["key"]
            membership = self.lab.psql_lines(
                "SELECT k.membership_id, m.id, k.owner_user_id, m.user_id, k.name "
                "FROM api_keys k JOIN memberships m ON m.id=k.membership_id AND m.org_id=k.org_id "
                "AND m.user_id=k.owner_user_id AND m.status='active' "
                "WHERE k.key_id='%s'" % metadata["key_id"])
            parts = membership[0].split("|") if len(membership) == 1 else []
            check.require(bool(parts) and parts[0] == parts[1] and parts[2] == actor.user_id and
                          parts[3] == actor.user_id and parts[4] == name,
                          "Key 未固定到本人当前 membership / 名称：actor=%s key=%r" % (actor.name, membership))
            check.require(metadata.get("name") == name and metadata.get("owner_user_id") == actor.user_id and
                          metadata.get("organization_id") == org,
                          "Key DTO 的名称/归属与创建者不一致：%r" % metadata)
            return {"secret": key, "key_id": metadata["key_id"], "prefix": metadata["prefix"],
                    "owner_user_id": metadata.get("owner_user_id"), "name": name,
                    "membership_id": parts[0] if parts else ""}

        self.keys["a-member"] = create_key(a_member, self.orgs["A"])
        self.keys["a-member3"] = create_key(a_member3, self.orgs["A"])
        self.keys["a-member2"] = create_key(a_member2, self.orgs["A"])
        self.keys["a-disabled"] = create_key(a_disabled, self.orgs["A"])
        self.keys["a-admin"] = create_key(a_admin, self.orgs["A"])
        self.keys["b-member"] = create_key(b_member, self.orgs["B"])
        self.keys["a-owner"] = create_key(a_owner, self.orgs["A"])

        # Prices: 1 micro per token in both directions for the model the load
        # test uses ("default") and the drill model ("t10-model").
        effective_at = (datetime.now(timezone.utc) - timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        for model in ("default", "t10-model"):
            response = self.client.call(check, "price-" + model, "POST", "/api/v1/admin/prices",
                                        base_url=self.url("console"),
                                        body={"model": model, "currency": "CNY",
                                              "input_price_micro_per_million": "1000000",
                                              "output_price_micro_per_million": "1000000",
                                              "effective_at": effective_at},
                                        session=admin.session)
            check.require_status(response, 201, label="create price " + model)
            self.prices[model] = {"id": str((response.json() or {}).get("id", "")), "v1_effective_at": effective_at}

        # Fund both organizations through the audited platform-admin adjustment.
        for org, amount in ((self.orgs["A"], 50_000_000), (self.orgs["B"], 5_000_000)):
            response = self.client.call(check, "fund-" + org, "POST",
                                        "/api/v1/admin/billing/accounts/%s/adjustments" % org,
                                        base_url=self.url("console"),
                                        body={"type": "recharge", "amount_micro": str(amount),
                                              "payment_reference": "t10-fund-%s" % org,
                                              "reason": "t10 security matrix funding"},
                                        session=admin.session,
                                        extra_headers={"Idempotency-Key": "t10-fund-%s" % org})
            check.require_status(response, 201, label="fund org " + org)

        # Wait for the gateway snapshot to carry the fixture keys (≤5s target).
        deadline = time.time() + 20
        while time.time() < deadline:
            status, _ = self.lab.http_json(self.url("gateway") + "/v1/models",
                                           headers={"Authorization": "Bearer " + self.keys["a-member"]["secret"]})
            if status == 200:
                break
            time.sleep(0.3)
        else:
            raise InfraFailure("Gateway 快照未在 20s 内包含 fixture Key")

        # Persist secrets (0600, outside the evidence dir) and a redacted summary.
        state = {
            "orgs": self.orgs,
            "keys": self.keys,
            "invitations": self.invitations,
            "opening_codes": self.opening_codes,
            "prices": self.prices,
            "actors": {name: {"email": a.email, "user_id": a.user_id, "password": a.password,
                              "session": a.session, "platform_admin": a.platform_admin}
                       for name, a in self.actors.items()},
        }
        with open(self.secrets_path, "w", encoding="utf-8") as handle:
            os.chmod(self.secrets_path, 0o600)
            json.dump(state, handle, indent=1)
        summary = {
            "orgs": self.orgs,
            "users": {name: {"email": a.email, "user_id": a.user_id, "platform_admin": a.platform_admin}
                      for name, a in self.actors.items()},
            "keys": {name: {"key_id": k["key_id"], "prefix": k["prefix"],
                            "owner_user_id": k["owner_user_id"], "name": k["name"],
                            "membership_id": k["membership_id"]}
                     for name, k in self.keys.items()},
            "prices": self.prices,
            "invitations": {name: {"email": v["email"], "user_id": v["user_id"]}
                            for name, v in self.invitations.items()},
            "opening_codes": {name: {"id": v["id"]} for name, v in self.opening_codes.items()},
        }
        with open(os.path.join(self.lab.evidence_dir, "fixtures.json"), "w", encoding="utf-8") as handle:
            json.dump(summary, handle, indent=2, ensure_ascii=False)
        return check


# ---------------------------------------------------------------------------
# SSE / load helpers
# ---------------------------------------------------------------------------

STREAM_BODY = {"model": "t10-model", "messages": [{"role": "user", "content": "hi"}], "stream": True}


def sse_request(lab, key, *, body=None, timeout=60, extra_headers=None, path="/v1/chat/completions",
                request_id="t10-sse"):
    """One streaming request measured event by event (curl --no-buffer + pipe)."""
    payload = json.dumps(body or STREAM_BODY)
    cmd = ["curl", "-s", "-N", "--max-time", str(timeout), "-X", "POST",
           lab.urls["gateway"] + path,
           "-H", "Authorization: Bearer " + key,
           "-H", "Content-Type: application/json",
           "-H", "Accept: text/event-stream",
           "-H", "X-Request-ID: " + request_id,
           "-w", "\n__HTTP__%{http_code}",
           "--data-binary", payload]
    for name, value in (extra_headers or {}).items():
        cmd += ["-H", "%s: %s" % (name, value)]
    started = time.time()
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)
    result = {"status": 0, "first_event": None, "first_content": None, "usage": None,
              "done": None, "total": None, "bytes": 0, "error_body": "", "incomplete": False,
              "curl_timeout": False, "text": ""}
    text_parts = []
    try:
        for line in proc.stdout:
            now = time.time() - started
            text_parts.append(line)
            result["bytes"] += len(line)
            if line.startswith("__HTTP__"):
                tail = line.strip().split("__HTTP__")[-1]
                if tail.isdigit():
                    result["status"] = int(tail)
                continue
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if not data:
                continue
            if data == "[DONE]":
                result["done"] = now
                continue
            try:
                event = json.loads(data)
            except Exception:
                continue
            if result["first_event"] is None:
                result["first_event"] = now
            choices = event.get("choices") or []
            if choices:
                delta = choices[0].get("delta") or {}
                if delta.get("content"):
                    if result["first_content"] is None:
                        result["first_content"] = now
            if event.get("usage"):
                result["usage"] = event["usage"]
                result["usage_at"] = now
        proc.wait(timeout=10)
    except Exception as err:  # noqa: BLE001
        result["error_body"] = repr(err)
    result["total"] = time.time() - started
    result["text"] = "".join(text_parts)
    # 去掉 curl 追加的 __HTTP__<code> 尾巴，得到纯响应体（非 200 时用于错误信封）。
    result["body"] = re.sub(r"__HTTP__\d+\s*$", "", result["text"]).strip()
    err = proc.stderr.read() if proc.stderr else ""
    if proc.returncode == 28:
        result["curl_timeout"] = True
    if proc.returncode not in (0, 28):
        result["error_body"] = (result["error_body"] + " " + err).strip()
    if result["status"] == 0 and result["text"]:
        match = re.search(r"__HTTP__(\d+)", result["text"])
        if match:
            result["status"] = int(match.group(1))
    if result["status"] != 200:
        result["error_body"] = result["body"][:500]
    result["incomplete"] = result["status"] == 200 and result["done"] is None
    return result


def sse_ok(check, result, label):
    if result["status"] != 200:
        raise CheckFailure("%s: 期望 HTTP 200，实际 %s（%s）" % (label, result["status"], result["error_body"][:200]))
    check.require(result["done"] is not None,
                  "%s: 流没有收到 [DONE]（incomplete）；bytes=%d" % (label, result["bytes"]))
    return result


def percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, int(round(fraction * (len(ordered) - 1)))))
    return ordered[index]


class LoadtestRun:
    """cmd/loadtest invocation; the API key goes through the environment only."""

    def __init__(self, lab, name, *, key, concurrency, total, shape="s", rate=0, timeout=60,
                 tolerate=True, extra_args=None):
        self.lab = lab
        self.name = name
        self.key = key
        self.json_path = os.path.join(lab.evidence_dir, "raw", "loadtest-%s.json" % name)
        os.makedirs(os.path.dirname(self.json_path), exist_ok=True)
        self.stdout_path = os.path.join(lab.log_dir, "loadtest-%s.log" % name)
        self.cmd = [lab.cfg["bin_dir"] + "/loadtest",
                    "-target", lab.urls["gateway"] + "/v1/chat/completions",
                    "-c", str(concurrency), "-n", str(total), "-shape", shape,
                    "-timeout", "%ds" % timeout, "-json", self.json_path]
        if rate:
            self.cmd += ["-rate", str(rate), "-mode", "open"]
        if tolerate:
            self.cmd += ["-tolerate-drops"]
        if extra_args:
            self.cmd += extra_args

    def run(self):
        started = time.time()
        proc = subprocess.run(self.cmd, capture_output=True, text=True, timeout=900,
                              env={**os.environ, "GATEWAY_API_KEY": self.key})
        with open(self.stdout_path, "w", encoding="utf-8") as handle:
            handle.write(proc.stdout + "\n--- stderr ---\n" + proc.stderr)
        report = {}
        try:
            with open(self.json_path, encoding="utf-8") as handle:
                report = json.load(handle)
        except Exception:
            pass
        return {"exit_code": proc.returncode, "elapsed": time.time() - started,
                "stdout_tail": proc.stdout[-1200:], "report": report,
                "json_path": self.json_path, "stdout_path": self.stdout_path}


def report_summary(report):
    """Normalize cmd/loadtest's JSON (loadtest.Report has no json tags: PascalCase)."""
    body = (report or {}).get("report") or {}

    def pct(name):
        value = body.get(name) or {}
        return {"count": value.get("Count"), "p50_ms": ns_to_ms(value.get("P50")),
                "p95_ms": ns_to_ms(value.get("P95")), "p99_ms": ns_to_ms(value.get("P99")),
                "max_ms": ns_to_ms(value.get("Max"))}

    return {
        "scheduled": body.get("Scheduled"), "started": body.get("Started"),
        "completed": body.get("Completed"), "rejected": body.get("Rejected"),
        "transport_error": body.get("TransportError"), "incomplete": body.get("Incomplete"),
        "cancelled": body.get("Cancelled"), "parse_error": body.get("ParseError"),
        "loadgen_dropped": body.get("LoadgenDropped"), "peak_active": body.get("PeakActive"),
        "status": body.get("Status"),
        "first_event": pct("FirstEvent"), "first_content": pct("FirstContent"),
        "stream_duration": pct("StreamDuration"), "ttft": pct("TTFT"),
        "success_per_second": (report or {}).get("success_per_second"),
        "capacity_round_valid": body.get("CapacityRoundValid"),
        "capacity_invalid_reasons": body.get("CapacityInvalidReasons"),
    }


def ns_to_ms(value):
    if value is None:
        return None
    if isinstance(value, str):
        match = re.match(r"^([0-9.]+)(ns|µs|us|ms|s)$", value)
        if match:
            number = float(match.group(1))
            unit = match.group(2)
            return number / 1e6 if unit == "ns" else number / 1e3 if unit in ("µs", "us") else number * 1e3 if unit == "s" else number
        try:
            value = float(value)
        except ValueError:
            return None
    return round(float(value) / 1e6, 3)

DRIVER_LIB_EOF
cat > "$driver_path" <<'DRIVER_EOF'
#!/usr/bin/env python3
"""T10 security matrix / F10 / F12 / isolation assertions (see security-matrix.sh)."""

import argparse
import json
import os
import signal
import subprocess
import sys
import threading
import time
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import t10_driver_lib as t10  # noqa: E402
from t10_driver_lib import *  # noqa: E402,F401,F403
from t10_driver_lib import (  # noqa: E402,F401
    Actor, Check, CheckFailure, Client, Fixtures, InfraFailure, Lab, Proc, RED,
    Response, SkipCheck, cookies_of, ensure_service, log, ns_to_ms, percentile,
    redact, sse_ok, sse_request, STREAM_BODY, slugify, utcnow,
)

# ---------------------------------------------------------------------------
# fixtures
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# F16 security matrix
# ---------------------------------------------------------------------------

class Matrix:
    def __init__(self, lab, fx, client):
        self.lab = lab
        self.fx = fx
        self.client = client
        self.checks = []
        self.findings = []

    # -- runner ------------------------------------------------------------
    def run(self, cid, title, fn, phase="security"):
        check = Check(cid, title)
        log("")
        log("[%s] %s" % (cid, title))
        try:
            fn(check)
            check.status = "PASS"
        except SkipCheck as err:
            check.status = "SKIP"
            check.skip_reason = str(err)
        except CheckFailure as err:
            check.status = "FAIL"
            check.failure = redact(str(err))
        except InfraFailure:
            raise
        except Exception as err:  # noqa: BLE001
            check.status = "FAIL"
            check.failure = "unexpected: %r" % (err,)
        check.finished_at = utcnow()
        self.checks.append(check)
        log("[%s] %s%s" % (cid, check.status, (" — " + check.failure) if check.failure else ""))
        return check

    def call(self, check, label, method, path, **kwargs):
        kwargs.setdefault("base_url", self.lab.urls["console"])
        return self.client.call(check, label, method, path, **kwargs)

    def finding(self, text):
        self.findings.append(redact(text))
        log("FINDING: " + text)

    # -- helpers -----------------------------------------------------------
    def actor(self, name):
        return self.fx.actors[name]

    def normalized_error(self, response):
        """错误信封去掉 request_id 后的规范化 JSON（用于"逐字节同形"比较）。"""
        payload = response.json() or {}
        payload.pop("request_id", None)
        if isinstance(payload.get("error"), dict):
            payload["error"].pop("request_id", None)
        return json.dumps(payload, sort_keys=True)

    def nonexistent_org_body(self, check, org="99999999"):
        response = self.call(check, "nonexistent-org-%s" % org, "GET", "/api/v1/orgs/%s/members" % org,
                             session=self.actor("a-owner").session)
        return response, self.normalized_error(response)

    def org_member_snapshot(self, check, org, session, label):
        response = self.call(check, "snapshot-members-" + label, "GET", "/api/v1/orgs/%s/members" % org,
                             session=session)
        check.require_status(response, 200, label="members snapshot " + label)
        return (response.json() or {}).get("items", [])

    def org_key_snapshot(self, check, org, session, label):
        response = self.call(check, "snapshot-keys-" + label, "GET", "/api/v1/orgs/%s/keys" % org,
                             session=session)
        check.require_status(response, 200, label="keys snapshot " + label)
        return (response.json() or {}).get("items", [])

    def money_snapshot(self, check, org, label):
        account = self.call(check, "snapshot-account-" + label, "GET", "/api/v1/orgs/%s/billing/account" % org,
                            session=self.actor("a-owner").session)
        entries = self.call(check, "snapshot-entries-" + label, "GET",
                            "/api/v1/orgs/%s/billing/entries?limit=100" % org,
                            session=self.actor("a-owner").session)
        check.require_status(account, 200, label="account " + label)
        check.require_status(entries, 200, label="entries " + label)
        return {
            "account": account.json(),
            "entry_ids": sorted(item.get("id") for item in (entries.json() or {}).get("items", [])),
        }

    def sensitive_strings(self):
        """Distinctive B-organization strings that must never appear in A's responses.

        Bare numeric ids are deliberately excluded: a 1-2 digit user id occurs in
        unrelated timestamps/ids and would produce false leaks. Quoted JSON forms
        are used instead.
        """
        b = self.fx.orgs["B"]
        out = ["t10-%s-beta" % self.fx.tag, "org:%s" % b,
               self.fx.keys["b-member"]["key_id"], self.fx.keys["b-member"]["prefix"]]
        for name in ("b-owner", "b-member"):
            actor = self.actor(name)
            out.append(actor.email)
            out.append('"user_id":"%s"' % actor.user_id)
            out.append('"actor_user_id":"%s"' % actor.user_id)
        out.append('"organization_id":"%s"' % b)
        return [value for value in out if value]

    def scan_no_leak(self, check, label, body):
        lowered = body or ""
        for needle in self.sensitive_strings():
            check.require(needle not in lowered,
                          "%s: 响应泄露了另一组织的元数据 %r" % (label, needle))

    # -- SEC-01 ------------------------------------------------------------
    def sec01(self, check):
        for label, path in (("me", "/api/v1/me"), ("organizations", "/api/v1/organizations"),
                            ("org-members", "/api/v1/orgs/%s/members" % self.fx.orgs["A"]),
                            ("org-keys", "/api/v1/orgs/%s/keys" % self.fx.orgs["A"]),
                            ("org-usage", "/api/v1/orgs/%s/usage" % self.fx.orgs["A"])):
            response = self.call(check, "console-no-session-" + label, "GET", path)
            check.require_status(response, 401, "unauthorized", label="console " + label)
            check.require(response.message() == "authentication required",
                          "console %s: message=%r" % (label, response.message()))
        # Gateway: no Authorization at all
        for label, headers in (("no-header", {}),):
            response = self.client.call(check, "gateway-no-auth-" + label, "POST", "/v1/chat/completions",
                                        base_url=self.lab.urls["gateway"], body=STREAM_BODY,
                                        extra_headers=headers)
            check.require_status(response, 401, "unauthorized", label="gateway " + label)
        # Console login itself is public: a missing session must not break it.
        response = self.call(check, "login-missing-body", "POST", "/api/v1/auth/login", body={}, csrf=None)
        check.require(response.status in (400, 401), "空登录请求应被拒绝，实际 %d" % response.status)

    # -- SEC-02 ------------------------------------------------------------
    def sec02(self, check):
        member = self.fx.keys["a-member"]["secret"]
        cases = [
            ("basic-scheme", {"Authorization": "Basic %s" % "Zm9vOmJhcg=="}),
            ("token-scheme", {"Authorization": "Token %s" % member}),
            ("raw-key-no-scheme", {"Authorization": member}),
            ("empty-bearer", {"Authorization": "Bearer "}),
            ("garbage-bearer", {"Authorization": "Bearer sk-t10-not-a-key"}),
        ]
        for label, headers in cases:
            response = self.client.call(check, "gateway-wrong-scheme-" + label, "POST", "/v1/chat/completions",
                                        base_url=self.lab.urls["gateway"], body=STREAM_BODY,
                                        extra_headers=headers)
            check.require_status(response, 401, "unauthorized", label="gateway " + label)
            check.require(response.message() in ("missing Authorization", "invalid Authorization", "invalid api key"),
                          "gateway %s: message=%r" % (label, response.message()))
        # RFC 7235 的 auth-scheme 大小写不敏感：`bearer <key>` 必须被接受（这是正确
        # 行为，不是漏洞）。sse_request 会自己拼 Bearer 头，这里直接用 curl 断言。
        response = self.client.call(check, "gateway-lowercase-bearer", "POST", "/v1/chat/completions",
                                    base_url=self.lab.urls["gateway"], body=STREAM_BODY,
                                    extra_headers={"Authorization": "bearer " + member})
        check.require(response.status == 200,
                      "RFC 7235 允许 auth-scheme 大小写不敏感，`bearer` 期望 200，实际 %d" % response.status)
        # Console: a cookie with junk instead of a session token.
        response = self.call(check, "console-junk-cookie", "GET", "/api/v1/me",
                             session={"token": "!!!not-a-token!!!", "csrf": "x"})
        check.require_status(response, 401, "unauthorized", label="console junk cookie")
        # API key secrets never work as a Console credential.
        response = self.call(check, "console-key-as-session", "GET", "/api/v1/me",
                             session={"token": member, "csrf": "x"})
        check.require_status(response, 401, "unauthorized", label="console key-as-session")

    # -- SEC-03 ------------------------------------------------------------
    def sec03(self, check):
        victim = self.actor("a-member")
        original = victim.session["token"]
        tampered = original[:-1] + ("A" if original[-1] != "A" else "B")
        response = self.call(check, "tampered-last-char", "GET", "/api/v1/me",
                             session={"token": tampered, "csrf": victim.session["csrf"]})
        check.require_status(response, 401, "unauthorized", label="tampered session")
        check.require(response.message() == "session is invalid or expired",
                      "篡改 session 的 message=%r" % response.message())
        # Truncated / extended / swapped tokens
        for label, token in (("truncated", original[:16]), ("extended", original + "00"),
                             ("prefix-only", "gw_" + original[:8])):
            response = self.call(check, "tampered-" + label, "GET", "/api/v1/me",
                                 session={"token": token, "csrf": victim.session["csrf"]})
            check.require_status(response, 401, "unauthorized", label="tampered " + label)
        # A valid session with a tampered CSRF token on an unsafe route.
        response = self.call(check, "tampered-csrf", "POST", "/api/v1/orgs/%s/keys" % self.fx.orgs["A"],
                             body={"name": "t10-csrf-should-not-create"}, session=victim.session,
                             csrf=victim.session["csrf"][:-2] + "zz")
        check.require_status(response, 403, "forbidden", label="tampered csrf")
        check.require(response.message() == "missing or invalid CSRF token",
                      "篡改 CSRF 的 message=%r" % response.message())
        # The session itself still works (proves the 401s above were about the
        # tampered value, not a broken fixture).
        response = self.call(check, "control-valid-session", "GET", "/api/v1/me", session=victim.session)
        check.require_status(response, 200, label="valid session control")

    # -- SEC-04 ------------------------------------------------------------
    def sec04(self, check):
        actor = self.actor("a-member")
        password = actor.password
        login = self.client.call(check, "short-ttl-login", "POST", "/api/v1/auth/login",
                                 base_url=self.lab.urls["console_short"],
                                 body={"email": actor.email, "password": password}, csrf=None)
        if login.status == 429:
            raise SkipCheck("短 TTL Console 登录被节流")
        check.require_status(login, 200, label="short-ttl login")
        cookies = cookies_of(login)
        session = {"token": cookies["gw_session"], "csrf": cookies.get("gw_csrf", "")}
        RED.add(session["token"])
        RED.add(session["csrf"])
        self.fx.session_tokens.append(session["token"])
        self.fx.csrf_tokens.append(session["csrf"])
        first = self.client.call(check, "short-ttl-immediate", "GET", "/api/v1/me",
                                 base_url=self.lab.urls["console_short"], session=session)
        check.require_status(first, 200, label="短 TTL session 立即可用（证明 2s 前是有效的）")
        time.sleep(3.2)
        expired = self.client.call(check, "short-ttl-expired", "GET", "/api/v1/me",
                                   base_url=self.lab.urls["console_short"], session=session)
        check.require_status(expired, 401, "unauthorized", label="过期 session")
        check.require(expired.message() == "session is invalid or expired",
                      "过期 session 的 message=%r" % expired.message())
        # The cookie must be cleared on the rejection.
        cleared = [value for value in expired.header_all("Set-Cookie") if "gw_session=" in value]
        check.require(any("Max-Age=0" in value or "Expires=" in value for value in cleared) or cleared,
                      "过期 session 的响应没有清除 cookie")
        # A fresh login on the long-TTL console still works (the account is fine).
        relogin = self.client.call(check, "short-ttl-relogin-main", "POST", "/api/v1/auth/login",
                                   base_url=self.lab.urls["console"],
                                   body={"email": actor.email, "password": password}, csrf=None)
        check.require_status(relogin, 200, label="主 Console 重新登录")
        relogin_cookies = cookies_of(relogin)
        RED.add(relogin_cookies.get("gw_session", ""))
        RED.add(relogin_cookies.get("gw_csrf", ""))
        self.fx.session_tokens.append(relogin_cookies.get("gw_session", ""))
        self.fx.csrf_tokens.append(relogin_cookies.get("gw_csrf", ""))

    # -- SEC-05 ------------------------------------------------------------
    def sec05(self, check):
        pending = self.fx.invitations["pending"]
        token = pending["token"]
        tampered = token[:-4] + ("AAAA" if token[-4:] != "AAAA" else "BBBB")
        cases = [
            ("tampered", tampered, self.actor("a-invitee"), 404, "not_found", "invitation not found"),
            ("unknown", "t10-unknown-" + secrets.token_urlsafe(8), self.actor("a-invitee"), 404,
             "not_found", "invitation not found"),
            ("wrong-email", token, self.actor("a-member"), 403, "forbidden",
             "invitation was issued for a different email"),
        ]
        for label, value, actor, status, code, message in cases:
            response = self.call(check, "invitation-" + label, "POST", "/api/v1/invitations/%s/accept" % value,
                                 body={}, session=actor.session)
            check.require_status(response, status, code, label="invitation " + label)
            check.require(response.message() == message,
                          "invitation %s: message=%r（期望 %r）" % (label, response.message(), message))
        # The pending invitation is untouched by the failed attempts.
        listing = self.call(check, "invitation-still-pending", "GET",
                            "/api/v1/orgs/%s/invitations" % self.fx.orgs["A"],
                            session=self.actor("a-owner").session)
        check.require_status(listing, 200, label="invitation list")
        pending_items = [item for item in (listing.json() or {}).get("items", [])
                         if item.get("email") == pending["email"]]
        check.require(len(pending_items) == 1 and pending_items[0]["status"] == "pending",
                      "失败尝试后待接受邀请的状态被改变: %r" % pending_items)
        # A second accept with the same token must be a conflict, not a second membership.
        proposal = self.call(check, "invitation-create-second", "POST",
                             "/api/v1/orgs/%s/invitations" % self.fx.orgs["A"],
                             body={"email": self.actor("a-invitee").email, "role": "admin"},
                             session=self.actor("a-owner").session)
        check.require_status(proposal, 201, label="second invitation")
        second_token = (proposal.json() or {}).get("token", "")
        RED.add(second_token)
        first_accept = self.call(check, "invitation-accept-second", "POST",
                                 "/api/v1/invitations/%s/accept" % second_token,
                                 body={"email": "attacker@t10.test", "role": "owner"},
                                 session=self.actor("a-invitee").session)
        check.require_status(first_accept, 200, label="接受邀请（请求体 email/role 必须被忽略）")
        membership = first_accept.json() or {}
        check.require(membership.get("role") == "admin",
                      "接受邀请时请求体 role 生效了：%r" % membership.get("role"))
        replay = self.call(check, "invitation-replay", "POST",
                           "/api/v1/invitations/%s/accept" % second_token, body={},
                           session=self.actor("a-invitee").session)
        check.require_status(replay, 409, "conflict", label="同 token 二次接受")
        check.require(replay.message() == "invitation was already accepted",
                      "二次接受的 message=%r" % replay.message())

    # -- SEC-06 ------------------------------------------------------------
    def sec06(self, check):
        org_b = self.fx.orgs["B"]
        month = datetime.now(timezone.utc).strftime("%Y-%m")
        a_owner = self.actor("a-owner").session
        _, reference = self.nonexistent_org_body(check)
        paths = [
            ("organization", "/api/v1/organizations/%s" % org_b),
            ("members", "/api/v1/orgs/%s/members" % org_b),
            ("invitations", "/api/v1/orgs/%s/invitations" % org_b),
            ("keys", "/api/v1/orgs/%s/keys" % org_b),
            ("usage", "/api/v1/orgs/%s/usage" % org_b),
            ("audit", "/api/v1/orgs/%s/audit" % org_b),
            ("account", "/api/v1/orgs/%s/billing/account" % org_b),
            ("entries", "/api/v1/orgs/%s/billing/entries" % org_b),
            ("statements", "/api/v1/orgs/%s/billing/statements?month=%s" % (org_b, month)),
            ("exceptions", "/api/v1/orgs/%s/billing/exceptions" % org_b),
        ]
        identical = 0
        for label, path in paths:
            response = self.call(check, "cross-org-read-" + label, "GET", path, session=a_owner)
            check.require_status(response, 404, "not_found", label="跨组织读 " + label)
            check.require(self.normalized_error(response) == reference,
                          '跨组织读 %s 与「不存在的组织」不同形: %s' % (
                              label, self.normalized_error(response)))
            identical += 1
            self.scan_no_leak(check, "跨组织读 " + label, response.body)
        check.detail("identical_404_count", identical)
        check.note("10 个跨组织读取全部 404 且与不存在的组织逐字节同形")
        # The same paths on the caller's own organization must work (the test is not vacuous).
        own = self.call(check, "own-org-members", "GET", "/api/v1/orgs/%s/members" % self.fx.orgs["A"],
                        session=a_owner)
        check.require_status(own, 200, label="本组织 members 必须可用")
        check.require(len((own.json() or {}).get("items", [])) >= 4,
                      "本组织成员列表不完整: %r" % own.json())

    # -- SEC-07 ------------------------------------------------------------
    def sec07(self, check):
        org_b = self.fx.orgs["B"]
        b_owner = self.actor("b-owner").session
        b_member_id = self.actor("b-member").user_id
        b_key_id = self.fx.keys["b-member"]["key_id"]
        members_before = self.org_member_snapshot(check, org_b, b_owner, "b-before")
        keys_before = self.org_key_snapshot(check, org_b, self.actor("b-member").session, "b-before")
        checks = [
            ("patch-member", "PATCH", "/api/v1/orgs/%s/members/%s" % (org_b, b_member_id),
             {"role": "admin"}),
            ("delete-member", "DELETE", "/api/v1/orgs/%s/members/%s" % (org_b, b_member_id), None),
            ("create-invitation", "POST", "/api/v1/orgs/%s/invitations" % org_b,
             {"email": "cross@t10.test", "role": "member", "monthly_limit_micro": "100000000"}),
            ("owner-transfer", "POST", "/api/v1/orgs/%s/owner-transfer" % org_b,
             {"user": b_member_id}),
            ("create-key", "POST", "/api/v1/orgs/%s/keys" % org_b, {"name": "cross-org-probe"}),
            ("revoke-key", "POST", "/api/v1/orgs/%s/keys/%s/revoke" % (org_b, b_key_id), None),
        ]
        for label, method, path, body in checks:
            response = self.call(check, "cross-org-write-" + label, method, path, body=body,
                                 session=self.actor("a-owner").session)
            # 跨组织写入：存在性不泄露 → 404（与不存在的组织同形）。
            check.require_status(response, 404, "not_found", label="跨组织写 " + label)
            self.scan_no_leak(check, "跨组织写 " + label, response.body)
        members_after = self.org_member_snapshot(check, org_b, b_owner, "b-after")
        keys_after = self.org_key_snapshot(check, org_b, self.actor("b-member").session, "b-after")
        check.require(members_before == members_after,
                      "跨组织写尝试改变了 B 的成员: before=%r after=%r" % (members_before, members_after))
        check.require(keys_before == keys_after,
                      "跨组织写尝试改变了 B 的 Key: before=%r after=%r" % (keys_before, keys_after))
        check.note("6 个跨组织写入全部 404，B 的成员与 Key 状态逐字段不变")

    # -- SEC-08 ------------------------------------------------------------
    def sec08(self, check):
        org = self.fx.orgs["A"]
        member = self.actor("a-member")
        admin = self.actor("a-admin")
        owner = self.actor("a-owner")
        # member → admin（改自己的角色）
        response = self.call(check, "member-self-promote", "PATCH",
                             "/api/v1/orgs/%s/members/%s" % (org, member.user_id),
                             body={"role": "admin"}, session=member.session)
        check.require_status(response, 403, "forbidden", label="member 自升 admin")
        # admin → owner（PATCH 与 owner-transfer 两条路径）
        response = self.call(check, "admin-self-promote-owner", "PATCH",
                             "/api/v1/orgs/%s/members/%s" % (org, admin.user_id),
                             body={"role": "owner"}, session=admin.session)
        check.require_status(response, 403, "forbidden", label="admin PATCH 自升 owner")
        response = self.call(check, "admin-self-transfer", "POST", "/api/v1/orgs/%s/owner-transfer" % org,
                             body={"user": admin.user_id}, session=admin.session)
        check.require(response.status in (400, 403),
                      "admin 自升 owner（transfer）期望 400/403，实际 %d" % response.status)
        # admin 不能降级 owner
        response = self.call(check, "admin-demote-owner", "PATCH",
                             "/api/v1/orgs/%s/members/%s" % (org, owner.user_id),
                             body={"role": "member", "monthly_limit_micro": "100000000"},
                             session=admin.session)
        check.require_status(response, 403, "forbidden", label="admin 降级 owner")
        # 自升 platform_admin：组织内任何角色都不行
        for name in ("a-member", "a-admin", "a-owner"):
            actor = self.actor(name)
            response = self.call(check, "promote-platform-admin-" + name, "POST", "/api/v1/admin/users",
                                 body={"email": "evil-%s@t10.test" % name, "password": secrets.token_urlsafe(12),
                                       "platform_admin": True}, session=actor.session)
            check.require_status(response, 403, "forbidden", label="%s 创建 platform_admin" % name)
        response = self.call(check, "invite-platform-admin", "POST", "/api/v1/orgs/%s/invitations" % org,
                             body={"email": "evil-invite@t10.test", "role": "platform_admin"},
                             session=owner.session)
        check.require_status(response, 403, "forbidden", label="邀请 platform_admin")
        check.require(response.message() == "platform_admin is not an organization role",
                      "platform_admin 邀请的 message=%r" % response.message())
        # 事后再确认角色没有变化（owner 仍是 owner，member 仍是 member）
        members = self.org_member_snapshot(check, org, owner.session, "a-after-escalation")
        roles = {item["user_id"]: item["role"] for item in members}
        check.require(roles.get(owner.user_id) == "owner", "owner 角色被改变: %r" % roles.get(owner.user_id))
        check.require(roles.get(admin.user_id) == "admin", "admin 角色被改变: %r" % roles.get(admin.user_id))
        check.require(roles.get(member.user_id) == "member", "member 角色被改变: %r" % roles.get(member.user_id))
        # 平台管理员创建 platform_admin 用户是允许的（证明 403 来自授权而不是路由不存在）
        email = "t10-%s-pa2@t10.test" % self.fx.tag
        password = secrets.token_urlsafe(16)
        response = self.call(check, "platform-admin-create-admin", "POST", "/api/v1/admin/users",
                             body={"email": email, "password": password, "platform_admin": True},
                             session=self.actor("platform-admin").session)
        check.require_status(response, 201, label="平台管理员创建 platform_admin")
        RED.add(password)
        self.fx.actors["platform-admin-2"] = Actor("platform-admin-2", email, password,
                                                   str((response.json() or {}).get("id", "")), None, True)
        self.fx.login(check, self.fx.actors["platform-admin-2"], label="login-platform-admin-2")
        # 新平台管理员可以访问 admin 端点，但**不是**任何组织的 owner/admin：
        # 组织作用域角色与平台角色互不叠加（首版不支持角色叠加）。
        response = self.call(check, "platform-admin-org-members", "GET", "/api/v1/orgs/%s/members" % org,
                             session=self.fx.actors["platform-admin-2"].session)
        check.require(response.status in (403, 404),
                      "platform_admin 不应自动获得组织内角色，实际 %d" % response.status)

    # -- SEC-09 ------------------------------------------------------------
    def csrf_probe(self, check, route_label, method, path, body, session, csrf, extra_headers=None):
        response = self.call(check, "csrf-%s-%s" % (route_label, csrf["label"]), method, path, body=body,
                             session=session, csrf=csrf["value"], extra_headers=extra_headers)
        check.require_status(response, 403, "forbidden", label="%s %s" % (route_label, csrf["label"]))
        check.require(response.message() == "missing or invalid CSRF token",
                      "%s %s: message=%r" % (route_label, csrf["label"], response.message()))

    def sec09(self, check):
        org = self.fx.orgs["A"]
        org_b = self.fx.orgs["B"]
        owner = self.actor("a-owner")
        member = self.actor("a-member")
        admin = self.actor("platform-admin")
        invitee = self.actor("csrf-invitee")
        csrf_created_user_email = "csrf-user-%s@t10.test" % self.fx.tag
        # 为了证明"本来可以成功"的路由也被 CSRF 保护，为 accept 专门建一个邀请。
        proposal = self.call(check, "csrf-invitation", "POST", "/api/v1/orgs/%s/invitations" % org,
                             body={"email": invitee.email, "role": "member",
                                   "monthly_limit_micro": "100000000"},
                             session=owner.session)
        check.require_status(proposal, 201, label="CSRF 探针邀请")
        token = (proposal.json() or {}).get("token", "")
        RED.add(token)
        self.fx.invitations["csrf"] = {"token": token, "email": invitee.email, "user_id": invitee.user_id}
        # Prepare valid one-time inputs so missing/wrong CSRF reaches the middleware
        # guard on routes that otherwise would mutate state.
        opening = self.call(check, "csrf-opening-code", "POST", "/api/v1/admin/activation-codes",
                            body={"enterprise_name": "t10-%s-csrf-opening" % self.fx.tag}, session=admin.session)
        check.require_status(opening, 201, label="CSRF 开户码 setup")
        opening_payload = opening.json() or {}
        opening_code = opening_payload.get("code", "")
        check.require(bool(opening_code), "CSRF 开户码 setup 缺少一次性 code")
        RED.add(opening_code)
        self.fx.opening_codes["csrf"] = {"code": opening_code, "id": opening_payload.get("id", "")}
        application = self.call(check, "csrf-join-request-setup", "POST",
                                "/api/v1/orgs/%s/join-requests" % org_b,
                                body={"display_name": "t10 csrf applicant"}, session=invitee.session)
        check.require_status(application, 201, label="CSRF 入企申请 setup")
        application_id = str((application.json() or {}).get("id", ""))
        check.require(bool(application_id), "入企申请 setup 缺少 id")
        csrf_price_effective_at = (datetime.now(timezone.utc) - timedelta(hours=2)).strftime("%Y-%m-%dT%H:%M:%SZ")
        csrf_new_password = secrets.token_urlsafe(18)
        RED.add(csrf_new_password)
        self.fx.extra_passwords.append(csrf_new_password)

        unsafe = [
            ("logout", "POST", "/api/v1/auth/logout", None, owner, "owner"),
            ("change-password", "POST", "/api/v1/me/password",
             {"old_password": invitee.password, "new_password": csrf_new_password}, invitee, "invitee"),
            ("create-org", "POST", "/api/v1/admin/organizations",
             {"name": "t10-%s-csrf-x" % self.fx.tag, "owner_email": invitee.email}, admin, "platform-admin"),
            ("create-opening-code", "POST", "/api/v1/admin/activation-codes",
             {"enterprise_name": "t10-%s-csrf-x" % self.fx.tag}, admin, "platform-admin"),
            ("activate-enterprise", "POST", "/api/v1/organizations/activate",
             {"code": opening_code}, invitee, "invitee"),
            ("create-join-request", "POST", "/api/v1/orgs/%s/join-requests" % org_b,
             {"display_name": "t10 should not join"}, invitee, "invitee"),
            ("decide-join-request", "POST", "/api/v1/orgs/%s/join-requests/%s/decision" % (org_b, application_id),
             {"status": "approved", "monthly_limit_micro": "100000000"},
             self.actor("b-owner"), "b-owner"),
            ("admin-create-user", "POST", "/api/v1/admin/users",
             {"email": csrf_created_user_email, "password": secrets.token_urlsafe(12)}, admin, "platform-admin"),
            ("admin-user-status", "POST", "/api/v1/admin/users/%s/status" % self.actor("a-disabled").user_id,
             {"status": "disabled"}, admin, "platform-admin"),
            ("admin-org-status", "POST", "/api/v1/admin/organizations/%s/status" % org_b,
             {"status": "suspended"}, admin, "platform-admin"),
            ("create-invitation", "POST", "/api/v1/orgs/%s/invitations" % org,
             {"email": "csrf2@t10.test", "role": "member", "monthly_limit_micro": "100000000"}, owner, "owner"),
            ("accept-invitation", "POST", "/api/v1/invitations/%s/accept" % token, {}, invitee, "invitee"),
            ("change-member-role", "PATCH", "/api/v1/orgs/%s/members/%s" % (org, member.user_id),
             {"role": "admin"}, owner, "owner"),
            ("change-member-quota", "PATCH",
             "/api/v1/orgs/%s/members/%s/quota?month=%s" % (org, self.actor("a-member3").user_id,
                                                             datetime.now(timezone.utc).strftime("%Y-%m")),
             {"monthly_limit_micro": "200000000"}, owner, "owner"),
            ("remove-member", "DELETE", "/api/v1/orgs/%s/members/%s" % (org, member.user_id), None,
             owner, "owner"),
            ("owner-transfer", "POST", "/api/v1/orgs/%s/owner-transfer" % org,
             {"user": self.actor("a-admin").user_id}, owner, "owner"),
            ("create-key", "POST", "/api/v1/orgs/%s/keys" % org,
             {"name": "t10-csrf-should-not-create"}, owner, "owner"),
            ("revoke-key", "POST", "/api/v1/orgs/%s/keys/%s/revoke" % (org, self.fx.keys["a-owner"]["key_id"]),
             None, owner, "owner"),
            ("create-price", "POST", "/api/v1/admin/prices",
             {"model": "default", "currency": "CNY", "input_price_micro_per_million": "1",
              "output_price_micro_per_million": "1", "effective_at": csrf_price_effective_at},
             admin, "platform-admin"),
            ("adjust-account", "POST", "/api/v1/admin/billing/accounts/%s/adjustments" % org,
             {"type": "recharge", "amount_micro": "1", "payment_reference": "t10-csrf-recharge",
              "reason": "csrf probe"},
             admin, "platform-admin"),
            ("resolve-exception", "POST",
             "/api/v1/admin/billing/exceptions/99999999/resolution",
             {"decision": "dismiss", "reason": "csrf probe"}, admin, "platform-admin"),
        ]
        def snapshot_items(label, path, session):
            response = self.call(check, label, "GET", path, session=session)
            check.require_status(response, 200, label="CSRF 状态快照 " + label)
            return (response.json() or {}).get("items", [])

        # 快照：CSRF 尝试前后必须逐字段一致。
        before = {
            "members": self.org_member_snapshot(check, org, owner.session, "before-csrf"),
            "b_members": self.org_member_snapshot(check, org_b, self.actor("b-owner").session, "b-before-csrf"),
            "keys": {
                "owner": self.org_key_snapshot(check, org, owner.session, "owner-before-csrf"),
                "admin": self.org_key_snapshot(check, org, self.actor("a-admin").session, "admin-before-csrf"),
                "member": self.org_key_snapshot(check, org, member.session, "member-before-csrf"),
            },
            "b_member_keys": self.org_key_snapshot(check, org_b, self.actor("b-member").session, "b-member-before-csrf"),
            "money": self.money_snapshot(check, org, "before-csrf"),
            "prices": snapshot_items("prices-before-csrf", "/api/v1/admin/prices", admin.session),
            "invitations": snapshot_items("invitations-before-csrf", "/api/v1/orgs/%s/invitations" % org,
                                           owner.session),
            "opening_codes": snapshot_items("opening-codes-before-csrf", "/api/v1/admin/activation-codes",
                                             admin.session),
            "applications": snapshot_items("applications-before-csrf",
                                            "/api/v1/orgs/%s/join-requests?status=pending" % org_b,
                                            self.actor("b-owner").session),
            "enterprises": snapshot_items("enterprises-before-csrf",
                                           "/api/v1/admin/organizations?type=enterprise&limit=100",
                                           admin.session),
            "disabled_user_status": self.lab.psql_lines(
                "SELECT status FROM users WHERE id=%s" % self.actor("a-disabled").user_id),
            "csrf_created_user_count": self.lab.psql_lines(
                "SELECT count(*) FROM users WHERE email='%s'" % csrf_created_user_email),
            "password_actor_auth_version": self.lab.psql_lines(
                "SELECT auth_version FROM users WHERE id=%s" % invitee.user_id),
        }
        for label, method, path, body, session, _actor in unsafe:
            for csrf in ({"label": "missing", "value": None}, {"label": "wrong", "value": "t10-wrong-token"}):
                # 每个 unsafe 路由都必须独立地拒绝缺 token 与错 token。
                extra = {}
                if label == "adjust-account":
                    extra["Idempotency-Key"] = "t10-csrf-probe"
                self.csrf_probe(check, label, method, path, body, session.session, csrf,
                                extra_headers=extra or None)
        after = {
            "members": self.org_member_snapshot(check, org, owner.session, "after-csrf"),
            "b_members": self.org_member_snapshot(check, org_b, self.actor("b-owner").session, "b-after-csrf"),
            "keys": {
                "owner": self.org_key_snapshot(check, org, owner.session, "owner-after-csrf"),
                "admin": self.org_key_snapshot(check, org, self.actor("a-admin").session, "admin-after-csrf"),
                "member": self.org_key_snapshot(check, org, member.session, "member-after-csrf"),
            },
            "b_member_keys": self.org_key_snapshot(check, org_b, self.actor("b-member").session, "b-member-after-csrf"),
            "money": self.money_snapshot(check, org, "after-csrf"),
            "prices": snapshot_items("prices-after-csrf", "/api/v1/admin/prices", admin.session),
            "invitations": snapshot_items("invitations-after-csrf", "/api/v1/orgs/%s/invitations" % org,
                                           owner.session),
            "opening_codes": snapshot_items("opening-codes-after-csrf", "/api/v1/admin/activation-codes",
                                             admin.session),
            "applications": snapshot_items("applications-after-csrf",
                                            "/api/v1/orgs/%s/join-requests?status=pending" % org_b,
                                            self.actor("b-owner").session),
            "enterprises": snapshot_items("enterprises-after-csrf",
                                           "/api/v1/admin/organizations?type=enterprise&limit=100",
                                           admin.session),
            "disabled_user_status": self.lab.psql_lines(
                "SELECT status FROM users WHERE id=%s" % self.actor("a-disabled").user_id),
            "csrf_created_user_count": self.lab.psql_lines(
                "SELECT count(*) FROM users WHERE email='%s'" % csrf_created_user_email),
            "password_actor_auth_version": self.lab.psql_lines(
                "SELECT auth_version FROM users WHERE id=%s" % invitee.user_id),
        }
        for key in before:
            check.require(before[key] == after[key],
                          "CSRF 失败尝试改变了状态 %s: before=%r after=%r" % (key, before[key], after[key]))
        check.detail("unsafe_routes", len(unsafe))
        check.detail("csrf_probes", len(unsafe) * 2)
        check.note("%d 个 unsafe 路由 × (缺 CSRF, 错 CSRF) 全部 403，且成员/Key/资金/价格/邀请无变化" % len(unsafe))
        # 反向：同一个 session 带正确 CSRF 时必须成功（否则上面的 403 可能只是路由坏了）。
        ok = self.call(check, "csrf-correct-control", "POST", "/api/v1/orgs/%s/invitations" % org,
                       body={"email": "csrf-ok@t10.test", "role": "member",
                             "monthly_limit_micro": "100000000"}, session=owner.session,
                       csrf=owner.session["csrf"])
        check.require_status(ok, 201, label="正确 CSRF 必须成功")

    # -- SEC-10 ------------------------------------------------------------
    def sec10(self, check):
        actor = self.actor("a-disabled")
        # 先证明当前 session 有效、Key 有效。
        before = self.call(check, "disabled-before-me", "GET", "/api/v1/me", session=actor.session)
        check.require_status(before, 200, label="禁用前 session 有效")
        stream = sse_request(self.lab, self.fx.keys["a-disabled"]["secret"], timeout=30,
                             request_id="t10-sec10-before")
        check.require(stream["status"] == 200 and stream["done"] is not None,
                      "禁用前 Key 应可用，实际 status=%s done=%s" % (stream["status"], stream["done"]))
        # 平台管理员禁用该用户：users.auth_version+1，旧 session 立即失效。
        response = self.call(check, "disable-user", "POST",
                             "/api/v1/admin/users/%s/status" % actor.user_id,
                             body={"status": "disabled"}, session=self.actor("platform-admin").session)
        check.require_status(response, 200, label="禁用用户")
        after = self.call(check, "disabled-after-me", "GET", "/api/v1/me", session=actor.session)
        check.require_status(after, 401, "unauthorized", label="禁用后旧 session")
        check.require(after.message() == "session is invalid or expired",
                      "禁用后 session 的 message=%r" % after.message())
        revoked_stream = sse_request(self.lab, self.fx.keys["a-disabled"]["secret"], timeout=30,
                                     request_id="t10-sec10-after")
        check.require(revoked_stream["status"] == 403,
                      "禁用用户的 Key 期望 403，实际 %s（%s）" % (revoked_stream["status"],
                                                              revoked_stream["error_body"][:120]))
        # 重新启用：旧 session 必须仍然无效（auth_version 已前进），新登录可用。
        response = self.call(check, "enable-user", "POST",
                             "/api/v1/admin/users/%s/status" % actor.user_id,
                             body={"status": "active"}, session=self.actor("platform-admin").session)
        check.require_status(response, 200, label="重新启用用户")
        still = self.call(check, "reenabled-old-session", "GET", "/api/v1/me", session=actor.session)
        check.require_status(still, 401, "unauthorized", label="重新启用后旧 session 仍必须无效")
        self.fx.login(check, actor, label="reenabled-fresh-login")
        fresh = self.call(check, "reenabled-new-session", "GET", "/api/v1/me", session=actor.session)
        check.require_status(fresh, 200, label="重新启用后新登录可用")
        recovered = sse_request(self.lab, self.fx.keys["a-disabled"]["secret"], timeout=30,
                                request_id="t10-sec10-recovered")
        check.require(recovered["status"] == 200 and recovered["done"] is not None,
                      "重新启用后 Key 应恢复，实际 status=%s" % recovered["status"])

    # -- SEC-11 ------------------------------------------------------------
    def sec11(self, check):
        member2 = self.actor("a-member2")
        key = self.fx.keys["a-member2"]["secret"]
        before = sse_request(self.lab, key, timeout=30, request_id="t10-sec11-before")
        check.require(before["status"] == 200 and before["done"] is not None,
                      "移除前成员 Key 应可用，实际 %s" % before["status"])
        owner = self.actor("a-owner")
        # 用一个长一点的流来观察"移除成员不打断在途流"。
        slow_body = {"model": "t10-model", "messages": [{"role": "user", "content": "hi"}],
                     "stream": True, "x_mock_ttft_ms": 300, "x_mock_token_interval_ms": 400,
                     "x_mock_tokens": 12}
        holder = {}

        def run_stream():
            holder["result"] = sse_request(self.lab, key, body=slow_body, timeout=60,
                                           request_id="t10-sec11-inflight")

        thread = threading.Thread(target=run_stream)
        thread.start()
        time.sleep(1.0)
        response = self.call(check, "remove-member", "DELETE",
                             "/api/v1/orgs/%s/members/%s" % (self.fx.orgs["A"], member2.user_id),
                             session=owner.session)
        check.require(response.status in (200, 204),
                      "移除成员期望 200/204，实际 %d（%s）" % (response.status, response.message()))
        thread.join(timeout=60)
        result = holder.get("result") or {}
        check.require(result.get("status") == 200 and result.get("done") is not None,
                      "移除成员打断了在途流: status=%s done=%s bytes=%s" % (
                          result.get("status"), result.get("done"), result.get("bytes")))
        deadline = time.time() + 10
        last = None
        while time.time() < deadline:
            last = sse_request(self.lab, key, timeout=30, request_id="t10-sec11-after")
            if last["status"] == 403:
                break
            time.sleep(0.3)
        check.require(last["status"] == 403,
                      "被移出成员的 Key 期望 403，实际 %s（%s）" % (last["status"], last["error_body"][:120]))
        check.note("在途流排水完成（%.2fs），新请求 403 access forbidden" % (result.get("total") or 0))

    # -- SEC-12 ------------------------------------------------------------
    def sec12(self, check):
        org = self.fx.orgs["A"]
        member = self.actor("a-member3")
        admin = self.actor("a-admin")
        month = datetime.now(timezone.utc).strftime("%Y-%m")
        writes = [
            ("invite", "POST", "/api/v1/orgs/%s/invitations" % org,
             {"email": "member-write@t10.test", "role": "member", "monthly_limit_micro": "100000000"}),
            ("change-role", "PATCH", "/api/v1/orgs/%s/members/%s" % (org, self.actor("a-member").user_id),
             {"role": "admin"}),
            ("remove-member", "DELETE", "/api/v1/orgs/%s/members/%s" % (org, self.actor("a-member").user_id), None),
            ("owner-transfer", "POST", "/api/v1/orgs/%s/owner-transfer" % org,
             {"user": member.user_id}),
            ("create-price", "POST", "/api/v1/admin/prices",
             {"model": "default", "currency": "CNY", "input_price_micro_per_million": "1",
              "output_price_micro_per_million": "1", "effective_at": self.fx.prices["default"]["v1_effective_at"]}),
            ("adjust", "POST", "/api/v1/admin/billing/accounts/%s/adjustments" % org,
             {"type": "recharge", "amount_micro": "1", "payment_reference": "t10-member-probe",
              "reason": "member probe"}),
            ("resolve-exception", "POST", "/api/v1/admin/billing/exceptions/99999999/resolution",
             {"decision": "dismiss", "reason": "member probe"}),
            ("admin-users", "POST", "/api/v1/admin/users",
             {"email": "member-admin@t10.test", "password": secrets.token_urlsafe(12)}),
        ]
        for label, method, path, body in writes:
            extra = {"Idempotency-Key": "t10-member-%s" % label} if label == "adjust" else None
            response = self.call(check, "member-write-" + label, method, path, body=body,
                                 session=member.session, extra_headers=extra)
            check.require_status(response, 403, "forbidden", label="普通 member 写 " + label)

        audit = self.call(check, "member-audit", "GET", "/api/v1/orgs/%s/audit" % org,
                          session=member.session)
        check.require_status(audit, 403, "forbidden", label="普通 member 读审计")

        # Members may read their own usage/quota, but cannot widen the scope to another user.
        usage = self.call(check, "member-own-usage", "GET", "/api/v1/orgs/%s/usage?limit=10" % org,
                          session=member.session)
        check.require_status(usage, 200, label="普通 member 读本人用量")
        check.require((usage.json() or {}).get("scope") == "self",
                      "member usage scope=%r（期望 self）" % (usage.json() or {}).get("scope"))
        own_usage = self.call(check, "member-filter-own-usage", "GET",
                              "/api/v1/orgs/%s/usage?user=%s" % (org, member.user_id),
                              session=member.session)
        check.require_status(own_usage, 200, label="普通 member 显式查询本人用量")
        other_usage = self.call(check, "member-filter-other-usage", "GET",
                                "/api/v1/orgs/%s/usage?user=%s" % (org, self.actor("a-member").user_id),
                                session=member.session)
        check.require_status(other_usage, 403, "forbidden", label="普通 member 不能读取他人用量")
        quota = self.call(check, "member-own-quota", "GET", "/api/v1/orgs/%s/quota?month=%s" % (org, month),
                          session=member.session)
        check.require_status(quota, 200, label="普通 member 读本人额度")

        # Enterprise account/ledger/reporting data is limited to owner/admin.
        reads = [
            ("account", "/api/v1/orgs/%s/billing/account" % org),
            ("entries", "/api/v1/orgs/%s/billing/entries?limit=10" % org),
            ("statements", "/api/v1/orgs/%s/billing/statements?month=%s" % (org, month)),
            ("exceptions", "/api/v1/orgs/%s/billing/exceptions" % org),
        ]
        for label, path in reads:
            response = self.call(check, "member-read-org-" + label, "GET", path, session=member.session)
            check.require_status(response, 403, "forbidden", label="普通 member 不能读企业 " + label)
        admin_account = self.call(check, "admin-read-org-account", "GET",
                                  "/api/v1/orgs/%s/billing/account" % org, session=admin.session)
        check.require_status(admin_account, 200, label="企业 admin 可以读企业账户汇总")
        admin_usage = self.call(check, "admin-read-org-usage", "GET",
                                "/api/v1/orgs/%s/usage?limit=10" % org, session=admin.session)
        check.require_status(admin_usage, 200, label="企业 admin 可以读企业成员用量汇总")
        check.require((admin_usage.json() or {}).get("scope") == "organization",
                      "admin usage scope=%r（期望 organization）" % (admin_usage.json() or {}).get("scope"))

        # A member and an admin can manage their own named keys only. Neither can
        # enumerate, create, or revoke another member's key by supplying its user id.
        for actor in (member, admin):
            own_keys = self.org_key_snapshot(check, org, actor.session, actor.name + "-own")
            expected_key_name = "a-member3" if actor.name == "a-member3" else "a-admin"
            check.require(self.fx.keys[expected_key_name]["key_id"] in
                          {item.get("key_id") for item in own_keys},
                          "%s 看不到自己的 active Key：%r" % (actor.name, own_keys))
            check.require(all(item.get("owner_user_id") == actor.user_id and item.get("name")
                              for item in own_keys),
                          "%s 的 Key 列表不是仅含本人命名 Key：%r" % (actor.name, own_keys))
            check.require(self.fx.keys["a-member"]["key_id"] not in
                          {item.get("key_id") for item in own_keys},
                          "%s 看到了普通成员的 Key" % actor.name)
            listed = self.call(check, actor.name + "-list-other-keys", "GET",
                               "/api/v1/orgs/%s/keys?owner_user_id=%s" % (org, self.actor("a-member").user_id),
                               session=actor.session)
            check.require_status(listed, 404, "not_found", label="%s 不能枚举其他成员 Key" % actor.name)
            created = self.call(check, actor.name + "-create-other-key", "POST",
                                "/api/v1/orgs/%s/keys" % org,
                                body={"name": "t10-forbidden-other-key", "owner_user_id": self.actor("a-member").user_id},
                                session=actor.session)
            check.require_status(created, 404, "not_found", label="%s 不能代建其他成员 Key" % actor.name)
            revoked = self.call(check, actor.name + "-revoke-other-key", "POST",
                                "/api/v1/orgs/%s/keys/%s/revoke" % (org, self.fx.keys["a-member"]["key_id"]),
                                session=actor.session)
            check.require_status(revoked, 404, "not_found", label="%s 不能撤销其他成员 Key" % actor.name)

        missing_name = self.call(check, "member-create-key-without-name", "POST",
                                 "/api/v1/orgs/%s/keys" % org, body={}, session=member.session)
        check.require_status(missing_name, 400, "invalid_request", label="Key 创建必须提供名称")

    # -- SEC-13 ------------------------------------------------------------
    def sec13(self, check):
        org = self.fx.orgs["A"]
        admin_routes = [
            ("admin-users", "POST", "/api/v1/admin/users",
             {"email": "x@t10.test", "password": secrets.token_urlsafe(12)}),
            ("admin-organizations", "POST", "/api/v1/admin/organizations",
             {"name": "t10-forbidden-enterprise", "owner_email": self.actor("a-owner").email}),
            ("admin-opening-codes", "POST", "/api/v1/admin/activation-codes",
             {"enterprise_name": "t10-forbidden-opening"}),
            ("admin-user-status", "POST", "/api/v1/admin/users/%s/status" % self.actor("a-member").user_id,
             {"status": "active"}),
            ("admin-prices-get", "GET", "/api/v1/admin/prices", None),
            ("admin-prices-post", "POST", "/api/v1/admin/prices",
             {"model": "default", "currency": "CNY", "input_price_micro_per_million": "1",
              "output_price_micro_per_million": "1", "effective_at": "2026-01-01T00:00:00Z"}),
            ("admin-adjustments", "POST", "/api/v1/admin/billing/accounts/%s/adjustments" % org,
             {"type": "recharge", "amount_micro": "1", "payment_reference": "t10-sec13-probe",
              "reason": "sec13 probe"}),
            ("admin-exception-resolution", "POST", "/api/v1/admin/billing/exceptions/99999999/resolution",
             {"decision": "dismiss", "reason": "sec13 probe"}),
        ]
        for name in ("a-owner", "a-admin", "a-member", "a-member3"):
            actor = self.actor(name)
            for label, method, path, body in admin_routes:
                extra = {"Idempotency-Key": "t10-sec13"} if label == "admin-adjustments" else None
                response = self.call(check, "%s-%s" % (name, label), method, path, body=body,
                                     session=actor.session, extra_headers=extra)
                check.require_status(response, 403, "forbidden",
                                     label="%s 访问 %s" % (name, label))
                check.require(response.message() == "platform administrator privileges required",
                              "%s %s: message=%r" % (name, label, response.message()))
        # 未认证：401，而不是 403。
        for label, method, path, body in admin_routes:
            response = self.call(check, "anonymous-" + label, method, path, body=body)
            check.require_status(response, 401, "unauthorized", label="未认证访问 " + label)
        # 平台管理员必须成功（证明前面的 403 是授权而不是路由缺失）。
        response = self.call(check, "platform-admin-prices", "GET", "/api/v1/admin/prices",
                             session=self.actor("platform-admin").session)
        check.require_status(response, 200, label="平台管理员读价格")
        check.detail("admin_routes", len(admin_routes))
        check.detail("actors_checked", 4)
        check.note("%d 个 admin 路由 × 4 个组织角色 = 403；未认证 = 401；平台管理员 = 200" % len(admin_routes))

    # -- SEC-14 ------------------------------------------------------------
    def sec14(self, check):
        """Gateway 凭据矩阵：归属由 Key 决定，客户端不能改写租户。"""
        org_a = self.fx.orgs["A"]
        member_key = self.fx.keys["a-member"]["secret"]
        member3 = self.actor("a-member3")
        member3_key = self.fx.keys["a-member3"]["secret"]
        b_key = self.fx.keys["b-member"]["secret"]
        allowed = sse_request(self.lab, member_key, timeout=30, request_id="t10-sec14-member")
        check.require(allowed["status"] == 200 and allowed["done"] is not None,
                      "member Key 应放行，实际 %s" % allowed["status"])
        member3_result = sse_request(self.lab, member3_key, timeout=30, request_id="t10-sec14-member3")
        check.require(member3_result["status"] == 200 and member3_result["done"] is not None,
                      "普通 member 的命名 Key 应能调用，实际 %s" % member3_result["status"])
        cross = sse_request(self.lab, b_key, timeout=30, request_id="t10-sec14-crossorg",
                            extra_headers={"X-Organization-ID": org_a, "X-Org-ID": org_a,
                                           "X-Tenant-ID": "org:%s" % org_a,
                                           "X-User-ID": self.actor("a-owner").user_id})
        check.require(cross["status"] == 200 and cross["done"] is not None,
                      "B 的 Key 应能正常调用（归属 B），实际 %s" % cross["status"])
        # 归属必须是 Key 的 org，而不是请求头里的 org：计量行核对。
        time.sleep(2)
        rows = self.lab.psql_lines(
            "SELECT org_id, user_id, count(*) FROM metering_events WHERE key_id='%s' GROUP BY org_id, user_id"
            % self.fx.keys["b-member"]["key_id"])
        check.require(bool(rows) and all(row.startswith(self.fx.orgs["B"] + "|" + self.actor("b-member").user_id + "|")
                                         for row in rows),
                      "伪造请求头改写了 B Key 的 organization/user 计量归属: %r" % rows)
        metadata = self.lab.psql_lines(
            "SELECT k.org_id, k.owner_user_id, k.membership_id, m.user_id, m.status, k.name "
            "FROM api_keys k JOIN memberships m ON m.id=k.membership_id AND m.org_id=k.org_id "
            "WHERE k.key_id='%s'" % self.fx.keys["b-member"]["key_id"])
        expected = [self.fx.orgs["B"], self.actor("b-member").user_id,
                    self.fx.keys["b-member"]["membership_id"], self.actor("b-member").user_id,
                    "active", self.fx.keys["b-member"]["name"]]
        check.require(metadata == ["|".join(expected)],
                      "B Key 的 membership/user/name 绑定错误：%r" % metadata)
        check.require(member3.name == "a-member3", "普通 member Key 测试 actor 不正确")
        check.note("伪造 X-Organization-ID/X-Tenant-ID/X-User-ID 未改写 B Key 归属；Key 元数据固定到 B member 当前 membership")

    # -- SEC-15 ------------------------------------------------------------
    def sec15(self, check):
        """全局泄密扫描：所有响应 + 所有进程日志。

        规则不是"任何地方都不能出现"，而是**来源唯一**：每个 secret 只允许出现在
        创建它的那一个响应里（明文只回一次），其余任何响应与任何进程日志里出现
        都算泄露。
        """
        provenance = []  # (value, allowed {(method, path-suffix)}, kind)

        def allow(value, patterns, kind):
            if value:
                provenance.append((value, patterns, kind))

        for token in self.fx.session_tokens:
            allow(token, set(), "session token")
        # Registration/login responses carry a CSRF token; /me echoes the
        # current session's token for the SPA's double-submit header.
        for csrf in self.fx.csrf_tokens:
            allow(csrf, {("POST", "/api/v1/auth/register"), ("POST", "/api/v1/auth/login"),
                         ("GET", "/api/v1/me")}, "csrf token")
        for name, actor in self.fx.actors.items():
            allow(actor.password, set(), "password")
        for password in self.fx.extra_passwords:
            allow(password, set(), "password")
        for name, entry in self.fx.keys.items():
            allow(entry["secret"], {("POST", "/keys")}, "key secret")
        for name, entry in self.fx.invitations.items():
            allow(entry["token"], {("POST", "/invitations")}, "invitation token")
        for name, entry in self.fx.opening_codes.items():
            allow(entry["code"], {("POST", "/api/v1/admin/activation-codes")}, "enterprise opening code")
        allow(open(self.lab.cfg["admin_password_file"]).read().strip(), set(), "platform admin password")
        for name, value in self.lab.cfg.get("tokens", {}).items():
            allow(value, set(), name + " admin token")
        allow(self.lab.cfg.get("provider_secret", ""), set(), "provider credential secret")

        leaks = []
        allowed_hits = 0
        for record in self.client.records:
            for value, patterns, kind in provenance:
                if value not in record["body"]:
                    continue
                matching = any(record["method"] == method and record["path"].endswith(suffix)
                               for method, suffix in patterns)
                if matching:
                    allowed_hits += 1
                else:
                    leaks.append("%s 在 %s %s（%s）的响应里出现" % (
                        kind, record["method"], record["path"].split("?")[0], record["label"]))

        log_files = sorted(os.path.join(self.lab.log_dir, name)
                           for name in os.listdir(self.lab.log_dir) if name.endswith(".log"))
        log_leaks = []
        for path in log_files:
            try:
                text = open(path, encoding="utf-8", errors="replace").read()
            except OSError:
                continue
            for value, _patterns, kind in provenance:
                if value and value in text:
                    log_leaks.append("%s 出现在日志 %s" % (kind, os.path.basename(path)))

        evidence_leaks = []
        evidence_files = 0
        for directory, _subdirs, filenames in os.walk(self.lab.evidence_dir):
            for filename in filenames:
                path = os.path.join(directory, filename)
                try:
                    if os.path.getsize(path) > 64 * 1024 * 1024:
                        continue
                    with open(path, "rb") as handle:
                        content = handle.read()
                except OSError:
                    continue
                evidence_files += 1
                for value, _patterns, kind in provenance:
                    if value and value.encode("utf-8") in content:
                        evidence_leaks.append("%s 出现在 evidence %s" % (
                            kind, os.path.relpath(path, self.lab.evidence_dir)))

        check.detail("responses_scanned", len(self.client.records))
        check.detail("logs_scanned", len(log_files))
        check.detail("evidence_files_scanned", evidence_files)
        check.detail("secret_values_tracked", len(provenance))
        check.detail("allowed_single_occurrences", allowed_hits)
        check.require(not leaks, "响应泄密: " + "; ".join(sorted(set(leaks))[:6]))
        check.require(not log_leaks, "日志泄密: " + "; ".join(sorted(set(log_leaks))[:6]))
        check.require(not evidence_leaks, "Evidence 泄密: " + "; ".join(sorted(set(evidence_leaks))[:6]))
        check.require(allowed_hits > 0,
                      "没有任何 secret 出现在创建响应里（fixture 可能没跑）")
        check.note("%d 个响应、%d 个进程日志、%d 个 evidence 文件：%d 个 secret 值只在其创建响应里出现 %d 次，其余零出现"
                   % (len(self.client.records), len(log_files), evidence_files, len(provenance), allowed_hits))


# ---------------------------------------------------------------------------
# F10 — control plane outage / lease / desired-vs-applied
# ---------------------------------------------------------------------------

class FailurePhases:
    def __init__(self, lab, fx, client, matrix):
        self.lab = lab
        self.fx = fx
        self.client = client
        self.matrix = matrix
        self.control = Proc(lab, "control")
        self.console = Proc(lab, "console")
        self.metering = Proc(lab, "metering")
        self.results = {}

    # -- shared helpers ----------------------------------------------------
    def gateway_view(self):
        status, body = self.lab.control_json("/gateways")
        if status != 200 or not isinstance(body, dict):
            return None
        for instance in body.get("instances", []):
            if instance.get("instance_id") == "gw-t10":
                instance = dict(instance)
                instance["_view"] = {key: body.get(key) for key in
                                     ("desired_version", "stale_after_seconds", "authoritative", "status_store")}
                return instance
        return None

    def wait_view(self, predicate, timeout, what):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            try:
                view = self.gateway_view()
            except Exception as err:  # noqa: BLE001
                view = None
                last = repr(err)
            if view and predicate(view):
                return view
            last = view
            time.sleep(0.25)
        raise CheckFailure("等待 %s 超时（最后视图=%r）" % (what, last))

    def push_policy(self, burst=6000, rpm=60000):
        org = self.fx.orgs["A"]
        status, body = self.lab.control_json("/config", method="POST", payload={
            "org_policies": {org: {"requests_per_minute": rpm, "burst": burst}}})
        if status != 200:
            raise InfraFailure("推送 org policy 失败: %s %s" % (status, body))
        return body

    def metrics(self):
        return self.lab.gateway_metrics()

    def new_request_status(self, key, check=None):
        result = sse_request(self.lab, key, timeout=30, request_id="t10-phase-probe")
        return result

    # -- CTRL-01 -----------------------------------------------------------
    def ctrl01(self, check):
        view = self.wait_view(lambda v: v.get("health") == "active" and v.get("in_sync"),
                              60, "初始 active+in_sync")
        self.ctrl01_view = view
        check.detail("initial_view", view)
        check.require(view["applied_version"] == view["desired_version"] == view["_view"]["desired_version"],
                      "desired/applied 不一致: %r" % view)
        check.require(view["_view"]["status_store"] == "postgres" and view["_view"]["authoritative"],
                      "实例视图不是 PG 权威: %r" % view["_view"])
        # 两把租约用途不同：Control 侧的实例陈旧判定（30s 默认）≠ Gateway 侧
        # 配置租约（CONTROL_CONFIG_LEASE，本实验 %ss）。这里只要求视图如实报告它用
        # 的那把租约。
        check.require(view["_view"]["stale_after_seconds"] > 0,
                      "Control 未报告 stale 租约: %r" % view["_view"])
        check.detail("control_stale_lease_seconds", view["_view"]["stale_after_seconds"])
        check.detail("gateway_config_lease_seconds", self.lab.cfg["lease_seconds"])
        check.note("Control 侧实例陈旧租约=%ss；Gateway 侧配置租约=%ss（两者不同）"
                   % (view["_view"]["stale_after_seconds"], self.lab.cfg["lease_seconds"]))

    # -- CTRL-02 -----------------------------------------------------------
    def ctrl02(self, check):
        ensure_service(self.lab, "control", self.lab.ports["control_http"])
        baseline = self.wait_view(lambda v: v.get("health") == "active", 60, "分区前 active")
        self.base_applied = baseline["applied_version"]   # 分区后会被"冻结版本"覆盖
        key = self.fx.keys["a-member"]["secret"]
        slow_body = {"model": "t10-model", "messages": [{"role": "user", "content": "hi"}], "stream": True,
                     "x_mock_ttft_ms": 300, "x_mock_token_interval_ms": 500, "x_mock_tokens": 16}
        holder = {}

        def run_stream():
            holder["result"] = sse_request(self.lab, key, body=slow_body, timeout=90,
                                           request_id="t10-ctrl02-inflight")

        thread = threading.Thread(target=run_stream)
        thread.start()
        time.sleep(0.8)

        # 1) 切断 Gateway→Control 的本地代理（Control 进程仍活着，/gateways 可读）。
        proxy_pid = int(self.lab.cfg["pids"]["control_proxy"])
        os.kill(proxy_pid, signal.SIGTERM)
        check.require(self.lab.wait_port_closed(self.lab.ports["control_proxy"], 15),
                      "控制面代理已停止但端口仍可连接")
        # 分区前可能还有一次已排队的 apply 正在落地：等它稳定下来，把"切断瞬间实际
        # 服务的版本"作为冻结基线（而不是分区前的乐观读数）。
        time.sleep(2.5)
        frozen = self.wait_view(lambda v: v.get("health") in ("active", "stale"), 15, "分区后视图可读")
        self.base_applied = frozen["applied_version"]
        baseline_desired = frozen["desired_version"]

        # 2) 分区期间推一个新版本：desired 前进，applied 必须留旧值。
        pushed = self.push_policy()
        desired = pushed.get("desired_version") or pushed.get("version")
        check.require(desired and desired > self.base_applied,
                      "分区期间的配置推送没有推进 desired: %r" % pushed)

        # 3) 等租约到期：新请求必须 fail-closed。
        deadline = time.time() + self.lab.cfg["lease_seconds"] + 12
        probe = None
        while time.time() < deadline:
            probe = self.new_request_status(key)
            if probe["status"] != 200:
                break
            time.sleep(0.5)
        check.require(probe is not None and probe["status"] == 401,
                      "租约到期后新请求期望 401（fail-closed），实际 %s" % (probe or {}).get("status"))

        # 4) desired vs applied 不许谎报 active。Control 侧的陈旧判定用 30s 租约，
        #    因此这里等的是控制面自己的窗口（而不是 8s 的 Gateway 配置租约）。
        stale_lease = float(self.ctrl01_view["_view"]["stale_after_seconds"]) if self.ctrl01_view else 30.0
        view = self.wait_view(lambda v: v.get("health") == "stale", stale_lease + 20, "分区后 health=stale")
        check.detail("desired_pushed", desired)
        check.detail("frozen_applied", self.base_applied)
        check.detail("frozen_desired", baseline_desired)
        # desired 只允许前进（Console 的发布 loop 可能同时推进它）；applied 必须停在
        # 分区前的版本，且两者不能再被报成 in_sync。
        check.require(view["desired_version"] >= desired,
                      "分区期间 desired 回退: view=%s pushed=%s frozen_baseline_desired=%s"
                      % (view.get("desired_version"), desired, baseline_desired))
        check.require(view["applied_version"] == self.base_applied,
                      "分区期间 applied 不等于分区前版本: view=%s base_applied=%s desired=%s"
                      % (view.get("applied_version"), self.base_applied, desired))
        check.require(view["in_sync"] is False and view["version_lag"] == desired - self.base_applied,
                      "分区期间 in_sync/version_lag 错误: %r" % view)
        metrics = self.metrics()
        check.require(metrics.get("gateway_control_stale", 0) == 1,
                      "gateway_control_stale=%r（期望 1）" % metrics.get("gateway_control_stale"))
        check.require(metrics.get("gateway_control_lease_expired_total", 0) >= 1,
                      "gateway_control_lease_expired_total=%r" % metrics.get("gateway_control_lease_expired_total"))
        check.require(metrics.get("gateway_control_applied_config_version") == self.base_applied,
                      "applied_config_version=%r（期望 %r）" % (
                          metrics.get("gateway_control_applied_config_version"), self.base_applied))

        # 5) 在途流必须按排水策略跑完。
        thread.join(timeout=90)
        result = holder.get("result") or {}
        check.require(result.get("status") == 200 and result.get("done") is not None,
                      "控制面分区打断了在途流: status=%s done=%s" % (result.get("status"), result.get("done")))
        self.ctrl02_view = view
        self.ctrl02_stream = result
        check.detail("stale_view", view)
        check.detail("inflight_stream_seconds", round(result.get("total") or 0, 3))
        check.note("分区 %ss 后新请求 401；/gateways health=stale、desired=%s、applied=%s；在途流排水完成"
                   % (self.lab.cfg["lease_seconds"], desired, self.base_applied))

    # -- CTRL-03 -----------------------------------------------------------
    def ctrl03(self, check):
        ensure_service(self.lab, "control", self.lab.ports["control_http"])
        # 恢复代理 → 收敛回 active，且新请求再次放行。
        env = dict(os.environ)
        env["GATEWAY_PROFILE"] = "demo"
        proxy_log = open(os.path.join(self.lab.log_dir, "control-proxy.log"), "ab")
        popen = subprocess.Popen(["python3", self.lab.cfg["proxy_script"],
                                  str(self.lab.ports["control_proxy"]), str(self.lab.ports["control_grpc"])],
                                 env=env, stdout=proxy_log, stderr=subprocess.STDOUT)
        with open(os.path.join(self.lab.log_dir, "pids.txt"), "a") as pids:
            pids.write("%d control-proxy\n" % popen.pid)
        check.require(self.lab.wait_tcp(self.lab.ports["control_proxy"], 10), "代理未重新监听")
        started = time.time()
        view = self.wait_view(lambda v: v.get("health") == "active" and v.get("in_sync"),
                              60, "恢复后 active+in_sync")
        recovery_seconds = time.time() - started
        check.require(view["applied_version"] == view["desired_version"],
                      "恢复后 applied(%s) != desired(%s)" % (view["applied_version"], view["desired_version"]))
        deadline = time.time() + 20
        probe = None
        while time.time() < deadline:
            probe = self.new_request_status(self.fx.keys["a-member"]["secret"])
            if probe["status"] == 200 and probe["done"] is not None:
                break
            time.sleep(0.5)
        check.require(probe and probe["status"] == 200 and probe["done"] is not None,
                      "恢复后新请求仍被拒: %r" % (probe or {}).get("status"))
        metrics = self.metrics()
        check.require(metrics.get("gateway_control_stale", 1) == 0,
                      "恢复后 gateway_control_stale=%r（期望 0）" % metrics.get("gateway_control_stale"))
        check.require(metrics.get("gateway_control_applied_config_version") == view["desired_version"],
                      "恢复后 applied 指标未跟上: %r" % metrics.get("gateway_control_applied_config_version"))
        check.detail("recovery_seconds", round(recovery_seconds, 2))
        check.detail("view_after_recovery", view)
        check.note("控制面恢复后 %.2fs 内 desired==applied==%s，stale=0，新请求 200"
                   % (recovery_seconds, view["desired_version"]))

    # -- CTRL-04 -----------------------------------------------------------
    def ctrl04(self, check):
        ensure_service(self.lab, "control", self.lab.ports["control_http"])
        org = self.fx.orgs["A"]
        owner = self.fx.actors["a-owner"]
        revoke_key = self.fx.keys["a-owner"]
        live_key = self.fx.keys["a-member"]
        # 先证明两把 Key 都可用。
        before = sse_request(self.lab, revoke_key["secret"], timeout=30, request_id="t10-ctrl04-before")
        check.require(before["status"] == 200, "撤销前目标 Key 应可用，实际 %s" % before["status"])
        # Measure the real end-to-end budget from before the revoke HTTP request
        # until a fresh request receives 401. Probe with a one-token stream so an
        # accepted response cannot hide the admission decision behind a long drain.
        revoke_started = time.monotonic()
        response = self.matrix.call(check, "revoke-owner-key", "POST",
                                    "/api/v1/orgs/%s/keys/%s/revoke" % (org, revoke_key["key_id"]),
                                    session=owner.session)
        check.require_status(response, 200, label="撤销 Key")
        revoke_acknowledged = time.monotonic()
        revocation_propagation = None
        post_ack_propagation = None
        revoke_probe_body = {"model": "t10-model", "messages": [{"role": "user", "content": "hi"}],
                             "stream": True, "x_mock_ttft_ms": 1,
                             "x_mock_token_interval_ms": 1, "x_mock_tokens": 1}
        deadline = revoke_started + 5.0
        probe_index = 0
        probe = None
        while time.monotonic() < deadline:
            probe_index += 1
            probe = sse_request(self.lab, revoke_key["secret"], body=revoke_probe_body,
                                timeout=max(0.1, min(2.0, deadline - time.monotonic())),
                                request_id="t10-ctrl04-revoked-%d" % probe_index)
            observed = time.monotonic()
            if probe["status"] == 401:
                revocation_propagation = observed - revoke_started
                post_ack_propagation = observed - revoke_acknowledged
                break
            remaining = deadline - observed
            if remaining > 0:
                time.sleep(min(0.2, remaining))
        check.require(revocation_propagation is not None and revocation_propagation <= 5.0,
                      "撤销提交开始后 5s 内新请求未收到 401：elapsed=%r last_status=%r" %
                      (revocation_propagation, (probe or {}).get("status")))
        revoke_status, revoke_view = self.lab.control_json("/gateways")
        check.require(revoke_status == 200, "撤销后 /gateways 不可读")
        # 停 Control：租约到期后所有本地 Key 被撤销，新请求 fail-closed。
        self.control.stop()
        check.require(not self.lab.wait_tcp(self.lab.ports["control_http"], 1), "Control 未停止")
        deadline = time.time() + self.lab.cfg["lease_seconds"] + 12
        probe = None
        while time.time() < deadline:
            probe = sse_request(self.lab, live_key["secret"], timeout=20, request_id="t10-ctrl04-outage")
            if probe["status"] != 200:
                break
            time.sleep(0.5)
        check.require(probe is not None and probe["status"] == 401,
                      "Control 停机 + 租约到期后新请求期望 401，实际 %s" % (probe or {}).get("status"))
        # 重启 Control（PG 权威状态必须恢复），Gateway 重新收敛。
        self.control = Proc(self.lab, "control")
        self.control.start()
        check.require(self.lab.wait_tcp(self.lab.ports["control_http"], 30), "Control 未重新监听")
        view = self.wait_view(lambda v: v.get("health") == "active" and v.get("in_sync"), 90,
                              "Control 重启后 active+in_sync")
        check.require(view["_view"]["status_store"] == "postgres",
                      "Control 重启后实例视图不再是 PG 权威: %r" % view["_view"])
        # 恢复后：被撤销的 Key 绝不能被"复活"。
        deadline = time.time() + 20
        recovered = None
        while time.time() < deadline:
            recovered = sse_request(self.lab, live_key["secret"], timeout=20, request_id="t10-ctrl04-recovered")
            if recovered["status"] == 200:
                break
            time.sleep(0.5)
        check.require(recovered and recovered["status"] == 200,
                      "Control 恢复后合法 Key 仍被拒: %r" % (recovered or {}).get("status"))
        resurrected = sse_request(self.lab, revoke_key["secret"], timeout=20, request_id="t10-ctrl04-resurrect")
        check.require(resurrected["status"] == 401,
                      "Control 重启后已撤销的 Key 被复活了（status=%s）" % resurrected["status"])
        check.detail("revocation_timing_seconds", {
            "request_to_first_refusal": round(revocation_propagation, 3),
            "revoke_http": round(revoke_acknowledged - revoke_started, 3),
            "post_ack_to_first_refusal": round(post_ack_propagation, 3),
        })
        check.detail("view_after_control_restart", view)
        check.note("撤销请求发出后 %.3fs 内首个新请求收到 401（HTTP 请求 %.3fs，响应后传播 %.3fs）；"
                   "Control 停机+租约到期 401；重启后合法 Key 200、已撤销 Key 仍 401"
                   % (revocation_propagation, revoke_acknowledged - revoke_started, post_ack_propagation))


# ---------------------------------------------------------------------------
# F12 — long-stream price / revocation / removal changes
# ---------------------------------------------------------------------------

class LongStreamPhases:
    def __init__(self, lab, fx, client, matrix):
        self.lab = lab
        self.fx = fx
        self.client = client
        self.matrix = matrix

    def wait_key_ready(self, timeout=60):
        """Wait until the fixture member key is accepted again (snapshot re-applied)."""
        key = self.fx.keys["a-member"]["secret"]
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            last = sse_request(self.lab, key, timeout=30, request_id="t10-wait-key")
            if last["status"] == 200:
                return last
            time.sleep(0.5)
        raise CheckFailure("fixture Key 在 %ss 内未恢复（最后 status=%s）" % (timeout, last and last["status"]))

    def price_version_id(self, model):
        rows = self.lab.psql_lines(
            "SELECT id FROM price_versions WHERE model='%s' ORDER BY effective_at DESC, id DESC LIMIT 1" % model)
        return rows[0] if rows else None

    def latest_receipt(self, org):
        rows = self.lab.psql_lines(
            "SELECT execution_id, price_version_id, charge_micro, prompt_tokens, output_tokens, status, "
            "created_at FROM usage_receipts WHERE org_id=%s ORDER BY created_at DESC, execution_id DESC LIMIT 1"
            % org)
        if not rows:
            return None
        parts = rows[0].split("|")
        return {"execution_id": parts[0], "price_version_id": parts[1], "charge_micro": int(parts[2]),
                "prompt_tokens": int(parts[3]), "output_tokens": int(parts[4]), "status": parts[5],
                "created_at": parts[6]}

    def wait_settled(self, execution_id, timeout=60):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            rows = self.lab.psql_lines(
                "SELECT status, charge_micro, price_version_id FROM usage_receipts WHERE execution_id='%s'"
                % execution_id)
            if rows:
                last = rows[0]
                if last.startswith("settled"):
                    return last
            time.sleep(0.3)
        return last

    def rev01(self, check):
        ensure_service(self.lab, "control", self.lab.ports["control_http"])
        self.wait_key_ready()
        org = self.fx.orgs["A"]
        model = "t10-model"
        v1 = self.price_version_id(model)
        check.require(v1, "找不到 %s 的价格版本" % model)
        before_count = int(self.lab.psql("SELECT count(*) FROM usage_receipts WHERE org_id=%s" % org))
        slow_body = {"model": model, "messages": [{"role": "user", "content": "hi"}], "stream": True,
                     "x_mock_ttft_ms": 300, "x_mock_token_interval_ms": 500, "x_mock_tokens": 14}
        holder = {}

        def run_stream():
            holder["result"] = sse_request(self.lab, self.fx.keys["a-member"]["secret"], body=slow_body,
                                           timeout=90, request_id="t10-rev01-inflight")

        thread = threading.Thread(target=run_stream)
        thread.start()
        time.sleep(1.2)
        # 在途改价：10 倍，立即生效。
        effective_at = (datetime.now(timezone.utc) - timedelta(seconds=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        response = self.matrix.call(check, "price-change-midflight", "POST", "/api/v1/admin/prices",
                                    body={"model": model, "currency": "CNY",
                                          "input_price_micro_per_million": "10000000",
                                          "output_price_micro_per_million": "10000000",
                                          "effective_at": effective_at},
                                    session=self.fx.actors["platform-admin"].session)
        check.require_status(response, 201, label="长流期间改价")
        v2 = str((response.json() or {}).get("id", ""))
        check.require(v2 and v2 != v1, "新价格版本没有产生新 id: %r" % v2)
        thread.join(timeout=90)
        result = holder.get("result") or {}
        check.require(result.get("status") == 200 and result.get("done") is not None,
                      "改价打断了在途流: status=%s done=%s" % (result.get("status"), result.get("done")))
        # 找到这条流的 receipt：created_at 最新的一条。
        deadline = time.time() + 30
        receipt = None
        while time.time() < deadline:
            candidate = self.latest_receipt(org)
            if candidate and candidate["execution_id"] not in (None, ""):
                receipt = candidate
                break
            time.sleep(0.3)
        check.require(receipt is not None, "找不到在途流的 receipt")
        self.wait_settled(receipt["execution_id"])
        receipt = self.latest_receipt(org)
        check.require(receipt["price_version_id"] == v1,
                      "在途流用了新价格版本 %s（期望钉住 %s）" % (receipt["price_version_id"], v1))
        tokens = receipt["prompt_tokens"] + receipt["output_tokens"]
        check.require(receipt["charge_micro"] == tokens,
                      "在途流计费 %s micro，期望 %s（1 micro/token）" % (receipt["charge_micro"], tokens))
        check.detail("inflight_receipt", receipt)
        check.detail("price_versions", {"v1": v1, "v2": v2})
        # 新请求必须用新价格（证明改价确实生效，而不是全局没变化）。
        fresh = sse_request(self.lab, self.fx.keys["a-member"]["secret"], timeout=60,
                            request_id="t10-rev01-fresh")
        check.require(fresh["status"] == 200 and fresh["done"] is not None,
                      "改价后新请求失败: %s" % fresh["status"])
        deadline = time.time() + 30
        fresh_receipt = None
        while time.time() < deadline:
            rows = self.lab.psql_lines(
                "SELECT execution_id, price_version_id, charge_micro, prompt_tokens, output_tokens FROM usage_receipts "
                "WHERE org_id=%s ORDER BY created_at DESC, execution_id DESC LIMIT 1" % org)
            if rows:
                parts = rows[0].split("|")
                if parts[1] == v2:
                    fresh_receipt = {"execution_id": parts[0], "price_version_id": parts[1],
                                     "charge_micro": int(parts[2]), "tokens": int(parts[3]) + int(parts[4])}
                    break
            time.sleep(0.3)
        check.require(fresh_receipt is not None, "改价后的新请求没有使用新价格版本 %s" % v2)
        check.require(fresh_receipt["charge_micro"] == fresh_receipt["tokens"] * 10,
                      "新请求计费 %s，期望 %s（10x）" % (fresh_receipt["charge_micro"], fresh_receipt["tokens"] * 10))
        check.detail("fresh_receipt", fresh_receipt)
        after_count = int(self.lab.psql("SELECT count(*) FROM usage_receipts WHERE org_id=%s" % org))
        check.require(after_count == before_count + 2,
                      "receipt 数从 %s 变为 %s（期望 +2：一条在途 + 一条新请求）" % (before_count, after_count))
        check.note("在途流钉住价格 v%s、计费 %s micro；改价后新请求用 v%s、计费 %s micro"
                   % (v1, receipt["charge_micro"], v2, fresh_receipt["charge_micro"]))

    def rev02(self, check):
        ensure_service(self.lab, "control", self.lab.ports["control_http"])
        self.wait_key_ready()
        org = self.fx.orgs["A"]
        owner = self.fx.actors["a-owner"]
        member = self.fx.actors["a-member"]
        # 一把专用 Key：本轮结束时会被撤销。
        response = self.matrix.call(check, "create-revocation-key", "POST", "/api/v1/orgs/%s/keys" % org,
                                    body={"name": "t10-revoke-inflight"}, session=member.session)
        check.require_status(response, 201, label="创建撤销靶子 Key")
        payload = response.json() or {}
        key = {"secret": payload.get("secret", ""), "key_id": payload["key"]["key_id"]}
        RED.add(key["secret"])
        self.fx.keys["rev02-revocation"] = {
            "secret": key["secret"], "key_id": key["key_id"],
            "prefix": payload["key"].get("prefix", ""),
            "owner_user_id": payload["key"].get("owner_user_id"),
            "name": payload["key"].get("name", "t10-revoke-inflight"),
        }
        slow_body = {"model": "t10-model", "messages": [{"role": "user", "content": "hi"}], "stream": True,
                     "x_mock_ttft_ms": 300, "x_mock_token_interval_ms": 500, "x_mock_tokens": 14}
        holder = {}

        def run_stream():
            holder["result"] = sse_request(self.lab, key["secret"], body=slow_body, timeout=90,
                                           request_id="t10-rev02-inflight")

        thread = threading.Thread(target=run_stream)
        thread.start()
        time.sleep(1.2)
        revoke_started = time.monotonic()
        revoke = self.matrix.call(check, "revoke-midflight", "POST",
                                  "/api/v1/orgs/%s/keys/%s/revoke" % (org, key["key_id"]),
                                  session=member.session)
        check.require_status(revoke, 200, label="长流期间撤销 Key")
        revoke_acknowledged = time.monotonic()
        propagation = None
        post_ack_propagation = None
        revoke_probe_body = {"model": "t10-model", "messages": [{"role": "user", "content": "hi"}],
                             "stream": True, "x_mock_ttft_ms": 1,
                             "x_mock_token_interval_ms": 1, "x_mock_tokens": 1}
        deadline = revoke_started + 5.0
        probe_index = 0
        probe = None
        while time.monotonic() < deadline:
            probe_index += 1
            probe = sse_request(self.lab, key["secret"], body=revoke_probe_body,
                                timeout=max(0.1, min(2.0, deadline - time.monotonic())),
                                request_id="t10-rev02-after-%d" % probe_index)
            observed = time.monotonic()
            if probe["status"] == 401:
                propagation = observed - revoke_started
                post_ack_propagation = observed - revoke_acknowledged
                break
            remaining = deadline - observed
            if remaining > 0:
                time.sleep(min(0.2, remaining))
        thread.join(timeout=90)
        check.require(not thread.is_alive(), "撤销后的在途流线程在 90s 内没有结束")
        result = holder.get("result") or {}
        check.require(result.get("status") == 200 and result.get("done") is not None,
                      "撤销打断了在途流: status=%s done=%s" % (result.get("status"), result.get("done")))
        check.require(propagation is not None and propagation <= 5.0,
                      "撤销提交开始后 5s 内新请求未收到 401：elapsed=%r last_status=%r" %
                      (propagation, (probe or {}).get("status")))
        check.detail("revocation_timing_seconds", {
            "request_to_first_refusal": round(propagation, 3),
            "revoke_http": round(revoke_acknowledged - revoke_started, 3),
            "post_ack_to_first_refusal": round(post_ack_propagation, 3),
            "inflight_drain": round(result.get("total") or 0, 3),
        })
        check.note("撤销请求发出后 %.3fs 内首个新请求收到 401（HTTP 请求 %.3fs，响应后传播 %.3fs）；"
                   "在途流排水完成（%.2fs）"
                   % (propagation, revoke_acknowledged - revoke_started, post_ack_propagation,
                      result.get("total") or 0))
        self.revoked_key = key

    def rev03(self, check):
        """长流期间移除成员：在途流排水、新请求 403、账本恰好一条 debit。"""
        org = self.fx.orgs["A"]
        # 专用成员 + 专用 Key：本轮结束时把成员移出组织。
        email = "t10-%s-rev03@t10.test" % self.fx.tag
        password = secrets.token_urlsafe(18)
        RED.add(password)
        admin = self.fx.actors["platform-admin"]
        created = self.client.call(check, "rev03-register-user", "POST", "/api/v1/auth/register",
                                   base_url=self.lab.urls["console"],
                                   body={"email": email, "password": password}, csrf=None)
        check.require_status(created, 201, label="REV-03 用户自助注册")
        registered = created.json() or {}
        registered_cookies = cookies_of(created)
        registration_token = registered_cookies.get("gw_session", "")
        registration_csrf = registered_cookies.get("gw_csrf") or registered.get("csrf_token", "")
        RED.add(registration_token)
        RED.add(registration_csrf)
        self.fx.session_tokens.append(registration_token)
        self.fx.csrf_tokens.append(registration_csrf)
        user_id = str((registered.get("user") or {}).get("id", ""))
        actor = Actor("rev03", email, password, user_id, None)
        self.fx.actors["rev03"] = actor
        invite = self.client.call(check, "rev03-invite", "POST", "/api/v1/orgs/%s/invitations" % org,
                                  base_url=self.lab.urls["console"],
                                  body={"email": email, "role": "member", "monthly_limit_micro": "100000000"},
                                  session=self.fx.actors["a-owner"].session)
        check.require_status(invite, 201, label="REV-03 邀请")
        token = (invite.json() or {}).get("token", "")
        RED.add(token)
        self.fx.invitations["rev03"] = {"token": token, "email": email, "user_id": user_id}
        self.fx.login(check, actor, label="rev03-login")
        accept = self.client.call(check, "rev03-accept", "POST", "/api/v1/invitations/%s/accept" % token,
                                  base_url=self.lab.urls["console"], body={}, session=actor.session)
        check.require_status(accept, 200, label="REV-03 接受邀请")
        key_response = self.client.call(check, "rev03-key", "POST", "/api/v1/orgs/%s/keys" % org,
                                        base_url=self.lab.urls["console"],
                                        body={"name": "t10-rev03-drain"}, session=actor.session)
        check.require_status(key_response, 201, label="REV-03 建 Key")
        key_payload = key_response.json() or {}
        key = key_payload.get("secret", "")
        RED.add(key)
        key_metadata = key_payload.get("key") or {}
        self.fx.keys["rev03"] = {"secret": key, "key_id": key_metadata.get("key_id", ""),
                                  "prefix": key_metadata.get("prefix", ""),
                                  "owner_user_id": key_metadata.get("owner_user_id"),
                                  "name": key_metadata.get("name", "t10-rev03-drain")}
        # 等快照带上这把 Key，避免把传播延迟误判成失败。
        deadline = time.time() + 20
        while time.time() < deadline:
            probe = sse_request(self.lab, key, timeout=30, request_id="t10-rev03-warmup")
            if probe["status"] == 200:
                break
            time.sleep(0.3)
        check.require(probe["status"] == 200, "REV-03 的 Key 未在 20s 内生效: %s" % probe["status"])
        slow_body = {"model": "t10-model", "messages": [{"role": "user", "content": "hi"}], "stream": True,
                     "x_mock_ttft_ms": 300, "x_mock_token_interval_ms": 500, "x_mock_tokens": 14}
        holder = {}

        def run_stream():
            holder["result"] = sse_request(self.lab, key, body=slow_body, timeout=90,
                                           request_id="t10-rev03-inflight")

        thread = threading.Thread(target=run_stream)
        thread.start()
        time.sleep(1.2)
        removed = self.matrix.call(check, "rev03-remove-member", "DELETE",
                                   "/api/v1/orgs/%s/members/%s" % (org, user_id),
                                   session=self.fx.actors["a-owner"].session)
        check.require(removed.status in (200, 204),
                      "REV-03 移除成员期望 200/204，实际 %d" % removed.status)
        thread.join(timeout=90)
        result = holder.get("result") or {}
        check.require(result.get("status") == 200 and result.get("done") is not None,
                      "移除成员打断了在途流: status=%s done=%s" % (result.get("status"), result.get("done")))
        deadline = time.time() + 10
        refused = None
        while time.time() < deadline:
            refused = sse_request(self.lab, key, timeout=20, request_id="t10-rev03-refused")
            if refused["status"] == 403:
                break
            time.sleep(0.3)
        check.require(refused["status"] == 403,
                      "被移出成员的新请求期望 403，实际 %s" % refused["status"])
        # 该成员名下的执行：每条必须恰好一条 receipt、一条 debit，且已结算。
        # 结算由 metering 的 dispatcher/consumer 异步完成，这里等到 settled（≤30s）。
        deadline = time.time() + 30
        rows = []
        while time.time() < deadline:
            rows = self.lab.psql_lines(
                "SELECT r.execution_id, r.status, count(d.id) FROM usage_receipts r "
                "LEFT JOIN billing_entries d ON d.business_id='billing_receipt:debit:'||r.execution_id "
                "WHERE r.org_id=%s AND r.user_id=%s GROUP BY r.execution_id, r.status" % (org, user_id))
            if rows and all(row.split("|")[1] == "settled" for row in rows):
                break
            time.sleep(0.5)
        rows = self.lab.psql_lines(
            "SELECT r.execution_id, r.status, count(d.id) FROM usage_receipts r "
            "LEFT JOIN billing_entries d ON d.business_id='billing_receipt:debit:'||r.execution_id "
            "WHERE r.org_id=%s AND r.user_id=%s GROUP BY r.execution_id, r.status" % (org, user_id))
        check.require(rows, "被移除成员的执行没有 receipt（资金事实缺失）")
        for row in rows:
            parts = row.split("|")
            check.require(parts[1] == "settled" and parts[2] == "1",
                          "被移除成员的执行 %s 状态=%s debit=%s（期望 settled/1）" % tuple(parts))
        check.detail("removed_member_receipts", rows)
        check.detail("inflight_seconds", round(result.get("total") or 0, 3))
        check.note("移除成员后在途流仍收到 [DONE]（%.2fs）；新请求 403；%d 条执行各恰好一条 debit"
                   % (result.get("total") or 0, len(rows)))

    def rev04(self, check):
        """F12 之后的资金一致性：四层对账 + 执行/收据/扣款 1:1。"""
        status, report = self.lab.metering_json("/reconcile?window=24h")
        check.require(status in (200, 409), "对账端点返回 %s" % status)
        check.require(status == 200 and report.get("consistent") and report.get("complete"),
                      "对账不一致: ok=%s consistent=%s complete=%s degraded=%s" % (
                          report.get("ok"), report.get("consistent"), report.get("complete"),
                          report.get("report", {}).get("degraded")))
        detail = {
            "executions": int(self.lab.psql("SELECT count(*) FROM billing_executions")),
            "executions_by_status": self.lab.psql_lines(
                "SELECT status, count(*) FROM billing_executions GROUP BY status ORDER BY status"),
            "settled_executions": int(self.lab.psql(
                "SELECT count(*) FROM billing_executions WHERE status='settled'")),
            "receipts": int(self.lab.psql("SELECT count(*) FROM usage_receipts")),
            "debits": int(self.lab.psql("SELECT count(*) FROM billing_entries WHERE kind='debit'")),
            "reservations": int(self.lab.psql("SELECT count(*) FROM billing_entries WHERE kind='reservation'")),
            "releases": int(self.lab.psql("SELECT count(*) FROM billing_entries WHERE kind='release'")),
            "account_vs_ledger_mismatch": self.lab.psql_lines(
                "SELECT a.org_id FROM billing_accounts a LEFT JOIN ("
                "  SELECT org_id, sum(CASE WHEN direction='credit' THEN amount_micro ELSE -amount_micro END) AS total "
                "  FROM billing_entries GROUP BY org_id) l ON l.org_id=a.org_id "
                "WHERE a.posted_balance_micro <> COALESCE(l.total,0)"),
            "open_receipts": self.lab.psql_lines(
                "SELECT execution_id, status FROM usage_receipts WHERE status <> 'settled'"),
        }
        check.require(detail["settled_executions"] == detail["receipts"],
                      "settled 执行数 %s 与 receipts %s 不相等（确定性请求必须恰好一条 receipt）"
                      % (detail["settled_executions"], detail["receipts"]))
        check.require(not detail["account_vs_ledger_mismatch"],
                      "账户投影与流水不一致: %r" % detail["account_vs_ledger_mismatch"])
        check.require(not detail["open_receipts"],
                      "存在未结算 receipt: %r" % detail["open_receipts"][:5])
        check.detail("counts", detail)
        check.detail("reconcile", report.get("report"))
        check.note("对账一致：executions=receipts=%s，debit=%s，账户投影==流水重算"
                   % (detail["executions"], detail["debits"]))


# ---------------------------------------------------------------------------
# Console isolation under load
# ---------------------------------------------------------------------------

class IsolationPhases:
    def __init__(self, lab, fx, client, matrix):
        self.lab = lab
        self.fx = fx
        self.client = client
        self.matrix = matrix
        self.console = Proc(lab, "console")

    def pg_activity(self):
        rows = self.lab.psql_lines(
            "SELECT coalesce(application_name,''), state, count(*) FROM pg_stat_activity "
            "WHERE datname='gateway_test' GROUP BY 1,2")
        by_app = {}
        by_state = {}
        for row in rows:
            app, state, count = (row.split("|") + ["", "", "0"])[:3]
            count = int(count)
            by_app[app or "<none>"] = by_app.get(app or "<none>", 0) + count
            by_state[state or "<none>"] = by_state.get(state or "<none>", 0) + count
        locks = self.lab.psql_lines(
            "SELECT count(*) FROM pg_stat_activity WHERE datname='gateway_test' "
            "AND wait_event_type IN ('Lock','LWLock')")
        by_app["<lock_waiters>"] = int(locks[0]) if locks else 0
        return {"by_application": by_app, "by_state": by_state}

    def console_worker(self, session, role, deadline, results, stop_event):
        org = self.fx.orgs["A"]
        month = datetime.now(timezone.utc).strftime("%Y-%m")
        # 只发**有权限**的读请求：否则 403/404 会把"报表负载"变成负向矩阵。
        if role in ("owner", "admin"):
            endpoints = [
                "/api/v1/orgs/%s/usage?limit=100" % org,
                "/api/v1/orgs/%s/usage?limit=100&user=%s" % (org, self.fx.actors["a-member"].user_id),
                "/api/v1/orgs/%s/billing/entries?limit=100" % org,
                "/api/v1/orgs/%s/billing/account" % org,
                "/api/v1/orgs/%s/billing/statements?month=%s" % (org, month),
                "/api/v1/orgs/%s/billing/exceptions" % org,
            ]
            endpoints.append("/api/v1/orgs/%s/audit?limit=100" % org)
        else:
            endpoints = [
                "/api/v1/orgs/%s/usage?limit=100" % org,
                "/api/v1/orgs/%s/quota?month=%s" % (org, month),
            ]
        if role == "admin":
            endpoints.append("/api/v1/orgs/%s/keys" % org)
        index = 0
        while time.time() < deadline and not stop_event.is_set():
            path = endpoints[index % len(endpoints)]
            index += 1
            started = time.time()
            status = 0
            try:
                status, _ = self.lab.http_json(self.lab.urls["console"] + path,
                                               headers={"Cookie": "gw_session=%s; gw_csrf=%s" % (
                                                   session["token"], session["csrf"])}, timeout=30)
            except Exception:  # noqa: BLE001
                status = -1
            results.append({"path": path.split("?")[0], "status": status,
                            "elapsed": time.time() - started})

    def iso01(self, check):
        # 先把组织 RPM 提到与隔离性实验无关的高位：否则 96 个请求会先撞上
        # 动态限流（429），把"Console 报表负载下的 Gateway 观察"变成"限流实验"。
        self.lab.control_json("/config", method="POST", payload={
            "org_policies": {self.fx.orgs["A"]: {"requests_per_minute": 60000, "burst": 6000}}})
        time.sleep(2.0)
        check.detail("org_rpm_override", {"requests_per_minute": 60000, "burst": 6000})
        # 预热一轮（不进入结论），避免把冷启动算成基线延迟。
        warmup = report_summary(LoadtestRun(self.lab, "warmup", key=self.fx.keys["a-member"]["secret"],
                                            concurrency=8, total=32, shape="s").run()["report"])
        check.detail("warmup", warmup)
        run = LoadtestRun(self.lab, "baseline", key=self.fx.keys["a-member"]["secret"],
                          concurrency=8, total=96, shape="s")
        result = run.run()
        summary = report_summary(result["report"])
        check.detail("baseline", summary)
        check.require(result["exit_code"] == 0,
                      "基线 loadtest 进程退出码非零：%s" % result["exit_code"])
        check.require(summary["completed"] == summary["scheduled"] and summary["scheduled"] == 96,
                      "基线轮次未全部完成（存在 429/拒绝）: %r" % summary)
        check.require((summary["incomplete"] or 0) == 0, "基线轮次出现 incomplete: %r" % summary)
        for field in ("rejected", "transport_error", "cancelled", "parse_error", "loadgen_dropped"):
            check.require((summary[field] or 0) == 0,
                          "基线 loadtest 有 %s=%r: %r" % (field, summary[field], summary))
        baseline_p95 = (summary["first_content"] or {}).get("p95_ms")
        check.require(baseline_p95 is not None, "基线缺少 first_content p95 样本")
        self.baseline = summary
        check.note("基线观察：%s/%s 完成，first_content p95=%sms，无错误/不完整请求；status=%s" % (
            summary["completed"], summary["scheduled"], baseline_p95, summary["status"]))

    def iso02(self, check):
        # 造压数据：只写 metering_events 与 console_audit_events（两者都不在
        # 对账四层里），避免污染资金事实。
        org = self.fx.orgs["A"]
        user_id = self.fx.actors["a-member"].user_id
        self.lab.psql(
            "INSERT INTO metering_events (event_id, tenant_id, model, provider, prompt_tokens, "
            "completion_tokens, total_tokens, cost_cents, created_at, org_id, user_id, key_id, execution_id) "
            "SELECT 't10-load-'||g, 'org:%s', 't10-model', 'primary', 10, 20, 30, 1, "
            "now() - (g %% 700) * interval '1 hour', %s, %s, '%s', 't10-load-exec-'||g "
            "FROM generate_series(1, 200000) g ON CONFLICT (event_id) DO NOTHING" % (
                org, org, user_id, self.fx.keys["a-member"]["key_id"]))
        # ORG RPM 已在 ISO-01 提到高位（同一个 lab），这里只确保它仍然生效。
        owner = self.fx.actors["a-owner"]
        roles = [("owner", owner.session), ("admin", self.fx.actors["a-admin"].session),
                 ("member", self.fx.actors["a-member3"].session)]
        results = []
        stop_event = threading.Event()
        deadline = time.time() + 45
        worker_count = 40
        threads = [threading.Thread(target=self.console_worker,
                                    args=(roles[i % len(roles)][1], roles[i % len(roles)][0],
                                          deadline, results, stop_event))
                   for i in range(worker_count)]
        for thread in threads:
            thread.start()
        time.sleep(2.0)
        samples = []
        for _ in range(6):
            samples.append(self.pg_activity())
            time.sleep(1.0)
        run = LoadtestRun(self.lab, "under-console-load", key=self.fx.keys["a-member"]["secret"],
                          concurrency=8, total=96, shape="s")
        result = run.run()
        summary = report_summary(result["report"])
        stop_event.set()
        for thread in threads:
            thread.join(timeout=60)
        check.require(all(not thread.is_alive() for thread in threads),
                      "Console 报表 worker 未在 60s 内停止")
        elapsed = [item["elapsed"] for item in results if item["status"] == 200]
        statuses = {}
        for item in results:
            statuses[item["status"]] = statuses.get(item["status"], 0) + 1
        console_stats = {
            "requests": len(results), "statuses": statuses,
            "p50_ms": round((percentile(elapsed, 0.5) or 0) * 1000, 1),
            "p95_ms": round((percentile(elapsed, 0.95) or 0) * 1000, 1),
            "max_ms": round((max(elapsed) if elapsed else 0) * 1000, 1),
        }
        check.detail("console_load", console_stats)
        check.detail("gateway_under_load", summary)
        check.detail("pg_samples", samples)
        check.require(len(results) >= worker_count,
                      "Console 报表负载不足：%d 个 worker 只返回 %d 次" % (worker_count, len(results)))
        check.require(statuses.get(200, 0) >= worker_count and
                      all(item["status"] == 200 for item in results),
                      "Console 报表负载存在错误响应：%r" % statuses)
        check.require(result["exit_code"] == 0,
                      "Console 负载期间 Gateway loadtest 进程退出码非零：%s" % result["exit_code"])
        baseline = self.baseline
        base_p95 = (baseline["first_content"] or {}).get("p95_ms")
        load_p95 = (summary["first_content"] or {}).get("p95_ms")
        check.require(summary["completed"] == summary["scheduled"] == 96,
                      "Console 压测期间 Gateway 流未全部完成: %r" % summary)
        check.require((summary["incomplete"] or 0) == 0,
                      "Console 压测期间出现 incomplete: %r" % summary)
        for field in ("rejected", "transport_error", "cancelled", "parse_error", "loadgen_dropped"):
            check.require((summary[field] or 0) == 0,
                          "Console 压测期间 Gateway 有 %s=%r: %r" % (field, summary[field], summary))
        check.require(base_p95 is not None and load_p95 is not None,
                      "基线或 Console 报表负载轮次缺少 first_content p95 样本")
        check.detail("first_content_p95_ms", {"baseline": base_p95, "under_console_load": load_p95})
        # 这些是负载期间的连接数观察；按进程 application_name 分开检查各自的池上限。
        gateway_conns = [sample["by_application"].get("t10-gateway", 0) for sample in samples]
        console_main_conns = [sample["by_application"].get("t10-console", 0) for sample in samples]
        console_short_conns = [sample["by_application"].get("t10-console-short", 0) for sample in samples]
        console_conns = [main + short for main, short in zip(console_main_conns, console_short_conns)]
        check.detail("pg_connections", {"gateway": gateway_conns, "console": console_conns,
                                        "console_main": console_main_conns, "console_short": console_short_conns})
        check.require(max(console_main_conns) > 0,
                      "PG 采样没有观察到 t10-console application_name")
        check.require(max(console_short_conns) <= 10,
                      "Console short 实例连接数超过自身池上限 10: %r" % console_short_conns)
        check.require(max(console_main_conns) <= 10,
                      "Console 主实例连接数超过自身池上限 10: %r" % console_main_conns)
        check.require(max(gateway_conns) >= 1,
                      "压测期间 Gateway 没有拿到任何 PG 连接: %r" % gateway_conns)
        contention = {
            "console_p95_ms": console_stats["p95_ms"],
            "gateway_first_content_p95_ms_baseline": base_p95,
            "gateway_first_content_p95_ms_loaded": load_p95,
            "lock_waiters_max": max(sample["by_application"].get("<lock_waiters>", 0) for sample in samples),
        }
        self.isolation = {"console": console_stats, "gateway": summary, "pg_samples": samples,
                          "contention": contention}
        check.detail("contention", contention)
        if load_p95 is not None and base_p95 is not None and load_p95 > max(3 * base_p95, base_p95 + 20):
            self.matrix.finding("共享 PG 争用可见：Console 压测下 Gateway first_content p95 从 %sms 升到 %sms "
                                "（Console 自身 p95=%sms）——进程/连接池是隔离的，但 PG 是共享的"
                                % (base_p95, load_p95, console_stats["p95_ms"]))
        check.note("Console 报表负载观察：%d 次请求（status=%s，p95=%sms）；Gateway 96/96 无错误，"
                   "first_content p95 基线/负载=%s/%sms（未设性能 SLO）；PG 采样连接 console=%s gateway=%s"
                   % (console_stats["requests"], statuses, console_stats["p95_ms"], base_p95, load_p95,
                      max(console_conns), max(gateway_conns)))

    def iso03(self, check):
        """Console 重启不能打断在途 Gateway 流。"""
        streams = []
        holder = []
        lock = threading.Lock()

        def run_stream(index):
            result = sse_request(self.lab, self.fx.keys["a-member"]["secret"], timeout=90,
                                 request_id="t10-iso03-%d" % index,
                                 body={"model": "t10-model",
                                       "messages": [{"role": "user", "content": "hi"}], "stream": True,
                                       "x_mock_ttft_ms": 400, "x_mock_token_interval_ms": 600,
                                       "x_mock_tokens": 14})
            with lock:
                holder.append(result)

        for index in range(6):
            thread = threading.Thread(target=run_stream, args=(index,))
            thread.start()
            streams.append(thread)
        time.sleep(1.5)
        restart_started = time.time()
        self.console.stop()
        self.console.start()
        ready = False
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                status, _ = self.lab.http_json(self.lab.urls["console"] + "/readyz", timeout=3)
                if status == 200:
                    ready = True
                    break
            except Exception:  # noqa: BLE001
                pass
            time.sleep(0.2)
        restart_seconds = time.time() - restart_started
        check.require(ready, "Console 重启后 30s 内未 ready")
        for thread in streams:
            thread.join(timeout=90)
        check.require(len(holder) == 6, "流数量不对: %d" % len(holder))
        for index, result in enumerate(holder):
            check.require(result["status"] == 200 and result["done"] is not None,
                          "Console 重启打断了第 %d 条流: status=%s done=%s" % (
                              index, result["status"], result["done"]))
        durations = [round(result["total"], 2) for result in holder]
        check.detail("restart_seconds", round(restart_seconds, 2))
        check.detail("stream_durations", durations)
        check.require(all(result["status"] == 200 for result in holder), "存在非 200 的流")
        check.note("Console 重启（%.2fs）期间 6 条在途流全部收到 [DONE]（耗时 %s）"
                   % (restart_seconds, durations))

    def iso04(self, check):
        """报表负载期间按 application_name 观察 Console 与 Gateway 各自连接池。"""
        samples = self.isolation["pg_samples"]
        check.require(bool(samples), "ISO-02 没有 PostgreSQL 连接池采样")
        gateway_max = max(sample["by_application"].get("t10-gateway", 0) for sample in samples)
        console_main_max = max(sample["by_application"].get("t10-console", 0) for sample in samples)
        console_short_max = max(sample["by_application"].get("t10-console-short", 0) for sample in samples)
        console_max = max(sample["by_application"].get("t10-console", 0) +
                          sample["by_application"].get("t10-console-short", 0) for sample in samples)
        total_max = max(sum(value for key, value in sample["by_application"].items()
                            if not key.startswith("<")) for sample in samples)
        check.detail("gateway_conns_max", gateway_max)
        check.detail("console_main_conns_max", console_main_max)
        check.detail("console_short_conns_max", console_short_max)
        check.detail("console_conns_max", console_max)
        check.detail("all_backends_max", total_max)
        check.require(console_main_max > 0,
                      "采样中没有观察到 t10-console application_name")
        check.require(gateway_max > 0,
                      "采样中没有观察到 t10-gateway application_name")
        check.require(console_main_max <= 10,
                      "Console 主实例连接数 %d 超过自身池上限 10" % console_main_max)
        check.require(console_short_max <= 10,
                      "Console short 实例连接数 %d 超过自身池上限 10" % console_short_max)
        check.require(gateway_max <= 16,
                      "Gateway 连接数 %d 超过自身两个池上限（8+8）" % gateway_max)
        check.note("报表负载期间的连接池隔离观察：t10-console=%d/10、t10-console-short=%d/10、"
                   "t10-gateway=%d/16、全部后端=%d；这些是观测上限检查，不代表池已饱和"
                   % (console_main_max, console_short_max, gateway_max, total_max))


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def write_summary(lab, checks, findings, extra):
    payload = {
        "run_id": lab.cfg["run_id"],
        "generated_at": utcnow(),
        "checks": [
            {"id": check.cid, "title": check.title, "status": check.status,
             "started_at": check.started_at, "finished_at": check.finished_at,
             "notes": check.notes, "details": check.details, "failure": check.failure,
             "skip_reason": check.skip_reason,
             "exchanges": check.exchanges}
            for check in checks
        ],
        "findings": findings,
        "extra": extra,
    }
    path = os.path.join(lab.evidence_dir, "summary.json")
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=False)
    return path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--lab", required=True)
    args = parser.parse_args()
    cfg = json.load(open(args.lab, encoding="utf-8"))
    t10.set_logfile(os.path.join(cfg["log_dir"], "matrix.log"))
    phases = cfg.get("phases", "all")
    wanted = {"security", "control", "longstream", "isolation"} if phases == "all" else set(phases.split(","))

    lab = Lab(cfg)
    client = Client(cfg["evidence_dir"], cfg["timeout_seconds"], scratch_dir=cfg["secret_dir"])
    fx = Fixtures(lab, client)  # cid 默认 FIX（与恢复演练的 BFIX 区分）
    matrix = Matrix(lab, fx, client)
    extra = {}

    log("== fixtures ==")
    try:
        fixture_check = fx.create()
        fixture_check.status = "PASS"
        fixture_check.finished_at = utcnow()
    except CheckFailure as err:
        fixture_check = Check("FIX", "fixtures")
        fixture_check.status = "FAIL"
        fixture_check.failure = redact(str(err))
        fixture_check.finished_at = utcnow()
        matrix.checks.append(fixture_check)
        log("[FIX] FAIL — %s" % fixture_check.failure)
        write_summary(lab, matrix.checks, matrix.findings, extra)
        return 1
    matrix.checks.append(fixture_check)
    log("[FIX] PASS — org A=%s org B=%s" % (fx.orgs["A"], fx.orgs["B"]))

    failure_phases = FailurePhases(lab, fx, client, matrix)
    long_phases = LongStreamPhases(lab, fx, client, matrix)
    isolation_phases = IsolationPhases(lab, fx, client, matrix)

    if "security" in wanted:
        log("\n== F16 安全负向矩阵 ==")
        matrix.run("SEC-01", "无 token / 缺失凭据（Console+Gateway）", matrix.sec01)
        matrix.run("SEC-02", "错误 scheme / 畸形凭据", matrix.sec02)
        matrix.run("SEC-03", "篡改 session 与 CSRF", matrix.sec03)
        matrix.run("SEC-04", "过期 session（短 TTL 实例）", matrix.sec04)
        matrix.run("SEC-05", "邀请 token 篡改/未知/错 email/重复接受", matrix.sec05)
        matrix.run("SEC-06", "跨组织读取枚举（orgs/users/keys/usage/audit/statements/exceptions）", matrix.sec06)
        matrix.run("SEC-07", "跨组织写入被拒且目标组织不变", matrix.sec07)
        matrix.run("SEC-08", "角色提权（member→admin、admin→owner、自升平台管理员）", matrix.sec08)
        matrix.run("SEC-09", "列出的 unsafe 路由缺/错 CSRF + 无状态变更", matrix.sec09)
        matrix.run("SEC-10", "禁用用户 + 活跃 session（旧 session 立即失效）", matrix.sec10)
        matrix.run("SEC-11", "被移出成员 + 有效 Key（在途流排水、新请求 403）", matrix.sec11)
        matrix.run("SEC-12", "普通 member 自有额度/用量及 Key 边界", matrix.sec12)
        matrix.run("SEC-13", "/api/v1/admin/* 非平台管理员全拒", matrix.sec13)
        matrix.run("SEC-14", "Gateway 凭据归属不可被请求头改写", matrix.sec14)

    if "control" in wanted:
        log("\n== F10 控制面失效与恢复 ==")
        matrix.run("CTRL-01", "desired==applied 基线（PG 权威实例视图）", failure_phases.ctrl01)
        matrix.run("CTRL-02", "控制面分区：stale 不谎报 active、租约到期拒新请求、在途流排水",
                   failure_phases.ctrl02)
        matrix.run("CTRL-03", "控制面恢复：desired/applied 收敛、新请求恢复", failure_phases.ctrl03)
        matrix.run("CTRL-04", "Control 重启：撤销不复活、实例视图durable", failure_phases.ctrl04)

    if "longstream" in wanted:
        log("\n== F12 长流期间的改价/撤销/移除 ==")
        matrix.run("REV-01", "长流期间改价：在途钉住原价、新请求用新价", long_phases.rev01)
        matrix.run("REV-02", "长流期间撤销 Key：在途排水、新请求 ≤5s 被拒", long_phases.rev02)
        matrix.run("REV-03", "移除成员后的资金正确性（每条执行恰好一条 debit）", long_phases.rev03)
        matrix.run("REV-04", "F12 后对账一致：四层 + 执行/收据/扣款 1:1", long_phases.rev04)

    if "isolation" in wanted:
        log("\n== 管理面/数据面隔离 ==")
        matrix.run("ISO-01", "Gateway SSE 基线（loadtest closed-loop）", isolation_phases.iso01)
        matrix.run("ISO-02", "Console 报表负载下 Gateway SSE 完成/错误/p95 实测", isolation_phases.iso02)
        matrix.run("ISO-03", "Console 重启不打断在途 Gateway 流", isolation_phases.iso03)
        matrix.run("ISO-04", "报表负载下连接池隔离观察（按 application_name 和池上限）",
                   isolation_phases.iso04)

    if "security" in wanted:
        log("\n== 全局泄密扫描 ==")
        matrix.run("SEC-15", "响应与进程日志的 secret 扫描", matrix.sec15)

    # 每个检查的原始交换记录已经写盘；这里补一份逐请求索引。
    extra["exchanges"] = client.records and [
        {"cid": record["cid"], "seq": record["seq"], "label": record["label"],
         "method": record["method"], "path": record["path"], "status": record["status"]}
        for record in client.records] or []
    path = write_summary(lab, matrix.checks, matrix.findings, extra)
    log("")
    log("== 结果 ==")
    failed = 0
    for check in matrix.checks:
        marker = check.status
        log("%-8s %-4s %s%s" % (check.cid, marker, check.title,
                                (" — " + check.failure) if check.failure else ""))
        if check.status == "FAIL":
            failed += 1
    log("")
    log("checks=%d pass=%d fail=%d skip=%d findings=%d summary=%s" % (
        len(matrix.checks),
        sum(1 for check in matrix.checks if check.status == "PASS"),
        failed,
        sum(1 for check in matrix.checks if check.status == "SKIP"),
        len(matrix.findings), path))
    return 1 if failed else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except InfraFailure as err:
        log("INFRA FAILURE: %s" % redact(str(err)))
        sys.exit(2)

DRIVER_EOF

t10_log "运行断言驱动（phases=${phases}）"
set +e
python3 "$driver_path" --lab "$private_lab_config" 2>&1 | tee "$T10_LOG_DIR/driver.log"
driver_rc=${PIPESTATUS[0]}
set -e
rm -rf -- "$driver_dir"
driver_dir=""

# 进程日志收尾：把 driver 之外的静态日志也放进 logs/（spawn 时已经直接写进去了）。
if [[ "$keep_lab" == "1" ]]; then
  t10_log "--keep：保留实验台（容器/进程/端口见 lab.json），不执行 teardown"
  trap - EXIT
else
  t10_log "回收实验台"
fi
exit "$driver_rc"
