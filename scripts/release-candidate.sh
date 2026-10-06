#!/usr/bin/env bash
#
# release-candidate.sh — T12 发布候选的一键可复现验收。
#
# 一次运行完成：构建镜像 → 空库迁移 → bootstrap 平台管理员 → 通过正式开户与加入流程
# 建立两家企业及成员 → 受控 Provider 与资金账户 → 全栈启动
# （Gateway/Control/Console/Metering/mocks/PG/Redis）→ 端到端验收
# （登录、成员建 Key 并流式调用、企业看到收费、余额不足在上游之前被拒、
# 撤销与被移除成员的 Key 被拒、平台异常/审计面、Redis 与 Gateway 故障恢复）
# → 安全/profile 检查 → 证据归档 → 只回收自己的资源。
#
# 约束（与 docs/DEVELOPMENT.md 一致）：
#   * 只操作自己的 compose project：每条 docker compose 命令都显式带
#     --project-name "$RC_PROJECT" 和自己的 -f 文件；down 只 drop 自己的 project。
#     绝不执行无 -p 的 `docker compose down`、`docker system prune`，也不触碰 demo 栈。
#   * 生成的 compose override / Dockerfile 全部写在 work dir（默认
#     data/release-candidate/…，被 .gitignore 的 /data/ 覆盖），不改仓库里的
#     docker-compose.yml 或 Dockerfile。
#   * 口令/令牌/主密钥/API Key secret 只存在于 0700 work dir 的 0600 文件与进程内存；
#     证据一律脱敏后落盘，并有秘密扫描把关。
#
# 用法见 --help。
set -euo pipefail

RC_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RC_ROOT="$(cd "$RC_SCRIPT_DIR/.." && pwd)"
RC_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

RC_COMMAND="run"
RC_PROJECT="${RC_PROJECT:-t12rc-$$}"
RC_WORK_DIR="${RC_WORK_DIR:-}"
RC_EVIDENCE_DIR="${RC_EVIDENCE_DIR:-}"
RC_KEEP=0
RC_SKIP_BUILD=0
RC_WEB_PORT_SET=0
RC_MOCK_BASE_PORT_SET=0
RC_WITH_PREVIEW=0
RC_WEB_PORT="${RC_WEB_PORT:-5173}"
RC_MOCK_BASE_PORT="${RC_MOCK_BASE_PORT:-18180}"
RC_WITH_E2E=0
RC_E2E_FAILED=0
RC_WITH_FAILURE=1
RC_GOPROXY="${RC_GOPROXY-}"
RC_PULL=0
RC_TIMEOUT="${RC_TIMEOUT:-900}"

RC_GATEWAY_PORT="${RC_GATEWAY_PORT:-18090}"
RC_METERING_PORT="${RC_METERING_PORT:-18091}"
RC_CONSOLE_PORT="${RC_CONSOLE_PORT:-18092}"
RC_CONTROL_GRPC_PORT="${RC_CONTROL_GRPC_PORT:-15051}"
RC_CONTROL_HTTP_PORT="${RC_CONTROL_HTTP_PORT:-15052}"
RC_POSTGRES_PORT="${RC_POSTGRES_PORT:-25433}"
RC_REDIS_PORT="${RC_REDIS_PORT:-26379}"

RC_ADMIN_EMAIL="${RC_ADMIN_EMAIL:-rc-admin@example.com}"
RC_ORG_A="${RC_ORG_A:-demo-enterprise-a}"
RC_ORG_B="${RC_ORG_B:-demo-enterprise-b}"
RC_PROBE_MODEL="${RC_PROBE_MODEL:-rc-refuse-probe}"
RC_PRICE_MODEL="${RC_PRICE_MODEL:-default}"
RC_RUN_ID="${RC_RUN_ID:-}"

# 受控上游：primary/backup 是仓库自带的 upstream-mock；rc-unreachable 故意指向
# 不可达地址，用来证明"余额不足在触达上游之前就被拒绝"：如果请求真的被转发，
# 只可能得到 502 upstream_unavailable，不可能得到 402 insufficient_balance。
RC_UNREACHABLE_PROVIDER_URL="${RC_UNREACHABLE_PROVIDER_URL:-http://127.0.0.1:1}"

RC_COMPOSE_FILES=()
RC_WORK_DIR_SET=0
RC_EVIDENCE_DIR_SET=0
RC_TEARDOWN=0
RC_STATE_FILE=""
RC_SECURITY_OK="not-run"

# ---------------------------------------------------------------------------
rc_log()  { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
rc_warn() { printf '[%s] WARNING: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
rc_die()  { printf '[%s] ERROR: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; exit 2; }

rc_usage() {
  cat <<'USAGE'
用法:
  scripts/release-candidate.sh [run] [选项]   构建 → 全栈 → bootstrap/seed → 端到端验收 → 安全检查 → 证据 → 回收
  scripts/release-candidate.sh up   [选项]    只做 构建/启动/bootstrap/seed，保留栈继续运行
  scripts/release-candidate.sh preview [选项] Web + 全栈 + 三个可控 Mock；已有 state 时保留数据更新
  scripts/release-candidate.sh stop [选项]   停止本项目服务，保留数据卷
  scripts/release-candidate.sh demo [选项]    对着已运行的栈走 5 分钟 Demo（操作见 docs/DEVELOPMENT.md）
  scripts/release-candidate.sh e2e  [选项]    对着已运行的栈跑 Playwright 三角色套件
  scripts/release-candidate.sh security [选]  对着已运行的栈只跑安全/profile 检查
  scripts/release-candidate.sh status [选项]  显示本项目容器与证据位置
  scripts/release-candidate.sh down [选项]    只回收本项目资源（含卷），不触碰其它 compose project
  scripts/release-candidate.sh help

选项:
  --project-name NAME    compose project 名（默认 t12rc-<pid>；拒绝 "default"）
  --work-dir DIR         运行期工作目录（默认 data/release-candidate/<UTC 时间戳>-<pid>）
  --evidence-dir DIR     证据目录（默认 data/verification/release-candidate/<时间戳>）
  --keep                 结束后保留容器与卷（run 默认回收）
  --skip-build           复用已存在的镜像，不重新 build
  --pull                 构建前 docker compose pull（基础镜像）
  --with-e2e             run 里追加 Playwright 三角色套件
  --no-failure           跳过 Redis/Gateway 故障恢复验收步骤
  --goproxy URL          构建镜像时注入的 GOPROXY（缺省取 `go env GOPROXY`）
  --timeout SECONDS      单阶段超时（默认 900）
  --admin-email EMAIL    平台管理员邮箱（默认 rc-admin@example.com）
  --run-id ID            运行 ID（默认 UTC 时间戳-pid）
  --port-web N          完整预览网页端口（默认 5173）
  --port-mock-base N    三个可控 Mock 起始端口（默认 18180）
  --port-gateway N       宿主 Gateway 端口（默认 18090，被占用时自动改选）
  --port-console N       宿主 Console 端口（默认 18092，被占用时自动改选）
  --port-metering N      宿主 Metering 端口（默认 18091）
  --port-postgres N      宿主 PostgreSQL 端口（默认 25433）
  --port-redis N         宿主 Redis 端口（默认 26379）
  --port-control-grpc N  宿主 Control gRPC 端口（默认 15051）
  --port-control-http N  宿主 Control HTTP 管理端口（默认 15052）
  -h, --help             显示本帮助

环境变量:
  DOCKER_CONFIG / GOCACHE / npm_config_cache 会被透传给 docker 与子进程。
  受限网络下 docker build 无法直连 proxy.golang.org：脚本按 `go env GOPROXY`
  （或 --goproxy）生成一份只在本次运行使用的 Dockerfile 覆盖，不改仓库 Dockerfile。

退出码: 0 = 全部硬检查通过；1 = 首个硬失败；2 = 用法/环境错误。
USAGE
}

rc_parse_args() {
  if [[ $# -gt 0 ]]; then
    case "$1" in
      run|up|preview|stop|demo|e2e|status|down|security|help) RC_COMMAND="$1"; shift ;;
      -h|--help) RC_COMMAND="help"; shift ;;
      -*) ;;
      *) rc_die "未知命令 $1（见 --help）" ;;
    esac
  fi
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --project-name) RC_PROJECT="${2:-}"; shift 2 ;;
      --project-name=*) RC_PROJECT="${1#*=}"; shift ;;
      --work-dir) RC_WORK_DIR="${2:-}"; RC_WORK_DIR_SET=1; shift 2 ;;
      --work-dir=*) RC_WORK_DIR="${1#*=}"; RC_WORK_DIR_SET=1; shift ;;
      --evidence-dir) RC_EVIDENCE_DIR="${2:-}"; RC_EVIDENCE_DIR_SET=1; shift 2 ;;
      --evidence-dir=*) RC_EVIDENCE_DIR="${1#*=}"; RC_EVIDENCE_DIR_SET=1; shift ;;
      --keep) RC_KEEP=1; shift ;;
      --skip-build) RC_SKIP_BUILD=1; shift ;;
      --pull) RC_PULL=1; shift ;;
      --with-e2e) RC_WITH_E2E=1; shift ;;
      --no-failure) RC_WITH_FAILURE=0; shift ;;
      --goproxy) RC_GOPROXY="${2:-}"; shift 2 ;;
      --goproxy=*) RC_GOPROXY="${1#*=}"; shift ;;
      --timeout) RC_TIMEOUT="${2:-}"; shift 2 ;;
      --timeout=*) RC_TIMEOUT="${1#*=}"; shift ;;
      --admin-email) RC_ADMIN_EMAIL="${2:-}"; shift 2 ;;
      --admin-email=*) RC_ADMIN_EMAIL="${1#*=}"; shift ;;
      --run-id) RC_RUN_ID="${2:-}"; shift 2 ;;
      --run-id=*) RC_RUN_ID="${1#*=}"; shift ;;
      --port-web) RC_WEB_PORT="${2:-}"; RC_WEB_PORT_SET=1; shift 2 ;;
      --port-mock-base) RC_MOCK_BASE_PORT="${2:-}"; RC_MOCK_BASE_PORT_SET=1; shift 2 ;;
      --port-gateway) RC_GATEWAY_PORT="${2:-}"; shift 2 ;;
      --port-console) RC_CONSOLE_PORT="${2:-}"; shift 2 ;;
      --port-metering) RC_METERING_PORT="${2:-}"; shift 2 ;;
      --port-postgres) RC_POSTGRES_PORT="${2:-}"; shift 2 ;;
      --port-redis) RC_REDIS_PORT="${2:-}"; shift 2 ;;
      --port-control-grpc) RC_CONTROL_GRPC_PORT="${2:-}"; shift 2 ;;
      --port-control-http) RC_CONTROL_HTTP_PORT="${2:-}"; shift 2 ;;
      -h|--help) RC_COMMAND="help"; shift ;;
      --) shift; break ;;
      *) rc_die "未知参数 $1（见 --help）" ;;
    esac
  done
  [[ "$RC_COMMAND" != "preview" ]] || RC_WITH_PREVIEW=1
  RC_RUN_ID="${RC_RUN_ID:-$RC_STAMP-$$}"
}

rc_require() {
  local missing=() cmd
  for cmd in "$@"; do command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd"); done
  [[ ${#missing[@]} -eq 0 ]] || rc_die "缺少依赖: ${missing[*]}"
}

rc_free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
}

rc_port_busy() {
  python3 - "$1" <<'PY'
import socket, sys
s = socket.socket(); s.settimeout(0.4)
try:
    s.connect(("127.0.0.1", int(sys.argv[1]))); print("busy")
except OSError:
    print("free")
finally:
    s.close()
PY
}

rc_pick_port() {
  local want="$1" name="$2" got
  if [[ "$(rc_port_busy "$want")" == "free" ]]; then printf '%s' "$want"; return 0; fi
  got="$(rc_free_port)"
  rc_warn "$name 端口 $want 已被占用，改选 $got" >&2
  printf '%s' "$got"
}

rc_now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }
rc_random_secret() { python3 -c 'import secrets; print(secrets.token_urlsafe(24))'; }
rc_random_master_key() { python3 -c 'import base64,os; print(base64.b64encode(os.urandom(32)).decode())'; }

# ---------------------------------------------------------------------------
# compose 包装：永远显式 -p 与本脚本自己的 -f 列表
# ---------------------------------------------------------------------------
rc_compose() {
  local profiles=()
  [[ "$RC_WITH_PREVIEW" != "1" ]] || profiles+=(--profile web --profile preview)
  docker compose --project-name "$RC_PROJECT" "${RC_COMPOSE_FILES[@]}" "${profiles[@]}" "$@"
}

# 把 -f/路径列表原样拼成一条可复制的命令字符串（供 --compose-cmd 与 DEMO 使用）。
# 注意不要写成 `printf ' -f %s' "${RC_COMPOSE_FILES[@]}"`：数组里本来就有 -f 元素，
# printf 会对每个元素重复格式，最终得到 `-f -f -f /path`（docker compose 会去
# stat 一个名叫 -f 的文件）。
rc_compose_command_string() {
  local out="docker compose --project-name $RC_PROJECT" f
  for f in "${RC_COMPOSE_FILES[@]}"; do out+=" $f"; done
  [[ "$RC_WITH_PREVIEW" != "1" ]] || out+=" --profile web --profile preview"
  printf '%s' "$out"
}

rc_compose_run() {
  # 在服务容器里跑一次性命令：注入各自独立的 Control 管理令牌、只读策略令牌与宿主 uid。
  CONTROL_ADMIN_TOKEN="$(cat "$RC_WORK_DIR/secrets/control-admin-token")" \
  METERING_POLICY_TOKEN="$(cat "$RC_WORK_DIR/secrets/metering-policy-token")" \
  HOST_UID="$(id -u)" HOST_GID="$(id -g)" rc_compose "$@"
}

# ---------------------------------------------------------------------------
rc_preflight() {
  rc_require docker python3 curl
  docker info >/dev/null 2>&1 || rc_die "docker daemon 不可用（检查 DOCKER_CONFIG / Docker Desktop）"
  docker compose version >/dev/null 2>&1 || rc_die "docker compose v2 不可用"

  case "$RC_PROJECT" in
    ""|default|llm-gateway-tests|llm-gateway)
      rc_die "拒绝使用 compose project 名 '$RC_PROJECT'：本项目名必须唯一，避免影响其它 compose project" ;;
  esac
  [[ "$RC_PROJECT" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || rc_die "compose project 名只允许小写字母/数字/下划线/连字符: $RC_PROJECT"

  if [[ "$RC_GOPROXY" == "" ]]; then
    if command -v go >/dev/null 2>&1; then
      RC_GOPROXY="$(go env GOPROXY 2>/dev/null || true)"
    fi
    [[ -n "$RC_GOPROXY" ]] || RC_GOPROXY="https://proxy.golang.org,direct"
  fi
}

rc_prepare_dirs() {
  rc_base_compose_file
  if [[ "$RC_WORK_DIR_SET" == "0" ]]; then
    RC_WORK_DIR="$RC_ROOT/data/release-candidate/$RC_RUN_ID"
  fi
  mkdir -p "$RC_WORK_DIR" "$RC_WORK_DIR/secrets" "$RC_WORK_DIR/logs"
  RC_WORK_DIR="$(cd "$RC_WORK_DIR" && pwd)"
  chmod 700 "$RC_WORK_DIR/secrets"

  if [[ "$RC_EVIDENCE_DIR_SET" == "0" ]]; then
    RC_EVIDENCE_DIR="$RC_ROOT/data/verification/release-candidate/$RC_RUN_ID"
  fi
  mkdir -p "$RC_EVIDENCE_DIR"
  RC_EVIDENCE_DIR="$(cd "$RC_EVIDENCE_DIR" && pwd)"
  RC_STATE_FILE="$RC_WORK_DIR/state.env"
}

rc_base_compose_file() {
  # 顺序固定：基础文件 → 构建覆盖（可选）→ 本次运行的 RC 覆盖。
  RC_COMPOSE_FILES=(-f "$RC_ROOT/docker-compose.yml" -f "$RC_ROOT/docker-compose.mock.yml")
}

rc_materialize_drivers() {
  cat > "$RC_WORK_DIR/rc_accept.py" <<'RC_ACCEPT_PY_EOF'
#!/usr/bin/env python3
"""rc_accept.py — T12 release-candidate 的黑盒验收驱动（由 scripts/release-candidate.sh 生成）。

只用 HTTP + python3 标准库：不读数据库、不 import 仓库里的 Go 代码。
所有落盘内容先脱敏：口令、session、CSRF、API Key secret 一律以
"前缀…<redacted:N>" 形式出现，明文只存在于进程内存与 0600 的 identities.json。

phase:
  seed    价格版本、资金账户、自注册企业成员与加入审批，写 identities.json
  accept  端到端验收（S1–S9），写 steps.jsonl / summary.json / responses/
  demo    5 分钟现场演示（打印真实命令与实际观测）

退出码: 0 = 全部硬检查通过；1 = 首个硬失败；2 = 用法/环境错误。
"""

import argparse
import http.cookiejar
import json
import os
import pathlib
import re
import shlex
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# ---------------------------------------------------------------------------
# 基础设施
# ---------------------------------------------------------------------------
SECRETS = []          # [(label, value)] 需要脱敏的原值
REDACT_MIN = 6


def register_secret(label, value):
    if value and isinstance(value, str) and len(value) >= REDACT_MIN:
        SECRETS.append((label, value))


def redact_text(text):
    if text is None:
        return None
    for label, value in SECRETS:
        if value and value in text:
            text = text.replace(value, "%s…<redacted:%d>" % (value[:6], len(value)))
    text = re.sub(r"(sk-[A-Za-z0-9_\-]{6})[A-Za-z0-9_\-]+", r"\1…<redacted>", text)
    text = re.sub(r'("csrf_token"\s*:\s*")[^"]+(")', r"\1<redacted>\2", text)
    text = re.sub(r"(?i)(gw_session|gw_csrf)=[^;\s\"]+", r"\1=<redacted>", text)
    text = re.sub(r"(?i)(x-csrf-token\s*[:=]\s*)[^;\s,\"]+", r"\1<redacted>", text)
    text = re.sub(r"(Bearer\s+)[A-Za-z0-9_\-\.]{8,}", r"\1<redacted>", text)
    return text


def redact_json(node):
    if isinstance(node, dict):
        out = {}
        for k, v in node.items():
            lk = k.lower()
            if lk in ("password", "secret", "raw_key", "csrf_token", "token", "authorization",
                      "cookie", "set-cookie", "admin_token"):
                out[k] = "<redacted>"
            elif lk in ("key_prefix", "prefix") and isinstance(v, str):
                out[k] = v
            else:
                out[k] = redact_json(v)
        return out
    if isinstance(node, list):
        return [redact_json(v) for v in node]
    if isinstance(node, str):
        return redact_text(node)
    return node


class Session:
    """一个带 cookie jar 与 CSRF 头的 Console 会话。"""

    def __init__(self, base):
        self.base = base.rstrip("/")
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = None
        self.email = None

    def request(self, method, path, body=None, headers=None, timeout=30):
        url = path if path.startswith("http") else self.base + path
        data = None
        hdrs = {"Accept": "application/json", "User-Agent": "rc-accept/1.0"}
        if body is not None:
            data = json.dumps(body).encode()
            hdrs["Content-Type"] = "application/json"
        if self.csrf:
            hdrs["X-CSRF-Token"] = self.csrf
        hdrs.update(headers or {})
        req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
        try:
            resp = self.opener.open(req, timeout=timeout)
        except urllib.error.HTTPError as exc:
            resp = exc
        except Exception as exc:  # 连接层失败
            return 0, {"error": str(exc)}, ""
        raw = resp.read()
        text = raw.decode("utf8", "replace")
        try:
            payload = json.loads(text) if text else None
        except Exception:
            payload = text
        headers = dict(resp.headers)
        try:
            cookies = resp.headers.get_all("Set-Cookie")
        except Exception:
            cookies = None
        if cookies:
            headers["Set-Cookie-All"] = " | ".join(cookies)
        return resp.status, headers, payload

    def login(self, email, password):
        status, headers, body = self.request("POST", "/api/v1/auth/login",
                                             {"email": email, "password": password})
        if status == 200 and isinstance(body, dict):
            self.csrf = body.get("csrf_token")
            self.email = email
            register_secret("csrf:" + email, self.csrf)
        return status, headers, body

    def register(self, email, password):
        status, headers, body = self.request("POST", "/api/v1/auth/register",
                                             {"email": email, "password": password})
        if status == 201 and isinstance(body, dict):
            self.csrf = body.get("csrf_token")
            self.email = email
            register_secret("csrf:" + email, self.csrf)
        return status, headers, body


class Recorder:
    def __init__(self, evidence, run_id, phase):
        self.dir = pathlib.Path(evidence)
        self.dir.mkdir(parents=True, exist_ok=True)
        (self.dir / "responses").mkdir(exist_ok=True)
        self.run_id = run_id
        self.phase = phase
        self.steps = []
        self.counter = 0
        self.t0 = time.time()

    def capture(self, name, request, response):
        self.counter += 1
        path = self.dir / "responses" / ("%03d-%s.json" % (self.counter, name))
        path.write_text(json.dumps({"request": redact_json(request),
                                    "response": redact_json(response)},
                                   indent=2, ensure_ascii=False) + "\n")

    def step(self, sid, title, ok, details, observations=None, hard=True):
        record = {
            "id": sid,
            "title": title,
            "ok": bool(ok),
            "hard": bool(hard),
            "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "elapsed_s": round(time.time() - self.t0, 3),
            "details": redact_json(details),
            "observations": redact_json(observations or {}),
        }
        self.steps.append(record)
        with (self.dir / "steps.jsonl").open("a") as fh:
            fh.write(json.dumps(record, ensure_ascii=False) + "\n")
        mark = "PASS" if ok else ("FAIL" if hard else "WARN")
        print("[%s] %s %s" % (mark, sid, title))
        for line in details if isinstance(details, list) else []:
            print("        %s" % redact_text(str(line)))
        return record

    def finish(self, extra=None):
        hard_failures = [s for s in self.steps if not s["ok"] and s["hard"]]
        soft = [s for s in self.steps if not s["ok"] and not s["hard"]]
        summary = {
            "run_id": self.run_id,
            "phase": self.phase,
            "ok": not hard_failures,
            "steps_total": len(self.steps),
            "steps_passed": len([s for s in self.steps if s["ok"]]),
            "hard_failures": [s["id"] for s in hard_failures],
            "soft_findings": [s["id"] for s in soft],
            "duration_s": round(time.time() - self.t0, 3),
            "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }
        summary.update(extra or {})
        path = self.dir / ("summary.json" if self.phase != "demo" else "demo-summary.json")
        path.write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
        print(json.dumps(summary, ensure_ascii=False))
        return 0 if not hard_failures else 1


def gateway_call(gateway, secret, model, stream=True, timeout=60):
    body = json.dumps({
        "model": model,
        "stream": stream,
        "messages": [{"role": "user", "content": "T12 release-candidate acceptance"}],
    }).encode()
    req = urllib.request.Request(
        gateway.rstrip("/") + "/v1/chat/completions", data=body,
        headers={"Authorization": "Bearer " + secret, "Content-Type": "application/json",
                 "Accept": "text/event-stream"}, method="POST")
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as exc:
        resp = exc
    except Exception as exc:
        return {"status": 0, "error": str(exc), "body": "", "done": False, "code": None,
                "headers": {}}
    raw = resp.read()
    text = raw.decode("utf8", "replace")
    code = None
    try:
        code = json.loads(text)["error"]["code"]
    except Exception:
        pass
    headers = {k.lower(): v for k, v in resp.headers.items()}
    return {"status": resp.status, "body": text, "done": "[DONE]" in text, "code": code,
            "headers": headers, "error": None}


def wait_gateway(gateway, secret, model, until, timeout=60, interval=0.25):
    """轮询 Gateway 直到响应满足 until(result)（用于等待策略传播/撤销传播收敛）。"""
    deadline = time.time() + timeout
    last = None
    start = time.time()
    while time.time() < deadline:
        last = gateway_call(gateway, secret, model)
        if until(last):
            return last, round(time.time() - start, 3)
        time.sleep(interval)
    return last, round(time.time() - start, 3)


def until_ok(result):
    return result["status"] == 200


def until_status(*codes):
    return lambda result: result["status"] in codes


def read_identities(path):
    p = pathlib.Path(path)
    if not p.is_file():
        return None
    return json.loads(p.read_text())


def write_identities(path, data):
    p = pathlib.Path(path)
    p.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    os.chmod(p, 0o600)


def load_admin_password(path):
    value = pathlib.Path(path).read_text().strip()
    register_secret("admin-password", value)
    return value


# ---------------------------------------------------------------------------
# seed
# ---------------------------------------------------------------------------
def do_seed(args, rec):
    admin_pw = load_admin_password(args.admin_password_file)
    register_secret("control-admin-token", args.admin_token)
    admin = Session(args.console)
    status, _, body = admin.login(args.admin_email, admin_pw)
    if status != 200:
        rec.step("SEED-0", "平台管理员登录", False,
                 ["login %s -> %s" % (args.admin_email, status)], hard=True)
        return rec.finish()

    identities = read_identities(args.identities)
    if identities is None:
        identities = {
            "run_id": args.run_id,
            "admin": {"email": args.admin_email},
            "org_a": {"id": "", "name": args.org_a},
            "org_b": {"id": "", "name": args.org_b},
            "effective_at": time.strftime("%Y-%m-%dT%H:%M:%SZ",
                                          time.gmtime(int(time.time() / 60) * 60 - 600)),
            "users": {},
            "keys": {},
        }
    admin_user_id = (body.get("user") or {}).get("id")
    check_cursor = None
    platform_accounts = []
    for _ in range(20):
        query = "?type=enterprise&limit=100"
        if check_cursor:
            query += "&cursor=" + urllib.parse.quote(str(check_cursor))
        status, _, page = admin.request("GET", "/api/v1/admin/organizations" + query)
        if status != 200 or not isinstance(page, dict):
            rec.step("SEED-0", "读取平台企业账户", False,
                     ["status=%s body=%s" % (status, page)])
            return rec.finish()
        platform_accounts.extend(page.get("items") or [])
        check_cursor = page.get("next_cursor")
        if not check_cursor:
            break
    if check_cursor:
        rec.step("SEED-0", "平台企业账户目录分页", False,
                 ["达到 2000 条安全读取上限，尚有后续账户"])
        return rec.finish()

    org_ids = {}
    for key, name in (("org_a", args.org_a), ("org_b", args.org_b)):
        existing = next((item for item in platform_accounts if item.get("name") == name), None)
        if existing:
            if existing.get("owner_user_id") != admin_user_id:
                rec.step("SEED-0", "确认企业负责人 %s" % name, False,
                         ["已有同名企业由其他用户负责；请使用新的 RC project/run-id 或重建开发库"])
                return rec.finish()
            organization_id = existing.get("id")
        else:
            status, _, created = admin.request("POST", "/api/v1/admin/organizations",
                                               {"name": name, "owner_email": args.admin_email})
            membership = (created or {}).get("organization") if isinstance(created, dict) else None
            organization_id = membership.get("id") if isinstance(membership, dict) else None
            if status != 201 or not isinstance(organization_id, str) or not organization_id.isdigit():
                rec.step("SEED-0", "平台创建企业 %s" % name, False,
                         ["status=%s body=%s" % (status, created)])
                return rec.finish()
            platform_accounts.append({"id": organization_id, "name": name,
                                      "owner_user_id": admin_user_id, "owner_email": args.admin_email})
        if not isinstance(organization_id, str) or not organization_id.isdigit():
            rec.step("SEED-0", "企业编号 %s" % name, False,
                     ["organization.id=%r 应为 JSON 字符串" % organization_id])
            return rec.finish()
        identities[key] = {"id": organization_id, "name": name}
        org_ids[key] = organization_id
    org_a, org_b = org_ids["org_a"], org_ids["org_b"]
    write_identities(args.identities, identities)

    # 价格版本（同 (model, effective_at) 幂等：重复得到 409）
    published = []
    for model in (args.price_model, args.probe_model):
        status, _, resp = admin.request("POST", "/api/v1/admin/prices", {
            "model": model, "currency": "CNY",
            "input_price_micro_per_million": "2000000",
            "output_price_micro_per_million": "8000000",
            "effective_at": identities["effective_at"]})
        published.append({"model": model, "status": status,
                          "id": (resp or {}).get("id") if isinstance(resp, dict) else None})
        if status not in (200, 201, 409):
            rec.step("SEED-1", "发布价格版本", False,
                     ["%s -> %s %s" % (model, status, resp)])
            return rec.finish()
    rec.capture("prices", {"models": [args.price_model, args.probe_model]},
                {"results": published})

    status, _, catalog = admin.request("GET", "/api/v1/admin/models")
    if status != 200 or not isinstance(catalog, dict):
        rec.step("SEED-1A", "读取平台模型目录", False,
                 ["status=%s body=%s" % (status, catalog)])
        return rec.finish()
    catalog_items = {item.get("model"): bool(item.get("enabled"))
                     for item in catalog.get("items", []) if isinstance(item, dict) and item.get("model")}
    for model in (args.price_model, args.probe_model):
        catalog_items[model] = True
    status, _, catalog = admin.request(
        "PUT", "/api/v1/admin/models",
        {"items": [{"model": model, "enabled": enabled}
                   for model, enabled in sorted(catalog_items.items())]})
    if status != 200:
        rec.step("SEED-1A", "开放 RC 模型", False,
                 ["status=%s body=%s" % (status, catalog)])
        return rec.finish()
    for organization_id in (org_a, org_b):
        status, _, settings = admin.request(
            "PUT", "/api/v1/admin/organizations/%s/models" % organization_id,
            {"model_allowlist": None})
        if status != 200:
            rec.step("SEED-1A", "开放企业模型范围", False,
                     ["org=%s status=%s body=%s" % (organization_id, status, settings)])
            return rec.finish()
    rec.capture("model-catalog", {"models": [args.price_model, args.probe_model]},
                {"enabled_models": [args.price_model, args.probe_model],
                 "account_scope": "inherit platform-enabled models"})

    # 资金账户：给企业 A 充值（Idempotency-Key 固定 → 重放不重复入账）
    adjust_key = "rc-credit-%s" % identities["run_id"]
    payment_reference = "rc-%s-initial-funding" % identities["run_id"]
    status, _, resp = admin.request(
        "POST", "/api/v1/admin/billing/accounts/%s/adjustments" % org_a,
        {"amount_micro": "100000000", "type": "recharge",
         "payment_reference": payment_reference,
         "reason": "T12 release-candidate seed credit"},
        headers={"Idempotency-Key": adjust_key})
    rec.capture("fund-org-a", {"org": org_a, "amount_micro": "100000000"},
                {"status": status, "body": resp})
    if status not in (200, 201):
        rec.step("SEED-2", "企业 A 资金账户入账", False, ["status=%s body=%s" % (status, resp)])
        return rec.finish()
    identities["funding"] = {"idempotency_key": adjust_key, "amount_micro": "100000000",
                             "status": status,
                             "replayed": bool(isinstance(resp, dict) and resp.get("replayed"))}

    # 身份：经公开注册创建个人账户，再通过企业加入申请由负责人审批成为普通成员。
    wanted = [
        ("member_a", args.org_a, "member"),
        ("member_b", args.org_b, "member"),
        ("member_c", args.org_a, "member"),
    ]
    for label, org_name, role in wanted:
        existing = identities["users"].get(label)
        if existing:
            register_secret("pw:" + label, existing.get("password", ""))
            continue
        email = "rc-%s-%s@rc.test" % (label.replace("_", "-"), args.run_id)
        password = "rc-%s-%s" % (label, args.run_id)
        register_secret("pw:" + label, password)
        member = Session(args.console)
        status, _, resp = member.register(email, password)
        if status == 409:
            status, _, resp = member.login(email, password)
        if status != 201 and status != 200:
            rec.step("SEED-3", "注册个人账户 %s" % label, False,
                     ["%s -> %s %s" % (email, status, resp)])
            return rec.finish()
        user_id = (resp.get("user") or {}).get("id") if isinstance(resp, dict) else None
        if not isinstance(user_id, str) or not user_id.isdigit():
            rec.step("SEED-3", "注册个人账户 %s" % label, False,
                     ["注册响应缺少字符串 user.id: %s" % resp])
            return rec.finish()

        org_id = identities[("org_a" if org_name == args.org_a else "org_b")]["id"]
        membership = next((m for m in (resp.get("organizations") or [])
                           if (m.get("organization") or {}).get("id") == org_id
                           and m.get("status") == "active"), None)
        if membership is None:
            status, _, requests = member.request("GET", "/api/v1/me/join-requests")
            if status != 200:
                rec.step("SEED-3", "读取加入申请 %s" % label, False,
                         ["status=%s body=%s" % (status, requests)])
                return rec.finish()
            request = next((item for item in (requests or {}).get("items", [])
                            if item.get("organization_id") == org_id), None)
            if request is None or request.get("status") != "pending":
                status, _, request = member.request(
                    "POST", "/api/v1/orgs/%s/join-requests" % org_id,
                    {"display_name": label.replace("_", " ").title()})
                if status != 201 or not isinstance(request, dict):
                    rec.step("SEED-3", "提交加入申请 %s" % label, False,
                             ["status=%s body=%s" % (status, request)])
                    return rec.finish()
            request_id = request.get("id")
            if not isinstance(request_id, str) or not request_id.isdigit():
                rec.step("SEED-3", "读取加入申请编号 %s" % label, False,
                         ["申请响应缺少字符串 id: %s" % request])
                return rec.finish()
            status, _, decision = admin.request(
                "POST", "/api/v1/orgs/%s/join-requests/%s/decision" % (org_id, request_id),
                {"status": "approved", "monthly_limit_micro": "100000000"})
            if status not in (200, 409):
                rec.step("SEED-3", "审批加入申请 %s" % label, False,
                         ["status=%s body=%s" % (status, decision)])
                return rec.finish()
            # Keep the one-time member session, then confirm the approved account membership.
            status, _, resp = member.request("GET", "/api/v1/me")
            membership = next((m for m in (resp or {}).get("organizations", [])
                               if (m.get("organization") or {}).get("id") == org_id
                               and m.get("status") == "active"), None)
            if status != 200 or membership is None:
                rec.step("SEED-3", "确认加入企业 %s" % label, False,
                         ["status=%s body=%s" % (status, resp)])
                return rec.finish()

        identities["users"][label] = {"email": email, "password": password,
                                      "user_id": user_id, "role": "member", "org": org_name}
        # Persist incrementally so a later seed retry can log these accounts in.
        write_identities(args.identities, identities)

    write_identities(args.identities, identities)
    rec.capture("identities-redacted", {"identities": args.identities},
                {"users": {k: {"email": v["email"], "user_id": v["user_id"], "role": v["role"]}
                           for k, v in identities["users"].items()}})
    rec.step("SEED-OK", "seed 完成（价格 / 资金 / 自注册成员 / 加入审批）", True, [
        "org_a=%s(%s) org_b=%s(%s)" % (args.org_a, org_a, args.org_b, org_b),
        "prices=%s" % ", ".join("%s:%s" % (p["model"], p["status"]) for p in published),
        "funding=%s micro (status=%s replayed=%s)" % (
            identities["funding"]["amount_micro"], identities["funding"]["status"],
            identities["funding"]["replayed"]),
        "users=%s" % ", ".join("%s(%s)" % (k, v["role"]) for k, v in identities["users"].items()),
    ])
    return rec.finish()


# ---------------------------------------------------------------------------
# accept
# ---------------------------------------------------------------------------
def do_accept(args, rec):
    admin_pw = load_admin_password(args.admin_password_file)
    register_secret("control-admin-token", args.admin_token)
    identities = read_identities(args.identities)
    if identities is None:
        rec.step("A0", "identities.json 存在", False, ["先执行 seed（scripts/release-candidate.sh up）"])
        return rec.finish()
    for label, user in identities["users"].items():
        register_secret("pw:" + label, user["password"])
    org_a = identities["org_a"]["id"]
    org_b = identities["org_b"]["id"]

    # ---------------- S1 平台管理员/企业负责人/普通成员登录 ----------------
    roles = {}
    logins = []
    ok = True
    for label, expected_role in (("admin", "owner"), ("member_a", "member"),
                                 ("member_b", "member"), ("member_c", "member")):
        sess = Session(args.console)
        if label == "admin":
            email, password = args.admin_email, admin_pw
        else:
            email, password = identities["users"][label]["email"], identities["users"][label]["password"]
        status, headers, body = sess.login(email, password)
        entry = {"role": label, "email": email, "status": status}
        if status == 200 and isinstance(body, dict):
            entry["user_id"] = body.get("user", {}).get("id")
            entry["platform_admin"] = body.get("user", {}).get("platform_admin")
            entry["organizations"] = [m["organization"]["name"] + ":" + m["role"]
                                      for m in body.get("organizations", [])]
            org_id = org_a if label in ("admin", "member_a", "member_c") else org_b
            membership = next((m for m in body.get("organizations", [])
                               if (m.get("organization") or {}).get("id") == org_id), None)
            entry["enterprise_role"] = membership.get("role") if membership else None
            if membership is None or membership.get("role") != expected_role:
                ok = False
                entry["role_error"] = "expected %s in %s" % (expected_role, org_id)
            raw_cookies = headers.get("Set-Cookie-All") or headers.get("Set-Cookie") or ""
            entry["set_cookie"] = redact_text(raw_cookies)
            entry["cookie_flags"] = {
                "HttpOnly": "HttpOnly" in raw_cookies,
                "SameSite": "SameSite=" in raw_cookies,
                "Secure": "Secure" in raw_cookies,
            }
            roles[label] = sess
        else:
            ok = False
            entry["body"] = body
            roles[label] = sess
        logins.append(entry)
    rec.capture("login-roles", {"roles": ["admin", "member_a", "member_b", "member_c"]},
                {"logins": logins})
    # 负例：错口令必须 401
    bad = Session(args.console)
    bad_status, _, bad_body = bad.login(args.admin_email, "definitely-not-the-password")
    neg_ok = bad_status == 401
    rec.step("S1", "负责人和普通成员登录（+ 错误口令 401 负例）", ok and neg_ok, [
        "platform_admin=%s" % logins[0].get("status"),
        "member_a/%s -> %s" % (identities["users"]["member_a"]["email"], logins[1].get("status")),
        "member_b -> %s" % logins[2].get("status"),
        "member_c -> %s (orgA role 应为 member)" % logins[3].get("status"),
        "错误口令 -> %s（应为 401 %s）" % (bad_status, (bad_body or {}).get("error", {}).get("code")
                                          if isinstance(bad_body, dict) else ""),
        "session cookie flags (member_a): %s" % logins[1].get("cookie_flags"),
    ], observations={"login_negative_status": bad_status})

    if not ok:
        return rec.finish()

    admin, member_a, member_b, member_c = roles["admin"], roles["member_a"], roles["member_b"], roles["member_c"]

    # ---------------- S2 成员创建命名 Key + 流式调用 ----------------
    status, _, key = member_a.request("POST", "/api/v1/orgs/%s/keys" % org_a,
                                      {"name": "release-candidate member A", "operation_id": "rc-%s-member-a-%s" % (args.run_id, secrets.token_hex(6))})
    if status != 201 or not isinstance(key, dict) or not key.get("secret"):
        rec.capture("member-key", {"org": org_a}, {"status": status, "body": key})
        rec.step("S2", "成员创建 API Key", False, ["status=%s body=%s" % (status, key)])
        return rec.finish()
    secret_a = key["secret"]
    key_a_id = key["key"]["key_id"]
    register_secret("key:member_a", secret_a)
    rec.capture("member-key", {"org": org_a, "actor": "member_a"},
                {"status": status, "key": key.get("key"),
                 "secret_returned_once": key.get("secret_returned_once")})

    _, _, pre_account = admin.request("GET", "/api/v1/orgs/%s/billing/account" % org_a)
    accepted, accept_latency = wait_gateway(args.gateway, secret_a, args.price_model, until_ok, timeout=60)
    rec.capture("gateway-call-primary", {"model": args.price_model, "stream": True},
                {"status": accepted["status"], "done": accepted["done"],
                 "request_id": accepted["headers"].get("x-request-id"),
                 "bytes": len(accepted["body"]), "code": accepted["code"]})
    stream_ok = accepted["status"] == 200 and accepted["done"] and "primary" in accepted["body"]
    rec.step("S2", "成员 Key 的流式调用（200 + [DONE] + provider=primary）", stream_ok, [
        "key_id=%s secret=%s" % (key_a_id, redact_text(secret_a)),
        "策略传播：Key 在 %.2fs 内被 Gateway 接受" % accept_latency,
        "调用 -> %s done=%s request_id=%s" % (
            accepted["status"], accepted["done"], accepted["headers"].get("x-request-id")),
    ], observations={"accept_latency_s": accept_latency,
                     "status": accepted["status"], "done": accepted["done"]})

    # ---------------- S3 企业看到收费 ----------------
    # 结算由 dispatcher 异步完成（receipt -> settle）：给一个有界等待窗口，
    # 并把"从调用结束到结算可见"的时延记进证据（对应 SLO：receipt 到可见结算）。
    settle_deadline = time.time() + 30
    settle_started = time.time()
    settle_visible_after = None
    account = entries = usage = None
    status = status2 = status3 = 0
    debit, rows = [], []
    while True:
        status, _, account = admin.request("GET", "/api/v1/orgs/%s/billing/account" % org_a)
        status2, _, entries = admin.request("GET", "/api/v1/orgs/%s/billing/entries?limit=20" % org_a)
        status3, _, usage = admin.request("GET", "/api/v1/orgs/%s/usage?limit=20" % org_a)
        debit = [e for e in (entries or {}).get("items", [])
                 if e.get("kind") == "debit" and "billing_receipt:debit:" in str(e.get("business_id", ""))]
        rows = [u for u in (usage or {}).get("items", []) if u.get("model") == args.price_model]
        if debit and rows:
            settle_visible_after = round(time.time() - settle_started, 3)
            break
        if time.time() > settle_deadline:
            break
        time.sleep(0.25)
    rec.capture("org-a-charge", {"org": org_a, "actor": "enterprise_owner"},
                {"account": account, "entries": entries, "usage": usage,
                 "settle_visible_after_s": settle_visible_after})
    available = None
    try:
        available = int(account["available_micro"])
    except Exception:
        pass
    pre_available = None
    try:
        pre_available = int(pre_account["available_micro"])
    except Exception:
        pass
    charge_ok = (status == 200 and status2 == 200 and status3 == 200
                 and available is not None and pre_available is not None
                 and available < pre_available
                 and len(debit) >= 1 and len(rows) >= 1)
    rec.step("S3", "企业视角看到真实收费（账户/流水/用量）", charge_ok, [
        "GET /billing/account -> %s available_micro %s -> %s（扣费后下降）reserved_micro=%s" % (
            status, pre_available, available, (account or {}).get("reserved_micro")),
        "GET /billing/entries -> %s 结算扣费条目 %d 条（business_id=billing_receipt:debit:…）" % (
            status2, len(debit)),
        "GET /usage -> %s 计量行 %d 条 model=%s provider=%s tokens=%s" % (
            status3, len(rows), args.price_model,
            rows[0].get("provider") if rows else None,
            rows[0].get("total_tokens") if rows else None),
        "企业成员只看到自己的用量；组织汇总不包含任何成员 Key 字段。",
    ], observations={"available_micro_before": pre_available, "available_micro_after": available,
                     "settled_entries": len(debit), "usage_rows": len(rows),
                     "settle_visible_after_s": settle_visible_after})
    status_neg, _, member_usage = member_a.request("GET", "/api/v1/orgs/%s/usage?limit=20" % org_a)
    member_items = (member_usage or {}).get("items", []) if isinstance(member_usage, dict) else []
    member_user_id = identities["users"]["member_a"]["user_id"]
    member_item_ids = [item.get("user_id") for item in member_items if item.get("user_id") is not None]
    member_execution_ids = {item.get("execution_id") for item in member_items}
    org_execution_ids = {item.get("execution_id") for item in rows}
    member_scope_ok = (status_neg == 200 and (member_usage or {}).get("scope") == "self"
                       and all(item_id == member_user_id for item_id in member_item_ids)
                       and member_execution_ids.issubset(org_execution_ids))
    org_items = (usage or {}).get("items", []) if isinstance(usage, dict) else []
    no_key_leak = all("key_id" not in item and "key_name" not in item for item in org_items)
    rec.capture("member-usage-scope", {"actor": "member_a"},
                {"status": status_neg, "scope": (member_usage or {}).get("scope"),
                 "row_count": len(member_items), "organization_rows_contain_key_fields": not no_key_leak})
    rec.step("S3B", "成员只读取本人消费，企业汇总不含 Key 信息",
             member_scope_ok and no_key_leak, [
                 "成员 GET /usage -> %s scope=%s rows=%d；本人 user_id=%s" % (
                     status_neg, (member_usage or {}).get("scope"), len(member_items), member_user_id),
                 "成员行 execution_id 均属于企业汇总；企业明细没有 key_id/key_name 字段。",
             ], observations={"member_usage_scope": (member_usage or {}).get("scope"),
                             "member_rows": len(member_items), "organization_key_fields": not no_key_leak})

    # ---------------- S4 余额不足在上游之前被拒 ----------------
    status, _, key_b = member_b.request("POST", "/api/v1/orgs/%s/keys" % org_b,
                                        {"name": "release-candidate member B", "operation_id": "rc-%s-member-b-%s" % (args.run_id, secrets.token_hex(6))})
    if status != 201 or not isinstance(key_b, dict):
        rec.step("S4", "企业 B 成员创建 Key", False, ["status=%s body=%s" % (status, key_b)])
        return rec.finish()
    secret_b = key_b["secret"]
    register_secret("key:member_b", secret_b)
    wait_gateway(args.gateway, secret_b, args.probe_model, until_status(402), timeout=60)

    refused = gateway_call(args.gateway, secret_b, args.probe_model)
    # 对照：同一个模型（同一个不可达 Provider），企业 A 余额充足 → 502 upstream_unavailable
    control_case = gateway_call(args.gateway, secret_a, args.probe_model)
    rec.capture("insufficient-balance", {"model": args.probe_model, "org": org_b},
                {"status": refused["status"], "code": refused["code"], "body": refused["body"]})
    rec.capture("unreachable-provider-control", {"model": args.probe_model, "org": org_a},
                {"status": control_case["status"], "code": control_case["code"],
                 "body": control_case["body"]})
    status_b, _, usage_b = member_b.request("GET", "/api/v1/orgs/%s/usage?limit=20" % org_b)
    rows_b = (usage_b or {}).get("items", []) if isinstance(usage_b, dict) else []
    refuse_ok = (refused["status"] == 402 and refused["code"] == "insufficient_balance"
                 and control_case["status"] == 502
                 and control_case["code"] == "upstream_unavailable"
                 and len(rows_b) == 0)
    rec.step("S4", "余额不足在触达上游之前被拒（402 insufficient_balance）", refuse_ok, [
        "企业 B（未充值）调用 model=%s -> %s %s" % (args.probe_model, refused["status"], refused["code"]),
        "对照：企业 A（已充值）调用同一 model -> %s %s" % (control_case["status"], control_case["code"]),
        "该 model 路由到的 Provider 故意不可达：若请求真的被转发，只可能得到 502；"
        "得到 402 即证明 admission 在调用上游之前就拒绝了。",
        "企业 B 的用量行数=%d（被拒的请求不产生计量/资金事实）" % len(rows_b),
        "响应体不含余额：%s" % (refused["body"] or "")[:160],
    ], observations={"refused_status": refused["status"], "control_status": control_case["status"],
                     "org_b_usage_rows": len(rows_b)})

    # ---------------- S5 撤销 Key 后拒绝 ----------------
    status, _, revoked = member_a.request(
        "POST", "/api/v1/orgs/%s/keys/%s/revoke" % (org_a, key_a_id), {})
    denied, revoke_latency = wait_gateway(args.gateway, secret_a, args.price_model, lambda r: r["status"] != 200, timeout=60)
    rec.capture("revoked-key", {"key_id": key_a_id},
                {"revoke_status": status, "key_status": (revoked or {}).get("key", {}).get("status"),
                 "gateway_status": denied["status"], "gateway_code": denied["code"]})
    revoke_ok = status in (200, 201) and denied["status"] == 401 and denied["code"] == "unauthorized"
    rec.step("S5", "撤销 Key 后数据面拒绝（401 unauthorized）", revoke_ok, [
        "POST /orgs/%s/keys/%s/revoke -> %s status=%s" % (
            org_a, key_a_id, status, (revoked or {}).get("key", {}).get("status")),
        "撤销传播：%.2fs 后 Gateway 返回 %s %s" % (
            revoke_latency, denied["status"], denied["code"]),
    ], observations={"revocation_propagation_s": revoke_latency,
                     "gateway_status": denied["status"]})

    # ---------------- S6 被移除成员的 Key 被拒 ----------------
    member_c = Session(args.console)
    user_c = identities["users"]["member_c"]
    member_c.login(user_c["email"], user_c["password"])
    status, _, key_c = member_c.request("POST", "/api/v1/orgs/%s/keys" % org_a,
                                        {"name": "release-candidate removed member", "operation_id": "rc-%s-member-c-%s" % (args.run_id, secrets.token_hex(6))})
    if status != 201 or not isinstance(key_c, dict):
        rec.step("S6", "成员 C 创建 Key", False, ["status=%s body=%s" % (status, key_c)])
        return rec.finish()
    secret_c = key_c["secret"]
    register_secret("key:member_c", secret_c)
    wait_gateway(args.gateway, secret_c, args.price_model, until_ok, timeout=60)
    status, _, removed = admin.request(
        "DELETE", "/api/v1/orgs/%s/members/%s" % (org_a, user_c["user_id"]))
    denied_c, remove_latency = wait_gateway(args.gateway, secret_c, args.price_model, lambda r: r["status"] != 200, timeout=60)
    status_uc, _, usage_c = member_c.request("GET", "/api/v1/orgs/%s/usage?limit=5" % org_a)
    rec.capture("removed-member", {"user_id": user_c["user_id"], "key_id": key_c["key"]["key_id"]},
                {"remove_status": status, "gateway_status": denied_c["status"],
                 "gateway_code": denied_c["code"], "member_org_read_status": status_uc})
    remove_ok = (status == 204 and denied_c["status"] == 403 and denied_c["code"] == "forbidden"
                 and status_uc == 404)
    rec.step("S6", "被移除成员的 Key 被拒（403 forbidden），其组织读也 404", remove_ok, [
        "DELETE /orgs/%s/members/%s -> %s" % (org_a, user_c["user_id"], status),
        "移除传播：%.2fs 后 Gateway 返回 %s %s" % (
            remove_latency, denied_c["status"], denied_c["code"]),
        "被移除成员的 GET /usage -> %s not_found（会话已不属于该组织）" % status_uc,
    ], observations={"removal_propagation_s": remove_latency,
                     "gateway_status": denied_c["status"]})

    # ---------------- S7 企业负责人账单/审计与平台策略面 ----------------
    status, _, exceptions = admin.request("GET", "/api/v1/orgs/%s/billing/exceptions" % org_a)
    status2, _, audit = admin.request("GET", "/api/v1/orgs/%s/audit?limit=50" % org_a)
    status3, _, statement = admin.request("GET", "/api/v1/orgs/%s/billing/statements" % org_a)
    review_status, _, review = admin.request("GET", "/api/v1/admin/billing/review-required")
    review_data = review if isinstance(review, dict) else {}
    rec.capture("platform-surfaces", {"org": org_a, "actor": "platform_admin"},
                {"exceptions": exceptions, "audit": audit, "statement": statement, "review": review})
    totals = (exceptions or {}).get("totals", {}) if isinstance(exceptions, dict) else {}
    actions = sorted({item.get("action") for item in (audit or {}).get("items", [])})
    has_settle = any(a and a.startswith("billing.settlement") for a in actions)
    surfaces_ok = (status == 200 and status2 == 200 and status3 == 200
                   and "open" in totals and has_settle and len(actions) >= 3
                   and review_status == 200 and isinstance(review, dict)
                   and review.get("min_age_seconds") == 7800
                   and review.get("sla_seconds") == 7200)
    rec.step("S7", "负责人查看企业资金异常 / 审计，平台查看策略队列", surfaces_ok, [
        "GET /billing/exceptions -> %s totals=%s（%s）" % (
            status, totals, "空队列也是真实事实" if not (exceptions or {}).get("items") else "有异常行"),
        "GET /audit -> %s %d 条，动作=%s" % (status2, len((audit or {}).get("items", [])),
                                            ", ".join(actions[:6])),
        "GET /billing/statements -> %s month=%s debit_micro=%s" % (
            status3, (statement or {}).get("month"),
            ((statement or {}).get("totals") or {}).get("debit_micro")),
        "GET /admin/billing/review-required -> %s window=%s SLA=%s (Metering authoritative policy)" % (review_status, review_data.get("min_age_seconds"), review_data.get("sla_seconds")),
    ], observations={"exception_open": totals.get("open"), "audit_actions": actions[:12],
                     "review_window_seconds": review_data.get("min_age_seconds"),
                     "review_sla_seconds": review_data.get("sla_seconds")})

    # ---------------- S8 Redis 故障：fail-closed 与恢复 ----------------
    if args.with_failure and args.compose_cmd:
        status, _, key_f = member_a.request("POST", "/api/v1/orgs/%s/keys" % org_a,
                                            {"name": "release-candidate redis recovery", "operation_id": "rc-%s-redis-%s" % (args.run_id, secrets.token_hex(6))})
        secret_f = key_f.get("secret") if isinstance(key_f, dict) else None
        if secret_f:
            register_secret("key:failure", secret_f)
            wait_gateway(args.gateway, secret_f, args.price_model, until_ok, timeout=60)
            before = gateway_call(args.gateway, secret_f, args.price_model)
            stop = subprocess.run(shlex.split(args.compose_cmd) + ["stop", "redis"],
                                  capture_output=True, text=True, timeout=180)
            # 等 Redis 真的不可达：网关 fail-closed 只在 Redis 命令报错时生效，
            # 容器 stop 完成与连接池感知之间有间隔，所以这里轮询而不是睡固定秒数。
            outage_started = time.time()
            during = before
            while time.time() - outage_started < 45:
                during = gateway_call(args.gateway, secret_f, args.price_model)
                if during["status"] != 200:
                    break
                time.sleep(0.5)
            outage_observed_after = round(time.time() - outage_started, 3)
            start = subprocess.run(shlex.split(args.compose_cmd) + ["start", "redis"],
                                   capture_output=True, text=True, timeout=180)
            recovered, recover_latency = wait_gateway(args.gateway, secret_f, args.price_model,
                                                      until_ok, timeout=90)
            rec.capture("redis-outage", {"step": "stop redis -> call -> start redis"},
                        {"before": {"status": before["status"]},
                         "during": {"status": during["status"], "code": during["code"]},
                         "after": {"status": recovered["status"], "done": recovered["done"]},
                         "stop_rc": stop.returncode, "stop_stderr": stop.stderr[-300:],
                         "start_rc": start.returncode, "start_stderr": start.stderr[-300:],
                         "outage_observed_after_s": outage_observed_after})
            failure_ok = (before["status"] == 200 and during["status"] == 503
                          and recovered["status"] == 200 and recovered["done"])
            rec.step("S8", "Redis 故障 fail-closed（503）与恢复（200）", failure_ok, [
                "故障前 -> %s" % before["status"],
                "docker compose stop redis 后 %.2fs 内 -> %s %s（LIMITER_ON_REDIS_ERROR=closed：拒绝而不是无额度放行）"
                % (outage_observed_after, during["status"], during["code"]),
                "docker compose start redis 后 %.2fs 内 -> %s done=%s" % (
                    recover_latency, recovered["status"], recovered["done"]),
            ], observations={"before": before["status"], "during": during["status"],
                             "after": recovered["status"],
                             "outage_observed_after_s": outage_observed_after,
                             "recover_after_s": recover_latency})
        else:
            rec.step("S8", "Redis 故障/恢复", False, ["无法为企业 A 再建一个 Key"], hard=False)

    # ---------------- S9 Gateway 重启恢复 ----------------
    if args.with_failure and args.compose_cmd:
        subprocess.run(shlex.split(args.compose_cmd) + ["restart", "gateway"],
                       capture_output=True, text=True)
        deadline = time.time() + 120
        ready = False
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(args.gateway.rstrip("/") + "/readyz", timeout=5) as resp:
                    ready = resp.status == 200
            except Exception:
                ready = False
            if ready:
                break
            time.sleep(1)
        status, _, key_g = member_a.request("POST", "/api/v1/orgs/%s/keys" % org_a,
                                        {"name": "release-candidate gateway restart", "operation_id": "rc-%s-restart-%s" % (args.run_id, secrets.token_hex(6))})
        secret_g = key_g.get("secret") if isinstance(key_g, dict) else None
        if secret_g:
            register_secret("key:restart", secret_g)
        after = wait_gateway(args.gateway, secret_g, args.price_model, until_ok, timeout=90) if secret_g else ({"status": 0, "done": False}, 0)
        rec.capture("gateway-restart", {"step": "docker compose restart gateway"},
                    {"readyz": ready, "after_status": after[0]["status"]})
        rec.step("S9", "Gateway 重启后恢复（readyz 200 + 新调用 200）",
                 bool(ready and after[0]["status"] == 200), [
                     "restart 后 /readyz 在 120s 内 -> %s" % ("200" if ready else "未就绪"),
                     "重启后用新 Key 调用 -> %s done=%s" % (after[0]["status"], after[0]["done"]),
                 ])

    return rec.finish()


# ---------------------------------------------------------------------------
# demo
# ---------------------------------------------------------------------------
DEMO_LINE = "─" * 72


def demo_print(index, title, command, observation):
    print()
    print(DEMO_LINE)
    print("STEP %d · %s" % (index, title))
    print(DEMO_LINE)
    for line in command.strip().splitlines():
        prefix = "      " if line[:1] in (" ", "\t") else "  $ "
        print("%s%s" % (prefix, redact_text(line)))
    print()
    for line in str(observation).strip().splitlines():
        print("  → %s" % redact_text(line))


def do_demo(args, rec):
    admin_pw = load_admin_password(args.admin_password_file)
    identities = read_identities(args.identities)
    if identities is None:
        raise SystemExit("identities.json 不存在：先运行 scripts/release-candidate.sh up")
    for label, user in identities["users"].items():
        register_secret("pw:" + label, user["password"])
    org_a = identities["org_a"]["id"]

    print(DEMO_LINE)
    print("T12 发布候选 · 5 分钟演示（Console=%s Gateway=%s）" % (args.console, args.gateway))
    print(DEMO_LINE)

    # 1 成员登录 + 建命名 Key + 流式调用
    member = Session(args.console)
    member.login(identities["users"]["member_a"]["email"], identities["users"]["member_a"]["password"])
    demo_operation = "rc-demo-%s-member-a-%s" % (args.run_id, secrets.token_hex(6))
    status, _, key = member.request("POST", "/api/v1/orgs/%s/keys" % org_a,
                                    {"name": "release-candidate demo", "operation_id": demo_operation})
    secret = key["secret"] if isinstance(key, dict) else None
    register_secret("key:demo", secret)
    call = wait_gateway(args.gateway, secret, args.price_model, until_ok, timeout=60)[0]
    demo_print(1, "成员登录 → 创建 Key → 流式调用", """
curl -s -c jar -X POST %s/api/v1/auth/login -H 'Content-Type: application/json' \\
  -d '{"email":"%s","password":"<来自 identities.json>"}'
curl -s -b jar -X POST %s/api/v1/orgs/%s/keys -H "X-CSRF-Token: <login.csrf_token>" \
  -H 'Content-Type: application/json' -d '{"name":"release-candidate demo","operation_id":"<unique-operation-id>"}'
curl -sN -X POST %s/v1/chat/completions -H "Authorization: Bearer <key.secret>" \\
  -H 'Accept: text/event-stream' -H 'Content-Type: application/json' \\
  -d '{"model":"%s","stream":true,"messages":[{"role":"user","content":"hello"}]}'
""" % (args.console, member.email, args.console, org_a, args.gateway, args.price_model),
               "login=%s  key_create=%s  key_id=%s\nstreaming 调用 status=%s 收到 data: [DONE]=%s  provider=primary"
               % ("200", status, key["key"]["key_id"], call["status"], call["done"]))
    exec_seen = None
    try:
        body = call["body"]
        for chunk in body.split("\n"):
            if chunk.startswith("data: ") and '"usage"' in chunk:
                exec_seen = json.loads(chunk[6:]).get("id")
    except Exception:
        exec_seen = None

    # 2 企业负责人查费。平台身份本身不授予组织访问；该账号通过 seed 取得真实 owner membership。
    owner = Session(args.console)
    owner.login(args.admin_email, admin_pw)
    _, _, account = owner.request("GET", "/api/v1/orgs/%s/billing/account" % org_a)
    # 结算由 dispatcher 异步完成：有界等待它可见，并记录时延（不要在这之前下结论）。
    settle_started = time.time()
    settle_visible_after = None
    debit = []
    entries = usage = None
    for _ in range(60):
        _, _, entries = owner.request("GET", "/api/v1/orgs/%s/billing/entries?limit=5" % org_a)
        _, _, usage = owner.request("GET", "/api/v1/orgs/%s/usage?limit=5" % org_a)
        debit = [e for e in (entries or {}).get("items", [])
                 if e.get("kind") == "debit"
                 and "billing_receipt:debit:" in str(e.get("business_id", ""))]
        if debit:
            settle_visible_after = round(time.time() - settle_started, 3)
            break
        time.sleep(0.25)
    demo_print(2, "企业负责人查看正式账单", """
curl -s -c owner.jar -X POST %s/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"%s","password":"<来自 secrets/admin-password>"}'
curl -s -b owner.jar %s/api/v1/orgs/%s/billing/account
curl -s -b owner.jar '%s/api/v1/orgs/%s/billing/entries?limit=5'
curl -s -b owner.jar '%s/api/v1/orgs/%s/usage?limit=5'
""" % (args.console, args.admin_email, args.console, org_a, args.console, org_a, args.console, org_a),
               "available_micro=%s reserved_micro=%s\n结算可见延迟 %s s；debit 条目 %d 条（billing_receipt:debit:<execution> 是结算扣费，release 条目是预占释放）\n用量行：model=%s provider=%s tokens=%s execution=%s"
               % (account.get("available_micro"), account.get("reserved_micro"),
                  settle_visible_after, len(debit),
                  (usage.get("items") or [{}])[0].get("model"),
                  (usage.get("items") or [{}])[0].get("provider"),
                  (usage.get("items") or [{}])[0].get("total_tokens"),
                  (usage.get("items") or [{}])[0].get("execution_id")))

    # 3 余额拒绝
    member_b = Session(args.console)
    member_b.login(identities["users"]["member_b"]["email"], identities["users"]["member_b"]["password"])
    org_b = identities["org_b"]["id"]
    _, _, key_b = member_b.request("POST", "/api/v1/orgs/%s/keys" % org_b,
                                  {"name": "release-candidate demo B", "operation_id": "rc-demo-%s-member-b-%s" % (args.run_id, secrets.token_hex(6))})
    secret_b = key_b["secret"]
    register_secret("key:demo-b", secret_b)
    wait_gateway(args.gateway, secret_b, args.probe_model, until_status(402), timeout=60)
    refused = gateway_call(args.gateway, secret_b, args.probe_model)
    control = gateway_call(args.gateway, secret, args.probe_model)
    demo_print(3, "余额不足：在上游之前被拒（402）", """
# 企业 B 没有充值；%s 路由到一个故意不可达的 Provider
curl -s -o /dev/null -w '%%{http_code}\\n' -X POST %s/v1/chat/completions \\
  -H "Authorization: Bearer <企业B的key>" -H 'Content-Type: application/json' \\
  -d '{"model":"%s","stream":true,"messages":[{"role":"user","content":"hello"}]}'
""" % (args.probe_model, args.gateway, args.probe_model),
               "企业 B -> %s %s\n对照（同一模型、企业 A 余额充足）-> %s %s\n请求体里的 Provider 不可达：真打上游只会是 502；得到 402 说明 admission 在调用上游之前就拒绝了。\n响应不含余额数字：%s"
               % (refused["status"], refused["code"], control["status"], control["code"],
                  (refused["body"] or "").strip()[:150]))

    # 4 撤销传播
    t = time.time()
    member.request("POST", "/api/v1/orgs/%s/keys/%s/revoke" % (org_a, key["key"]["key_id"]), {})
    denied, latency = wait_gateway(args.gateway, secret, args.price_model, lambda r: r["status"] != 200, timeout=60)
    demo_print(4, "撤销 Key 的传播", """
curl -s -b jar -X POST %s/api/v1/orgs/%s/keys/%s/revoke -H "X-CSRF-Token: <csrf>" -d '{}'
curl -s -o /dev/null -w '%%{http_code}\\n' -X POST %s/v1/chat/completions \\
  -H "Authorization: Bearer <刚被撤销的key>" -H 'Content-Type: application/json' \\
  -d '{"model":"%s","stream":true,"messages":[{"role":"user","content":"hello"}]}'
""" % (args.console, org_a, key["key"]["key_id"], args.gateway, args.price_model),
               "撤销后 %.2fs 内 Gateway 开始拒绝：%s %s（旧凭据不会被旧快照恢复）"
               % (latency, denied["status"], denied["code"]))

    # 5 故障恢复
    if args.compose_cmd:
        _, _, key_f = member.request("POST", "/api/v1/orgs/%s/keys" % org_a,
                                     {"name": "release-candidate demo recovery", "operation_id": "rc-demo-%s-recovery-%s" % (args.run_id, secrets.token_hex(6))})
        secret_f = key_f["secret"]
        register_secret("key:demo-f", secret_f)
        wait_gateway(args.gateway, secret_f, args.price_model, until_ok, timeout=60)
        before = gateway_call(args.gateway, secret_f, args.price_model)
        subprocess.run(shlex.split(args.compose_cmd) + ["stop", "redis"], capture_output=True)
        time.sleep(3)
        during = gateway_call(args.gateway, secret_f, args.price_model)
        subprocess.run(shlex.split(args.compose_cmd) + ["start", "redis"], capture_output=True)
        after, recover_s = wait_gateway(args.gateway, secret_f, args.price_model, until_ok, timeout=90)
        demo_print(5, "故障恢复：Redis 停机 fail-closed → 恢复", """
%s stop redis
curl -s -o /dev/null -w '%%{http_code}\\n' -X POST %s/v1/chat/completions -H "Authorization: Bearer <key>" \\
  -H 'Content-Type: application/json' -d '{"model":"%s","stream":true,"messages":[{"role":"user","content":"hello"}]}'
%s start redis
# 再发一次同样的请求
""" % (args.compose_cmd, args.gateway, args.price_model, args.compose_cmd),
                   "停机前 -> %s\n停 Redis 后 -> %s %s（LIMITER_ON_REDIS_ERROR=closed：拒绝，而不是无额度放行）\nRedis 恢复后 %.2fs 内 -> %s done=%s"
                   % (before["status"], during["status"], during["code"], recover_s,
                      after["status"], after["done"]))
    else:
        demo_print(5, "故障恢复（跳过）", "# 需要 --compose-cmd", "本次运行未提供 compose 命令")

    # 6 平台审计/异常
    admin = Session(args.console)
    admin.login(args.admin_email, admin_pw)
    _, _, exceptions = admin.request("GET", "/api/v1/orgs/%s/billing/exceptions" % org_a)
    _, _, audit = admin.request("GET", "/api/v1/orgs/%s/audit?limit=20" % org_a)
    actions = sorted({i.get("action") for i in (audit or {}).get("items", [])})
    demo_print(6, "企业负责人的异常队列与审计", """
curl -s -b admin.jar -X POST %s/api/v1/auth/login -H 'Content-Type: application/json' \\
  -d '{"email":"%s","password":"<来自 secrets/admin-password>"}'
curl -s -b admin.jar %s/api/v1/orgs/%s/billing/exceptions
curl -s -b admin.jar '%s/api/v1/orgs/%s/audit?limit=20'
""" % (args.console, args.admin_email, args.console, org_a, args.console, org_a),
               "异常队列 totals=%s（没有异常就是空队列，不是假数据）\n审计 %d 条，动作：%s"
               % ((exceptions or {}).get("totals"), len((audit or {}).get("items", [])),
                  ", ".join(actions[:8])))
    print()
    print(DEMO_LINE)
    print("演示结束，用时 %.1fs" % (time.time() - t0))
    print(DEMO_LINE)
    return rec.finish({"demo_seconds": round(time.time() - t0, 1)})


# ---------------------------------------------------------------------------
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--phase", required=True, choices=["seed", "accept", "demo"])
    ap.add_argument("--console", required=True)
    ap.add_argument("--gateway", required=True)
    ap.add_argument("--control", default="")
    ap.add_argument("--admin-token", default="")
    ap.add_argument("--admin-email", required=True)
    ap.add_argument("--admin-password-file", required=True)
    ap.add_argument("--identities", required=True)
    ap.add_argument("--evidence", required=True)
    ap.add_argument("--run-id", required=True)
    ap.add_argument("--price-model", default="default")
    ap.add_argument("--probe-model", default="rc-refuse-probe")
    ap.add_argument("--org-a", default="demo-enterprise-a")
    ap.add_argument("--org-b", default="demo-enterprise-b")
    ap.add_argument("--with-failure", action="store_true")
    ap.add_argument("--compose-cmd", default="")
    args = ap.parse_args()

    rec = Recorder(args.evidence, args.run_id, args.phase)
    if args.phase == "seed":
        return do_seed(args, rec)
    if args.phase == "accept":
        return do_accept(args, rec)
    return do_demo(args, rec)


if __name__ == "__main__":
    sys.exit(main())
RC_ACCEPT_PY_EOF
  cat > "$RC_WORK_DIR/rc_security.py" <<'RC_SECURITY_PY_EOF'
#!/usr/bin/env python3
"""rc_security.py — T12 发布候选的安全/profile 检查（由 scripts/release-candidate.sh 生成）。

覆盖：
  1. prod profile 的拒绝路径（gateway/control/metering/console 四个二进制）
  2. session cookie 标志（HttpOnly / SameSite / Secure）
  3. 诊断端点暴露面（/metrics、/debug/pprof/、/ledger/*、/reconcile、/gateways）
  4. 默认凭据（仓库常量 + 运行时是否真的接受 demo 默认口令/令牌）
  5. 秘密文件权限（work dir 里的口令/令牌/主密钥必须 0600）

只做只读探测：不修改任何配置，不写数据库。
"""

import argparse
import http.cookiejar
import json
import os
import pathlib
import re
import subprocess
import sys
import urllib.error
import urllib.request

RESULTS = []


def record(name, ok, details, observations=None, hard=True):
    RESULTS.append({"id": name, "ok": bool(ok), "hard": bool(hard),
                    "details": details, "observations": observations or {}})
    print("[%s] %s" % ("PASS" if ok else ("FAIL" if hard else "OBSERVED"), name))
    for line in details:
        print("        %s" % line)


def probe(url, timeout=4):
    req = urllib.request.Request(url, headers={"User-Agent": "rc-security/1.0"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            resp.read(256)
            return resp.status
    except urllib.error.HTTPError as exc:
        return exc.code
    except Exception:
        return 0


def login_raw(console, email, password):
    body = json.dumps({"email": email, "password": password}).encode()
    req = urllib.request.Request(console.rstrip("/") + "/api/v1/auth/login", data=body,
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return resp.status, resp.headers.get_all("Set-Cookie") or []
    except urllib.error.HTTPError as exc:
        return exc.code, exc.headers.get_all("Set-Cookie") or [] if exc.headers else []
    except Exception as exc:
        return 0, [str(exc)]


# ---------------------------------------------------------------------------
def check_prod_refusal(args):
    expected = {
        "gateway": ["GATEWAY_ACCESS_GUARD", "GATEWAY_BILLING", "LIMITER_ON_REDIS_ERROR",
                    "GATEWAY_DIAGNOSTICS_ADDR", "CONTROL_TLS_CA_FILE"],
        "control": ["CONTROL_REQUIRE_POSTGRES", "POSTGRES_DSN", "CONTROL_REQUIRE_ADMIN_AUTH",
                    "CONTROL_ADMIN_TOKEN", "CONTROL_TLS_CERT_FILE/CONTROL_TLS_KEY_FILE"],
        "metering": ["METERING_REQUIRE_POSTGRES", "POSTGRES_DSN", "METERING_ADMIN_TOKEN",
                     "METERING_DIAGNOSTICS_ADDR"],
        "console": ["POSTGRES_DSN", "CONSOLE_DIAGNOSTICS_ADDR", "CONSOLE_SECURE_COOKIES",
                    "CONTROL_ADDR", "CONTROL_TLS_CA_FILE"],
    }
    all_ok = True
    details = []
    observations = {}
    outdir = pathlib.Path(args.evidence) / "security"
    outdir.mkdir(parents=True, exist_ok=True)
    for binary, names in expected.items():
        image = "%s-%s" % (args.project, binary)
        try:
            proc = subprocess.run(
                ["docker", "run", "--rm", "--entrypoint", "/usr/local/bin/" + binary,
                 "-e", "GATEWAY_PROFILE=prod", image],
                capture_output=True, text=True, timeout=120)
            code, output = proc.returncode, proc.stdout + proc.stderr
        except subprocess.TimeoutExpired:
            code, output = -1, "timeout: process did not exit (it may have started serving)"
        except Exception as exc:
            code, output = -2, str(exc)
        (outdir / ("prod-refusal-%s.log" % binary)).write_text(output)
        missing = [n for n in names if n not in output]
        ok = code != 0 and not missing
        all_ok = all_ok and ok
        observations[binary] = {"exit_code": code, "missing_checks": missing,
                                "mentions_preflight": "preflight failed" in output}
        details.append("%s: exit=%s 拒绝启动=%s 缺少的检查项=%s"
                       % (binary, code, code != 0, missing or "无"))
    record("prod_refusal", all_ok, details, observations)


# ---------------------------------------------------------------------------
def check_cookie_flags(args):
    code, cookies = login_raw(args.console, args.admin_email, args.admin_password)
    joined = " | ".join(cookies)
    session = [c for c in cookies if c.startswith("gw_session=")]
    httponly = any("HttpOnly" in c for c in session)
    samesite = any("SameSite=" in c for c in session)
    secure = any("Secure" in c for c in session)
    safe = re.sub(r"(?i)(gw_session|gw_csrf)=[^;,\s]+", lambda m: m.group(1) + "=<redacted>", joined)
    safe = re.sub(r"(?i)(x-csrf-token\s*[:=]\s*)[^;,\s]+", r"\1<redacted>", safe)
    # demo profile 明确关闭 Secure（明文 http 调试）；HttpOnly/SameSite 是硬要求。
    ok = code == 200 and bool(session) and httponly and samesite
    record("cookie_flags", ok, [
        "login -> %s" % code,
        "Set-Cookie: %s" % safe[:220],
        "HttpOnly=%s SameSite=%s Secure=%s（demo profile 下 Secure=false 是显式放宽；"
        "prod 会因 CONSOLE_SECURE_COOKIES!=true 拒绝启动，见 prod_refusal）"
        % (httponly, samesite, secure),
    ], {"httponly": httponly, "samesite": samesite, "secure": secure})


# ---------------------------------------------------------------------------
def check_diagnostics(args):
    targets = [
        ("gateway-biz", "http://127.0.0.1:%s" % args.gateway_port,
         ["/metrics", "/debug/pprof/", "/livez", "/readyz"]),
        ("console-biz", "http://127.0.0.1:%s" % args.console_port,
         ["/metrics", "/debug/pprof/", "/livez", "/readyz"]),
        ("metering-biz", "http://127.0.0.1:%s" % args.metering_port,
         ["/metrics", "/debug/pprof/", "/ledger/count", "/reconcile"]),
        ("control-admin", "http://127.0.0.1:%s" % args.control_http_port,
         ["/metrics", "/gateways", "/healthz"]),
    ]
    exposure = {}
    for label, base, paths in targets:
        for path in paths:
            exposure["%s%s" % (label, path)] = probe(base + path)
    # 硬断言：control 管理面未认证必须是 401（不允许匿名 /gateways）。
    control_anon = exposure.get("control-admin/gateways")
    # 硬断言：console 不把 pprof 暴露在业务监听上。
    console_pprof = exposure.get("console-biz/debug/pprof/")
    ok = control_anon == 401 and console_pprof in (0, 404, 405)
    demo_relaxations = []
    if exposure.get("gateway-biz/debug/pprof/") == 200:
        demo_relaxations.append("gateway /debug/pprof/ 在业务监听 200（demo 放宽；prod 要求 GATEWAY_DIAGNOSTICS_ADDR）")
    if exposure.get("gateway-biz/metrics") == 200:
        demo_relaxations.append("gateway /metrics 在业务监听 200（demo 放宽）")
    if exposure.get("console-biz/metrics") == 200:
        demo_relaxations.append("console /metrics 在业务监听 200（demo 放宽）")
    if exposure.get("metering-biz/ledger/count") == 200:
        demo_relaxations.append("metering /ledger/count 匿名 200（demo 明确放开并打 WARNING）")
    if exposure.get("metering-biz/reconcile") == 200:
        demo_relaxations.append("metering /reconcile 匿名 200（demo 明确放开并打 WARNING）")
    record("diagnostic_exposure", ok, [
        "control /gateways 未带 token -> %s（要求 401）" % control_anon,
        "console /debug/pprof/ -> %s（要求 404/405 或不可达）" % console_pprof,
        "端口暴露矩阵: %s" % json.dumps(exposure, ensure_ascii=False),
        "demo profile 的已知放宽: %s" % ("；".join(demo_relaxations) or "无"),
    ], {"exposure": exposure, "demo_relaxations": demo_relaxations})


# ---------------------------------------------------------------------------
def check_default_credentials(args):
    outdir = pathlib.Path(args.evidence) / "security"
    outdir.mkdir(parents=True, exist_ok=True)
    known = [
        ("control_admin_token_default", re.compile(r"demo-control-token")),
        ("postgres_password_default", re.compile(r"postgres://gateway:gateway@")),
        ("test_postgres_password", re.compile(r"test_only_local")),
        ("grafana_admin_admin", re.compile(r"GF_SECURITY_ADMIN_PASSWORD=admin")),
    ]
    files = subprocess.run(["git", "ls-files"], cwd=args.root,
                           capture_output=True, text=True).stdout.split()
    occurrences = {}
    for rel in files:
        path = pathlib.Path(args.root) / rel
        if not path.is_file() or path.stat().st_size > 2_000_000:
            continue
        try:
            text = path.read_text(errors="ignore")
        except Exception:
            continue
        for name, rx in known:
            n = len(rx.findall(text))
            if n:
                occurrences.setdefault(name, []).append({"file": rel, "count": n})
    (outdir / "default-credentials.json").write_text(
        json.dumps({"tracked_file_occurrences": occurrences}, indent=2, ensure_ascii=False) + "\n")

    # 运行时：RC 栈必须拒绝 demo 默认 control token 与弱口令。
    weak_token_status = probe_token(args.control_http_port, "demo-control-token")
    weak_login_status, _ = login_raw(args.console, args.admin_email, "admin")
    weak_login_status2, _ = login_raw(args.console, args.admin_email, "changeme")
    ok = weak_token_status in (401, 403) and weak_login_status == 401 and weak_login_status2 == 401
    record("default_credentials", ok, [
        "仓库中已知默认常量出现位置: %s" % (
            "; ".join("%s×%d(%s)" % (k, sum(o["count"] for o in v), ",".join(o["file"] for o in v[:3]))
                      for k, v in occurrences.items()) or "无"),
        "运行中的 Control 管理面用 'demo-control-token' -> %s（要求 401/403；RC 每次生成随机 token）" % weak_token_status,
        "Console 用弱口令 'admin' -> %s，'changeme' -> %s（要求 401）" % (weak_login_status, weak_login_status2),
    ], {"weak_token_status": weak_token_status,
        "weak_login_status": [weak_login_status, weak_login_status2],
        "tracked_occurrences": {k: len(v) for k, v in occurrences.items()}})


def probe_token(port, token):
    req = urllib.request.Request("http://127.0.0.1:%s/gateways" % port,
                                 headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            resp.read(64)
            return resp.status
    except urllib.error.HTTPError as exc:
        return exc.code
    except Exception:
        return 0


# ---------------------------------------------------------------------------
def check_secret_files(args):
    sd = pathlib.Path(args.work_dir) / "secrets"
    bad = []
    modes = {}
    for path in sorted(sd.rglob("*")):
        mode = oct(path.stat().st_mode & 0o777)
        modes[str(path.relative_to(sd))] = mode
        if path.is_dir():
            if mode != "0o700":
                bad.append("%s dir mode %s" % (path.name, mode))
        elif mode not in ("0o600", "0o400"):
            bad.append("%s mode %s" % (path.name, mode))
    record("secret_file_permissions", not bad, [
        "work/secrets 权限: %s" % json.dumps(modes, ensure_ascii=False),
        "违规项: %s" % (bad or "无"),
    ], {"modes": modes, "violations": bad})


# ---------------------------------------------------------------------------
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--evidence", required=True)
    ap.add_argument("--work-dir", required=True)
    ap.add_argument("--root", required=True)
    ap.add_argument("--project", required=True)
    ap.add_argument("--console", required=True)
    ap.add_argument("--control-http-port", required=True)
    ap.add_argument("--gateway-port", required=True)
    ap.add_argument("--metering-port", required=True)
    ap.add_argument("--console-port", required=True)
    ap.add_argument("--admin-email", required=True)
    ap.add_argument("--admin-password-file", required=True)
    args = ap.parse_args()
    args.admin_password = pathlib.Path(args.admin_password_file).read_text().strip()

    outdir = pathlib.Path(args.evidence) / "security"
    outdir.mkdir(parents=True, exist_ok=True)

    check_prod_refusal(args)
    check_cookie_flags(args)
    check_diagnostics(args)
    check_default_credentials(args)
    check_secret_files(args)

    hard = [r for r in RESULTS if not r["ok"] and r["hard"]]
    summary = {"ok": not hard, "checks": RESULTS,
               "failed": [r["id"] for r in hard]}
    (outdir / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
    print(json.dumps({"security_ok": summary["ok"], "failed": summary["failed"]},
                     ensure_ascii=False))
    return 0 if not hard else 1


if __name__ == "__main__":
    sys.exit(main())
RC_SECURITY_PY_EOF
  chmod 600 "$RC_WORK_DIR/rc_accept.py" "$RC_WORK_DIR/rc_security.py"
  cat > "$RC_WORK_DIR/redact_state.py" <<'RC_REDACT_PY_EOF'
#!/usr/bin/env python3
"""把 Playwright 的 .state.json 脱敏后再归档（口令与会话 Key 不落进证据）。"""
import json, pathlib, sys

src, dst = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
if not src.is_file():
    raise SystemExit(0)


def scrub(node):
    if isinstance(node, dict):
        return {k: ("<redacted>" if k in ("password", "gatewayKey") else scrub(v))
                for k, v in node.items()}
    if isinstance(node, list):
        return [scrub(v) for v in node]
    return node


dst.parent.mkdir(parents=True, exist_ok=True)
dst.write_text(json.dumps(scrub(json.loads(src.read_text())), indent=2,
                          ensure_ascii=False) + "\n")
RC_REDACT_PY_EOF
  chmod 600 "$RC_WORK_DIR/redact_state.py"
  python3 -c "import ast,sys;ast.parse(open(sys.argv[1]).read());ast.parse(open(sys.argv[2]).read())" \
    "$RC_WORK_DIR/rc_accept.py" "$RC_WORK_DIR/rc_security.py" \
    || rc_die "内置驱动语法检查失败"
}

rc_secrets_init() {
  local sd="$RC_WORK_DIR/secrets"
  [[ -r "$sd/admin-password" ]] || rc_random_secret > "$sd/admin-password"
  [[ -r "$sd/control-admin-token" ]] || rc_random_secret > "$sd/control-admin-token"
  [[ -r "$sd/metering-policy-token" ]] || rc_random_secret > "$sd/metering-policy-token"
  [[ -r "$sd/provider-credential-master-key" ]] || rc_random_master_key > "$sd/provider-credential-master-key"
  chmod 700 "$sd"
  chmod 600 "$sd"/*
}

# 生成只属于本次运行的 compose 覆盖：端口、资金数据面、隔离的 Provider 主密钥。
rc_write_compose_files() {
  if [[ "$RC_GOPROXY" != "https://proxy.golang.org,direct" ]]; then
    python3 - "$RC_ROOT/Dockerfile" "$RC_WORK_DIR/Dockerfile.rc" "$RC_GOPROXY" \
      "$RC_ROOT" "$RC_WORK_DIR/compose.build.yml" "$RC_WITH_PREVIEW" <<'PY'
import sys, pathlib
src, dst, goproxy, root, buildyml, preview = sys.argv[1:7]
out = []
for line in pathlib.Path(src).read_text().splitlines():
    out.append(line)
    if line.strip().startswith("FROM ") and "AS build" in line:
        out.append("ARG GOPROXY=%s" % goproxy)
        out.append("ENV GOPROXY=${GOPROXY}")
pathlib.Path(dst).write_text("\n".join(out) + "\n")
lines = ["# 由 scripts/release-candidate.sh 生成：只为构建注入 GOPROXY，不改仓库 Dockerfile。",
         "services:"]
services = ["gateway", "control", "console", "metering", "migrate", "mock-primary", "mock-backup"]
if preview == "1": services += ["mock-fixed", "mock-echo", "mock-flaky"]
for svc in services:
    lines += ["  %s:" % svc, "    build:", "      context: %s" % root,
              "      dockerfile: %s" % dst, "      args:", "        GOPROXY: %s" % goproxy]
pathlib.Path(buildyml).write_text("\n".join(lines) + "\n")
PY
    RC_COMPOSE_FILES+=(-f "$RC_WORK_DIR/compose.build.yml")
  fi

  python3 - "$RC_WORK_DIR/compose.rc.yml" \
    "$RC_GATEWAY_PORT" "$RC_METERING_PORT" "$RC_CONSOLE_PORT" \
    "$RC_CONTROL_GRPC_PORT" "$RC_CONTROL_HTTP_PORT" \
    "$RC_POSTGRES_PORT" "$RC_REDIS_PORT" "$RC_WORK_DIR/secrets/provider-credential-master-key" <<'PY'
import sys, pathlib
(dst, gport, mport, cport, cgport, chport, pgport, rport, masterkey) = sys.argv[1:10]
pathlib.Path(dst).write_text(f"""# 由 scripts/release-candidate.sh 生成，仅用于本次发布候选运行。
# ports 用 !override（Compose >= 2.24）；否则序列会与基础文件相加。
services:
  gateway:
    ports: !override
      - "127.0.0.1:{gport}:8090"
    environment:
      GATEWAY_ACCESS_GUARD: "pg"
      GATEWAY_BILLING: "pg"
      POSTGRES_DSN: postgres://gateway:gateway@postgres:5432/llm_gateway?sslmode=disable
  metering:
    ports: !override
      - "127.0.0.1:{mport}:8091"
    environment:
      # Only Metering configures these bounds; Console must read its effective policy.
      METERING_RECOVERY_MAX_EXECUTION: 2h
      METERING_RECOVERY_GRACE: 10m
      METERING_RECOVERY_SLA: 2h
  console:
    ports: !override
      - "127.0.0.1:{cport}:8092"
    environment:
      # Playwright 三角色套件一次运行会建立多个身份；默认 burst=5 是给人手输入用的。
      CONSOLE_LOGIN_BURST: "50"
      CONSOLE_LOGIN_RATE: "60"
      # 策略/撤销传播：验收会等它收敛。
      CONSOLE_PUBLICATION_INTERVAL: 2s
  control:
    ports: !override
      - "127.0.0.1:{cgport}:50051"
      - "127.0.0.1:{chport}:50052"
    volumes:
      # 本次运行专属的 Provider 凭据主密钥（0600），不使用 demo 栈的 data/ 主密钥。
      - {masterkey}:/run/secrets/provider-credential-master-key:ro
  postgres:
    ports: !override
      - "127.0.0.1:{pgport}:5432"
  redis:
    ports: !override
      - "127.0.0.1:{rport}:6379"
""")
PY
  RC_COMPOSE_FILES+=(-f "$RC_WORK_DIR/compose.rc.yml")

  if [[ "$RC_WITH_PREVIEW" == "1" ]]; then
    python3 - "$RC_WORK_DIR/compose.preview.ports.yml" "$RC_WEB_PORT" "$RC_MOCK_BASE_PORT" <<'PY'
import pathlib, sys
path, web, base = sys.argv[1:]
web, base = int(web), int(base)
if not 1024 <= web <= 65535 or not 1024 <= base <= 65533:
    raise SystemExit("preview ports out of range")
lines = ["services:", "  web:", "    ports: !override", f'      - "127.0.0.1:{web}:8080"']
for i, service in enumerate(("mock-fixed", "mock-echo", "mock-flaky")):
    lines += [f"  {service}:", "    ports: !override", f'      - "127.0.0.1:{base+i}:18080"']
pathlib.Path(path).write_text("\n".join(lines)+"\n")
PY
    RC_COMPOSE_FILES+=(-f "$RC_WORK_DIR/compose.preview.ports.yml")
  fi

  rc_compose config >"$RC_WORK_DIR/logs/compose.config.yml" 2>&1 \
    || rc_die "compose 配置合并失败（需要 docker compose >= 2.24 才支持 !override）：见 $RC_WORK_DIR/logs/compose.config.yml"
  rc_compose config --format json >"$RC_WORK_DIR/logs/compose.config.raw.json" 2>&1 || true
  # 归档到证据的版本必须脱敏：环境变量的值里可能有令牌。
  python3 - "$RC_WORK_DIR/logs/compose.config.raw.json" "$RC_WORK_DIR/logs/compose.config.json" <<'PY'
import json, pathlib, re, sys
src, dst = sys.argv[1:3]
try:
    data = json.loads(pathlib.Path(src).read_text())
except Exception:
    pathlib.Path(dst).write_text("{}\n"); raise SystemExit(0)
secret_key = re.compile(r"(TOKEN|PASSWORD|SECRET|MASTER_KEY|_DSN$)", re.I)
for name, svc in (data.get("services") or {}).items():
    env = svc.get("environment") or {}
    svc["environment"] = {k: ("<redacted>" if secret_key.search(k) else v) for k, v in env.items()}
pathlib.Path(dst).write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
PY
}

rc_write_state() {
  # 只把路径写进 state：-f 由恢复时补，避免把 -f 当路径再包一层。
  RC_COMPOSE_PATHS=()
  local _f
  for _f in "${RC_COMPOSE_FILES[@]}"; do
    [[ "$_f" == "-f" ]] && continue
    RC_COMPOSE_PATHS+=("$_f")
  done
  cat > "$RC_STATE_FILE" <<EOF
RC_WITH_PREVIEW=$RC_WITH_PREVIEW
RC_WEB_PORT=$RC_WEB_PORT
RC_MOCK_BASE_PORT=$RC_MOCK_BASE_PORT
RC_RUN_ID=$RC_RUN_ID
RC_PROJECT=$RC_PROJECT
RC_WORK_DIR=$RC_WORK_DIR
RC_EVIDENCE_DIR=$RC_EVIDENCE_DIR
RC_GATEWAY_PORT=$RC_GATEWAY_PORT
RC_METERING_PORT=$RC_METERING_PORT
RC_CONSOLE_PORT=$RC_CONSOLE_PORT
RC_CONTROL_GRPC_PORT=$RC_CONTROL_GRPC_PORT
RC_CONTROL_HTTP_PORT=$RC_CONTROL_HTTP_PORT
RC_POSTGRES_PORT=$RC_POSTGRES_PORT
RC_REDIS_PORT=$RC_REDIS_PORT
RC_ADMIN_EMAIL=$RC_ADMIN_EMAIL
RC_ORG_A=$RC_ORG_A
RC_ORG_B=$RC_ORG_B
RC_PRICE_MODEL=$RC_PRICE_MODEL
RC_PROBE_MODEL=$RC_PROBE_MODEL
RC_STARTED_AT=$RC_STARTED_AT
COMPOSE_PATHS='${RC_COMPOSE_PATHS[*]}'
EOF
  chmod 600 "$RC_STATE_FILE"
  mkdir -p "$RC_ROOT/data/release-candidate"
  ln -sfn "$RC_WORK_DIR" "$RC_ROOT/data/release-candidate/current" 2>/dev/null || true
}

rc_load_state() {
  [[ -n "$RC_WORK_DIR" ]] || RC_WORK_DIR="$RC_ROOT/data/release-candidate/current"
  RC_STATE_FILE="$RC_WORK_DIR/state.env"
  [[ -r "$RC_STATE_FILE" ]] || rc_die "找不到运行状态 ${RC_STATE_FILE}（先执行 up 或 run）"
  # shellcheck disable=SC1090
  source "$RC_STATE_FILE"
  RC_WORK_DIR="$(cd "$RC_WORK_DIR" && pwd)"
  RC_EVIDENCE_DIR="$(cd "$RC_EVIDENCE_DIR" && pwd)"
  RC_COMPOSE_FILES=()
  local f
  for f in $COMPOSE_PATHS; do RC_COMPOSE_FILES+=(-f "$f"); done
  # Old local runs predate the separate mock file; restore complete test-only definitions.
  if [[ "$COMPOSE_PATHS" != *"docker-compose.mock.yml"* ]]; then
    RC_COMPOSE_FILES=(-f "$RC_ROOT/docker-compose.yml" -f "$RC_ROOT/docker-compose.mock.yml")
    for f in $COMPOSE_PATHS; do
      [[ "$f" == "$RC_ROOT/docker-compose.yml" || "$f" == *"docker-compose.preview.yml" ]] || RC_COMPOSE_FILES+=(-f "$f")
    done
  fi
  [[ ${#RC_COMPOSE_FILES[@]} -gt 0 ]] || rc_die "state.env 里没有 compose 文件列表"
}

rc_write_run_env() {
  {
    printf 'run_id=%s\n' "$RC_RUN_ID"
    printf 'started_at=%s\n' "$RC_STARTED_AT"
    printf 'generated_at=%s\n' "$(rc_now_utc)"
    printf 'root=%s\n' "$RC_ROOT"
    printf 'compose_project=%s\n' "$RC_PROJECT"
    printf 'git_head=%s\n' "$(cd "$RC_ROOT" && git rev-parse HEAD 2>/dev/null || echo unknown)"
    printf 'git_branch=%s\n' "$(cd "$RC_ROOT" && git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
    printf 'git_dirty_files=%s\n' "$(cd "$RC_ROOT" && git status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
    printf 'go_version=%s\n' "$(go version 2>/dev/null || echo 'not installed')"
    printf 'docker_server=%s\n' "$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown)"
    printf 'compose_version=%s\n' "$(docker compose version --short 2>/dev/null || echo unknown)"
    printf 'node_version=%s\n' "$(node --version 2>/dev/null || echo 'not installed')"
    printf 'goproxy=%s\n' "$RC_GOPROXY"
    printf 'ports gateway=%s console=%s metering=%s control_grpc=%s control_http=%s postgres=%s redis=%s\n' \
      "$RC_GATEWAY_PORT" "$RC_CONSOLE_PORT" "$RC_METERING_PORT" "$RC_CONTROL_GRPC_PORT" \
      "$RC_CONTROL_HTTP_PORT" "$RC_POSTGRES_PORT" "$RC_REDIS_PORT"
  } > "$RC_WORK_DIR/run.env"
  cp "$RC_WORK_DIR/run.env" "$RC_EVIDENCE_DIR/run.env"
}

# ---------------------------------------------------------------------------
# 隔离护栏：记录其它 compose project 与容器，结束后要求集合不变
# ---------------------------------------------------------------------------
rc_isolation_snapshot() {
  local out="$1"
  {
    echo "## docker compose ls (all projects)"
    docker compose ls -a 2>/dev/null || true
    echo "## containers of other projects"
    docker ps -a --format '{{.Label "com.docker.compose.project"}}\t{{.Names}}\t{{.Status}}' 2>/dev/null \
      | awk -F'\t' -v p="$RC_PROJECT" '$1 != p' || true
    echo "## volumes of other projects"
    docker volume ls --format '{{.Label "com.docker.compose.project"}}\t{{.Name}}' 2>/dev/null \
      | awk -F'\t' -v p="$RC_PROJECT" '$1 != p' || true
  } > "$out"
}

rc_isolation_diff() {
  local b="$1" a="$2"
  # 只比较其它 compose project 的容器名/卷名。
  #   消失 = 我们破坏了别人的资源 → 硬失败；
  #   新增 = 并行运行的别人新建的资源 → 只记录（不是本脚本造成的）。
  local fb fa
  fb="$(mktemp)"; fa="$(mktemp)"
  awk -F'\t' 'NF>=2 {print $2}' "$b" | sort -u > "$fb"
  awk -F'\t' 'NF>=2 {print $2}' "$a" | sort -u > "$fa"
  local removed added
  removed="$(comm -23 "$fb" "$fa")"
  added="$(comm -13 "$fb" "$fa")"
  {
    echo "## removed (属于其它 project，运行前有、运行后没有)"
    printf '%s\n' "$removed"
    echo "## added (属于其它 project，运行后新增；并行运行的其它任务造成)"
    printf '%s\n' "$added"
  } > "$RC_WORK_DIR/logs/isolation.diff"
  cp "$RC_WORK_DIR/logs/isolation.diff" "$RC_EVIDENCE_DIR/stack/isolation.diff" 2>/dev/null || true
  rm -f "$fb" "$fa"
  if [[ -n "$added" ]]; then
    rc_log "    隔离观察：其它 project 在本次运行期间新增了 $(printf '%s\n' "$added" | wc -l | tr -d ' ') 个容器/卷（非本脚本创建）"
  fi
  if [[ -n "$removed" ]]; then
    rc_warn "隔离校验失败：本次运行期间其它 compose project 丢掉了资源："
    printf '%s\n' "$removed" >&2
    return 1
  fi
  rc_log "==> 隔离校验：其它 compose project 的资源没有被删除"
  return 0
}

# ---------------------------------------------------------------------------
# 栈生命周期
# ---------------------------------------------------------------------------
rc_build() {
  [[ "$RC_SKIP_BUILD" == "1" ]] && { rc_log "跳过镜像构建（--skip-build）"; return 0; }
  [[ "$RC_PULL" == "1" ]] && { rc_compose pull --ignore-buildable >>"$RC_WORK_DIR/logs/build.log" 2>&1 || true; }
  rc_log "==> 构建镜像（首次可能需要数分钟；GOPROXY=${RC_GOPROXY}）"
  local t0; t0="$(date +%s)"
  if ! rc_compose build >>"$RC_WORK_DIR/logs/build.log" 2>&1; then
    rc_warn "docker compose build 失败；tail:"
    tail -30 "$RC_WORK_DIR/logs/build.log" >&2
    rc_die "镜像构建失败"
  fi
  rc_log "    构建完成，用时 $(( $(date +%s) - t0 ))s"
}

rc_up() {
  rc_log "==> 启动全栈（project=${RC_PROJECT}，profile=demo-mocks）"
  if ! rc_compose_run --profile demo-mocks up -d --wait >>"$RC_WORK_DIR/logs/stack.log" 2>&1; then
    tail -40 "$RC_WORK_DIR/logs/stack.log" >&2
    rc_die "栈启动失败"
  fi
  rc_compose ps --format '    {{.Service}}\t{{.Status}}' || true
}

rc_assert_migrations() {
  local out
  out="$(rc_compose_run run --rm -T --no-deps migrate version 2>&1 || true)"
  printf '%s\n' "$out" > "$RC_WORK_DIR/logs/migrate-version.txt"
  printf '%s' "$out" | grep -Eq 'dirty[=: ]+false' \
    || rc_warn "migrate version 输出未包含 dirty=false：$out"
  rc_log "==> 迁移版本：$(printf '%s' "$out" | tr '\n' ' ')"
}

rc_container() {
  local svc="$1" name
  name="$(docker ps -a --filter "label=com.docker.compose.project=$RC_PROJECT" \
            --filter "label=com.docker.compose.service=$svc" --format '{{.Names}}' | head -1)"
  [[ -n "$name" ]] || rc_die "找不到 project=$RC_PROJECT 的 $svc 容器"
  printf '%s' "$name"
}

rc_bootstrap() {
  rc_log "==> 配置受控 Provider 与路由（Control 管理面）"
  python3 - "$RC_CONTROL_HTTP_PORT" "$(cat "$RC_WORK_DIR/secrets/control-admin-token")" \
    "$RC_PROBE_MODEL" "$RC_UNREACHABLE_PROVIDER_URL" >"$RC_WORK_DIR/logs/control-config.json" <<'PY'
import json, sys, urllib.request
port, token, probe_model, unreachable = sys.argv[1:5]
payload = {
    "providers": {
        "primary": {"id": "primary", "protocol": "openai-compatible", "url": "http://mock-primary:18080"},
        "backup": {"id": "backup", "protocol": "openai-compatible", "url": "http://mock-backup:18081"},
        "rc-unreachable": {"id": "rc-unreachable", "protocol": "openai-compatible", "url": unreachable},
    },
    "routes": {"default": ["primary", "backup"], probe_model: ["rc-unreachable"]},
}
req = urllib.request.Request("http://127.0.0.1:%s/config" % port, data=json.dumps(payload).encode(),
                             headers={"Authorization": "Bearer " + token,
                                      "Content-Type": "application/json"}, method="POST")
with urllib.request.urlopen(req, timeout=30) as resp:
    print(resp.read().decode())
PY
  rc_log "    desired_version=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("desired_version"))' "$RC_WORK_DIR/logs/control-config.json" 2>/dev/null || echo '?')"

  rc_log "==> bootstrap 平台管理员与个人账户（不注入演示企业/旧角色）"
  if ! printf '%s' "$(cat "$RC_WORK_DIR/secrets/admin-password")" | \
      rc_compose_run run --rm -T --no-deps console bootstrap \
        --admin-email "$RC_ADMIN_EMAIL" \
        --admin-password-file /dev/stdin \
        >>"$RC_WORK_DIR/logs/bootstrap.log" 2>&1; then
    tail -30 "$RC_WORK_DIR/logs/bootstrap.log" >&2
    rc_die "console bootstrap 失败"
  fi
  grep -E 'console bootstrap: ' "$RC_WORK_DIR/logs/bootstrap.log" | sed 's/^/    /' || true
}

rc_wait_gateway_applied() {
  local deadline; deadline=$(( $(date +%s) + 60 ))
  while [[ $(date +%s) -lt $deadline ]]; do
    if python3 - "$RC_CONTROL_HTTP_PORT" "$(cat "$RC_WORK_DIR/secrets/control-admin-token")" <<'PY'
import json, sys, urllib.request
port, token = sys.argv[1], sys.argv[2]
req = urllib.request.Request("http://127.0.0.1:%s/gateways" % port,
                             headers={"Authorization": "Bearer " + token})
with urllib.request.urlopen(req, timeout=10) as resp:
    data = json.load(resp)
instances = data.get("instances") or []
sys.exit(0 if [i for i in instances if i.get("state") == "applied" and i.get("in_sync")] else 1)
PY
    then return 0; fi
    sleep 1
  done
  rc_die "Gateway 未在 60s 内报告 applied/in_sync"
}

rc_accept() {
  local phase="$1"; shift
  python3 "$RC_WORK_DIR/rc_accept.py" \
    --phase "$phase" \
    --console "http://127.0.0.1:$RC_CONSOLE_PORT" \
    --gateway "http://127.0.0.1:$RC_GATEWAY_PORT" \
    --control "http://127.0.0.1:$RC_CONTROL_HTTP_PORT" \
    --admin-token="$(cat "$RC_WORK_DIR/secrets/control-admin-token")" \
    --admin-email "$RC_ADMIN_EMAIL" \
    --admin-password-file "$RC_WORK_DIR/secrets/admin-password" \
    --identities "$RC_WORK_DIR/secrets/identities.json" \
    --evidence "$RC_EVIDENCE_DIR" \
    --run-id "$RC_RUN_ID" \
    --price-model "$RC_PRICE_MODEL" --probe-model "$RC_PROBE_MODEL" \
    --org-a "$RC_ORG_A" --org-b "$RC_ORG_B" \
    --compose-cmd "$(rc_compose_command_string)" \
    "$@" 2>&1 | tee -a "$RC_WORK_DIR/logs/acceptance.log"
  local -a pipeline_status=("${PIPESTATUS[@]}")
  local driver_exit="${pipeline_status[0]:-1}"
  local tee_exit="${pipeline_status[1]:-1}"
  local command_exit="$driver_exit"
  [[ "$command_exit" -ne 0 ]] || command_exit="$tee_exit"
  printf '{"phase":"%s","driver_exit":%s,"tee_exit":%s,"exit_code":%s}\n' \
    "$phase" "$driver_exit" "$tee_exit" "$command_exit" \
    > "$RC_WORK_DIR/logs/accept-${phase}.exit.json"
  return "$command_exit"
}

rc_security_checks() {
  rc_log "==> 安全 / profile 检查"
  set +e
  python3 "$RC_WORK_DIR/rc_security.py" \
    --evidence "$RC_EVIDENCE_DIR" \
    --work-dir "$RC_WORK_DIR" \
    --root "$RC_ROOT" \
    --project "$RC_PROJECT" \
    --console "http://127.0.0.1:$RC_CONSOLE_PORT" \
    --control-http-port "$RC_CONTROL_HTTP_PORT" \
    --gateway-port "$RC_GATEWAY_PORT" \
    --metering-port "$RC_METERING_PORT" \
    --console-port "$RC_CONSOLE_PORT" \
    --admin-email "$RC_ADMIN_EMAIL" \
    --admin-password-file "$RC_WORK_DIR/secrets/admin-password" \
    2>&1 | tee "$RC_WORK_DIR/logs/security.log"
  local rc=${PIPESTATUS[0]}
  set -e
  [[ $rc -eq 0 ]] && RC_SECURITY_OK="pass" || RC_SECURITY_OK="fail"
  return 0
}

rc_evidence_collect() {
  rc_log "==> 归档证据到 $RC_EVIDENCE_DIR"
  mkdir -p "$RC_EVIDENCE_DIR/logs" "$RC_EVIDENCE_DIR/stack"
  cp "$RC_WORK_DIR/rc_accept.py" "$RC_EVIDENCE_DIR/rc_accept.py"
  cp "$RC_WORK_DIR/rc_security.py" "$RC_EVIDENCE_DIR/rc_security.py"
  cp "$RC_WORK_DIR/logs/compose.config.json" "$RC_EVIDENCE_DIR/stack/compose.config.json" 2>/dev/null || true
  cp "$RC_WORK_DIR/logs/migrate-version.txt" "$RC_EVIDENCE_DIR/stack/migrate-version.txt" 2>/dev/null || true
  cp "$RC_WORK_DIR/logs/control-config.json" "$RC_EVIDENCE_DIR/stack/control-config.json" 2>/dev/null || true
  cp "$RC_WORK_DIR/compose.rc.yml" "$RC_EVIDENCE_DIR/stack/compose.rc.yml" 2>/dev/null || true
  [[ -f "$RC_WORK_DIR/Dockerfile.rc" ]] && cp "$RC_WORK_DIR/Dockerfile.rc" "$RC_EVIDENCE_DIR/stack/Dockerfile.rc" || true
  grep -E 'console bootstrap: |runtime profile|WARNING' "$RC_WORK_DIR/logs/bootstrap.log" \
    > "$RC_EVIDENCE_DIR/logs/bootstrap.log" 2>/dev/null || true
  local svc
  for svc in postgres redis migrate control console gateway metering mock-primary mock-backup; do
    docker logs "$(rc_container "$svc")" >"$RC_EVIDENCE_DIR/logs/$svc.log" 2>&1 || true
    tail -400 "$RC_EVIDENCE_DIR/logs/$svc.log" > "$RC_EVIDENCE_DIR/logs/$svc.log.tmp" \
      && mv "$RC_EVIDENCE_DIR/logs/$svc.log.tmp" "$RC_EVIDENCE_DIR/logs/$svc.log" || true
  done
}

rc_e2e() {
  command -v npm >/dev/null 2>&1 || rc_die "缺少 npm（Playwright 三角色套件需要 Node >= 20.19）"
  rc_log "==> 前端依赖（npm ci）"
  ( cd "$RC_ROOT/web" && npm ci --no-audit --no-fund >>"$RC_WORK_DIR/logs/e2e.log" 2>&1 ) \
    || { tail -20 "$RC_WORK_DIR/logs/e2e.log" >&2; rc_die "npm ci 失败"; }
  rc_log "==> Playwright 三角色套件（Console=$RC_CONSOLE_PORT Gateway=${RC_GATEWAY_PORT}）"
  # 一并运行角色流程与桌面/窄屏截图，输出到 data/verification/web-screenshots。
  set +e
  ( cd "$RC_ROOT/web" && \
    E2E_CONSOLE_URL="http://127.0.0.1:$RC_CONSOLE_PORT" \
    E2E_GATEWAY_URL="http://127.0.0.1:$RC_GATEWAY_PORT" \
    E2E_GATEWAY_MODEL="$RC_PRICE_MODEL" \
    E2E_REQUIRE_FULL_STACK=1 \
    E2E_ADMIN_EMAIL="$RC_ADMIN_EMAIL" \
    E2E_ADMIN_PASSWORD="$(cat "$RC_WORK_DIR/secrets/admin-password")" \
    CI=1 npm run e2e -- --reporter=list ) \
    2>&1 | tee -a "$RC_WORK_DIR/logs/e2e.log" | tail -45
  local rc=${PIPESTATUS[0]}
  set -e
  mkdir -p "$RC_EVIDENCE_DIR/e2e"
  tail -300 "$RC_WORK_DIR/logs/e2e.log" > "$RC_EVIDENCE_DIR/e2e/playwright.log" 2>/dev/null || true
  # web/e2e/.state.json 里带测试账号的明文口令（套件的既有设计，
  # 文件被 gitignore）。归档到证据前必须把凭据字段抹掉。
  python3 "$RC_WORK_DIR/redact_state.py" "$RC_ROOT/web/e2e/.state.json" \
    "$RC_EVIDENCE_DIR/e2e/state.json" 2>/dev/null || true
  # 会话 jar（含 cookie）不复制进证据：它是本地会话凭据。
  ls "$RC_ROOT/web/e2e/.auth" > "$RC_EVIDENCE_DIR/e2e/auth-jars-not-archived.txt" 2>/dev/null || true
  [[ $rc -ne 0 ]] && rc_warn "Playwright 退出码 ${rc}（详见 $RC_EVIDENCE_DIR/e2e/playwright.log）"
  return $rc
}

rc_down() {
  rc_log "==> 回收 project=$RC_PROJECT 的资源（只 drop 本项目）"
  local log="${RC_WORK_DIR:-/tmp}/logs/down.log"
  mkdir -p "$(dirname "$log")"
  rc_compose_run --profile demo-mocks down -v --remove-orphans >>"$log" 2>&1 || true
  local ids
  ids="$(docker ps -aq --filter "label=com.docker.compose.project=$RC_PROJECT" || true)"
  [[ -n "$ids" ]] && docker rm -f $ids >/dev/null 2>&1 || true
  ids="$(docker volume ls -q --filter "label=com.docker.compose.project=$RC_PROJECT" || true)"
  [[ -n "$ids" ]] && docker volume rm $ids >/dev/null 2>&1 || true
  ids="$(docker network ls -q --filter "label=com.docker.compose.project=$RC_PROJECT" || true)"
  [[ -n "$ids" ]] && docker network rm $ids >/dev/null 2>&1 || true
  rc_log "    完成。其它 compose project 未被触碰。"
}

rc_trap() {
  local code=$?
  if [[ "$RC_TEARDOWN" == "1" ]]; then rc_down || true; fi
  [[ $code -ne 0 ]] && rc_warn "退出码 ${code}；work dir=$RC_WORK_DIR evidence=$RC_EVIDENCE_DIR"
  exit $code
}

# ---------------------------------------------------------------------------
# 证据与安全检查
# ---------------------------------------------------------------------------
# 秘密扫描：区分"真泄漏"（硬失败）与"仓库里已知的 demo 默认常量"（记录并报告）。
rc_secret_scan() {
  rc_log "==> 秘密扫描（仓库 + 运行日志 + 证据）"
  mkdir -p "$RC_EVIDENCE_DIR"
  set +e
  python3 - "$RC_ROOT" "$RC_EVIDENCE_DIR" "$RC_WORK_DIR" <<'PY'
import json, pathlib, re, subprocess, sys

root, evidence, work = sys.argv[1:4]

secret_values = []
sd = pathlib.Path(work) / "secrets"
for name in ("admin-password", "control-admin-token", "metering-policy-token", "provider-credential-master-key"):
    p = sd / name
    if p.is_file():
        v = p.read_text().strip()
        if len(v) >= 8:
            secret_values.append((name, v))
ident = sd / "identities.json"
if ident.is_file():
    try:
        data = json.loads(ident.read_text())
    except Exception:
        data = {}

    def walk(node, path=""):
        if isinstance(node, dict):
            for k, v in node.items():
                walk(v, "%s.%s" % (path, k))
        elif isinstance(node, list):
            for i, v in enumerate(node):
                walk(v, "%s[%d]" % (path, i))
        elif isinstance(node, str) and any(t in path for t in (".password", ".secret")):
            secret_values.append((path.lstrip("."), node))

    walk(data)

# 硬失败模式：出现在**本次运行的证据/日志**里就是真泄漏。
hard_patterns = [
    ("api_key_secret", re.compile(r"sk-[A-Za-z0-9_\-]{20,}")),
    ("session_cookie", re.compile(r"gw_session=(?!<redacted>)[A-Za-z0-9_\-%]{10,}")),
    ("csrf_cookie", re.compile(r"gw_csrf=(?!<redacted>)[A-Za-z0-9_\-%]{8,}")),
    ("csrf_token", re.compile(r"(?i)(?:csrf_token|x-csrf-token)\s*[:=]\s*[\"']?(?!<redacted>)[A-Za-z0-9_\-%]{8,}")),
    ("private_key", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----")),
    ("aws_access_key", re.compile(r"AKIA[0-9A-Z]{16}")),
    ("bearer_token", re.compile(r"Bearer\s+(?!<redacted>)[A-Za-z0-9_\-\.]{20,}")),
]
# 观察模式：仓库里本来就有的 key 形状字面量（测试夹具、历史证据）。
# 它们不是本次运行产生的凭据，但要在报告里点名。
tracked_patterns = [
    ("api_key_literal", re.compile(r"sk-[A-Za-z0-9_\-]{16,}")),
    ("private_key_literal", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----")),
]
soft_patterns = [
    ("demo_control_token", re.compile(r"demo-control-token")),
    ("demo_postgres_password", re.compile(r"postgres://gateway:gateway@")),
    ("test_postgres_password", re.compile(r"test_only_local")),
]

def scan_file(path, label, patterns, with_secrets):
    found = []
    try:
        text = path.read_text(errors="ignore")
    except Exception:
        return found
    if with_secrets:
        for kind, value in secret_values:
            if value and value in text:
                found.append({"file": label, "kind": "run_secret:" + kind,
                              "match": "<redacted>", "severity": "leak"})
    for kind, rx in patterns:
        for _ in rx.finditer(text):
            found.append({"file": label, "kind": kind,
                          "match": "<redacted>", "severity": "leak"})
    return found

def scan_soft(path, label):
    found = []
    try:
        text = path.read_text(errors="ignore")
    except Exception:
        return found
    for kind, rx in soft_patterns:
        for _ in rx.finditer(text):
            found.append({"file": label, "kind": kind, "match": kind, "severity": "known_default"})
    return found

leaks, defaults = [], []
for base in (pathlib.Path(evidence), pathlib.Path(work) / "logs"):
    if not base.exists():
        continue
    for p in sorted(base.rglob("*")):
        if not p.is_file() or p.name == "identities.json":
            continue
        leaks += scan_file(p, str(p), hard_patterns, True)

tracked_literals = []
tracked = subprocess.run(["git", "ls-files"], cwd=root, capture_output=True, text=True).stdout.split()
for rel in tracked:
    p = pathlib.Path(root) / rel
    if not p.is_file() or p.stat().st_size > 2_000_000:
        continue
    text = p.read_text(errors="ignore")
    # 本次运行的真实秘密出现在仓库跟踪文件里 = 硬失败。
    for kind, value in secret_values:
        if value and value in text:
            leaks.append({"file": rel, "kind": "run_secret:" + kind,
                          "match": "<redacted>", "severity": "leak"})
    for kind, rx in tracked_patterns:
        for _ in rx.finditer(text):
            category = ("test_fixture" if rel.endswith("_test.go")
                        else "prior_evidence" if rel.startswith("data/verification/")
                        else "other")
            tracked_literals.append({"file": rel, "kind": kind, "category": category})
    defaults += scan_soft(p, rel)

def dedupe(items):
    seen, out = set(), []
    for it in items:
        key = (it["file"], it["kind"], it["match"], it["severity"])
        if key in seen:
            continue
        seen.add(key)
        out.append(it)
    return out

leaks, defaults = dedupe(leaks), dedupe(defaults)
literal_files = {}
for item in tracked_literals:
    key = (item["category"], item["file"])
    literal_files[key] = literal_files.get(key, 0) + 1
report = {
    "run_secrets_tracked": len(secret_values),
    "leaks": leaks,
    "known_defaults": defaults,
    "tracked_secret_literals": {
        "total": len(tracked_literals),
        "by_category": {c: len([i for i in tracked_literals if i["category"] == c])
                        for c in sorted({i["category"] for i in tracked_literals})},
        "files": sorted("%s:%s" % (cat, f) for (cat, f) in literal_files)[:40],
        "note": ("仓库里既有的 key 形状字面量：Go 测试夹具与历史任务证据里的临时 Key，"
                 "不是本次运行产生的凭据，也不是任何在跑系统的有效凭据。"),
    },
    "leaks_ok": len(leaks) == 0,
}
(pathlib.Path(evidence) / "secret-scan.json").write_text(
    json.dumps(report, indent=2, ensure_ascii=False) + "\n")
print("secret scan: %s — 本次运行证据/日志泄漏 %d 条；仓库已知 demo 默认常量 %d 处；"
      "仓库既有 key 形状字面量 %d 处（%s，见 README）"
      % ("PASS" if report["leaks_ok"] else "FAIL", len(leaks), len(defaults),
         len(tracked_literals), report["tracked_secret_literals"]["by_category"]))
for item in leaks[:20]:
    print("  LEAK %s %s %s" % (item["file"], item["kind"], item["match"]))
kinds = {}
for item in defaults:
    kinds[item["kind"]] = kinds.get(item["kind"], 0) + 1
for kind, count in sorted(kinds.items()):
    print("  known-default %s x%d" % (kind, count))
sys.exit(0 if report["leaks_ok"] else 1)
PY
  local rc=$?
  set -e
  [[ $rc -eq 0 ]] || rc_warn "秘密扫描发现泄漏（见 $RC_EVIDENCE_DIR/secret-scan.json）"
  return 0
}

rc_summary() {
  python3 - "$RC_EVIDENCE_DIR" "$RC_RUN_ID" "$RC_PROJECT" "$RC_SECURITY_OK" \
    "$RC_WORK_DIR/logs/accept-accept.exit.json" "$RC_WITH_FAILURE" \
    "$1" "$2" "$3" "$4" <<'PY'
import json, pathlib, sys

(evidence, run_id, project, security, driver_path, with_failure,
 final_failed, with_e2e, e2e_failed, isolation_failed) = sys.argv[1:11]
base = pathlib.Path(evidence)
def read_json(path, default=None):
    p = pathlib.Path(path)
    if not p.is_file():
        return default if default is not None else {}
    try:
        value = json.loads(p.read_text())
        return value if isinstance(value, dict) else (default if default is not None else {})
    except Exception:
        return default if default is not None else {}

accept = read_json(base / "summary.json")
driver = read_json(driver_path)
all_steps = []
steps_path = base / "steps.jsonl"
if steps_path.is_file():
    for line in steps_path.read_text().splitlines():
        try:
            all_steps.append(json.loads(line))
        except Exception:
            continue
step_count = accept.get("steps_total")
steps = all_steps[-step_count:] if isinstance(step_count, int) and step_count > 0 else []
observations = {step.get("id"): step.get("observations") for step in steps}
scan_path = base / "secret-scan.json"
sec_path = base / "security" / "summary.json"
scan = read_json(scan_path)
sec = read_json(sec_path)
scan_valid = scan_path.is_file() and isinstance(scan.get("leaks"), list) and scan.get("leaks_ok") is True
security_valid = security == "pass" and sec_path.is_file() and sec.get("ok") is True
e2e_requested = with_e2e == "1"
e2e_failure_observed = e2e_failed == "1"
isolation_failure_observed = isolation_failed == "1"
overall_failure_observed = final_failed == "1"
required_steps = ["S1", "S2", "S3", "S3B", "S4", "S5", "S6", "S7"]
if with_failure == "1":
    required_steps += ["S8", "S9"]
passed_ids = {step.get("id") for step in steps if step.get("ok") is True and step.get("hard", True) is True}
missing_steps = [sid for sid in required_steps if sid not in passed_ids]
expected_skips = [] if with_failure == "1" else ["S8", "S9"]
driver_exit = driver.get("driver_exit", -1)
acceptance_valid = (
    driver.get("phase") == "accept"
    and driver_exit == 0
    and driver.get("tee_exit", -1) == 0
    and accept.get("phase") == "accept"
    and accept.get("run_id") == run_id
    and accept.get("ok") is True
    and not missing_steps
)
overall_ok = (
    not overall_failure_observed
    and acceptance_valid
    and security_valid
    and scan_valid
    and not scan.get("leaks")
    and (not e2e_requested or not e2e_failure_observed)
    and not isolation_failure_observed
)
summary = {
    "run_id": run_id,
    "compose_project": project,
    "acceptance_ok": acceptance_valid,
    "acceptance_phase": accept.get("phase"),
    "acceptance_driver_exit_code": driver_exit,
    "acceptance_tee_exit_code": driver.get("tee_exit", -1),
    "acceptance_steps": accept.get("steps_total"),
    "acceptance_expected_steps": required_steps,
    "acceptance_missing_steps": missing_steps,
    "acceptance_skipped_steps": expected_skips,
    "acceptance_hard_failures": accept.get("hard_failures", []),
    "acceptance_observations": observations,
    "security_ok": security_valid,
    "security_execution_status": security,
    "secret_scan_valid": scan_valid,
    "security_failed": sec.get("failed", []),
    "secret_scan_leaks": len(scan.get("leaks", [])),
    "secret_scan_known_defaults": len(scan.get("known_defaults", [])),
    "e2e_requested": e2e_requested,
    "e2e_status": ("not_requested" if not e2e_requested else
                    ("failed" if e2e_failure_observed else "passed")),
    "e2e_failed": e2e_failure_observed if e2e_requested else False,
    "isolation_status": "failed" if isolation_failure_observed else "passed",
    "isolation_failed": isolation_failure_observed,
    "overall_failure_observed_before_summary": overall_failure_observed,
    "overall_failed": not overall_ok,
    "ok": overall_ok,
}
(base / "release-candidate-summary.json").write_text(
    json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
print(json.dumps(summary, ensure_ascii=False))
PY
}

# ---------------------------------------------------------------------------
# 子命令
# ---------------------------------------------------------------------------
rc_setup() {
  rc_preflight
  rc_prepare_dirs
  rc_materialize_drivers
  rc_secrets_init
  rc_write_compose_files
  rc_write_state
  rc_write_run_env
  rc_isolation_snapshot "$RC_WORK_DIR/logs/isolation-before.txt"
}

rc_cmd_run() {
  rc_setup
  rm -f "$RC_WORK_DIR/logs/accept-seed.exit.json" "$RC_WORK_DIR/logs/accept-accept.exit.json"
  rm -f "$RC_EVIDENCE_DIR/secret-scan.json" "$RC_EVIDENCE_DIR/security/summary.json" \
    "$RC_EVIDENCE_DIR/release-candidate-summary.json"
  [[ "$RC_KEEP" == "1" ]] || RC_TEARDOWN=1
  trap rc_trap EXIT
  local failed=0
  local isolation_failed=0
  # 栈本身起不来是环境问题，直接 die；验收/安全/E2E 的失败一律记录下来，
  # 保证证据一定被收集，最后再以非零码退出。
  rc_build
  rc_up
  rc_assert_migrations
  rc_bootstrap
  rc_wait_gateway_applied
  if ! rc_accept seed; then
    rc_warn "seed 阶段失败"
    failed=1
  fi
  if [[ $failed -eq 0 ]]; then
    if ! rc_accept accept $([[ "$RC_WITH_FAILURE" == "1" ]] && printf '%s' --with-failure); then
      rc_warn "端到端验收存在硬失败"
      failed=1
    fi
  fi
  rc_security_checks
  [[ "$RC_SECURITY_OK" == "pass" ]] || { rc_warn "安全/profile 检查存在硬失败"; failed=1; }
  if [[ "$RC_WITH_E2E" == "1" ]]; then
    rc_e2e || { rc_warn "Playwright 三角色套件未全绿"; RC_E2E_FAILED=1; failed=1; }
  fi
  rc_evidence_collect
  rc_secret_scan
  [[ "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["leaks_ok"])' "$RC_EVIDENCE_DIR/secret-scan.json" 2>/dev/null || echo False)" == "True" ]] \
    || { rc_warn "秘密扫描发现泄漏"; failed=1; }
  rc_isolation_snapshot "$RC_WORK_DIR/logs/isolation-after.txt" || true
  rc_isolation_diff "$RC_WORK_DIR/logs/isolation-before.txt" "$RC_WORK_DIR/logs/isolation-after.txt" \
    || { rc_warn "隔离校验失败：其它 compose project 的容器/卷集合发生变化"; isolation_failed=1; failed=1; }
  cp "$RC_WORK_DIR/logs/isolation-before.txt" "$RC_EVIDENCE_DIR/stack/isolation-before.txt" 2>/dev/null || true
  cp "$RC_WORK_DIR/logs/isolation-after.txt" "$RC_EVIDENCE_DIR/stack/isolation-after.txt" 2>/dev/null || true
  rc_summary "$failed" "$RC_WITH_E2E" "$RC_E2E_FAILED" "$isolation_failed"
  local summary_ok
  summary_ok="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("ok") is True)' \
    "$RC_EVIDENCE_DIR/release-candidate-summary.json" 2>/dev/null || printf false)"
  if [[ "$summary_ok" != "True" ]]; then
    rc_warn "发布候选摘要未确认全部必需阶段和检查通过"
    failed=1
  fi
  if [[ $failed -ne 0 ]]; then
    rc_warn "发布候选验收未通过（见 $RC_EVIDENCE_DIR/release-candidate-summary.json）"
    exit 1
  fi
  rc_log "==> 完成：证据在 $RC_EVIDENCE_DIR"
}

rc_cmd_up() {
  rc_setup
  RC_TEARDOWN=0
  rc_build
  rc_up
  rc_assert_migrations
  rc_bootstrap
  rc_wait_gateway_applied
  rc_accept seed
  rc_log "==> 栈已就绪并保留运行。"
  if [[ "$RC_WITH_PREVIEW" == "1" ]]; then
    rc_log "    Web      http://127.0.0.1:$RC_WEB_PORT"
    rc_log "    可控 Provider 内网地址：http://mock-fixed:18080 / http://mock-echo:18080 / http://mock-flaky:18080"
  fi
  rc_log "    Console  http://127.0.0.1:$RC_CONSOLE_PORT"
  rc_log "    Gateway  http://127.0.0.1:$RC_GATEWAY_PORT"
  rc_log "    Metering http://127.0.0.1:$RC_METERING_PORT"
  rc_log "    work dir $RC_WORK_DIR"
  rc_log "    下一步：scripts/release-candidate.sh demo | e2e | security | down"
}

rc_cmd_preview() {
  # Repeat startup reuses the saved ports/secrets and never reruns onboarding seed.
  local state_dir="${RC_WORK_DIR:-$RC_ROOT/data/release-candidate/current}"
  if [[ ! -f "$state_dir/state.env" ]]; then
    rc_cmd_up
    return
  fi
  local requested_web="$RC_WEB_PORT" requested_mock="$RC_MOCK_BASE_PORT"
  rc_load_state
  [[ "$RC_WEB_PORT_SET" != "1" ]] || RC_WEB_PORT="$requested_web"
  [[ "$RC_MOCK_BASE_PORT_SET" != "1" ]] || RC_MOCK_BASE_PORT="$requested_mock"
  RC_WITH_PREVIEW=1
  rc_preflight
  rc_base_compose_file
  rc_write_compose_files
  rc_write_state
  RC_TEARDOWN=0
  rc_build
  rc_up
  rc_log "==> 完整预览已就绪，已有账户和数据保留。"
  rc_log "    Web http://127.0.0.1:$RC_WEB_PORT"
  rc_log "    Provider 内网地址：http://mock-fixed:18080 / http://mock-echo:18080 / http://mock-flaky:18080"
}

rc_cmd_stop() {
  rc_preflight
  rc_load_state
  rc_compose_run --profile demo-mocks stop
  rc_log "==> 已停止本项目服务，数据卷保留。"
}

rc_cmd_demo() {
  rc_preflight
  rc_load_state
  rc_materialize_drivers 2>/dev/null || true
  [[ -f "$RC_WORK_DIR/rc_accept.py" ]] || rc_die "驱动缺失，请重新执行 up"
  rc_accept demo 2>&1 | tee "$RC_EVIDENCE_DIR/demo-transcript.txt"
  return "${PIPESTATUS[0]}"
}

rc_cmd_e2e() {
  rc_preflight
  rc_load_state
  rc_materialize_drivers
  rc_e2e
}

rc_cmd_security() {
  rc_preflight
  rc_load_state
  rc_materialize_drivers
  rc_security_checks
  rc_secret_scan
  rc_summary
}

rc_cmd_status() {
  rc_preflight
  rc_load_state
  rc_compose ps --format '{{.Service}}\t{{.Status}}\t{{.Ports}}' || true
  printf 'project=%s\nwork_dir=%s\nevidence_dir=%s\n' "$RC_PROJECT" "$RC_WORK_DIR" "$RC_EVIDENCE_DIR"
}

rc_cmd_down() {
  rc_preflight
  rc_load_state
  RC_TEARDOWN=0
  rc_down
}

rc_main() {
  rc_parse_args "$@"
  case "$RC_COMMAND" in
    help) rc_usage; exit 0 ;;
    run) rc_cmd_run ;;
    up) rc_cmd_up ;;
    preview) rc_cmd_preview ;;
    stop) rc_cmd_stop ;;
    demo) rc_cmd_demo ;;
    e2e) rc_cmd_e2e ;;
    security) rc_cmd_security ;;
    status) rc_cmd_status ;;
    down) rc_cmd_down ;;
    *) rc_die "未知命令 $RC_COMMAND" ;;
  esac
}

rc_main "$@"
