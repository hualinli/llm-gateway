#!/usr/bin/env bash
#
# backup-restore-drill.sh — T10 F15 备份/恢复演练 + Redis 清空后的 PG 驱动补投（黑盒 + 真实进程）。
#
# 本脚本自己拉起**两套完全隔离**的环境：
#
#   A（源，live）：PostgreSQL + Redis + control + console + gateway + metering + upstream-mock
#                  — 有组织/成员/Key、价格版本、账本/账户投影、usage_receipts（含未结算）、
#                    以及一把用主密钥加密的 provider credential（ciphertext 落 PG）。
#   B（恢复目标）：全新的 PostgreSQL + Redis 容器与端口；A 的 `pg_dump` 恢复到 B，
#                  再用**同一个主密钥文件**启动 B 的 control/console/gateway/metering，
#                  验证身份/价格/账本/收据/密文/对账全部可用。
#
# 然后做两件事：
#   1. 主密钥依赖：用**错误**主密钥和**完全没有**主密钥分别启动 B 的 control，记录实际行为
#      （进程拒绝启动还是降级为不可用），并给出确切的日志/退出码。
#   2. Redis 清空演练：在 PG 里存在"已投递但未结算（dispatched）"收据时 FLUSHALL
#      （传输丢失），再停 Redis 证明新请求 fail-closed，随后恢复 Redis，证明
#      PG 驱动的重投（METERING_SETTLEMENT_REDISPATCH）把结算补齐且**不重复扣款**。
#
# 退出码：0 = 全部硬检查通过；1 = 存在 FAIL；2 = 用法/环境错误。
#
# 用法见 --help。
set -uo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
script_name="$(basename "$0")"
# shellcheck source=scripts/t10-lab-common.sh
source "$script_dir/t10-lab-common.sh"

evidence_dir="$T10_ROOT/data/verification/t10-security"
timeout_seconds=20
keep_lab=0
live_requests=12
flush_requests=8

usage() {
  cat <<'USAGE'
用法:
  scripts/backup-restore-drill.sh [选项]

选项:
  --evidence-dir DIR    证据目录（默认 data/verification/t10-security）
  --timeout-seconds N   单个 curl 超时，默认 20
  --live-requests N     备份前用真实流量写入的计费请求数，默认 12
  --flush-requests N    Redis 清空演练里"已投递未结算"的收据数，默认 8
  --keep                结束后保留两套实验台，便于人工复现
  -h, --help            显示本帮助

检查项:
  BK-01  备份前事实快照 + live pg_dump（备份窗口内继续写，测量 RPO）
  BK-02  恢复到新环境：身份/组织/成员/Key、价格版本、账本与账户投影、receipts/outbox、
         control 配置版本、provider credential 密文（逐字节）全部存活
  BK-03  主密钥依赖：错误主密钥 / 无主密钥时 control 的真实行为，正确主密钥下解密可用
  BK-04  恢复后对账一致，且 A/B 两套资金事实逐项比对
  BK-05  RPO / RTO 实测数字（并声明单节点 PG 不是 HA）
  RF-01  Redis 清空（传输丢失）+ Redis 停机 fail-closed + PG 驱动重投恢复结算
  RF-02  恢复后恰好一次扣款：receipt/execution/debit 1:1、账户投影==流水、对账一致
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --evidence-dir) evidence_dir="${2:-}"; shift 2 ;;
    --evidence-dir=*) evidence_dir="${1#*=}"; shift ;;
    --timeout-seconds) timeout_seconds="${2:-}"; shift 2 ;;
    --timeout-seconds=*) timeout_seconds="${1#*=}"; shift ;;
    --live-requests) live_requests="${2:-}"; shift 2 ;;
    --live-requests=*) live_requests="${1#*=}"; shift ;;
    --flush-requests) flush_requests="${2:-}"; shift 2 ;;
    --flush-requests=*) flush_requests="${1#*=}"; shift ;;
    --keep) keep_lab=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf '%s: 未知参数 %s\n\n' "$script_name" "$1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$timeout_seconds" in ''|*[!0-9]*) t10_die "--timeout-seconds 必须是非负整数";; esac
case "$live_requests" in ''|*[!0-9]*) t10_die "--live-requests 必须是非负整数";; esac
case "$flush_requests" in ''|*[!0-9]*) t10_die "--flush-requests 必须是非负整数";; esac

t10_require docker curl python3 go git shasum
t10_install_traps

prefix="t10bk-$(printf '%s' "$T10_RUN_ID" | tr '[:upper:]' '[:lower:]')"
prefix="${prefix:0:40}"
ports=()
for _ in pg_a redis_a control_grpc_a control_http_a gateway_a diag_a metering_a console_a upstream_a \
         pg_b redis_b control_grpc_b control_http_b gateway_b diag_b metering_b console_b upstream_b; do
  ports+=("$(t10_free_port)")
done
pg_a="${ports[0]}"; redis_a="${ports[1]}"; control_grpc_a="${ports[2]}"; control_http_a="${ports[3]}"
gateway_a="${ports[4]}"; diag_a="${ports[5]}"; metering_a="${ports[6]}"; console_a="${ports[7]}"
upstream_a="${ports[8]}"
pg_b="${ports[9]}"; redis_b="${ports[10]}"; control_grpc_b="${ports[11]}"; control_http_b="${ports[12]}"
gateway_b="${ports[13]}"; diag_b="${ports[14]}"; metering_b="${ports[15]}"; console_b="${ports[16]}"
upstream_b="${ports[17]}"

t10_prepare_lab "$evidence_dir"
t10_log "run=$T10_RUN_ID evidence=$T10_EVIDENCE_DIR"
t10_log "A: pg=$pg_a redis=$redis_a control=$control_grpc_a/$control_http_a gateway=$gateway_a console=$console_a"
t10_log "B: pg=$pg_b redis=$redis_b control=$control_grpc_b/$control_http_b gateway=$gateway_b console=$console_b"
t10_write_env "$T10_EVIDENCE_DIR/restore-run.env" \
  "script=$script_name" \
  "script_sha256=$(shasum -a 256 "$script_dir/$script_name" | awk '{print $1}')" \
  "common_sha256=$(shasum -a 256 "$script_dir/t10-lab-common.sh" | awk '{print $1}')" \
  "live_requests=$live_requests" "flush_requests=$flush_requests" \
  "pg_a_port=$pg_a" "redis_a_port=$redis_a" "pg_b_port=$pg_b" "redis_b_port=$redis_b"

pg_a_container="${prefix}-pga"; redis_a_container="${prefix}-redisa"
pg_b_container="${prefix}-pgb"; redis_b_container="${prefix}-redisb"
admin_email="admin@t10bk.test"
admin_password_file="$T10_SECRETS_DIR/admin-password"
admin_password="$(t10_random_secret)"
printf '%s' "$admin_password" > "$admin_password_file"
chmod 600 "$admin_password_file"

# 主密钥：base64 的 32 字节；正确的一份 + 故意错误的一份（都只在 0600 文件里）。
master_key_file="$T10_SECRETS_DIR/provider-credential-master-key"
wrong_key_file="$T10_SECRETS_DIR/provider-credential-master-key-wrong"
python3 -c 'import base64,os,sys; sys.stdout.write(base64.b64encode(os.urandom(32)).decode())' > "$master_key_file"
python3 -c 'import base64,os,sys; sys.stdout.write(base64.b64encode(os.urandom(32)).decode())' > "$wrong_key_file"
chmod 600 "$master_key_file" "$wrong_key_file"
provider_secret="sk-t10bk-provider-$(t10_random_secret)"
control_admin_token="$(t10_random_secret)"
metering_admin_token="$(t10_random_secret)"

dsn_a="postgres://gateway_test:test_only_local@127.0.0.1:${pg_a}/gateway_test?sslmode=disable"
dsn_b="postgres://gateway_test:test_only_local@127.0.0.1:${pg_b}/gateway_test?sslmode=disable"

t10_log "构建二进制"
t10_build control gateway console metering upstream-mock migrate || exit 2

t10_log "启动两套隔离 PostgreSQL + Redis"
t10_start_pg "$pg_a_container" "$pg_a" || exit 2
t10_start_redis "$redis_a_container" "$redis_a" || exit 2
t10_start_pg "$pg_b_container" "$pg_b" || exit 2
t10_start_redis "$redis_b_container" "$redis_b" || exit 2

t10_log "A：migrate up + console bootstrap"
(cd "$T10_ROOT" && POSTGRES_DSN="$dsn_a" GATEWAY_PROFILE=demo "$(t10_bin migrate)" up) \
  > "$T10_LOG_DIR/migrate-a.log" 2>&1 || t10_die "A 的 migrate up 失败"
(cd "$T10_ROOT" && POSTGRES_DSN="$dsn_a" GATEWAY_PROFILE=demo "$(t10_bin console)" bootstrap \
  --admin-email "$admin_email" --admin-password-file "$admin_password_file") \
  > "$T10_LOG_DIR/bootstrap-a.log" 2>&1 || t10_die "A 的 console bootstrap 失败"

# A 的 control：挂主密钥（provider credential 加密/解密），并允许本地明文 resolve。
control_env_a=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_a}&application_name=t10bk-control-a"
  "REDIS_ADDR=127.0.0.1:${redis_a}"
  "CONTROL_GRPC_ADDR=127.0.0.1:${control_grpc_a}"
  "CONTROL_HTTP_ADDR=127.0.0.1:${control_http_a}"
  "CONTROL_REQUIRE_POSTGRES=true"
  "CONTROL_ADMIN_TOKEN=$control_admin_token"
  "CONTROL_ADMIN_SERVICE_ENABLED=true"
  "CONTROL_CONSOLE_SERVICE_NAME=console"
  "CONTROL_ALLOW_INSECURE_ADMIN_SERVICE=true"
  "CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE=true"
  "PROVIDER_CREDENTIAL_MASTER_KEY_FILE=$master_key_file"
)
t10_spawn control-a "$T10_LOG_DIR/control-a.log" "${control_env_a[@]}" -- "$(t10_bin control)"
control_a_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${control_http_a}/healthz" 30 200

t10_log "A：创建 provider credential（明文只在进程内存/临时文件里）"
credential_ref="$(curl -s -X POST "http://127.0.0.1:${control_http_a}/provider-credentials" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer ${control_admin_token}" \
  -d "$(printf '{"provider_id":"primary","secret":"%s"}' "$provider_secret")" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("credential_ref",""))')"
[[ -n "$credential_ref" ]] || t10_die "provider credential 创建失败"
t10_log "credential_ref=$credential_ref"

t10_log "A：upstream-mock（要求 Bearer，只有解密出凭据的 Gateway 才能成功）"
t10_spawn upstream-a "$T10_LOG_DIR/upstream-a.log" "GATEWAY_PROFILE=demo" -- \
  "$(t10_bin upstream-mock)" -listen "127.0.0.1:${upstream_a}" -name primary \
  -ttft 5ms -token-interval 5ms -tokens 8 -authorization "$provider_secret"
upstream_a_pid="$T10_LAST_PID"
t10_wait_tcp "$upstream_a" 30 "upstream A"

t10_log "A：下发 provider（带 credential_ref）+ route"
curl -s -X POST "http://127.0.0.1:${control_http_a}/config" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer ${control_admin_token}" \
  -d "$(printf '{"providers":{"primary":{"id":"primary","protocol":"openai-compatible","url":"http://127.0.0.1:%s","model":"t10bk-model","credential_ref":"%s","timeouts":{"attempt_timeout_ms":90000}}},"routes":{"t10bk-model":["primary"]}}' "$upstream_a" "$credential_ref")" \
  > "$T10_LOG_DIR/config-a.json" || t10_die "A 的 /config 失败"

gateway_env_a=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_a}&application_name=t10bk-gateway-a"
  "REDIS_ADDR=127.0.0.1:${redis_a}"
  "GATEWAY_ADDR=127.0.0.1:${gateway_a}"
  "GATEWAY_DIAGNOSTICS_ADDR=127.0.0.1:${diag_a}"
  "GATEWAY_INSTANCE_ID=gw-a"
  "CONTROL_ADDR=127.0.0.1:${control_grpc_a}"
  "GATEWAY_ACCESS_GUARD=pg"
  "GATEWAY_BILLING=pg"
  "LIMITER_ON_REDIS_ERROR=closed"
  "GATEWAY_LIMITER_RATE=1000"
  "GATEWAY_LIMITER_BURST=2000"
  "CONTROL_CONFIG_POLL_INTERVAL=1s"
  "CONTROL_CONFIG_LEASE=30s"
  "CONTROL_STATUS_REPORT_INTERVAL=2s"
  "METERING_SPOOL_DIR=$T10_SECRETS_DIR/spool-a-gw"
)
t10_spawn gateway-a "$T10_LOG_DIR/gateway-a.log" "${gateway_env_a[@]}" -- "$(t10_bin gateway)"
gateway_a_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${gateway_a}/healthz" 60 200

metering_env_a=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_a}&application_name=t10bk-metering-a"
  "REDIS_ADDR=127.0.0.1:${redis_a}"
  "METERING_ADDR=127.0.0.1:${metering_a}"
  "METERING_REQUIRE_POSTGRES=true"
  "METERING_SPOOL_DIR=$T10_SECRETS_DIR/spool-a-metering"
  "METERING_ADMIN_TOKEN=$metering_admin_token"
  "METERING_SETTLEMENT_REDISPATCH=3s"
)
t10_spawn metering-a "$T10_LOG_DIR/metering-a.log" "${metering_env_a[@]}" -- "$(t10_bin metering)"
metering_a_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${metering_a}/healthz" 60 200

console_env_a=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_a}&application_name=t10bk-console-a"
  "CONSOLE_ADDR=127.0.0.1:${console_a}"
  "CONTROL_ADDR=127.0.0.1:${control_grpc_a}"
  "CONTROL_CONSOLE_SERVICE_NAME=console"
  "CONSOLE_PUBLICATION_INTERVAL=2s"
  "CONSOLE_LOGIN_RATE=60"
  "CONSOLE_LOGIN_BURST=30"
)
t10_spawn console-a "$T10_LOG_DIR/console-a.log" "${console_env_a[@]}" -- "$(t10_bin console)" serve
console_a_pid="$T10_LAST_PID"
t10_wait_http "http://127.0.0.1:${console_a}/readyz" 60 200

# B 的服务由 python 驱动按阶段启动（恢复、错误主密钥、无主密钥三种形态）。
control_env_b=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_b}&application_name=t10bk-control-b"
  "REDIS_ADDR=127.0.0.1:${redis_b}"
  "CONTROL_GRPC_ADDR=127.0.0.1:${control_grpc_b}"
  "CONTROL_HTTP_ADDR=127.0.0.1:${control_http_b}"
  "CONTROL_REQUIRE_POSTGRES=true"
  "CONTROL_ADMIN_TOKEN=$control_admin_token"
  "CONTROL_ADMIN_SERVICE_ENABLED=true"
  "CONTROL_CONSOLE_SERVICE_NAME=console"
  "CONTROL_ALLOW_INSECURE_ADMIN_SERVICE=true"
  "CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE=true"
  "PROVIDER_CREDENTIAL_MASTER_KEY_FILE=$master_key_file"
)
gateway_env_b=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_b}&application_name=t10bk-gateway-b"
  "REDIS_ADDR=127.0.0.1:${redis_b}"
  "GATEWAY_ADDR=127.0.0.1:${gateway_b}"
  "GATEWAY_DIAGNOSTICS_ADDR=127.0.0.1:${diag_b}"
  "GATEWAY_INSTANCE_ID=gw-b"
  "CONTROL_ADDR=127.0.0.1:${control_grpc_b}"
  "GATEWAY_ACCESS_GUARD=pg"
  "GATEWAY_BILLING=pg"
  "LIMITER_ON_REDIS_ERROR=closed"
  "GATEWAY_LIMITER_RATE=1000"
  "GATEWAY_LIMITER_BURST=2000"
  "CONTROL_CONFIG_POLL_INTERVAL=1s"
  "CONTROL_CONFIG_LEASE=30s"
  "CONTROL_STATUS_REPORT_INTERVAL=2s"
  "METERING_SPOOL_DIR=$T10_SECRETS_DIR/spool-b-gw"
)
metering_env_b=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_b}&application_name=t10bk-metering-b"
  "REDIS_ADDR=127.0.0.1:${redis_b}"
  "METERING_ADDR=127.0.0.1:${metering_b}"
  "METERING_REQUIRE_POSTGRES=true"
  "METERING_SPOOL_DIR=$T10_SECRETS_DIR/spool-b-metering"
  "METERING_ADMIN_TOKEN=$metering_admin_token"
  "METERING_SETTLEMENT_REDISPATCH=3s"
)
console_env_b=(
  "GATEWAY_PROFILE=demo"
  "POSTGRES_DSN=${dsn_b}&application_name=t10bk-console-b"
  "CONSOLE_ADDR=127.0.0.1:${console_b}"
  "CONTROL_ADDR=127.0.0.1:${control_grpc_b}"
  "CONTROL_CONSOLE_SERVICE_NAME=console"
  "CONSOLE_PUBLICATION_INTERVAL=2s"
  "CONSOLE_LOGIN_RATE=60"
  "CONSOLE_LOGIN_BURST=30"
)

json_env() { python3 -c 'import json,sys;print(json.dumps(dict(a.split("=",1) for a in sys.argv[1:])))' "$@"; }

cat > "$T10_EVIDENCE_DIR/restore-lab.json" <<JSON
{
  "run_id": "$T10_RUN_ID",
  "root": "$T10_ROOT",
  "evidence_dir": "$T10_EVIDENCE_DIR",
  "secret_dir": "$T10_SECRETS_DIR",
  "bin_dir": "$T10_BUILD_DIR",
  "log_dir": "$T10_LOG_DIR",
  "timeout_seconds": $timeout_seconds,
  "live_requests": $live_requests,
  "flush_requests": $flush_requests,
  "admin_email": "$admin_email",
  "admin_password_file": "$admin_password_file",
  "master_key_file": "$master_key_file",
  "wrong_key_file": "$wrong_key_file",
  "provider_secret": "$provider_secret",
  "credential_ref": "$credential_ref",
  "tokens": {"control_admin": "$control_admin_token", "metering_admin": "$metering_admin_token"},
  "a": {
    "dsn": "$dsn_a",
    "ports": {"pg": $pg_a, "redis": $redis_a, "control_grpc": $control_grpc_a, "control_http": $control_http_a,
              "gateway": $gateway_a, "gateway_diag": $diag_a, "metering": $metering_a,
              "console": $console_a, "upstream": $upstream_a},
    "urls": {"console": "http://127.0.0.1:$console_a", "gateway": "http://127.0.0.1:$gateway_a",
             "gateway_diag": "http://127.0.0.1:$diag_a", "control": "http://127.0.0.1:$control_http_a",
             "metering": "http://127.0.0.1:$metering_a", "upstream": "http://127.0.0.1:$upstream_a"},
    "containers": {"pg": "$pg_a_container", "redis": "$redis_a_container"},
    "pids": {"control": "$control_a_pid", "gateway": "$gateway_a_pid", "metering": "$metering_a_pid",
             "console": "$console_a_pid", "upstream": "$upstream_a_pid"},
    "commands": {
      "control": {"argv": ["$(t10_bin control)"], "env": $(json_env "${control_env_a[@]}"), "log": "$T10_LOG_DIR/control-a.log"},
      "gateway": {"argv": ["$(t10_bin gateway)"], "env": $(json_env "${gateway_env_a[@]}"), "log": "$T10_LOG_DIR/gateway-a.log"},
      "metering": {"argv": ["$(t10_bin metering)"], "env": $(json_env "${metering_env_a[@]}"), "log": "$T10_LOG_DIR/metering-a.log"}
    }
  },
  "b": {
    "dsn": "$dsn_b",
    "ports": {"pg": $pg_b, "redis": $redis_b, "control_grpc": $control_grpc_b, "control_http": $control_http_b,
              "gateway": $gateway_b, "gateway_diag": $diag_b, "metering": $metering_b,
              "console": $console_b, "upstream": $upstream_b},
    "urls": {"console": "http://127.0.0.1:$console_b", "gateway": "http://127.0.0.1:$gateway_b",
             "gateway_diag": "http://127.0.0.1:$diag_b", "control": "http://127.0.0.1:$control_http_b",
             "metering": "http://127.0.0.1:$metering_b", "upstream": "http://127.0.0.1:$upstream_b"},
    "containers": {"pg": "$pg_b_container", "redis": "$redis_b_container"},
    "commands": {
      "control": {"argv": ["$(t10_bin control)"], "env": $(json_env "${control_env_b[@]}"), "log": "$T10_LOG_DIR/control-b.log"},
      "gateway": {"argv": ["$(t10_bin gateway)"], "env": $(json_env "${gateway_env_b[@]}"), "log": "$T10_LOG_DIR/gateway-b.log"},
      "metering": {"argv": ["$(t10_bin metering)"], "env": $(json_env "${metering_env_b[@]}"), "log": "$T10_LOG_DIR/metering-b.log"},
      "console": {"argv": ["$(t10_bin console)", "serve"], "env": $(json_env "${console_env_b[@]}"), "log": "$T10_LOG_DIR/console-b.log"},
      "upstream": {"argv": ["$(t10_bin upstream-mock)", "-listen", "127.0.0.1:$upstream_b", "-name", "primary",
                            "-ttft", "5ms", "-token-interval", "5ms", "-tokens", "8",
                            "-authorization", "$provider_secret"], "env": {"GATEWAY_PROFILE": "demo"},
                   "log": "$T10_LOG_DIR/upstream-b.log"}
    }
  }
}
JSON

driver_dir="$(mktemp -d "${TMPDIR:-/tmp}/t10bk-driver.XXXXXX")"
driver_path="$driver_dir/backup_restore_driver.py"
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
    def __init__(self, evidence_dir, timeout):
        self.evidence_dir = evidence_dir
        self.timeout = timeout
        self.scratch = tempfile.mkdtemp(prefix="t10sec-http.")
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
        resp_dump.append(response.body[:MAX_STORED_BODY])
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
    """Two enterprises, four roles each, created through real HTTP."""

    def __init__(self, lab, client, cid="FIX"):
        self.lab = lab
        self.client = client
        self.cid = cid
        self.actors = {}
        self.orgs = {}
        self.keys = {}
        self.invitations = {}
        self.prices = {}
        self.secrets_path = os.path.join(lab.secret_dir, "fixtures.json")

    def url(self, name):
        return self.lab.urls[name]

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

        tag = self.lab.cfg["run_id"][-6:].lower().replace("-", "")[-6:]
        self.tag = tag

        def create_user(name, platform_admin=False):
            email = "t10-%s-%s@t10.test" % (tag, name)
            password = secrets.token_urlsafe(18)
            response = self.client.call(check, "create-user-" + name, "POST", "/api/v1/admin/users",
                                        base_url=self.url("console"),
                                        body={"email": email, "password": password,
                                              "platform_admin": platform_admin},
                                        session=admin.session)
            check.require_status(response, 201, label="create user " + email)
            RED.add(password)
            actor = Actor(name, email, password, str((response.json() or {}).get("id", "")), None, platform_admin)
            self.actors[name] = actor
            return actor

        # Enterprise A
        a_owner = create_user("a-owner")
        a_admin = create_user("a-admin")
        a_member = create_user("a-member")
        a_viewer = create_user("a-viewer")
        a_member2 = create_user("a-member2")     # removed-member-with-live-key case
        a_disabled = create_user("a-disabled")   # disabled-user-with-live-session case
        invitee = create_user("a-invitee")       # invitation tamper case
        # Enterprise B
        b_owner = create_user("b-owner")
        b_member = create_user("b-member")

        # Organization A: created by its owner (POST /api/v1/organizations).
        self.login(check, a_owner)
        response = self.client.call(check, "create-org-acme", "POST", "/api/v1/organizations",
                                    base_url=self.url("console"), body={"name": "t10-%s-acme" % tag},
                                    session=a_owner.session)
        check.require_status(response, 201, label="create org A")
        self.orgs["A"] = str(response.json()["organization"]["id"])

        # Organization B.
        self.login(check, b_owner)
        response = self.client.call(check, "create-org-beta", "POST", "/api/v1/organizations",
                                    base_url=self.url("console"), body={"name": "t10-%s-beta" % tag},
                                    session=b_owner.session)
        check.require_status(response, 201, label="create org B")
        self.orgs["B"] = str(response.json()["organization"]["id"])

        def invite_and_accept(owner, actor, org, role):
            response = self.client.call(check, "invite-" + actor.name, "POST",
                                        "/api/v1/orgs/%s/invitations" % org, base_url=self.url("console"),
                                        body={"email": actor.email, "role": role}, session=owner.session)
            check.require_status(response, 201, label="invite " + actor.name)
            token = (response.json() or {}).get("token", "")
            check.require(bool(token), "邀请响应缺少 token")
            RED.add(token)
            self.login(check, actor)
            accept = self.client.call(check, "accept-" + actor.name, "POST",
                                      "/api/v1/invitations/%s/accept" % token, base_url=self.url("console"),
                                      body={}, session=actor.session)
            check.require_status(accept, 200, label="accept " + actor.name)
            return token

        invite_and_accept(a_owner, a_admin, self.orgs["A"], "admin")
        invite_and_accept(a_owner, a_member, self.orgs["A"], "member")
        invite_and_accept(a_owner, a_viewer, self.orgs["A"], "billing_viewer")
        invite_and_accept(a_owner, a_member2, self.orgs["A"], "member")
        invite_and_accept(a_owner, a_disabled, self.orgs["A"], "member")
        invite_and_accept(b_owner, b_member, self.orgs["B"], "member")
        # 待接受邀请需要用受邀者自己的 session 去试（否则 401 掩盖了邀请逻辑），
        # 但**不**接受它：SEC-05 要拿它做篡改/未知/错 email 的靶子。
        self.login(check, invitee, label="login-a-invitee")

        # A pending invitation (used by the tamper/negative cases).
        response = self.client.call(check, "invite-pending", "POST", "/api/v1/orgs/%s/invitations" % self.orgs["A"],
                                    base_url=self.url("console"),
                                    body={"email": invitee.email, "role": "member"}, session=a_owner.session)
        check.require_status(response, 201, label="invite pending")
        pending_token = (response.json() or {}).get("token", "")
        RED.add(pending_token)
        self.invitations["pending"] = {"token": pending_token, "email": invitee.email,
                                       "user_id": invitee.user_id}

        # Keys: every key is created through the Console → Control AdminService path.
        def create_key(actor, org, owner_user_id=None, operation_id=None):
            body = {}
            if owner_user_id:
                body["owner_user_id"] = str(owner_user_id)
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
            return {"secret": key, "key_id": payload["key"]["key_id"], "prefix": payload["key"]["prefix"],
                    "owner_user_id": payload["key"].get("owner_user_id")}

        self.keys["a-member"] = create_key(a_member, self.orgs["A"])
        self.keys["a-member2"] = create_key(a_member2, self.orgs["A"])
        self.keys["a-disabled"] = create_key(a_disabled, self.orgs["A"])
        self.keys["a-viewer"] = create_key(a_admin, self.orgs["A"], owner_user_id=a_viewer.user_id)
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
                                        body={"amount_micro": str(amount), "direction": "credit",
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
            "keys": {name: {"key_id": k["key_id"], "prefix": k["prefix"], "owner_user_id": k["owner_user_id"]}
                     for name, k in self.keys.items()},
            "prices": self.prices,
            "invitations": {name: {"email": v["email"], "user_id": v["user_id"]}
                            for name, v in self.invitations.items()},
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
"""T10 F15 backup/restore and Redis-flush assertions (see backup-restore-drill.sh)."""

import argparse
import json
import os
import shutil
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
    Check, CheckFailure, Client, EnvLab, Fixtures, InfraFailure, Lab, Proc, RED,
    ensure_service, log, redact, sse_request, utcnow,
)


def sha256_file(path):
    import hashlib
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


class Drill:
    def __init__(self, lab, cfg, client):
        self.lab = lab
        self.cfg = cfg
        self.client = client
        self.a = EnvLab(lab, cfg["a"])
        self.b = EnvLab(lab, cfg["b"])
        self.checks = []
        self.findings = []
        self.extra = {}

    # -- runner ------------------------------------------------------------
    def run(self, cid, title, fn):
        check = Check(cid, title)
        log("")
        log("[%s] %s" % (cid, title))
        try:
            fn(check)
            check.status = "PASS"
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

    def finding(self, text):
        self.findings.append(redact(text))
        log("FINDING: " + text)

    # -- facts -------------------------------------------------------------
    def facts(self, env):
        q = env.psql
        lines = env.psql_lines
        return {
            "users": q("SELECT count(*) FROM users"),
            "organizations": lines("SELECT id||'|'||name||'|'||type||'|'||status FROM organizations ORDER BY id"),
            "memberships": lines("SELECT org_id||'|'||user_id||'|'||role||'|'||status FROM memberships ORDER BY org_id, user_id"),
            "api_keys": lines("SELECT key_id||'|'||org_id||'|'||coalesce(owner_user_id::text,'')||'|'||status FROM api_keys ORDER BY key_id"),
            "price_versions": lines("SELECT id||'|'||model||'|'||input_price_micro_per_million||'|'||output_price_micro_per_million FROM price_versions ORDER BY id"),
            "executions_by_status": lines("SELECT status||'|'||count(*) FROM billing_executions GROUP BY status ORDER BY status"),
            "receipts_by_status": lines("SELECT status||'|'||count(*)||'|'||sum(charge_micro) FROM usage_receipts GROUP BY status ORDER BY status"),
            "entries_by_kind": lines("SELECT kind||'|'||direction||'|'||count(*)||'|'||sum(amount_micro) FROM billing_entries GROUP BY kind, direction ORDER BY kind, direction"),
            "accounts": lines("SELECT org_id||'|'||posted_balance_micro||'|'||reserved_micro FROM billing_accounts ORDER BY org_id"),
            "ledger_sum_by_org": lines("SELECT org_id||'|'||sum(CASE WHEN direction='credit' THEN amount_micro ELSE -amount_micro END) FROM billing_entries GROUP BY org_id ORDER BY org_id"),
            "provider_credentials": lines("SELECT credential_id||'|'||provider_id||'|'||key_id||'|'||status||'|'||encode(sha256(ciphertext||nonce),'hex') FROM provider_credentials ORDER BY credential_id"),
            "control_config": lines("SELECT version FROM control_config"),
            "policy_publications": q("SELECT count(*) FROM policy_publications"),
            "schema_version": q("SELECT version FROM schema_migrations"),
        }

    def money_fingerprint(self, env):
        """Order-independent digest of the four durable money layers."""
        return env.psql(
            "SELECT md5(string_agg(row, E'\\n' ORDER BY row)) FROM ("
            " SELECT 'entry|'||id||'|'||org_id||'|'||kind||'|'||direction||'|'||amount_micro||'|'||business_id AS row FROM billing_entries"
            " UNION ALL SELECT 'receipt|'||execution_id||'|'||status||'|'||charge_micro||'|'||price_version_id||'|'||total_tokens FROM usage_receipts"
            " UNION ALL SELECT 'exec|'||execution_id||'|'||status||'|'||org_id||'|'||price_version_id FROM billing_executions"
            " UNION ALL SELECT 'acct|'||org_id||'|'||posted_balance_micro||'|'||reserved_micro FROM billing_accounts"
            ") t")

    def ledger_invariants(self, env):
        return {
            # 只有**已结算**的执行才必须恰好一条 debit：被拒绝（限流/余额）的请求会留下
            # 已释放的 execution，没有 debit 是正确的。
            "settled_not_one_debit": env.psql_lines(
                "SELECT r.execution_id||'|'||count(d.id) FROM usage_receipts r "
                "LEFT JOIN billing_entries d ON d.business_id='billing_receipt:debit:'||r.execution_id "
                "WHERE r.status='settled' GROUP BY r.execution_id HAVING count(d.id) <> 1"),
            "unsettled_executions_with_debit": env.psql_lines(
                "SELECT r.execution_id||'|'||r.status FROM usage_receipts r JOIN billing_entries d "
                "ON d.business_id='billing_receipt:debit:'||r.execution_id WHERE r.status <> 'settled'"),
            "executions_without_debit": env.psql_lines(
                "SELECT e.execution_id||'|'||e.status FROM billing_executions e WHERE NOT EXISTS ("
                "SELECT 1 FROM billing_entries d WHERE d.business_id='billing_receipt:debit:'||e.execution_id)"),
            "account_mismatch": env.psql_lines(
                "SELECT a.org_id FROM billing_accounts a LEFT JOIN ("
                " SELECT org_id, sum(CASE WHEN direction='credit' THEN amount_micro ELSE -amount_micro END) AS total"
                " FROM billing_entries GROUP BY org_id) l ON l.org_id=a.org_id "
                "WHERE a.posted_balance_micro <> COALESCE(l.total,0)"),
            "settled_executions_without_receipt": env.psql_lines(
                "SELECT execution_id FROM billing_executions e WHERE e.status='settled' "
                "AND NOT EXISTS (SELECT 1 FROM usage_receipts r WHERE r.execution_id=e.execution_id)"),
            "debit_without_receipt": env.psql_lines(
                "SELECT d.business_id FROM billing_entries d WHERE d.kind='debit' "
                "AND NOT EXISTS (SELECT 1 FROM usage_receipts r WHERE d.business_id='billing_receipt:debit:'||r.execution_id)"),
        }

    # -- dump / restore ----------------------------------------------------
    def dump(self, env, path):
        started = time.time()
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as handle:
            proc = subprocess.run(
                ["docker", "exec", "-i", env.containers["pg"], "pg_dump", "-Fc", "-U", "gateway_test",
                 "-d", "gateway_test"], stdout=handle, stderr=subprocess.PIPE, timeout=900)
        if proc.returncode != 0:
            raise InfraFailure("pg_dump 失败: %s" % redact(proc.stderr.decode("utf-8", "replace")))
        return {"path": path, "committed": False,
                "note": "dump 文件不上库（含 password_hash/key_hash/会话行）；此处只记录 sha256 与尺寸",
                "seconds": round(time.time() - started, 3),
                "bytes": os.path.getsize(path), "sha256": sha256_file(path)}

    def restore(self, env, dump_path, db="gateway_test"):
        started = time.time()
        if db != "gateway_test":
            env.psql_db("postgres", "DROP DATABASE IF EXISTS %s" % db)
            env.psql_db("postgres", "CREATE DATABASE %s" % db)
        with open(dump_path, "rb") as handle:
            proc = subprocess.run(
                ["docker", "exec", "-i", env.containers["pg"], "pg_restore", "--no-owner", "--no-acl",
                 "-U", "gateway_test", "-d", db], stdin=handle, capture_output=True, timeout=900)
        if proc.returncode != 0:
            raise InfraFailure("pg_restore 失败(%s): %s" % (db, redact(proc.stderr.decode("utf-8", "replace"))))
        return {"database": db, "seconds": round(time.time() - started, 3)}

    # -- traffic -----------------------------------------------------------
    def drive(self, check, count, env=None, label="request"):
        env = env or self.a
        key = self.fx.keys["a-member"]["secret"]
        before = set(env.psql_lines("SELECT execution_id FROM billing_executions"))
        for index in range(count):
            result = sse_request(env, key, timeout=60,
                                 body={"model": self.model, "messages": [{"role": "user", "content": "hi"}],
                                       "stream": True},
                                 request_id="t10bk-%s-%d" % (label, index))
            if result["status"] != 200 or result["done"] is None:
                raise CheckFailure("%s #%d 失败: status=%s（%s）" % (
                    label, index, result["status"], (result["error_body"] or "")[:160]))
        executions = []
        deadline = time.time() + 30
        while time.time() < deadline:
            executions = sorted(set(env.psql_lines("SELECT execution_id FROM billing_executions")) - before)
            if len(executions) >= count:
                break
            time.sleep(0.3)
        if len(executions) != count:
            raise CheckFailure("驱动了 %d 个请求但只新增 %d 条 execution" % (count, len(executions)))
        return executions

    def release_lock(self, env, org):
        """Terminate the psql backend that holds the account row lock.

        Killing the local `docker exec` client is not enough: the server-side
        session keeps running `pg_sleep` and holds the lock, which would make the
        next admission reserve time out (that is exactly what the first version
        of this drill measured). Terminating the backend is deterministic, and
        the release is verified with a bounded FOR UPDATE.
        """
        env.psql("SELECT pg_terminate_backend(pid) FROM pg_stat_activity "
                 "WHERE query LIKE '%pg_sleep%' AND pid <> pg_backend_pid()")
        deadline = time.time() + 15
        while time.time() < deadline:
            try:
                probe = env.psql("SET lock_timeout='500ms'; SELECT 1 FROM billing_accounts "
                                 "WHERE org_id=%s FOR UPDATE" % org)
            except InfraFailure:
                time.sleep(0.3)  # 仍被锁住：lock_timeout 触发 → psql 非零退出
                continue
            if probe.strip().endswith("1"):
                return True
            time.sleep(0.3)
        return False

    def wait_settled(self, env, timeout=180, executions=None):
        deadline = time.time() + timeout
        statuses = []
        while time.time() < deadline:
            if executions:
                statuses = env.psql_lines(
                    "SELECT status||'|'||count(*) FROM usage_receipts WHERE execution_id IN (%s) GROUP BY status"
                    % ",".join("'%s'" % item for item in executions))
                if statuses and all(row.startswith("settled|") for row in statuses):
                    return True, statuses
            else:
                statuses = env.psql_lines("SELECT status||'|'||count(*) FROM usage_receipts GROUP BY status")
                if statuses and all(row.startswith("settled|") for row in statuses):
                    return True, statuses
            time.sleep(0.5)
        return False, statuses

    # -- BK-01 -------------------------------------------------------------
    def bk01(self, check):
        settled, statuses = self.wait_settled(self.a, timeout=120)
        check.require(settled, "备份前 A 仍有未结算收据: %r" % statuses)
        self.facts_a = self.facts(self.a)
        self.fingerprint_a = self.money_fingerprint(self.a)
        check.detail("facts_before_backup", self.facts_a)
        check.detail("money_fingerprint_before_backup", self.fingerprint_a)

        # 静默备份：只证明"备份/恢复不改变事实"（无并发写窗口）。
        # 备份文件本身不入库（dump 里有 password_hash / key_hash / 会话行），只记录
        # 尺寸与 sha256；文件留在 evidence 目录之外的临时目录。
        self.dump_static = self.dump(self.a, os.path.join(self.lab.secret_dir, "dumps", "dump-static.pgc"))
        check.detail("static_dump", self.dump_static)

        # live 备份：备份窗口内继续写，用于测量 RPO。
        holder = {}

        def writer():
            holder["executions"] = self.drive(check, self.cfg["live_requests"], label="rpo")

        thread = threading.Thread(target=writer)
        thread.start()
        time.sleep(0.2)
        self.dump_live = self.dump(self.a, os.path.join(self.lab.secret_dir, "dumps", "dump-live.pgc"))
        thread.join(timeout=300)
        self.rpo_executions = holder.get("executions") or []
        check.require(len(self.rpo_executions) == self.cfg["live_requests"],
                      "RPO 窗口内的写入没有全部完成: %d/%d" % (len(self.rpo_executions), self.cfg["live_requests"]))
        self.facts_after = self.facts(self.a)
        check.detail("live_dump", self.dump_live)
        check.detail("rpo_probe_executions", self.rpo_executions)
        check.detail("facts_after_live_window", self.facts_after)
        check.note("静默备份 %.3fs/%d B；live 备份 %.3fs，窗口内写入 %d 个请求"
                   % (self.dump_static["seconds"], self.dump_static["bytes"],
                      self.dump_live["seconds"], len(self.rpo_executions)))

    # -- BK-02 -------------------------------------------------------------
    def bk02(self, check):
        self.restore_result = self.restore(self.b, self.dump_static["path"])
        check.detail("restore", self.restore_result)
        facts_b = self.facts(self.b)
        check.detail("facts_after_restore", facts_b)
        for field in ("organizations", "memberships", "api_keys", "price_versions", "accounts",
                      "ledger_sum_by_org", "executions_by_status", "receipts_by_status", "entries_by_kind",
                      "control_config", "schema_version", "provider_credentials"):
            check.require(facts_b[field] == self.facts_a[field],
                          "恢复后 %s 与备份前不一致:\n  A=%r\n  B=%r" % (field, self.facts_a[field], facts_b[field]))
        self.fingerprint_b = self.money_fingerprint(self.b)
        check.require(self.fingerprint_b == self.fingerprint_a,
                      "恢复后的资金指纹不同: A=%s B=%s" % (self.fingerprint_a, self.fingerprint_b))
        check.detail("money_fingerprint_after_restore", self.fingerprint_b)
        crypto_a = [row for row in self.facts_a["provider_credentials"] if row.split("|")[3] == "active"]
        crypto_b = [row for row in facts_b["provider_credentials"] if row.split("|")[3] == "active"]
        check.require(crypto_a == crypto_b and len(crypto_a) >= 1,
                      "provider credential 密文在恢复后不同: A=%r B=%r" % (crypto_a, crypto_b))
        check.note("身份/成员/Key/价格/账本/账户/收据/control 配置/密文逐项一致；资金指纹 %s"
                   % self.fingerprint_b[:16])

    # -- BK-03 -------------------------------------------------------------
    def bk03(self, check):
        key_file = self.cfg["master_key_file"]

        def control_log_tail():
            try:
                with open(self.b.cfg["commands"]["control"]["log"], encoding="utf-8", errors="replace") as handle:
                    return handle.read()[-5000:]
            except OSError:
                return ""

        def start_control_expect_failure(overrides, label):
            proc = Proc(self.b, "control")
            proc.start(overrides)
            try:
                code = proc.popen.wait(timeout=30)
            except subprocess.TimeoutExpired:
                proc.stop()
                raise CheckFailure("%s：control 没有拒绝启动（30s 后仍在运行）" % label)
            return code, control_log_tail()

        # (1) 错误主密钥：有活跃密文时必须拒绝启动。
        code_wrong, tail_wrong = start_control_expect_failure(
            {"PROVIDER_CREDENTIAL_MASTER_KEY_FILE": self.cfg["wrong_key_file"]}, "错误主密钥")
        check.require("cannot decrypt active credentials" in tail_wrong,
                      "错误主密钥的失败原因不明确: %r" % tail_wrong[-400:])
        self.wrong_key_behaviour = {
            "exit_code": code_wrong,
            "log_lines": [line for line in tail_wrong.splitlines() if "master key" in line][-3:]}

        # (2) 主密钥文件缺失：同样必须拒绝启动。
        code_missing, tail_missing = start_control_expect_failure(
            {"PROVIDER_CREDENTIAL_MASTER_KEY_FILE": os.path.join(self.lab.secret_dir, "does-not-exist")},
            "无主密钥")
        check.require("master key" in tail_missing or "cannot decrypt active credentials" in tail_missing,
                      "无主密钥的失败原因不明确: %r" % tail_missing[-400:])
        self.missing_key_behaviour = {
            "exit_code": code_missing,
            "log_lines": [line for line in tail_missing.splitlines() if "master key" in line][-3:]}
        check.detail("wrong_key", self.wrong_key_behaviour)
        check.detail("missing_key", self.missing_key_behaviour)

        # (3) 正确主密钥：control 启动，凭据可解密并真的被送到上游（mock 校验 Bearer）。
        proc = Proc(self.b, "control")
        proc.start({"PROVIDER_CREDENTIAL_MASTER_KEY_FILE": key_file})
        check.require(self.b.wait_http(self.b.urls["control"] + "/healthz", 45),
                      "正确主密钥下 control 未就绪")
        upstream = Proc(self.b, "upstream")
        upstream.start()
        check.require(self.b.wait_tcp(self.b.ports["upstream"], 30), "B 的 upstream 未监听")
        ensure_service(self.b, "metering", self.b.ports["metering"])
        ensure_service(self.b, "console", self.b.ports["console"])
        gateway = Proc(self.b, "gateway")
        gateway.start()
        check.require(self.b.wait_http(self.b.urls["gateway"] + "/healthz", 60), "B 的 gateway 未就绪")
        deadline = time.time() + 90
        result = None
        while time.time() < deadline:
            result = sse_request(self.b, self.fx.keys["a-member"]["secret"], timeout=60,
                                 body={"model": self.model, "messages": [{"role": "user", "content": "hi"}],
                                       "stream": True}, request_id="t10bk-restored-request")
            if result["status"] == 200 and result["done"] is not None:
                break
            time.sleep(0.5)
        check.require(result and result["status"] == 200 and result["done"] is not None,
                      "恢复后的 Key 无法调用: status=%s（%s）" % (
                          result and result["status"], (result and result["error_body"] or "")[:160]))
        check.note("错误主密钥/缺失主密钥都让 control 拒绝启动（exit=%s/%s）；正确主密钥下恢复后的 Key "
                   "可调用，且 provider 凭据解密后被 mock 的 Bearer 校验接受" % (code_wrong, code_missing))

    # -- BK-04 -------------------------------------------------------------
    def bk04(self, check):
        status, report = self.b.metering_json("/reconcile?window=24h")
        check.require(status in (200, 409), "B 的对账端点返回 %s" % status)
        check.require(status == 200 and report.get("consistent") and report.get("complete"),
                      "恢复环境对账不一致: %s" % json.dumps(report)[:400])
        check.detail("reconcile_b", report.get("report"))
        # BK-03 在 B 里发过一发验证请求：先等它结算，否则会把"正在结算"误判成不变量违反。
        settled, statuses = self.wait_settled(self.b, timeout=120)
        check.require(settled, "恢复环境仍有未结算收据: %r" % statuses)
        invariants_b = self.ledger_invariants(self.b)
        for name, rows in invariants_b.items():
            check.require(not rows, "恢复环境违反不变量 %s: %r" % (name, rows[:5]))
        check.detail("invariants_b", invariants_b)
        b_ledger = self.b.psql_lines(
            "SELECT org_id||'|'||kind||'|'||direction||'|'||count(*)||'|'||sum(amount_micro) "
            "FROM billing_entries GROUP BY org_id, kind, direction ORDER BY org_id, kind, direction")
        check.detail("ledger_b", b_ledger)
        check.require(len(b_ledger) >= 1, "恢复环境没有账本行")
        check.note("恢复环境对账 consistent+complete；账户投影与流水重算一致（B 另有一发验证请求）")

    # -- BK-05 -------------------------------------------------------------
    def bk05(self, check):
        self.restore(self.b, self.dump_live["path"], db="gateway_test_rpo")
        present = self.b.psql_db(
            "gateway_test_rpo",
            "SELECT execution_id FROM billing_executions WHERE execution_id IN (%s) ORDER BY execution_id"
            % ",".join("'%s'" % execution for execution in self.rpo_executions))
        present_set = set(present.splitlines()) if present else set()
        newest = self.a.psql("SELECT to_char(max(created_at),'YYYY-MM-DD\"T\"HH24:MI:SS.USOF') FROM billing_entries")
        rpo = {
            "live_window_requests": len(self.rpo_executions),
            "captured_in_dump": len(present_set),
            "lost_after_snapshot": len(self.rpo_executions) - len(present_set),
            "dump_seconds": self.dump_live["seconds"],
            "dump_bytes": self.dump_live["bytes"],
            "newest_fact_in_A": newest,
            "interpretation": "pg_dump 是单事务一致性快照；快照之后提交的资金事实不在备份里，"
                              "只能靠下一次备份覆盖 —— 这就是单节点 PG 的实际 RPO。",
        }
        check.detail("rpo", rpo)
        check.require(len(present_set) <= len(self.rpo_executions), "恢复出的 execution 多于备份窗口写入")
        rto = {
            "db_restore_seconds": self.restore_result["seconds"],
            "static_dump_seconds": self.dump_static["seconds"],
            "dump_bytes": self.dump_static["bytes"],
            "scope_note": "B 与 A 同机、同 docker daemon：RTO 不含机器/磁盘/镜像冷启动，也不含 DNS/网络重建",
        }
        check.detail("rto", rto)
        check.require(rto["db_restore_seconds"] > 0, "RTO 未测量")
        check.note("RPO：live 备份窗口内 %d 个请求，快照捕获 %d、快照后丢失 %d；"
                   "RTO：静默备份 %.3fs + 恢复到新库 %.3fs（%d B）"
                   % (rpo["live_window_requests"], rpo["captured_in_dump"], rpo["lost_after_snapshot"],
                      rto["static_dump_seconds"], rto["db_restore_seconds"], rto["dump_bytes"]))
        check.detail("ha_statement", "单节点 PostgreSQL + 单节点 Redis：无 standby/副本/自动故障转移；"
                                     "本次恢复手段是离线备份 + 重建，RPO/RTO 如上，不能当作 HA 承诺")

    # -- RF-01 -------------------------------------------------------------
    def rf01(self, check):
        env = self.b
        org = self.fx.orgs["A"]
        key = self.fx.keys["a-member"]["secret"]
        metering = Proc(env, "metering")
        metering.stop()
        check.require(env.wait_port_closed(env.ports["metering"], 10), "metering 未停止")

        executions = self.drive(check, self.cfg["flush_requests"], env=env, label="flush")
        rows = env.psql_lines(
            "SELECT status||'|'||count(*) FROM usage_receipts WHERE execution_id IN (%s) GROUP BY status"
            % ",".join("'%s'" % execution for execution in executions))
        check.detail("receipts_before_dispatch", rows)
        check.require(all(not row.startswith("settled|") for row in rows),
                      "停掉 metering 后仍有收据被结算: %r" % rows)

        # 冻结结算：锁住 org A 的账户行，让 consumer 无法提交，而 dispatcher 仍能投递。
        lock_sql = ("BEGIN; SELECT 1 FROM billing_accounts WHERE org_id=%s FOR UPDATE; SELECT pg_sleep(60); COMMIT;"
                    % org)
        locker = subprocess.Popen(
            ["docker", "exec", "-i", env.containers["pg"], "psql", "-q", "-v", "ON_ERROR_STOP=1",
             "-U", "gateway_test", "-d", "gateway_test", "-c", lock_sql],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(1.5)
        metering = Proc(env, "metering")
        metering.start()
        check.require(env.wait_http(env.urls["metering"] + "/healthz", 45), "metering 未重新就绪")
        deadline = time.time() + 40
        dispatched = []
        stream_len = 0
        while time.time() < deadline:
            dispatched = env.psql_lines(
                "SELECT execution_id FROM usage_receipts WHERE status='dispatched' AND execution_id IN (%s)"
                % ",".join("'%s'" % execution for execution in executions))
            stream_len = int(env.redis("XLEN", "usage:events") or 0)
            if len(dispatched) >= len(executions) and stream_len >= len(executions):
                break
            time.sleep(0.5)
        check.detail("dispatched_before_flush", len(dispatched))
        check.detail("stream_length_before_flush", stream_len)
        check.require(dispatched, "没有收据处于 dispatched（未结算）状态，Redis 清空演练的前提不成立")
        check.require(stream_len > 0, "Redis 流里没有已投递的消息")

        try:
            # 先做传输丢失（清空 Redis），再释放行锁：这样"已投递未结算"的收据确实
            # 丢掉了传输条目，而后续的停机探测不会被我们自己的行锁干扰。
            env.redis("FLUSHALL")
            check.require(int(env.redis("XLEN", "usage:events") or 0) == 0, "FLUSHALL 后流仍非空")
            check.detail("flushed", True)
            # 立刻停掉 metering：否则 dispatcher/consumer 会在我们停 Redis 之前
            # 就把刚被清空的消息重新投递并结算，观测窗口就不再确定。
            Proc(env, "metering").stop()
            check.require(env.wait_port_closed(env.ports["metering"], 10), "metering 未停止")
            check.require(self.release_lock(env, org), "账户行锁没有被释放（reserve 会被自己的锁挡住）")
            try:
                locker.kill()
                locker.wait(timeout=10)
            except Exception:  # noqa: BLE001
                pass
            self.rf01_outage(check, env, key, executions)
        finally:
            # 无论断言是否失败，都要把 Redis 与行锁恢复，避免把环境留在坏状态。
            self.release_lock(env, org)
            if not env.wait_tcp(env.ports["redis"], 1):
                env.start_container(env.containers["redis"])
                env.wait_tcp(env.ports["redis"], 30)
            try:
                locker.kill()
                locker.wait(timeout=10)
            except Exception:  # noqa: BLE001
                pass
            if not env.wait_tcp(env.ports["metering"], 1):
                Proc(env, "metering").start()
        self.rf01_recovery(check, env, executions)
        check.note("Redis 清空+停机期间：新请求 503 rate limiter unavailable、/readyz 503、"
                   "%d 条收据仍在 PG；恢复后 %.1fs 内全部 settled"
                   % (len(executions), self.flush_recovery_seconds))

    def rf01_outage(self, check, env, key, executions):
        # Redis 停机：新请求必须 fail-closed（限流器 fail-closed）。
        env.stop_container(env.containers["redis"])
        check.require(env.wait_port_closed(env.ports["redis"], 20), "Redis 未停止")
        outage = sse_request(env, key, timeout=30, request_id="t10bk-redis-outage",
                             body={"model": self.model, "messages": [{"role": "user", "content": "hi"}],
                                   "stream": True})
        check.require(outage["status"] == 503,
                      "Redis 停机时新请求期望 503（fail-closed），实际 %s" % outage["status"])
        check.require("rate limiter unavailable" in (outage["error_body"] or ""),
                      "Redis 停机的错误不是 limiter fail-closed: %s" % (outage["error_body"] or "")[:200])
        ready = env.http_json(env.urls["gateway"] + "/readyz")[0]
        check.detail("gateway_readyz_during_redis_outage", ready)
        check.require(ready == 503, "Redis 停机时 /readyz 期望 503，实际 %s" % ready)
        still = env.psql_lines(
            "SELECT execution_id FROM usage_receipts WHERE execution_id IN (%s) AND status <> 'settled'"
            % ",".join("'%s'" % execution for execution in executions))
        check.detail("unsettled_during_outage", len(still))
        check.require(len(still) == len(executions),
                      "Redis 停机期间 PG 里的未结算收据状态丢失: %d/%d" % (len(still), len(executions)))

    def rf01_recovery(self, check, env, executions):
        # 恢复 Redis（空实例）→ PG 驱动的重投（dispatched + dispatch_after）把结算补齐。
        env.start_container(env.containers["redis"])
        check.require(env.wait_tcp(env.ports["redis"], 30), "Redis 未恢复")
        if not env.wait_tcp(env.ports["metering"], 1):
            Proc(env, "metering").start()
        check.require(env.wait_http(env.urls["metering"] + "/healthz", 45), "metering 未重新就绪")
        released = time.time()
        ok, statuses = self.wait_settled(env, timeout=180, executions=executions)
        recovered_at = time.time() - released if ok else None
        check.detail("recovery_seconds", recovered_at)
        check.detail("receipt_status_after_recovery", statuses)
        check.require(ok, "180s 内没有把 %d 条未结算收据补齐: %r" % (len(executions), statuses))
        self.flush_executions = executions
        self.flush_recovery_seconds = recovered_at

    # -- RF-02 -------------------------------------------------------------
    def rf02(self, check):
        env = self.b
        settled, statuses = self.wait_settled(env, timeout=120)
        check.require(settled, "仍有未结算收据: %r" % statuses)
        invariants = self.ledger_invariants(env)
        check.require(not invariants["settled_not_one_debit"],
                      "已结算执行的 debit 数 != 1: %r" % invariants["settled_not_one_debit"][:5])
        check.require(not invariants["unsettled_executions_with_debit"],
                      "未结算收据却已经有 debit: %r" % invariants["unsettled_executions_with_debit"][:5])
        check.require(not invariants["account_mismatch"],
                      "账户投影与流水不一致: %r" % invariants["account_mismatch"])
        check.require(not invariants["settled_executions_without_receipt"],
                      "存在无 receipt 的 settled 执行: %r" % invariants["settled_executions_without_receipt"][:5])
        check.require(not invariants["debit_without_receipt"],
                      "存在没有对应 receipt 的 debit: %r" % invariants["debit_without_receipt"][:5])
        rows = env.psql_lines(
            "SELECT r.execution_id||'|'||r.status||'|'||r.charge_micro||'|'||count(d.id)||'|'||"
            "coalesce(sum(d.amount_micro),0) FROM usage_receipts r LEFT JOIN billing_entries d "
            "ON d.business_id='billing_receipt:debit:'||r.execution_id WHERE r.execution_id IN (%s) "
            "GROUP BY r.execution_id, r.status, r.charge_micro"
            % ",".join("'%s'" % execution for execution in self.flush_executions))
        check.detail("flush_executions", rows)
        for row in rows:
            parts = row.split("|")
            check.require(parts[1] == "settled" and parts[3] == "1",
                          "清空演练的执行 %s: status=%s debit=%s" % (parts[0], parts[1], parts[3]))
            check.require(parts[2] == parts[4],
                          "清空演练的执行 %s 扣款 %s != receipt 金额 %s" % (parts[0], parts[4], parts[2]))
        status, report = env.metering_json("/reconcile?window=24h")
        check.require(status == 200 and report.get("consistent") and report.get("complete"),
                      "Redis 演练后对账不一致: %s" % json.dumps(report)[:400])
        check.detail("reconcile_after_flush", report.get("report"))
        recovered = sse_request(env, self.fx.keys["a-member"]["secret"], timeout=60,
                                body={"model": self.model, "messages": [{"role": "user", "content": "hi"}],
                                      "stream": True}, request_id="t10bk-after-flush")
        check.require(recovered["status"] == 200 and recovered["done"] is not None,
                      "Redis 恢复后新请求仍失败: %s" % recovered["status"])
        check.note("清空演练的 %d 条收据全部 settled、各恰好一条 debit、金额与 receipt 一致；对账一致，"
                   "新请求恢复 200" % len(self.flush_executions))


    # -- RF-03 -------------------------------------------------------------
    def rf03(self, check):
        """长 Redis 停机边界：超过投递重试预算后，收据进入审计 exception（不再自动重投）。"""
        env = self.b
        org = self.fx.orgs["A"]
        metering = Proc(env, "metering")
        metering.stop()
        check.require(env.wait_port_closed(env.ports["metering"], 10), "metering 未停止")
        executions = self.drive(check, 3, env=env, label="poison")
        posted_before = env.psql("SELECT posted_balance_micro FROM billing_accounts WHERE org_id=%s" % org)
        reserved_before = env.psql("SELECT reserved_micro FROM billing_accounts WHERE org_id=%s" % org)
        # 让 Redis 在 dispatcher 投递期间一直不可用（先停 metering，保证这些收据还没被投递）。
        env.stop_container(env.containers["redis"])
        check.require(env.wait_port_closed(env.ports["redis"], 20), "Redis 未停止")
        outage_started = time.time()
        Proc(env, "metering").start()
        check.require(env.wait_tcp(env.ports["metering"], 45), "metering 未就绪")
        poisoned_at = None
        statuses = []
        deadline = time.time() + 300
        while time.time() < deadline:
            statuses = env.psql_lines(
                "SELECT status||'|'||count(*) FROM usage_receipts WHERE execution_id IN (%s) GROUP BY status"
                % ",".join("'%s'" % execution for execution in executions))
            if statuses and all(row.startswith("exception|") for row in statuses):
                poisoned_at = time.time() - outage_started
                break
            time.sleep(2.0)
        check.detail("poison_boundary_seconds", poisoned_at)
        check.detail("statuses", statuses)
        check.require(poisoned_at is not None,
                      "300s 内收据没有进入 exception（预期长停机后会被投递重试预算判定为终态）: %r" % statuses)
        rows = env.psql_lines(
            "SELECT execution_id||'|'||attempts||'|'||coalesce(last_error,'') FROM usage_receipts "
            "WHERE execution_id IN (%s) ORDER BY execution_id"
            % ",".join("'%s'" % execution for execution in executions))
        check.detail("poisoned_receipts", rows)
        check.require(all(int(row.split("|")[1]) >= 8 for row in rows),
                      "被 poison 的收据 attempts 不足: %r" % rows)
        # 资金：没有 debit、预占仍被持有、posted 不变（钱没有丢，也没有重复扣）。
        debits = env.psql_lines(
            "SELECT count(*) FROM billing_entries WHERE kind='debit' AND business_id IN (%s)"
            % ",".join("'billing_receipt:debit:%s'" % execution for execution in executions))
        check.detail("debits_for_poisoned", debits)
        check.require(debits and int(debits[0]) == 0, "被 poison 的执行产生了 debit: %r" % debits)
        posted_after = env.psql("SELECT posted_balance_micro FROM billing_accounts WHERE org_id=%s" % org)
        reserved_after = env.psql("SELECT reserved_micro FROM billing_accounts WHERE org_id=%s" % org)
        check.require(posted_before == posted_after,
                      "长停机期间 posted 余额变化: %s -> %s" % (posted_before, posted_after))
        check.require(int(reserved_after) >= int(reserved_before),
                      "预占没有被保留: %s -> %s" % (reserved_before, reserved_after))
        check.detail("account", {"posted_before": posted_before, "posted_after": posted_after,
                                 "reserved_before": reserved_before, "reserved_after": reserved_after})
        # 恢复 Redis：这些收据**不会**自动复活（需要人工/后续版本处理）。
        env.start_container(env.containers["redis"])
        check.require(env.wait_tcp(env.ports["redis"], 30), "Redis 未恢复")
        check.require(env.wait_http(env.urls["metering"] + "/readyz", 90), "metering 未恢复就绪")
        time.sleep(20)
        after = env.psql_lines(
            "SELECT status||'|'||count(*) FROM usage_receipts WHERE execution_id IN (%s) GROUP BY status"
            % ",".join("'%s'" % execution for execution in executions))
        check.detail("statuses_after_redis_back", after)
        check.require(all(row.startswith("exception|") for row in after),
                      "Redis 恢复后 exception 收据被自动处理了（预期仍需人工）: %r" % after)
        # 可见性：审计 exception 队列与对账 backlog 必须能看到它们（不是静默丢失）。
        status, exceptions = env.metering_json("/receipts/exceptions")
        check.detail("receipts_exceptions_endpoint_status", status)
        status2, report = env.metering_json("/reconcile?window=24h")
        backlog = ((report or {}).get("report") or {}).get("backlog") or {}
        check.detail("reconcile_after_poison", {"status": status2, "consistent": (report or {}).get("consistent"),
                                                "complete": (report or {}).get("complete"), "backlog": backlog})
        check.require(status2 == 200 and (report or {}).get("consistent"),
                      "长停机后四层对账不一致: %s" % json.dumps(report)[:300])
        check.require(backlog.get("receipt_exceptions", 0) >= len(executions),
                      "异常收据没有出现在 backlog: %r" % backlog)
        self.finding("长 Redis 停机（>%.0fs）会让 receipt dispatcher 用尽投递重试预算"
                     "（max_attempts=8，退避 1s..5m）：%d 条收据进入终态 exception（日志「receipt poisoned」），"
                     "预占资金仍被持有、没有 debit、posted 不变、对账仍 consistent，但**不会**在 Redis 恢复后自动重投，"
                     "需要人工/后续机制处理（F08/§6.2 的 30s 故障仍在预算内，本边界是更长的停机）。"
                     % (poisoned_at, len(executions)))
        check.note("边界：Redis 停机 %.0fs 后 %d 条收据被 poison（attempts=8、last_error=publish 连接拒绝）；"
                   "资金未动，异常在 backlog 可见，恢复 Redis 后不自动复活" % (poisoned_at, len(executions)))


def write_summary(lab, checks, findings, extra):
    payload = {
        "run_id": lab.cfg["run_id"],
        "generated_at": utcnow(),
        "checks": [{"id": c.cid, "title": c.title, "status": c.status, "notes": c.notes,
                    "details": c.details, "failure": c.failure, "started_at": c.started_at,
                    "finished_at": c.finished_at} for c in checks],
        "findings": findings,
        "extra": extra,
    }
    path = os.path.join(lab.evidence_dir, "restore-summary.json")
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=False)
    return path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--lab", required=True)
    args = parser.parse_args()
    cfg = json.load(open(args.lab, encoding="utf-8"))
    t10.set_logfile(os.path.join(cfg["log_dir"], "drill.log"))
    lab = Lab(cfg)
    client = Client(cfg["evidence_dir"], cfg["timeout_seconds"])
    drill = Drill(lab, cfg, client)
    os.makedirs(os.path.join(cfg["evidence_dir"], "raw"), exist_ok=True)

    log("== A 环境 fixtures（真实 HTTP；provider 凭据走加密存储）==")
    try:
        drill.fx = Fixtures(drill.a, client, cid="BFIX")
        fixture_check = drill.fx.create()
        effective_at = (datetime.now(timezone.utc) - timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        response = client.call(fixture_check, "price-drill-model", "POST", "/api/v1/admin/prices",
                               base_url=drill.a.urls["console"],
                               body={"model": "t10bk-model", "currency": "CNY",
                                     "input_price_micro_per_million": "1000000",
                                     "output_price_micro_per_million": "1000000",
                                     "effective_at": effective_at},
                               session=drill.fx.actors["platform-admin"].session)
        fixture_check.require_status(response, 201, label="演练模型价格")
        drill.model = "t10bk-model"
        fixture_check.status = "PASS"
        fixture_check.finished_at = utcnow()
    except CheckFailure as err:
        fixture_check = Check("BFIX", "fixtures")
        fixture_check.status = "FAIL"
        fixture_check.failure = redact(str(err))
        fixture_check.finished_at = utcnow()
        drill.checks.append(fixture_check)
        log("[BFIX] FAIL — %s" % fixture_check.failure)
        write_summary(lab, drill.checks, drill.findings, {})
        return 1
    drill.checks.append(fixture_check)
    log("[BFIX] PASS — org A=%s org B=%s" % (drill.fx.orgs["A"], drill.fx.orgs["B"]))

    log("")
    log("[SETUP] 写入真实计费流量（%d 个请求）" % cfg["live_requests"])
    executions = drill.drive(fixture_check, cfg["live_requests"], label="setup")
    fixture_check.detail("setup_executions", executions)

    drill.run("BK-01", "备份前事实快照 + 静默/并发两种 pg_dump（RPO 窗口）", drill.bk01)
    drill.run("BK-02", "恢复到全新环境：身份/价格/账本/账户/收据/密文逐项存活", drill.bk02)
    drill.run("BK-03", "主密钥依赖：错误/缺失主密钥的真实行为 + 正确主密钥下解密可用", drill.bk03)
    drill.run("BK-04", "恢复环境对账一致 + 账本不变量", drill.bk04)
    drill.run("BK-05", "RPO / RTO 实测（并声明单节点 PG 不是 HA）", drill.bk05)
    drill.run("RF-01", "Redis 清空（传输丢失）+ 停机 fail-closed + PG 驱动重投", drill.rf01)
    drill.run("RF-02", "恰好一次扣款与对账一致（清空演练之后）", drill.rf02)
    drill.run("RF-03", "长 Redis 停机的边界：收据进入审计 exception 且资金不动", drill.rf03)

    extra = {
        "static_dump": getattr(drill, "dump_static", None),
        "live_dump": getattr(drill, "dump_live", None),
        "restore": getattr(drill, "restore_result", None),
        "money_fingerprint_a": getattr(drill, "fingerprint_a", None),
        "money_fingerprint_b": getattr(drill, "fingerprint_b", None),
        "master_key_behaviour": {"wrong_key": getattr(drill, "wrong_key_behaviour", None),
                                 "missing_key": getattr(drill, "missing_key_behaviour", None)},
        "flush_recovery_seconds": getattr(drill, "flush_recovery_seconds", None),
        "flush_executions": getattr(drill, "flush_executions", None),
        "rpo_probe_executions": getattr(drill, "rpo_executions", None),
    }
    path = write_summary(lab, drill.checks, drill.findings, extra)
    log("")
    log("== 结果 ==")
    failed = 0
    for check in drill.checks:
        log("%-6s %-4s %s%s" % (check.cid, check.status, check.title,
                                (" — " + check.failure) if check.failure else ""))
        if check.status == "FAIL":
            failed += 1
    log("")
    log("checks=%d pass=%d fail=%d findings=%d summary=%s" % (
        len(drill.checks), sum(1 for c in drill.checks if c.status == "PASS"), failed,
        len(drill.findings), path))
    return 1 if failed else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except InfraFailure as err:
        log("INFRA FAILURE: %s" % redact(str(err)))
        sys.exit(2)

DRIVER_EOF

t10_log "运行演练驱动"
set +e
python3 "$driver_path" --lab "$T10_EVIDENCE_DIR/restore-lab.json" 2>&1 | tee "$T10_LOG_DIR/drill-run.log"
driver_rc=${PIPESTATUS[0]}
set -e
rm -rf "$driver_dir"

if [[ "$keep_lab" == "1" ]]; then
  t10_log "--keep：保留两套实验台（端口/容器见 restore-lab.json），不执行 teardown"
  trap - EXIT
else
  t10_log "回收实验台"
fi
exit "$driver_rc"
