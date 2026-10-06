#!/usr/bin/env bash
#
# console-rbac-accept.sh — T03 企业 RBAC / Key / 用量 / 审计 HTTP 验收（黑盒）。
#
# 假设 Console 已经在 --base-url 上运行（本脚本不启动、不停止、不迁移、不读数据库、
# 不依赖仓库内 Go 代码）：
#   * HTTP 只用 curl；解析与断言只用 python3 标准库；
#   * 密码、session token、CSRF token、邀请 token、API Key secret 在输出与证据文件里
#     一律只以 "前缀…<redacted:N>" 形式出现，明文既不打印也不落盘（state file 除外，
#     见下）；
#   * 全部检查都是**负例优先**：越权读取必须 404（且与"不存在的组织"逐字节同形，
#     不泄露元数据），越权写入必须 403/404，最后一个 owner 必须 409。
#
# 契约来源：
#   * docs/decisions.md
#   * docs/openapi/console-v1.yaml（/api/v1/orgs/* 冻结子集）
#   * docs/decisions.md（会话/CSRF）
#
# 退出码：0 = 无硬失败（可能含 SKIP / NOT VERIFIED）；1 = 首个硬失败；2 = 用法/环境错误。
#
# 用法见 --help。
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
script_name="$(basename "$0")"

base_url="http://127.0.0.1:8092"
admin_email=""
admin_password_file=""
state_file=""
evidence_dir=""
run_id=""
timeout_seconds=15
gateway_url=""
gateway_model="gpt-4o-mini"
seed_local_test_credit=0

usage() {
  cat <<'USAGE'
用法:
  scripts/console-rbac-accept.sh --admin-email EMAIL --admin-password-file PATH [选项]

必需:
  --admin-email EMAIL            平台管理员邮箱（bootstrap 创建的那个）
  --admin-password-file PATH     平台管理员密码文件（内容不会被打印；只读，不做权限校验）

选项:
  --base-url URL                 目标 Console 地址（默认 http://127.0.0.1:8092）
  --state-file PATH              创建出来的 fixture 状态文件（默认 <evidence-dir>/state.json）。
                                 存在且可解析时复用其中的账号/组织，不重复创建。
                                 注意：该文件含本次运行的临时随机口令，0600，只用于一次性
                                 验收数据库；证据目录里另有一份脱敏的 fixtures.json。
  --evidence-dir DIR             证据目录（默认 data/console-rbac-accept/<UTC 时间戳>）
  --run-id ID                    fixture 命名后缀（默认用 state file 里的值，否则自动生成）
  --timeout-seconds N            单个 curl 请求超时，默认 15
  --gateway-url URL              Gateway 地址（例如 http://127.0.0.1:18090）。提供后启用
                                 check08（GATEWAY_ACCESS_GUARD=pg：成员自己的 Key 放行、
                                 撤销后 401、成功请求写入带归属的计量行）；
                                 不提供则 check08 记为 SKIP，不做任何猜测。余额不足时，
                                 需另显式启用下方受限的本地测试充值选项。
  --gateway-model MODEL          check08 使用的模型名，默认 gpt-4o-mini（必须已在 Control
                                 的 routes 里配置，否则 Gateway 返回 404 unknown model）
  --seed-local-test-credit      显式为本次新建的随机测试企业 A 充值 100000000 微元；仅当
                                 Console/Gateway 都是 loopback 且 state file 不存在时可用。
                                 默认不充值；不能配合既有 state file 或显式 --run-id。
  -h, --help                     显示本帮助

检查项（ADR-0003 §5 验收映射）:
  01 登录与角色矩阵读取         A/B 两家企业各 4 个账号（owner/admin/member/member），
                                owner/admin 看全组织，member 只看到自己
  02 跨组织读取                 A 的 owner 读 B 的 members/invitations/keys/audit/usage
                                → 404 not_found，且与"不存在的组织"逐字节同形、无元数据泄露
  03 跨组织写入                 A 的 owner 改 B 的成员/角色/邀请/owner 转移/Key/撤销
                                → 404；B 的状态与 Key 不变
  04 角色矩阵负例               普通成员的邀请、成员管理、审计 → 403；
                                非法角色 → 400；platform_admin 不是组织角色 → 403
  05 最后 owner 保护            admin 不能移除/降级 owner、不能自升 owner；
                                最后一个 owner 不能离开/自我降级 → 409
  06 邀请一次性与不可篡改       错 email 接受 → 403；接受请求体不能改 email/role；
                                同 token 二次接受 → 409；未知 token → 404；TTL 越界 → 400
  07 Key 归属与 secret 一次性    各角色只管理本人命名 Key，管理员看不到成员 Key；
                                secret 只在 create 响应出现一次；?operation_id= 只能回查元数据
                                （Console 未接 Control 时整段 SKIP，不猜）
  08 Gateway access guard        GATEWAY_ACCESS_GUARD=pg：成员 Key 放行到上游；撤销后 401；
                                另一成员自己的 Key 也能放行；成功请求写入带 org/user/key/
                                execution 归属的计量行（需 --gateway-url，否则 SKIP）
  09 用量隔离                   member → scope=self 且只看本人；admin/owner
                                → scope=organization；member 指名他人 → 403；B 的数据不可见
  10 审计归属                   每个变更出现在**操作者组织**的审计里（含失败），
                                绝不作为 success 出现在另一家企业的审计里
  11 全局泄密扫描               所有已捕获响应 + 审计/邀请/Key 列表里不得再出现 API secret、
                                邀请 token 或口令
USAGE
}

die_usage() {
  printf '%s: %s\n\n' "$script_name" "$1" >&2
  usage >&2
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-url) base_url="${2:-}"; shift 2 ;;
    --base-url=*) base_url="${1#*=}"; shift ;;
    --admin-email) admin_email="${2:-}"; shift 2 ;;
    --admin-email=*) admin_email="${1#*=}"; shift ;;
    --admin-password-file) admin_password_file="${2:-}"; shift 2 ;;
    --admin-password-file=*) admin_password_file="${1#*=}"; shift ;;
    --state-file) state_file="${2:-}"; shift 2 ;;
    --state-file=*) state_file="${1#*=}"; shift ;;
    --evidence-dir) evidence_dir="${2:-}"; shift 2 ;;
    --evidence-dir=*) evidence_dir="${1#*=}"; shift ;;
    --run-id) run_id="${2:-}"; shift 2 ;;
    --run-id=*) run_id="${1#*=}"; shift ;;
    --timeout-seconds) timeout_seconds="${2:-}"; shift 2 ;;
    --timeout-seconds=*) timeout_seconds="${1#*=}"; shift ;;
    --gateway-url) gateway_url="${2:-}"; shift 2 ;;
    --gateway-url=*) gateway_url="${1#*=}"; shift ;;
    --gateway-model) gateway_model="${2:-}"; shift 2 ;;
    --gateway-model=*) gateway_model="${1#*=}"; shift ;;
    --seed-local-test-credit) seed_local_test_credit=1; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    -*) die_usage "未知参数 $1" ;;
    *) die_usage "多余的参数 $1" ;;
  esac
done

[[ -n "$admin_email" ]] || die_usage "缺少 --admin-email"
[[ -n "$admin_password_file" ]] || die_usage "缺少 --admin-password-file"
[[ -r "$admin_password_file" ]] || die_usage "--admin-password-file 不存在或不可读: $admin_password_file"
case "$base_url" in
  http://*|https://*) ;;
  *) die_usage "--base-url 必须是 http(s) URL: $base_url" ;;
esac
case "$timeout_seconds" in
  ''|*[!0-9]*) die_usage "--timeout-seconds 必须是非负整数: $timeout_seconds" ;;
esac

command -v curl >/dev/null 2>&1 || { printf '%s: 需要 curl\n' "$script_name" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { printf '%s: 需要 python3\n' "$script_name" >&2; exit 2; }

if [[ -z "$evidence_dir" ]]; then
  evidence_dir="data/console-rbac-accept/$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$evidence_dir"
evidence_dir="$(cd "$evidence_dir" && pwd)"

if [[ -z "$state_file" ]]; then
  state_file="$evidence_dir/state.json"
fi
if [[ -e "$state_file" && ! -r "$state_file" ]]; then
  die_usage "--state-file 存在但不可读: $state_file"
fi

# 非机密运行元数据，便于证据目录自解释。
{
  printf 'script=%s\n' "$script_name"
  printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'base_url=%s\n' "$base_url"
  printf 'admin_email=%s\n' "$admin_email"
  printf 'admin_password_file=%s\n' "$admin_password_file"
  printf 'state_file=%s\n' "$state_file"
  printf 'run_id=%s\n' "${run_id:-<auto>}"
  printf 'timeout_seconds=%s\n' "$timeout_seconds"
  printf 'gateway_url=%s\n' "${gateway_url:-<none>}"
  printf 'gateway_model=%s\n' "$gateway_model"
  printf 'seed_local_test_credit=%s\n' "$seed_local_test_credit"
  printf 'script_sha256=%s\n' "$(shasum -a 256 "$script_dir/$script_name" 2>/dev/null | awk '{print $1}')"
} > "$evidence_dir/run.env"

# python 驱动写在临时目录：它只是实现细节，不属于证据。
helper_dir=""
cleanup_helper() { [[ -n "$helper_dir" ]] && rm -rf "$helper_dir"; }
trap cleanup_helper EXIT
if ! helper_dir="$(mktemp -d "${TMPDIR:-/tmp}/console-rbac-accept.XXXXXX" 2>/dev/null)"; then
  helper_dir="$evidence_dir/.harness"
  mkdir -p "$helper_dir"
fi
helper="$helper_dir/rbac_accept_lib.py"

cat > "$helper" <<'PYEOF'
#!/usr/bin/env python3
"""T03 enterprise RBAC black-box HTTP acceptance checks (ADR-0003 §2.3–§2.9, §5).

The driver owns every assertion. HTTP is done by shelling out to curl (the only
network client allowed); JSON parsing and assertions use the standard library
only. Nothing here reads the database or the product source tree.
"""

import argparse
import hashlib
import ipaddress
import json
import os
import re
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.parse
from datetime import datetime, timezone

MAX_STORED_BODY = 8192
STATE_VERSION = 2

# Fixture actors: each enterprise has an owner, an admin, and two ordinary members.
ACTOR_ROLES = {
    "a-owner": ("A", "owner"),
    "a-admin": ("A", "admin"),
    "a-member": ("A", "member"),
    "a-member2": ("A", "member"),
    "b-owner": ("B", "owner"),
    "b-admin": ("B", "admin"),
    "b-member": ("B", "member"),
    "b-member2": ("B", "member"),
}
INVITEE_ACTOR = "a-invitee"
ORG_LABELS = {"A": "acme", "B": "beta"}


class CheckFailure(Exception):
    pass


class SkipCheck(Exception):
    pass


class InfraFailure(Exception):
    pass


def utcnow():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def slugify(text):
    return re.sub(r"[^a-z0-9]+", "-", str(text).lower()).strip("-")[:48] or "call"


def is_loopback_http_url(raw):
    try:
        parsed = urllib.parse.urlsplit(raw)
        host = parsed.hostname
        _ = parsed.port
    except (TypeError, ValueError):
        return False
    if parsed.scheme not in ("http", "https") or not host or parsed.username or parsed.password:
        return False
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


class Redactor:
    """Masks every secret it has been told about, plus the obvious JSON keys."""

    def __init__(self):
        self.secrets = []

    def add(self, value):
        value = "" if value is None else str(value)
        if len(value) >= 4 and value not in self.secrets:
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
            r'("(?:password|csrf_token|token|session_token|access_token|secret)"\s*:\s*")([^"]*)(")',
            lambda m: m.group(1) + (self._mask(m.group(2)) if m.group(2) else "") + m.group(3),
            out,
        )
        out = re.sub(r"(gw_session=)([^;,\s]+)", lambda m: m.group(1) + self._mask(m.group(2)), out)
        out = re.sub(r"(gw_csrf=)([^;,\s]+)", lambda m: m.group(1) + self._mask(m.group(2)), out)
        out = re.sub(r"((?i:X-CSRF-Token):\s*)(\S+)", lambda m: m.group(1) + self._mask(m.group(2)), out)
        return out


class Response:
    def __init__(self, method, path, status, headers, body, elapsed):
        self.method = method
        self.path = path
        self.status = status
        self.headers = headers  # list[(name, value)] preserving duplicates
        self.body = body
        self.elapsed = elapsed
        # True when curl hit --max-time although a body was captured (the
        # Gateway leaves SSE responses open; see README 的产品发现).
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
        name, raw, attrs = parse_set_cookie(value)
        if name:
            out[name] = raw
    return out


class Check:
    def __init__(self, cid, title, kind="check"):
        self.cid = cid
        self.title = title
        self.kind = kind
        self.status = "PASS"
        self.started_at = utcnow()
        self.finished_at = None
        self.details = {}
        self.notes = []
        self.failure = None
        self.skip_reason = None
        self.exchanges = []
        self.sequence = 0

    def note(self, text):
        line = redact(text)
        self.notes.append(line)
        log("    note: " + line)

    def require(self, condition, message):
        if not condition:
            raise CheckFailure(message)

    def skip(self, reason):
        raise SkipCheck(reason)


_RED = Redactor()


def redact(text):
    return _RED.text(text)


def log(line):
    line = _RED.text(line)
    print(line, flush=True)
    logfile = getattr(log, "path", None)
    if logfile:
        with open(logfile, "a", encoding="utf-8") as handle:
            handle.write(line + "\n")


log.path = None

CSRF_DEFAULT = object()


class Client:
    def __init__(self, base_url, timeout, evidence_dir, red):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.evidence_dir = evidence_dir
        self.red = red
        self.scratch = tempfile.mkdtemp(prefix="console-rbac-http.")
        self.records = []  # every captured exchange, unredacted, in memory only

    def call(self, check, label, method, path, *, query=None, body=None,
             session=None, csrf=CSRF_DEFAULT, extra_headers=None, raw_body=None,
             base_url=None, max_time=None, tolerate_timeout=False):
        check.sequence += 1
        seq = check.sequence
        url = (base_url or self.base_url).rstrip("/") + path
        timeout = max_time if max_time else self.timeout
        if query:
            url += "?" + query
        request_id = "t03-%s-%02d" % (check.cid, seq)

        headers = {"X-Request-ID": request_id, "Accept": "application/json"}
        if session is not None:
            headers["Cookie"] = "gw_session=%s; gw_csrf=%s" % (session["token"], session.get("csrf", ""))
        if csrf is CSRF_DEFAULT:
            token = session.get("csrf") if session is not None else None
        else:
            token = csrf
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

        header_file = os.path.join(self.scratch, "headers.%d.txt" % seq)
        body_file = os.path.join(self.scratch, "body.%d.txt" % seq)
        cmd = [
            "curl", "--silent", "--show-error", "--max-time", str(timeout),
            "--request", method, "--dump-header", header_file, "--output", body_file,
            "--write-out", "%{http_code} %{time_total}",
        ]
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
                                  stderr=subprocess.PIPE, timeout=timeout + 10)
        except subprocess.TimeoutExpired:
            raise InfraFailure("curl 超时: %s %s" % (method, url))
        if proc.returncode != 0:
            # rc=28 (--max-time) with a complete body is the documented SSE
            # behavior of the Gateway: the stream body ends with data: [DONE]
            # but the chunked response is not terminated until much later.
            # Callers that opt in keep the captured bytes and record the fact.
            if tolerate_timeout and proc.returncode == 28 and os.path.exists(body_file) and os.path.getsize(body_file) > 0:
                curl_timeout = True
            else:
                raise InfraFailure("curl 失败 rc=%d: %s %s: %s" % (
                    proc.returncode, method, url, self.red.text(proc.stderr.decode("utf-8", "replace").strip())))

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
            body_bytes = handle.read()
        body_text = body_bytes.decode("utf-8", "replace")

        response = Response(method, path + (("?" + query) if query else ""), status, headers_list, body_text, elapsed)
        response.curl_timeout = curl_timeout
        self._record(check, label, url, headers, payload, response)
        return response

    def _record(self, check, label, url, headers, payload, response):
        seq = check.sequence
        check.exchanges.append({
            "method": response.method, "path": response.path, "status": response.status,
            "error_code": response.code(), "request_id": "t03-%s-%02d" % (check.cid, seq),
            "label": label,
        })
        self.records.append({
            "cid": check.cid, "seq": seq, "label": label, "url": url,
            "status": response.status, "body": response.body, "path": response.path,
            "method": response.method,
        })
        stem = "%s-%02d-%s" % (check.cid, seq, slugify(label))
        request_dump = ["%s %s" % (response.method, url)]
        for key, value in headers.items():
            request_dump.append("%s: %s" % (key, value))
        request_dump.append("")
        if payload:
            request_dump.append(payload)
        request_dump.append("")
        with open(os.path.join(self.evidence_dir, stem + ".req"), "w", encoding="utf-8") as handle:
            handle.write(self.red.text("\n".join(request_dump)))
        response_dump = ["HTTP %d (%.3fs)" % (response.status, response.elapsed)]
        for key, value in response.headers:
            response_dump.append("%s: %s" % (key, value))
        response_dump.append("")
        response_dump.append(response.body[:MAX_STORED_BODY])
        if len(response.body) > MAX_STORED_BODY:
            response_dump.append("\n…(%d bytes truncated)" % (len(response.body) - MAX_STORED_BODY))
        with open(os.path.join(self.evidence_dir, stem + ".resp"), "w", encoding="utf-8") as handle:
            handle.write(self.red.text("\n".join(response_dump)))

    def raw_bodies(self):
        return [record["body"] for record in self.records]


# ---------------------------------------------------------------------------
# assertion helpers
# ---------------------------------------------------------------------------

def expect_status(check, response, status, code=None, label=""):
    where = (" " + label) if label else ""
    if response.status != status:
        raise CheckFailure("%s%s: 期望 HTTP %d，实际 %d，code=%s message=%s" % (
            response.method, where, status, response.status, response.code(), response.message()))
    if code is not None and response.code() != code:
        raise CheckFailure("%s%s: 期望 error.code=%s，实际 %s（message=%s）" % (
            response.method, where, code, response.code(), response.message()))
    return response


def require_envelope(check, response, status, code, label=""):
    expect_status(check, response, status, code, label)
    err = response.error()
    check.require(isinstance(err, dict) and isinstance(err.get("message"), str),
                  "%s: 4xx/5xx 必须是 T01A 错误信封" % label)
    return err


def login(client, check, email, password, label):
    response = client.call(check, label, "POST", "/api/v1/auth/login",
                           body={"email": email, "password": password}, csrf=None)
    if response.status == 429:
        raise InfraFailure("登录被节流（429）：%s；同一 email 每分钟只允许 5 次，等待后重跑" % email)
    expect_status(check, response, 200, label=label)
    payload = response.json() or {}
    cookies = cookies_of(response)
    if "gw_session" not in cookies:
        raise InfraFailure("%s: 登录响应没有 gw_session cookie" % label)
    session = {
        "token": cookies["gw_session"],
        "csrf": cookies.get("gw_csrf") or payload.get("csrf_token", ""),
        "user_id": str((payload.get("user") or {}).get("id", "")),
        "email": email,
    }
    client.red.add(session["token"])
    client.red.add(session["csrf"])
    return session, payload


# ---------------------------------------------------------------------------
# fixtures
# ---------------------------------------------------------------------------

class Env:
    def __init__(self, args, client):
        self.args = args
        self.client = client
        self.state_path = args.state_file
        self.evidence_dir = args.evidence_dir
        self.state_file_existed = os.path.lexists(args.state_file)
        if args.seed_local_test_credit and self.state_file_existed:
            raise InfraFailure("--seed-local-test-credit 不能复用已存在的 state file")
        self.state = self._load_state()
        self.run_id = (self.state.get("run_id") or args.run_id or
                       secrets.token_hex(16 if args.seed_local_test_credit else 3))
        self.password = self.state.get("password") or secrets.token_urlsafe(15)
        self.sessions = {}
        self.bugs = []
        self.skipped = []
        # (name, value, [labels whose response may legitimately contain it])
        self.secrets_to_scan = []
        self.control_available = None
        self.orgs = self.state.setdefault("orgs", {})
        self.users = self.state.setdefault("users", {})
        self.keys = self.state.setdefault("keys", {})
        self.invitations = self.state.setdefault("invitations", {})
        self.created_orgs = set()
        self.pass_id = "%s-%s" % (utcnow().replace(":", "").replace("-", ""), secrets.token_hex(2))

    # -- state -------------------------------------------------------------
    def _load_state(self):
        if not os.path.exists(self.state_path):
            return {}
        try:
            with open(self.state_path, "r", encoding="utf-8") as handle:
                state = json.load(handle)
        except Exception as exc:
            raise InfraFailure("state file 无法解析: %s: %r" % (self.state_path, exc))
        if not isinstance(state, dict):
            raise InfraFailure("state file 顶层必须是 JSON object: %s" % self.state_path)
        if state.get("base_url") and state["base_url"] != self.args.base_url:
            raise InfraFailure("state file 属于另一个 base_url（%s），拒绝复用" % state["base_url"])
        if state and state.get("version") != STATE_VERSION:
            raise InfraFailure("state file 使用旧版 fixture 契约；请为本次验收删除旧 state file 或指定新路径")
        if state.get("admin_email") and state["admin_email"] != self.args.admin_email:
            raise InfraFailure("state file 属于另一个 admin_email，拒绝复用")
        return state

    def save_state(self):
        self.state["tool"] = "scripts/console-rbac-accept.sh"
        self.state["version"] = STATE_VERSION
        self.state["base_url"] = self.args.base_url
        self.state["admin_email"] = self.args.admin_email
        self.state["run_id"] = self.run_id
        self.state["password"] = self.password
        self.state["updated_at"] = utcnow()
        tmp = self.state_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as handle:
            json.dump(self.state, handle, indent=2, sort_keys=True)
            handle.write("\n")
        os.chmod(tmp, 0o600)
        os.replace(tmp, self.state_path)

    def write_redacted_fixtures(self):
        """Evidence copy: identities only, never a password or a secret."""
        view = {
            "tool": "scripts/console-rbac-accept.sh",
            "run_id": self.run_id,
            "base_url": self.args.base_url,
            "admin_email": self.args.admin_email,
            "written_at": utcnow(),
            "organizations": self.orgs,
            "users": {name: {"email": item.get("email"), "id": item.get("id")}
                      for name, item in self.users.items()},
            "keys": self.keys,
            "note": "创建口令、session/CSRF token、邀请 token、API Key secret 均已脱敏；"
                    "原始 state file 位于 --state-file，不在证据目录里。",
        }
        path = os.path.join(self.evidence_dir, "fixtures.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(view, handle, indent=2, sort_keys=True, ensure_ascii=False)
            handle.write("\n")

    # -- sessions ----------------------------------------------------------
    def session(self, actor):
        if actor not in self.sessions:
            raise InfraFailure("actor %s 尚未登录" % actor)
        return self.sessions[actor]

    def user_id(self, actor):
        return str(self.users[actor]["id"])

    def email(self, actor):
        return self.users[actor]["email"]

    def org_id(self, label):
        return str(self.orgs[label]["id"])

    def org_path(self, label, suffix=""):
        return "/api/v1/orgs/%s%s" % (self.org_id(label), suffix)

    # -- fixture creation --------------------------------------------------
    def ensure_fixtures(self, check):
        admin_password = read_password(self.args.admin_password_file)
        self.client.red.add(admin_password)
        admin_session, _ = login(self.client, check, self.args.admin_email, admin_password, "login-admin")
        self.sessions["admin"] = admin_session

        created_any = False
        actors = sorted(set(ACTOR_ROLES) | {INVITEE_ACTOR})
        for actor in actors:
            if actor in self.users:
                continue
            email = "t03-%s-%s@t03.test" % (self.run_id, actor)
            response = self.client.call(
                check, "register-%s" % actor, "POST", "/api/v1/auth/register",
                body={"email": email, "password": self.password}, csrf=None)
            expect_status(check, response, 201, label="register-%s" % actor)
            payload = response.json() or {}
            user_id = str((payload.get("user") or {}).get("id", ""))
            if not user_id:
                raise CheckFailure("register-%s: 响应缺少 user.id" % actor)
            self.users[actor] = {"email": email, "id": user_id}
            created_any = True
            self.save_state()

        for actor in actors:
            session, payload = login(self.client, check, self.users[actor]["email"], self.password, "login-%s" % actor)
            self.sessions[actor] = session
            if session["user_id"] != str(self.users[actor]["id"]):
                raise CheckFailure("%s: 登录返回的 user.id 与 fixture 不一致" % actor)

        # Platform admin provisions each enterprise for the explicitly named owner.
        for label in ("A", "B"):
            if label not in self.orgs:
                owner = "%s-owner" % label.lower()
                name = "t03-%s-%s" % (self.run_id, ORG_LABELS[label])
                response = self.client.call(
                    check, "create-org-%s" % label, "POST", "/api/v1/admin/organizations",
                    body={"name": name, "owner_email": self.users[owner]["email"]}, session=admin_session)
                expect_status(check, response, 201, label="create-org-%s" % label)
                payload = response.json() or {}
                org = payload.get("organization") or {}
                if str(payload.get("role")) != "owner":
                    raise CheckFailure("create-org-%s: 指定 owner 的角色应为 owner，实际 %r"
                                       % (label, payload.get("role")))
                org_id = str(org.get("id", ""))
                if not org_id:
                    raise CheckFailure("create-org-%s: response missing organization.id" % label)
                if self.args.seed_local_test_credit and org.get("type") != "enterprise":
                    raise CheckFailure("--seed-local-test-credit target is not a newly created enterprise")
                if (self.args.seed_local_test_credit and label == "A" and
                        ("model_allowlist" not in org or org.get("model_allowlist") is not None)):
                    raise CheckFailure("--seed-local-test-credit requires the new enterprise to inherit platform models")
                self.orgs[label] = {"id": org_id, "name": org.get("name", name)}
                self.created_orgs.add(label)
                created_any = True
                self.save_state()

        # Members join through the ordinary application workflow; the enterprise owner approves.
        for label in ("A", "B"):
            owner = "%s-owner" % label.lower()
            member_path = self.org_path(label, "/members")
            response = self.client.call(check, "members-before-join-%s" % label, "GET", member_path,
                                        session=self.sessions[owner])
            expect_status(check, response, 200, label="members-before-join-%s" % label)
            current = {str(item.get("user_id")): item for item in (response.json() or {}).get("items", [])}
            for actor, (actor_org, role) in ACTOR_ROLES.items():
                if actor_org != label or role == "owner":
                    continue
                if self.user_id(actor) not in current:
                    response = self.client.call(
                        check, "join-request-%s" % actor, "POST",
                        self.org_path(label, "/join-requests"),
                        body={"display_name": actor}, session=self.sessions[actor])
                    expect_status(check, response, 201, label="join-request-%s" % actor)
                    request_id = str((response.json() or {}).get("id", ""))
                    if not request_id:
                        raise CheckFailure("join-request-%s: 响应缺少 id" % actor)
                    decision = self.client.call(
                        check, "approve-%s" % actor, "POST",
                        self.org_path(label, "/join-requests/%s/decision" % request_id),
                        body={"status": "approved", "monthly_limit_micro": "100000000"},
                        session=self.sessions[owner])
                    expect_status(check, decision, 200, label="approve-%s" % actor)
                    created_any = True
                    self.save_state()
                if role == "admin":
                    response = self.client.call(
                        check, "promote-%s" % actor, "PATCH",
                        self.org_path(label, "/members/%s" % self.user_id(actor)),
                        body={"role": "admin"}, session=self.sessions[owner])
                    expect_status(check, response, 204, label="promote-%s" % actor)

        if created_any:
            log("fixtures: run_id=%s orgA=%s orgB=%s users=%d" % (
                self.run_id, self.orgs["A"]["id"], self.orgs["B"]["id"], len(self.users)))
        else:
            log("fixtures: 复用 state file 中的既有 fixture（run_id=%s）" % self.run_id)
        if self.args.seed_local_test_credit and "A" not in self.created_orgs:
            raise InfraFailure("--seed-local-test-credit 仅允许给本次新建的测试企业 A 充值")
        self.write_redacted_fixtures()
        self.actor_sessions_check(check)

    def actor_sessions_check(self, check):
        """Every fixture session must still be alive; a stale session is infra."""
        for actor in sorted(self.sessions):
            if actor == "admin":
                continue
            response = self.client.call(check, "me-%s" % actor, "GET", "/api/v1/me", session=self.sessions[actor])
            if response.status == 401:
                session, _ = login(self.client, check, self.users[actor]["email"], self.password, "relogin-%s" % actor)
                self.sessions[actor] = session
            elif response.status != 200:
                raise InfraFailure("me-%s: 期望 200，实际 %d" % (actor, response.status))

    def control_probe(self, check):
        """GET the org key list as the owner: distinguishes 'no control channel'
        (503 unavailable) from a broken tunnel (502 upstream_unavailable)."""
        if self.control_available is not None:
            return self.control_available
        response = self.client.call(check, "probe-control-keys", "GET", self.org_path("A", "/keys"),
                                    query="limit=1", session=self.sessions["a-owner"])
        if response.status == 200:
            self.control_available = True
        elif response.status == 503 and response.code() == "unavailable":
            self.control_available = False
        elif response.status == 502:
            raise CheckFailure("Console → Control 通道已配置但不可用（502 upstream_unavailable）："
                               "T03 Key 端点无法验收")
        else:
            raise CheckFailure("probe keys: 期望 200/503，实际 %d code=%s message=%s"
                               % (response.status, response.code(), response.message()))
        return self.control_available

    def leak_tokens(self, label):
        tokens = []
        org = self.orgs.get(label, {})
        if org.get("name"):
            tokens.append(org["name"])
        for actor, role in ACTOR_ROLES.items():
            if role[0] != label:
                continue
            tokens.append(self.users[actor]["email"])
        for key, item in self.keys.items():
            if item.get("org") == label:
                for field in ("key_id", "prefix"):
                    if item.get(field):
                        tokens.append(str(item[field]))
        return tokens


def read_password(path):
    with open(path, "r", encoding="utf-8") as handle:
        value = handle.read().strip()
    if not value:
        raise InfraFailure("密码文件为空: %s" % path)
    return value


# ---------------------------------------------------------------------------
# checks
# ---------------------------------------------------------------------------

def check01(env, check):
    """登录、fixture 就绪、角色矩阵读取一致性。"""
    client = env.client
    for label in ("A", "B"):
        for actor in ("%s-owner" % label.lower(), "%s-admin" % label.lower()):
            response = client.call(check, "members-%s" % actor, "GET", env.org_path(label, "/members"),
                                   session=env.session(actor))
            expect_status(check, response, 200, label="members-%s" % actor)
            members = {item["email"]: item for item in (response.json() or {}).get("items", [])}
            for expected_actor, (actor_org, role) in ACTOR_ROLES.items():
                if actor_org != label:
                    continue
                email = env.email(expected_actor)
                check.require(email in members, "%s 的成员列表缺少 %s" % (label, email))
                check.require(members[email]["role"] == role,
                              "%s 中 %s 的角色应为 %s，实际 %s" % (label, email, role, members[email]["role"]))
                check.require(members[email]["status"] == "active",
                              "%s 中 %s 应为 active，实际 %s" % (label, email, members[email]["status"]))

        for suffix in ("member", "member2"):
            actor = "%s-%s" % (label.lower(), suffix)
            response = client.call(check, "members-%s-self" % actor, "GET", env.org_path(label, "/members"),
                                   session=env.session(actor))
            expect_status(check, response, 200, label="members-%s(self)" % actor)
            items = (response.json() or {}).get("items", [])
            check.require(len(items) == 1 and str(items[0].get("user_id")) == env.user_id(actor),
                          "%s: member 的成员列表必须只含自己，实际 %r"
                          % (actor, [item.get("user_id") for item in items]))

    response = client.call(check, "me-a-owner", "GET", "/api/v1/me", session=env.session("a-owner"))
    expect_status(check, response, 200, label="me-a-owner")
    orgs = {str(item["organization"]["id"]): item["role"]
            for item in (response.json() or {}).get("organizations", [])}
    check.require(orgs.get(env.org_id("A")) == "owner",
                  "/me 的 A 组织角色应为 owner，实际 %r" % orgs.get(env.org_id("A")))
    check.note("每家企业 owner/admin 可管理全体成员；普通成员只看到本人关系")

def check02(env, check):
    """跨组织读取必须 404，且与“组织不存在”逐字节同形、无元数据泄露。"""
    client = env.client
    unknown = "99999999"
    leak_a = env.leak_tokens("A")
    leak_b = env.leak_tokens("B")
    control_ok = env.control_probe(check)

    cross = [
        ("a-owner", "B", "/members", None, "members"),
        ("a-owner", "B", "/invitations", None, "invitations"),
        ("a-owner", "B", "/audit", "limit=1", "audit"),
        ("a-owner", "B", "/usage", "limit=1", "usage"),
        ("b-owner", "A", "/members", None, "members"),
        ("b-owner", "A", "/invitations", None, "invitations"),
    ]
    if control_ok:
        cross.insert(2, ("a-owner", "B", "/keys", "limit=1", "keys"))
    for actor, target, suffix, query, name in cross:
        response = client.call(check, "cross-%s-%s-%s" % (actor, target, name), "GET",
                               env.org_path(target, suffix), query=query, session=env.session(actor))
        require_envelope(check, response, 404, "not_found", "%s 读 %s%s" % (actor, target, suffix))
        check.require(response.message() == "organization not found",
                      "%s 读 %s%s: message 应为 organization not found，实际 %r"
                      % (actor, target, suffix, response.message()))
        for token in (leak_b if target == "B" else leak_a):
            check.require(token not in response.body,
                          "%s 读 %s%s: 响应体泄露了 %s 的元数据 %r" % (actor, target, suffix, target, token))

    # 与“不存在的组织”对比：状态码/错误码/message 必须一致（存在性不可区分）
    baseline = client.call(check, "unknown-org-members", "GET", "/api/v1/orgs/%s/members" % unknown,
                           session=env.session("a-owner"))
    require_envelope(check, baseline, 404, "not_found", "unknown-org members")
    other = client.call(check, "cross-a-owner-B-members-compare", "GET", env.org_path("B", "/members"),
                        session=env.session("a-owner"))
    require_envelope(check, other, 404, "not_found", "cross-org members compare")
    check.require(baseline.message() == other.message() and baseline.code() == other.code(),
                  "另一个组织的 404 与不存在的组织不同形：%r vs %r" % (other.message(), baseline.message()))
    check.require(unknown not in other.body, "跨组织 404 响应体里出现了目标组织 id")
    check.require(other.header("X-Content-Type-Options") == "nosniff",
                  "404 响应缺少 X-Content-Type-Options: nosniff")
    check.note("跨组织读取统一 404 not_found/organization not found，与非存在组织同形")


def check03(env, check):
    """跨组织写入必须 404，且目标组织状态不变。"""
    client = env.client
    org_b = env.org_id("B")
    b_member = env.user_id("b-member")
    b_owner = env.user_id("b-owner")

    # 先由 B 的 owner 建一把 Key，作为跨组织撤销/回查的目标
    b_key_id = None
    b_operation = None
    if env.control_probe(check):
        b_operation = "t03-%s-b-owner-key" % env.pass_id
        response = client.call(check, "b-owner-create-key", "POST", env.org_path("B", "/keys"),
                               body={"name": "t03-b-owner", "operation_id": b_operation}, session=env.session("b-owner"))
        expect_status(check, response, 201, label="b-owner-create-key")
        payload = response.json() or {}
        b_key_id = (payload.get("key") or {}).get("key_id")
        secret = payload.get("secret", "")
        if secret:
            client.red.add(secret)
            env.secrets_to_scan.append(("b-owner-key-secret", secret, ["b-owner-create-key"]))
        env.keys["b-owner-key"] = {"org": "B", "key_id": b_key_id, "prefix": (payload.get("key") or {}).get("prefix")}
        check.require(b_key_id, "b-owner-create-key: 响应缺少 key.key_id")

    writes = [
        ("a-owner", "b-members-patch", "PATCH", env.org_path("B", "/members/%s" % b_member), {"role": "member"}),
        ("a-owner", "b-members-delete", "DELETE", env.org_path("B", "/members/%s" % b_member), None),
        ("a-owner", "b-invitations-create", "POST", env.org_path("B", "/invitations"),
         {"email": "t03-%s-intruder@t03.test" % env.run_id, "role": "member", "monthly_limit_micro": "100000000"}),
        # 目标必须不是调用者自己：TransferOwnership 先做 self-target 校验（400），
        # 再做成员关系校验（404），见 README 的“偏差观察”。
        ("a-owner", "b-owner-transfer", "POST", env.org_path("B", "/owner-transfer"), {"user": b_member}),
    ]
    if b_key_id:
        writes.append(("a-owner", "b-keys-revoke", "POST", env.org_path("B", "/keys/%s/revoke" % b_key_id), None))
        writes.append(("a-member", "b-keys-create", "POST", env.org_path("B", "/keys"), {"name": "cross-org"}))
    for actor, name, method, path, body in writes:
        response = client.call(check, "cross-write-%s" % name, method, path, body=body, session=env.session(actor))
        require_envelope(check, response, 404, "not_found", "cross-write %s" % name)
        check.require(response.message() == "organization not found",
                      "cross-write %s: message 应为 organization not found，实际 %r" % (name, response.message()))

    # 偏差观察（非硬失败）：非成员把 owner 转给自己时，服务先做 self-target 校验，
    # 返回 400 invalid_request 而不是 404；该判断与组织是否存在无关，因此不泄露
    # 组织存在性，但与 "非成员一律 404" 的映射顺序不同。
    self_transfer = client.call(check, "cross-write-b-owner-transfer-self", "POST",
                                env.org_path("B", "/owner-transfer"), body={"user": env.user_id("a-owner")},
                                session=env.session("a-owner"))
    check.details["self_transfer_status"] = self_transfer.status
    check.details["self_transfer_code"] = self_transfer.code()
    check.details["self_transfer_message"] = self_transfer.message()
    if self_transfer.status != 404:
        check.note("偏差观察：非成员 self-target owner-transfer → %d %s（%r）；无存在性泄露，"
                   "但与 ADR-0003 §2.5 的 404 映射顺序不同"
                   % (self_transfer.status, self_transfer.code(), self_transfer.message()))

    if b_key_id:
        response = client.call(check, "cross-recover-b-operation", "GET", env.org_path("B", "/keys"),
                               query="operation_id=" + urllib.parse.quote(b_operation), session=env.session("a-owner"))
        require_envelope(check, response, 404, "not_found", "cross-org operation recovery")
        check.require(b_key_id not in response.body, "跨组织 operation 回查泄露了 B 的 key_id")

    # B 的状态必须原封不动
    response = client.call(check, "b-owner-members-after", "GET", env.org_path("B", "/members"),
                           session=env.session("b-owner"))
    expect_status(check, response, 200, label="b-owner members after")
    members = {item["email"]: item for item in (response.json() or {}).get("items", [])}
    for actor, (label, role) in ACTOR_ROLES.items():
        if label != "B":
            continue
        check.require(members.get(env.email(actor), {}).get("role") == role,
                      "B 的 %s 角色被跨组织请求改动了" % actor)
        check.require(members.get(env.email(actor), {}).get("status") == "active",
                      "B 的 %s 状态被跨组织请求改动了" % actor)
    check.require("t03-%s-intruder@t03.test" % env.run_id not in members, "跨组织邀请在 B 创建了成员")

    if b_key_id:
        response = client.call(check, "b-owner-keys-after", "GET", env.org_path("B", "/keys"),
                               query="limit=100", session=env.session("b-owner"))
        expect_status(check, response, 200, label="b-owner keys after")
        keys = {item["key_id"]: item for item in (response.json() or {}).get("items", [])}
        check.require(keys.get(b_key_id, {}).get("status") == "active", "跨组织撤销把 B 的 Key 改成非 active")
    check.note("跨组织写入一律 404；B 的成员/角色/邀请/Key 均未被改动")


def check04(env, check):
    """普通成员与企业管理员的权限边界。"""
    client = env.client
    member = env.session("a-member")
    other_member_id = env.user_id("a-member2")
    admin = env.session("a-admin")
    owner = env.session("a-owner")

    denials = [
        ("member-invite", member, "POST", env.org_path("A", "/invitations"),
         {"email": "t03-%s-x1@t03.test" % env.run_id, "role": "member", "monthly_limit_micro": "100000000"}, 403, "forbidden"),
        ("member-manage-member", member, "PATCH", env.org_path("A", "/members/%s" % other_member_id),
         {"status": "suspended"}, 403, "forbidden"),
        ("member-remove", member, "DELETE", env.org_path("A", "/members/%s" % other_member_id),
         None, 403, "forbidden"),
        ("member-audit", member, "GET", env.org_path("A", "/audit"), None, 403, "forbidden"),
        ("member-list-invitations", member, "GET", env.org_path("A", "/invitations"), None, 403, "forbidden"),
        ("admin-invite-admin", admin, "POST", env.org_path("A", "/invitations"),
         {"email": "t03-%s-x3@t03.test" % env.run_id, "role": "admin"}, 403, "forbidden"),
        ("admin-invite-owner", admin, "POST", env.org_path("A", "/invitations"),
         {"email": "t03-%s-x4@t03.test" % env.run_id, "role": "owner"}, 403, "forbidden"),
        ("admin-change-owner-quota", admin, "PATCH",
         env.org_path("A", "/members/%s/quota" % env.user_id("a-owner")),
         {"monthly_limit_micro": "500"}, 403, "forbidden"),
        ("owner-invite-platform-admin", owner, "POST", env.org_path("A", "/invitations"),
         {"email": "t03-%s-x6@t03.test" % env.run_id, "role": "platform_admin"}, 403, "forbidden"),
        ("owner-grant-platform-admin", owner, "PATCH",
         env.org_path("A", "/members/%s" % env.user_id("a-member")),
         {"role": "platform_admin"}, 403, "forbidden"),
        ("owner-grant-bogus-role", owner, "PATCH",
         env.org_path("A", "/members/%s" % env.user_id("a-member")),
         {"role": "superuser"}, 400, "invalid_request"),
        ("owner-invite-bogus-role", owner, "POST", env.org_path("A", "/invitations"),
         {"email": "t03-%s-x7@t03.test" % env.run_id, "role": "superuser"}, 400, "invalid_request"),
        ("owner-invite-member-missing-cap", owner, "POST", env.org_path("A", "/invitations"),
         {"email": "t03-%s-x10@t03.test" % env.run_id, "role": "member"}, 400, "invalid_request"),
        ("owner-invite-bad-ttl", owner, "POST", env.org_path("A", "/invitations"),
         {"email": "t03-%s-x8@t03.test" % env.run_id, "role": "member", "monthly_limit_micro": "100000000", "expires_in_seconds": 5}, 400, "invalid_request"),
        ("member-cross-user-usage", member, "GET", env.org_path("A", "/usage"),
         None, 403, "forbidden", "user=" + env.user_id("a-owner")),
    ]
    for entry in denials:
        name, session, method, path, body, status, code = entry[:7]
        query = entry[7] if len(entry) > 7 else None
        response = client.call(check, "deny-%s" % name, method, path, body=body, query=query, session=session)
        require_envelope(check, response, status, code, "deny %s" % name)

    owner_invite_owner = client.call(check, "deny-owner-invite-owner", "POST", env.org_path("A", "/invitations"),
                                     body={"email": "t03-%s-owner2@t03.test" % env.run_id, "role": "owner"}, session=owner)
    require_envelope(check, owner_invite_owner, 403, "forbidden", "owner 不能邀请新的 owner")
    check.note("普通成员不能管理成员、邀请或审计；管理员不能调整自身额度或授予管理员/Owner；平台角色只由平台管理")

    # Every active member can manage only personal Keys. The list/revoke privacy boundary is checked in check07.
    if not env.control_probe(check):
        check.note("Console 未接 Control（key 端点 503），Key 权限在 check07 中记为 SKIP")

def check05(env, check):
    """最后 owner 保护：admin 不能动 owner、不能自升 owner；最后一个 owner 不能离开。"""
    client = env.client
    admin = env.session("a-admin")
    owner = env.session("a-owner")
    owner_id = env.user_id("a-owner")
    admin_id = env.user_id("a-admin")

    cases = [
        ("admin-demote-owner", admin, "PATCH", env.org_path("A", "/members/%s" % owner_id), {"role": "admin"}, 403, "forbidden"),
        ("admin-remove-owner", admin, "DELETE", env.org_path("A", "/members/%s" % owner_id), None, 403, "forbidden"),
        ("admin-self-promote", admin, "PATCH", env.org_path("A", "/members/%s" % admin_id), {"role": "owner"}, 403, "forbidden"),
        ("admin-transfer-other", admin, "POST", env.org_path("A", "/owner-transfer"),
         {"user": env.user_id("a-member")}, 403, "forbidden"),
        ("owner-leave", owner, "DELETE", env.org_path("A", "/members/%s" % owner_id), None, 409, "conflict"),
        ("owner-self-demote", owner, "PATCH", env.org_path("A", "/members/%s" % owner_id), {"role": "member", "monthly_limit_micro": None}, 409, "conflict"),
    ]
    for name, session, method, path, body, status, code in cases:
        response = client.call(check, "owner-protect-%s" % name, method, path, body=body, session=session)
        require_envelope(check, response, status, code, "owner-protect %s" % name)

    # 偏差观察：self-target 的 owner-transfer 在任何角色下都先被请求校验拦成 400
    # （TransferOwnership 在成员关系校验之前判断 target==actor），因此 admin
    # 自升 owner 走转移路径时拿到的是 400 而不是 403；admin 通过 PATCH 自升仍为
    # 403，且两条路径都没有产生任何角色变更。
    self_transfer = client.call(check, "owner-protect-admin-self-transfer", "POST",
                                env.org_path("A", "/owner-transfer"), body={"user": admin_id}, session=admin)
    check.details["admin_self_transfer_status"] = self_transfer.status
    check.details["admin_self_transfer_code"] = self_transfer.code()
    check.require(self_transfer.status in (400, 403),
                  "admin 自升 owner 既不是 400 也不是 403：%d" % self_transfer.status)
    if self_transfer.status == 400:
        check.note("偏差观察：admin self-target owner-transfer → 400 %s（%r）；PATCH 自升同名角色为 403，"
                   "两条路径都未改变角色" % (self_transfer.code(), self_transfer.message()))

    response = client.call(check, "owner-protect-members-after", "GET", env.org_path("A", "/members"),
                           session=owner)
    expect_status(check, response, 200, label="members after owner-protect")
    members = {item["email"]: item for item in (response.json() or {}).get("items", [])}
    check.require(members.get(env.email("a-owner"), {}).get("role") == "owner",
                  "owner 被降级/移除后不再是 owner")
    check.require(members.get(env.email("a-owner"), {}).get("status") == "active", "owner 被移除")
    check.require(members.get(env.email("a-admin"), {}).get("role") == "admin", "admin 角色被意外改动")

    # 409 的语义检查：最后 owner 的拒绝必须是 conflict 而不是 forbidden/404
    last = client.call(check, "owner-protect-leave-envelope", "DELETE", env.org_path("A", "/members/%s" % owner_id),
                       session=owner)
    require_envelope(check, last, 409, "conflict", "owner leave envelope")
    check.require("owner" in (last.message() or ""), "409 message 未说明最后 owner 约束: %r" % last.message())
    check.note("最后一个 owner 的离开/自我降级 → 409 conflict；admin 动 owner → 403")


def check06(env, check):
    """邀请：一次性、email/role 不可篡改、TTL 边界。"""
    client = env.client
    owner = env.session("a-owner")
    invitee = INVITEE_ACTOR
    invitee_email = env.email(invitee)
    invitee_session = env.session(invitee)

    response = client.call(check, "invite-invitee", "POST", env.org_path("A", "/invitations"),
                           body={"email": invitee_email, "role": "member", "monthly_limit_micro": "100000000", "expires_in_seconds": 3600},
                           session=owner)
    expect_status(check, response, 201, label="invite invitee")
    payload = response.json() or {}
    token = payload.get("token", "")
    invitation = payload.get("invitation") or {}
    check.require(bool(token), "create invitation 响应缺少 token（secret 只出现一次）")
    client.red.add(token)
    env.secrets_to_scan.append(("invitation-token-invitee", token, ["invite-invitee"]))
    check.require(invitation.get("email") == invitee_email, "邀请记录的 email 与请求不一致")
    check.require(invitation.get("role") == "member", "邀请记录的 role 与请求不一致")
    check.require(invitation.get("status") == "pending", "新邀请状态应为 pending，实际 %r" % invitation.get("status"))

    # 列表只返回元数据：没有 token、没有 hash
    response = client.call(check, "list-invitations-owner", "GET", env.org_path("A", "/invitations"), session=owner)
    expect_status(check, response, 200, label="list invitations")
    body = response.body
    check.require(token not in body, "邀请列表泄露了 raw token")
    check.require("token_hash" not in body and "token_hash" not in json.dumps(payload),
                  "邀请对象里出现了 token_hash 字段")
    listed = [item for item in (response.json() or {}).get("items", []) if item.get("email") == invitee_email]
    check.require(listed, "邀请列表里找不到刚创建的邀请")
    check.require("token" not in listed[-1], "邀请列表项包含 token 字段")

    # 错 email：B 的成员拿同一个 token 去接受 → 403，且不产生成员
    wrong = client.call(check, "accept-wrong-email", "POST",
                        "/api/v1/invitations/%s/accept" % urllib.parse.quote(token),
                        body={}, session=env.session("b-member"))
    require_envelope(check, wrong, 403, "forbidden", "accept wrong email")
    check.require("different email" in (wrong.message() or ""),
                  "错 email 的拒绝 message 应说明 email 不匹配，实际 %r" % wrong.message())

    # 接受请求体不能改 email/role：带 owner/他人 email 的 body 被忽略
    accepted = client.call(check, "accept-invitee-spoofed-body", "POST",
                           "/api/v1/invitations/%s/accept" % urllib.parse.quote(token),
                           body={"email": env.email("b-admin"), "role": "owner"}, session=invitee_session)
    expect_status(check, accepted, 200, label="accept invitation")
    membership = accepted.json() or {}
    check.require(str(membership.get("role")) == "member",
                  "接受请求体把角色改成了 %r（应为邀请固定的 member）" % membership.get("role"))
    check.require(str(membership.get("organization_id")) == env.org_id("A"),
                  "接受的成员关系不属于目标组织 A")
    check.require(str(membership.get("user_id")) == env.user_id(invitee), "接受的成员不是邀请 email 对应的用户")
    check.require(membership.get("monthly_limit_micro") == "100000000",
                  "接受邀请后没有保留邀请时明确设定的月额度")

    # 一次性：同 token 再来一次 → 409
    reuse = client.call(check, "accept-invitee-reuse", "POST",
                        "/api/v1/invitations/%s/accept" % urllib.parse.quote(token),
                        body={}, session=invitee_session)
    require_envelope(check, reuse, 409, "conflict", "reuse invitation")

    # 未知 token → 404（不泄露 token 是否存在以外的信息）
    unknown = client.call(check, "accept-unknown-token", "POST",
                          "/api/v1/invitations/%s/accept" % urllib.parse.quote("t03-not-a-real-token-" + env.run_id),
                          body={}, session=invitee_session)
    require_envelope(check, unknown, 404, "not_found", "unknown invitation token")

    # TTL 上界越界 → 400（下界 5s 已在 check04 覆盖）
    ttl = client.call(check, "invite-ttl-too-long", "POST", env.org_path("A", "/invitations"),
                      body={"email": "t03-%s-x9@t03.test" % env.run_id, "role": "member",
                            "monthly_limit_micro": "100000000", "expires_in_seconds": 99999999}, session=owner)
    require_envelope(check, ttl, 400, "invalid_request", "invitation ttl too long")

    # 成员关系生效且角色固定
    response = client.call(check, "members-after-invite", "GET", env.org_path("A", "/members"), session=owner)
    expect_status(check, response, 200, label="members after invite")
    members = {item["email"]: item for item in (response.json() or {}).get("items", [])}
    check.require(members.get(invitee_email, {}).get("role") == "member",
                  "接受邀请后角色不是邀请固定的 member")
    check.note("同 token 二次接受 → 409；接受请求体的 email/role 被完全忽略")


def check07(env, check):
    """每人自管多把有名称的 Key；secret 只出现一次，管理角色看不到成员 Key。"""
    client = env.client
    if not env.control_probe(check):
        check.skip("Console 未接 Control（GET /keys → 503 unavailable）：Key 端点未验收")
    member = env.session("a-member")
    admin = env.session("a-admin")
    owner = env.session("a-owner")
    member_id = env.user_id("a-member")
    admin_id = env.user_id("a-admin")

    op_member = "t03-%s-member-own" % env.pass_id
    key_name = "t03-notebook"
    created = client.call(check, "member-create-own-key", "POST", env.org_path("A", "/keys"),
                          body={"name": key_name, "operation_id": op_member}, session=member)
    expect_status(check, created, 201, label="member create own key")
    payload = created.json() or {}
    key = payload.get("key") or {}
    secret = payload.get("secret", "")
    member_key_id = str(key.get("key_id", ""))
    check.require(bool(secret) and bool(member_key_id), "create key 响应缺少 raw secret 或 key_id")
    check.require(key.get("name") == key_name, "Key 名称未按创建请求保存")
    check.require(payload.get("secret_returned_once") is True, "secret_returned_once 应为 true")
    check.require(payload.get("replayed") is False, "首次 create 的 replayed 应为 false")
    check.require(str(key.get("organization_id")) == env.org_id("A"), "Key 的 organization_id 不是 A")
    check.require(key.get("status") == "active", "新 Key 状态应为 active")
    client.red.add(secret)
    env.secrets_to_scan.append(("a-member-own-key-secret", secret, ["member-create-own-key"]))
    env.keys["a-member-own"] = {"org": "A", "key_id": member_key_id, "prefix": key.get("prefix")}

    # Retrying the same create must return metadata only; changing the same operation's payload conflicts.
    replay = client.call(check, "member-replay-key", "POST", env.org_path("A", "/keys"),
                         body={"name": key_name, "operation_id": op_member}, session=member)
    expect_status(check, replay, 201, label="replay create key")
    replay_payload = replay.json() or {}
    check.require(replay_payload.get("replayed") is True, "同 operation_id 重放应 replayed=true")
    check.require("secret" not in replay_payload, "重放响应不应再携带 secret 字段")
    check.require((replay_payload.get("key") or {}).get("key_id") == member_key_id,
                  "重放返回了另一把 Key")

    conflict = client.call(check, "member-key-op-conflict", "POST", env.org_path("A", "/keys"),
                           body={"name": "different-name", "operation_id": op_member}, session=member)
    require_envelope(check, conflict, 409, "conflict", "operation_id payload conflict")

    recovered = client.call(check, "member-recover-by-operation", "GET", env.org_path("A", "/keys"),
                            query="operation_id=" + urllib.parse.quote(op_member), session=member)
    expect_status(check, recovered, 200, label="recover key by operation_id")
    recovered_payload = recovered.json() or {}
    check.require(recovered_payload.get("replayed") is True, "恢复路径必须标记 replayed=true")
    check.require("secret" not in recovered_payload, "恢复路径不得返回 secret")
    recovered_items = recovered_payload.get("items") or []
    check.require(len(recovered_items) == 1 and recovered_items[0].get("key_id") == member_key_id,
                  "恢复路径没有返回原 Key 的元数据")
    check.require(recovered_items[0].get("name") == key_name, "恢复路径没有返回本人的 Key 名称")
    check.require("secret" not in json.dumps(recovered_payload), "恢复响应里出现了 secret 字段")

    # A second named key verifies that cursors expose every one of the member's keys.
    second_op = "t03-%s-member-second" % env.pass_id
    second = client.call(check, "member-create-second-key", "POST", env.org_path("A", "/keys"),
                         body={"name": "t03-playground", "operation_id": second_op}, session=member)
    expect_status(check, second, 201, label="member create second key")
    second_payload = second.json() or {}
    second_key = second_payload.get("key") or {}
    second_secret = second_payload.get("secret", "")
    second_key_id = str(second_key.get("key_id", ""))
    check.require(second_secret and second_key_id and second_key.get("name") == "t03-playground",
                  "第二把 Key 没有独立 ID、secret 或名称")
    client.red.add(second_secret)
    env.secrets_to_scan.append(("a-member-second-key-secret", second_secret, ["member-create-second-key"]))
    env.keys["a-member-second"] = {"org": "A", "key_id": second_key_id, "prefix": second_key.get("prefix")}

    member_page1 = client.call(check, "member-key-page1", "GET", env.org_path("A", "/keys"),
                               query="limit=1", session=member)
    expect_status(check, member_page1, 200, label="member key page 1")
    page1 = member_page1.json() or {}
    cursor = page1.get("next_cursor")
    check.require(cursor, "两把 Key 时 limit=1 应返回 next_cursor")
    member_page2 = client.call(check, "member-key-page2", "GET", env.org_path("A", "/keys"),
                               query="limit=1&cursor=" + urllib.parse.quote(str(cursor)), session=member)
    expect_status(check, member_page2, 200, label="member key page 2")
    page2 = member_page2.json() or {}
    member_rows = (page1.get("items") or []) + (page2.get("items") or [])
    check.require({str(item.get("key_id")) for item in member_rows} >= {member_key_id, second_key_id},
                  "游标分页没有提供 member 的两把 Key")
    check.require({str(item.get("name")) for item in member_rows} >= {key_name, "t03-playground"},
                  "Key 列表缺少用户设置的名称")
    check.require(all("secret" not in item for item in member_rows), "Key 列表项包含 secret")

    # Other users' operation recovery and revoke attempts are intentionally indistinguishable from missing Keys.
    other_recovery = client.call(check, "admin-recover-member-operation", "GET", env.org_path("A", "/keys"),
                                 query="operation_id=" + urllib.parse.quote(op_member), session=admin)
    require_envelope(check, other_recovery, 404, "not_found", "another user recovers member operation")
    admin_op = "t03-%s-admin-own" % env.pass_id
    admin_created = client.call(check, "admin-create-own-key", "POST", env.org_path("A", "/keys"),
                                 body={"name": "t03-admin-console", "operation_id": admin_op}, session=admin)
    expect_status(check, admin_created, 201, label="admin creates own key")
    admin_payload = admin_created.json() or {}
    admin_key = admin_payload.get("key") or {}
    admin_secret = admin_payload.get("secret", "")
    admin_key_id = str(admin_key.get("key_id", ""))
    check.require(admin_secret and admin_key_id and admin_key.get("name") == "t03-admin-console",
                  "admin 创建的本人 Key 缺少名称、ID 或 secret")
    client.red.add(admin_secret)
    env.secrets_to_scan.append(("a-admin-own-key-secret", admin_secret, ["admin-create-own-key"]))
    env.keys["a-admin-own"] = {"org": "A", "key_id": admin_key_id, "prefix": admin_key.get("prefix")}
    other_revoke = client.call(check, "member-revoke-admin-key", "POST",
                               env.org_path("A", "/keys/%s/revoke" % admin_key_id), session=member)
    require_envelope(check, other_revoke, 404, "not_found", "member revokes admin's key")
    check.require(admin_key_id not in other_revoke.body, "拒绝撤销时回显了他人的 Key ID")

    # Explicit user filters and other-user lists/recovery never reveal a member Key to owner/admin.
    for actor in ("a-admin", "a-owner"):
        response = client.call(check, "%s-list-keys" % actor, "GET", env.org_path("A", "/keys"),
                               query="limit=100", session=env.session(actor))
        expect_status(check, response, 200, label="%s own key list" % actor)
        rows = (response.json() or {}).get("items", [])
        check.require(all(str(item.get("key_id")) != member_key_id and str(item.get("key_id")) != second_key_id
                          for item in rows), "%s 看到了成员的 Key 元数据" % actor)
        check.require(all("secret" not in item for item in rows), "%s 的 Key 列表包含 secret" % actor)
        check.require(member_key_id not in response.body and "t03-notebook" not in response.body,
                      "%s 的响应泄露了成员 Key ID 或名称" % actor)
    filtered = client.call(check, "member-filter-other-key", "GET", env.org_path("A", "/keys"),
                           query="owner_user_id=" + admin_id, session=member)
    require_envelope(check, filtered, 404, "not_found", "member filters another user's keys")
    check.require(admin_key_id not in filtered.body and "t03-admin-console" not in filtered.body,
                  "他人 Key 过滤响应泄露了 Key 元数据")

    # The owner also cannot recover a member's operation, even though owner/admin can see member records.
    owner_recovery = client.call(check, "owner-recover-member-operation", "GET", env.org_path("A", "/keys"),
                                 query="operation_id=" + urllib.parse.quote(op_member), session=owner)
    require_envelope(check, owner_recovery, 404, "not_found", "owner recovers member operation")

    # A member may revoke only their own Key; repeat revoke remains idempotent.
    revoked = client.call(check, "member-revoke-own-key", "POST",
                          env.org_path("A", "/keys/%s/revoke" % second_key_id), session=member)
    expect_status(check, revoked, 200, label="member revokes own key")
    check.require(((revoked.json() or {}).get("key") or {}).get("status") == "revoked",
                  "撤销后 Key 状态不是 revoked")
    again = client.call(check, "member-revoke-own-key-again", "POST",
                        env.org_path("A", "/keys/%s/revoke" % second_key_id), session=member)
    expect_status(check, again, 200, label="member re-revokes own key")
    check.require(((again.json() or {}).get("key") or {}).get("status") == "revoked",
                  "重复撤销改变了状态")

    # Each secret is captured exactly once, in the corresponding create response.
    for label, value in (("member", secret), ("second member", second_secret), ("admin", admin_secret)):
        hits = [record for record in client.records if value and value in record["body"]]
        check.require(len(hits) == 1, "%s raw secret 出现在 %d 个响应里" % (label, len(hits)))
    check.note("owner/admin/member 各自只管理本人 Key；多把命名 Key 可翻页，成员 Key 元数据不进入管理视图")

def poll_status(check, client, label, method, path, *, expect, timeout=15.0, interval=1.0, **kwargs):
    """Repeat one request until it reaches `expect` (snapshot/metering are async)."""
    deadline = time.time() + timeout
    attempt = 0
    last = None
    while True:
        attempt += 1
        last = client.call(check, "%s-wait%02d" % (label, attempt), method, path, **kwargs)
        if last.status == expect or time.time() >= deadline:
            return last
        time.sleep(interval)


def check08(env, check):
    """Gateway access guard：本人的 active Key 放行，撤销后拒绝，计量归属可追溯。"""
    if not env.args.gateway_url:
        check.skip("未提供 --gateway-url：Gateway access guard 与计量归属未验收")
    client = env.client
    gateway = env.args.gateway_url
    model = env.args.gateway_model
    member = env.session("a-member")
    second_member = env.session("a-member2")
    admin = env.session("a-admin")

    ready = client.call(check, "gateway-readyz", "GET", "/readyz", base_url=gateway, csrf=None)
    expect_status(check, ready, 200, label="gateway readyz")

    if env.args.seed_local_test_credit:
        if "A" not in env.created_orgs:
            raise InfraFailure("--seed-local-test-credit 仅允许给本次新建的测试企业 A 充值")
        # This explicit opt-in is limited to a fresh, loopback-only fixture.
        # The stable operation/reference pair makes a same-run retry idempotent.
        funding_suffix = hashlib.sha256(env.run_id.encode("utf-8")).hexdigest()[:24]
        funding_key = "t03-check08-funding-" + funding_suffix
        payment_reference = "t03-check08-" + funding_suffix
        funded = client.call(
            check, "gateway-fund-org-a", "POST",
            "/api/v1/admin/billing/accounts/%s/adjustments" % env.org_id("A"),
            body={"type": "recharge", "amount_micro": "100000000",
                  "payment_reference": payment_reference,
                  "reason": "T03 Check08 Gateway access guard acceptance funding"},
            session=env.session("admin"),
            extra_headers={"Idempotency-Key": funding_key})
        if funded.status not in (200, 201):
            raise CheckFailure("POST fund organization A for Check08: expected HTTP 200 or 201, "
                               "got %d code=%s message=%s" % (funded.status, funded.code(), funded.message()))
        entry = (funded.json() or {}).get("entry") or {}
        if (entry.get("manual_operation") != "recharge" or
                entry.get("amount_micro") != "100000000" or
                entry.get("payment_reference") != payment_reference or
                entry.get("business_id") != funding_key):
            raise CheckFailure("Check08 funding response did not match its idempotent recharge request")
        check.note("仅为本次新建的 loopback 测试企业 A 充值 100000000 微元；平台默认模型范围保持继承")

    request_body = {"model": model, "messages": [{"role": "user", "content": "t03 acceptance"}], "stream": True}

    op = "t03-%s-gateway-member" % env.pass_id
    created = client.call(check, "gateway-create-member-key", "POST", env.org_path("A", "/keys"),
                          body={"name": "t03-gateway-member", "operation_id": op}, session=member)
    expect_status(check, created, 201, label="gateway member key")
    payload = created.json() or {}
    secret = payload.get("secret", "")
    key_id = str((payload.get("key") or {}).get("key_id", ""))
    check.require(bool(secret) and bool(key_id), "Gateway 检查需要一把新建的成员 Key")
    client.red.add(secret)
    env.secrets_to_scan.append(("gateway-member-key-secret", secret, ["gateway-create-member-key"]))

    allowed = poll_status(check, client, "gateway-member-allowed", "POST", "/v1/chat/completions",
                          expect=200, base_url=gateway, body=request_body, csrf=None, max_time=8,
                          tolerate_timeout=True,
                          extra_headers={"Authorization": "Bearer " + secret})
    expect_status(check, allowed, 200, label="gateway member request")
    check.require("primary" in allowed.body and "[DONE]" in allowed.body,
                  "放行请求未完整到达上游 mock")

    revoked = client.call(check, "gateway-revoke-member-key", "POST",
                          env.org_path("A", "/keys/%s/revoke" % key_id), session=member)
    expect_status(check, revoked, 200, label="gateway revoke key")
    denied = poll_status(check, client, "gateway-revoked", "POST", "/v1/chat/completions",
                         expect=401, base_url=gateway, body=request_body, csrf=None, max_time=8,
                         tolerate_timeout=True,
                         extra_headers={"Authorization": "Bearer " + secret})
    require_envelope(check, denied, 401, "unauthorized", "revoked key at gateway")
    check.require("primary" not in denied.body, "被撤销的 Key 仍拿到了上游响应")

    # A second ordinary member owns and uses a separate key; role never prevents personal access.
    second_op = "t03-%s-gateway-member2" % env.pass_id
    second_created = client.call(check, "gateway-create-second-member-key", "POST", env.org_path("A", "/keys"),
                                 body={"name": "t03-gateway-member2", "operation_id": second_op},
                                 session=second_member)
    expect_status(check, second_created, 201, label="gateway second member key")
    second_payload = second_created.json() or {}
    second_secret = second_payload.get("secret", "")
    second_key_id = str((second_payload.get("key") or {}).get("key_id", ""))
    check.require(second_secret and second_key_id, "第二名成员的自有 Key 创建缺少 secret 或 ID")
    client.red.add(second_secret)
    env.secrets_to_scan.append(("gateway-second-member-key-secret", second_secret,
                                ["gateway-create-second-member-key"]))
    second_allowed = poll_status(check, client, "gateway-second-member-allowed", "POST", "/v1/chat/completions",
                                 expect=200, base_url=gateway, body=request_body, csrf=None, max_time=8,
                                 tolerate_timeout=True,
                                 extra_headers={"Authorization": "Bearer " + second_secret})
    expect_status(check, second_allowed, 200, label="gateway second member request")
    check.require("primary" in second_allowed.body, "第二名成员的 Key 没有到达上游 mock")

    # An admin's own Key is valid too, but remains private from member and owner list views.
    admin_op = "t03-%s-gateway-admin" % env.pass_id
    admin_created = client.call(check, "gateway-create-admin-own-key", "POST", env.org_path("A", "/keys"),
                                body={"name": "t03-gateway-admin", "operation_id": admin_op}, session=admin)
    expect_status(check, admin_created, 201, label="gateway admin own key")
    admin_payload = admin_created.json() or {}
    admin_secret = admin_payload.get("secret", "")
    check.require(admin_secret, "admin 自有 Key 没有返回首次 secret")
    client.red.add(admin_secret)
    env.secrets_to_scan.append(("gateway-admin-key-secret", admin_secret, ["gateway-create-admin-own-key"]))
    admin_allowed = poll_status(check, client, "gateway-admin-allowed", "POST", "/v1/chat/completions",
                                expect=200, base_url=gateway, body=request_body, csrf=None, max_time=8,
                                tolerate_timeout=True,
                                extra_headers={"Authorization": "Bearer " + admin_secret})
    expect_status(check, admin_allowed, 200, label="gateway admin request")
    check.require("primary" in admin_allowed.body, "admin 的自有 Key 没有到达上游 mock")

    deadline = time.time() + 20
    found = None
    attempt = 0
    while True:
        attempt += 1
        usage = client.call(check, "gateway-usage-attribution-wait%02d" % attempt, "GET",
                            env.org_path("A", "/usage"), query="limit=100", session=member)
        expect_status(check, usage, 200, label="gateway usage attribution")
        rows = [item for item in (usage.json() or {}).get("items", []) if item.get("key_id") == key_id]
        if rows:
            found = rows[0]
            break
        if time.time() >= deadline:
            break
        time.sleep(2)
    check.require(found is not None, "Gateway 放行的请求没有在 20s 内出现在本人消费记录里")
    check.require(str(found.get("user_id")) == env.user_id("a-member"), "计量记录的 user_id 不是调用者")
    check.require(str(found.get("execution_id") or "") != "", "计量记录缺少 execution_id")
    check.note("member、member2、admin 均使用本人 Key 到达 mock 上游；撤销后的 Key 在上游前被拒绝")

def check09(env, check):
    """企业消费记录按角色隔离：成员仅本人，owner/admin 查看汇总且看不到 Key。"""
    client = env.client
    member = env.session("a-member")
    member_id = env.user_id("a-member")
    owner_id = env.user_id("a-owner")
    org_usage = client.call(check, "owner-usage-org", "GET", env.org_path("A", "/usage"),
                            query="limit=100", session=env.session("a-owner"))
    expect_status(check, org_usage, 200, label="owner usage")
    org_payload = org_usage.json() or {}
    check.require(org_payload.get("scope") == "organization", "owner 的 usage scope 应为 organization")
    check.require(re.match(r"^\d{4}-\d{2}$", str(org_payload.get("month", ""))) is not None,
                  "usage 响应 month 应为 YYYY-MM")
    totals = org_payload.get("totals") or {}
    check.require(isinstance(totals.get("sales_micro"), str) and totals["sales_micro"].isdigit(),
                  "sales_micro 必须是后端返回的十进制字符串")
    org_items = org_payload.get("items") or []
    for item in org_items:
        check.require("key_id" not in item and "key_name" not in item and "key_prefix" not in item,
                      "组织级用量明细泄露了成员 Key 元数据")

    member_usage = client.call(check, "member-usage-self", "GET", env.org_path("A", "/usage"),
                               query="limit=100", session=member)
    expect_status(check, member_usage, 200, label="member usage")
    member_payload = member_usage.json() or {}
    check.require(member_payload.get("scope") == "self", "member 的 usage scope 应为 self")
    for item in member_payload.get("items") or []:
        check.require(str(item.get("user_id")) == member_id,
                      "member 看到了他人（%r）的用量行" % item.get("user_id"))
        if item.get("settlement_status") in ("pending_dispatch", "dispatched", "reserved", "executing", "receipt_recorded"):
            check.require(item.get("charge_micro") is None, "待结算用量不应混入实际扣费金额")

    cross_user = client.call(check, "member-usage-other-user", "GET", env.org_path("A", "/usage"),
                             query="user=" + owner_id, session=member)
    require_envelope(check, cross_user, 403, "forbidden", "member reads another user's usage")

    for actor in ("a-admin", "a-owner"):
        response = client.call(check, "usage-%s" % actor, "GET", env.org_path("A", "/usage"),
                               query="limit=100", session=env.session(actor))
        expect_status(check, response, 200, label="usage %s" % actor)
        payload = response.json() or {}
        check.require(payload.get("scope") == "organization",
                      "%s 的 usage scope 应为 organization，实际 %r" % (actor, payload.get("scope")))
        check.require((payload.get("totals") or {}).get("sales_micro") == totals.get("sales_micro"),
                      "%s 与 owner 看到的正式消费汇总不一致" % actor)
        for item in payload.get("items") or []:
            check.require("key_id" not in item and "key_name" not in item and "key_prefix" not in item,
                          "%s 的组织消费明细泄露了成员 Key 元数据" % actor)

    # Administrators can explicitly inspect their own rows, which still uses self scope.
    admin_id = env.user_id("a-admin")
    admin_self = client.call(check, "admin-usage-self", "GET", env.org_path("A", "/usage"),
                             query="user=" + admin_id, session=env.session("a-admin"))
    expect_status(check, admin_self, 200, label="admin own usage")
    check.require((admin_self.json() or {}).get("scope") == "self", "管理员查询本人时应得到 self scope")
    check.require(all(str(item.get("user_id")) == admin_id
                      for item in ((admin_self.json() or {}).get("items") or [])),
                  "管理员本人查询返回了其他成员的消费行")

    member_summary = client.call(check, "owner-member-summary", "GET",
                                 env.org_path("A", "/billing/member-summary"),
                                 query="month=" + urllib.parse.quote(str(org_payload.get("month", ""))),
                                 session=env.session("a-owner"))
    expect_status(check, member_summary, 200, label="owner member summary")
    for item in (member_summary.json() or {}).get("items", []):
        check.require("key_id" not in item and "key_name" not in item and "provider_cost_micro" not in item,
                      "成员汇总泄露 Key 或上游成本")

    b_usage = client.call(check, "b-owner-usage-org", "GET", env.org_path("B", "/usage"),
                          query="limit=100", session=env.session("b-owner"))
    expect_status(check, b_usage, 200, label="b-owner usage")
    for item in (b_usage.json() or {}).get("items", []):
        check.require("key_id" not in item and "key_name" not in item,
                      "B 企业用量泄露 Key 元数据")

    if org_items:
        user_ids = {str(item.get("user_id")) for item in org_items}
        check.require(all(str(item.get("user_id")) == member_id
                          for item in (member_payload.get("items") or [])),
                      "成员用量里出现了他人的行")
        if len(user_ids) > 1:
            check.require(len(member_payload.get("items") or []) < len(org_items),
                          "组织内有多名用户的用量，但成员视图未隔离到本人")
        check.note("组织 A 有 %d 条消费记录，用户范围=%s；member 仅见本人记录"
                   % (len(org_items), sorted(user_ids)))
    else:
        check.note("当前月份没有消费行；已验证角色范围、正式金额格式和 Key/成本隐私")

def check10(env, check):
    """审计归属：变更必须落到操作者组织的审计，绝不作为 success 落到别家。"""
    client = env.client
    owner = env.session("a-owner")
    admin = env.session("a-admin")
    member = env.session("a-member")

    before_a = audit_ids(check, client, env, "A")
    before_b = audit_ids(check, client, env, "B")

    invitee_id = env.user_id(INVITEE_ACTOR)
    invitee_email = env.email(INVITEE_ACTOR)
    new_invite_email = "t03-%s-audit-invite@t03.test" % env.run_id

    expected = []
    role_change = client.call(check, "audit-role-change", "PATCH",
                              env.org_path("A", "/members/%s" % invitee_id), body={"role": "admin"}, session=owner)
    expect_status(check, role_change, 204, label="audit role change")
    expected.append(("org.member.role_change", "success", invitee_id))

    remove = client.call(check, "audit-member-remove", "DELETE",
                         env.org_path("A", "/members/%s" % invitee_id), session=owner)
    expect_status(check, remove, 204, label="audit member remove")
    expected.append(("org.member.remove", "success", invitee_id))

    invite = client.call(check, "audit-invitation-create", "POST", env.org_path("A", "/invitations"),
                         body={"email": new_invite_email, "role": "member", "monthly_limit_micro": "100000000"}, session=owner)
    expect_status(check, invite, 201, label="audit invitation create")
    expected.append(("org.invitation.create", "success", ""))

    denied_invite = client.call(check, "audit-invitation-create-failure", "POST", env.org_path("A", "/invitations"),
                                body={"email": "t03-%s-audit-x@t03.test" % env.run_id, "role": "admin"}, session=admin)
    require_envelope(check, denied_invite, 403, "forbidden", "audit invitation create failure")
    expected.append(("org.invitation.create", "failure", ""))

    denied_self = client.call(check, "audit-role-change-failure", "PATCH",
                              env.org_path("A", "/members/%s" % env.user_id("a-member")),
                              body={"role": "owner"}, session=member)
    require_envelope(check, denied_self, 403, "forbidden", "audit role change failure")
    expected.append(("org.member.role_change", "failure", env.user_id("a-member")))

    member_key_private = []
    if env.control_probe(check):
        member_key = client.call(check, "audit-member-key-create", "POST", env.org_path("A", "/keys"),
                                 body={"name": "t03-member-audit-key", "operation_id": "t03-%s-member-audit-key" % env.pass_id},
                                 session=member)
        expect_status(check, member_key, 201, label="audit member key create")
        member_key_body = member_key.json() or {}
        member_key_dto = member_key_body.get("key") or {}
        member_secret = member_key_body.get("secret", "")
        if member_secret:
            client.red.add(member_secret)
            env.secrets_to_scan.append(("audit-member-key-secret", member_secret, ["audit-member-key-create"]))
        member_key_id = member_key_dto.get("key_id")
        check.require(isinstance(member_key_id, str) and member_key_id,
                      "成员 Key 创建响应缺少 key_id")
        check.require(member_key_dto.get("name") == "t03-member-audit-key",
                      "成员 Key 创建响应缺少命名元数据")
        member_key_private.extend(value for value in (member_key_id, member_key_dto.get("name"), member_key_dto.get("prefix"))
                                  if isinstance(value, str) and value)
        revoked = client.call(check, "audit-member-key-revoke", "POST",
                              env.org_path("A", "/keys/%s/revoke" % member_key_id), session=member)
        expect_status(check, revoked, 200, label="audit member key revoke")

        # A team owner can audit their own Key actions, but cannot inspect another
        # member's Key events or any of that Key's identifiers or metadata.
        owner_key = client.call(check, "audit-owner-key-create", "POST", env.org_path("A", "/keys"),
                                body={"name": "t03-owner-audit-key", "operation_id": "t03-%s-owner-audit-key" % env.pass_id},
                                session=owner)
        expect_status(check, owner_key, 201, label="audit owner key create")
        owner_key_body = owner_key.json() or {}
        owner_key_id = ((owner_key_body.get("key") or {}).get("key_id"))
        owner_secret = owner_key_body.get("secret", "")
        if owner_secret:
            client.red.add(owner_secret)
            env.secrets_to_scan.append(("audit-owner-key-secret", owner_secret, ["audit-owner-key-create"]))
        check.require(isinstance(owner_key_id, str) and owner_key_id,
                      "owner Key 创建响应缺少 key_id")
        expected.append(("org.key.create", "success", owner_key_id))
        owner_revoked = client.call(check, "audit-owner-key-revoke", "POST",
                                    env.org_path("A", "/keys/%s/revoke" % owner_key_id), session=owner)
        expect_status(check, owner_revoked, 200, label="audit owner key revoke")
        expected.append(("org.key.revoke", "success", owner_key_id))

    after_a = audit_fetch(check, client, env, "A")
    after_b = audit_fetch(check, client, env, "B")
    new_a = [row for row in after_a if row["id"] not in before_a]
    check.require(new_a, "组织 A 的审计里没有出现任何新行")

    for action, outcome, target in expected:
        match = [row for row in new_a
                 if row.get("action") == action and row.get("outcome") == outcome
                 and (not target or str(row.get("target_id")) == str(target))]
        check.require(match, "组织 A 的审计缺少 %s/%s(target=%s)" % (action, outcome, target or "-"))
    for row in new_a:
        check.require(str(row.get("actor_organization_id")) == env.org_id("A"),
                      "组织 A 的新审计行 actor_organization_id=%r" % row.get("actor_organization_id"))

    # B 的审计里不得出现 A 的 success 变更；A 的组织 id 不得出现在 B 的审计里
    a_actor_ids = {env.user_id(name) for name in ACTOR_ROLES if ACTOR_ROLES[name][0] == "A"}
    for row in after_b:
        check.require(not (str(row.get("actor_user_id")) in a_actor_ids and row.get("outcome") == "success"),
                      "A 的 actor 在 B 的审计里有 success 行：%s/%s" % (row.get("action"), row.get("target_id")))
        check.require(str(row.get("actor_organization_id")) != env.org_id("A"),
                      "B 的审计里出现了 actor_organization_id=A 的行")
    for row in after_a:
        check.require(str(row.get("actor_organization_id")) != env.org_id("B"),
                      "A 的审计里出现了 actor_organization_id=B 的行")
    cross_failures = [row for row in after_b if str(row.get("actor_user_id")) in a_actor_ids
                      and row.get("outcome") == "failure"]
    check.note("B 的审计里有 %d 条来自 A actor 的 failure 行（跨组织探测按设计记入被探测组织，"
               "ADR-0003 §2.3 的实现注释；不含任何 B 的数据）" % len(cross_failures))

    # 审计内容不得包含任何 secret / token / 口令。Key 审计对 owner 仅保留
    # owner 自己的动作；成员 Key 的事件及标识、名称、前缀都不可见。
    audit_text = json.dumps(after_a) + json.dumps(after_b)
    member_key_audit_rows = [row for row in after_a
                             if row.get("action") in ("org.key.create", "org.key.revoke")
                             and str(row.get("actor_user_id")) == str(env.user_id("a-member"))]
    check.require(not member_key_audit_rows,
                  "owner 审计中出现了成员 Key 事件")
    for value in member_key_private:
        check.require(value not in audit_text, "owner 审计泄露了成员 Key 标识或元数据")
    for name, value, _allowed in env.secrets_to_scan:
        check.require(value not in audit_text, "审计里出现了 %s" % name)
    check.require(invitee_email not in json.dumps(after_b), "B 的审计里出现了 A 的成员 email")

    # 只有 owner/admin 能读审计
    for actor in ("a-member", "a-member2"):
        response = client.call(check, "audit-read-%s" % actor, "GET", env.org_path("A", "/audit"),
                               session=env.session(actor))
        require_envelope(check, response, 403, "forbidden", "audit read %s" % actor)


def check11(env, check):
    """全局泄密扫描：secret / token / 口令只能出现在各自的创建响应里。"""
    bodies = env.client.raw_bodies()
    check.require(bodies, "没有捕获到任何响应")
    for name, value, allowed_labels in env.secrets_to_scan:
        hits = [record["label"] for record in env.client.records if value and value in record["body"]]
        check.require(hits, "%s 从未被捕获（harness 自检失败）" % name)
        unexpected = [label for label in hits if label not in allowed_labels]
        check.require(not unexpected,
                      "%s 出现在非创建响应里：%s（允许：%s）" % (name, unexpected, allowed_labels))
        check.require(len(hits) == len(allowed_labels),
                      "%s 出现 %d 次，期望恰好 %d 次（创建响应）" % (name, len(hits), len(allowed_labels)))
    check.require(env.password not in "".join(bodies), "fixture 口令出现在某个响应体里")
    check.note("扫描了 %d 个响应体；%d 个 secret/token 均只出现在各自的创建响应里"
               % (len(bodies), len(env.secrets_to_scan)))


def audit_fetch(check, client, env, label):
    response = client.call(check, "audit-list-%s" % label, "GET", env.org_path(label, "/audit"),
                           query="limit=100", session=env.session("%s-owner" % label.lower()))
    expect_status(check, response, 200, label="audit list %s" % label)
    return (response.json() or {}).get("items") or []


def audit_ids(check, client, env, label):
    return {str(row["id"]) for row in audit_fetch(check, client, env, label)}


CHECKS = [
    ("check01", "登录与角色矩阵读取", check01, "check"),
    ("check02", "跨组织读取 404 且无元数据泄露", check02, "check"),
    ("check03", "跨组织写入被拒绝且目标组织不变", check03, "check"),
    ("check04", "权限矩阵负例（member/admin/owner）", check04, "check"),
    ("check05", "最后 owner 保护与 owner 晋升路径", check05, "check"),
    ("check06", "邀请一次性与 email/role 不可篡改", check06, "check"),
    ("check07", "本人命名 Key、隐私与 secret 一次性", check07, "check"),
    ("check08", "Gateway access guard 与计量归属", check08, "check"),
    ("check09", "正式消费范围与成员 Key 隐私", check09, "check"),
    ("check10", "审计归属", check10, "check"),
    ("check11", "全局泄密扫描", check11, "check"),
]


def write_summary(path, summary):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(summary, handle, indent=2, sort_keys=True, ensure_ascii=False)
        handle.write("\n")
    os.replace(tmp, path)


def main():
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--admin-email", required=True)
    parser.add_argument("--admin-password-file", required=True)
    parser.add_argument("--state-file", required=True)
    parser.add_argument("--evidence-dir", required=True)
    parser.add_argument("--run-id", default="")
    parser.add_argument("--timeout-seconds", type=int, default=15)
    parser.add_argument("--gateway-url", default="")
    parser.add_argument("--gateway-model", default="gpt-4o-mini")
    parser.add_argument("--seed-local-test-credit", action="store_true")
    args, unknown = parser.parse_known_args()
    if unknown:
        print("console-rbac-accept: 未知内部参数 %s" % unknown, file=sys.stderr)
        return 2
    if args.seed_local_test_credit:
        if not args.gateway_url:
            print("console-rbac-accept: --seed-local-test-credit requires --gateway-url", file=sys.stderr)
            return 2
        if args.run_id:
            print("console-rbac-accept: --seed-local-test-credit requires a fresh random run_id", file=sys.stderr)
            return 2
        if os.path.lexists(args.state_file):
            print("console-rbac-accept: --seed-local-test-credit cannot reuse an existing state file", file=sys.stderr)
            return 2
        if not is_loopback_http_url(args.base_url) or not is_loopback_http_url(args.gateway_url):
            print("console-rbac-accept: --seed-local-test-credit requires loopback Console and Gateway URLs", file=sys.stderr)
            return 2

    global _RED
    _RED = Redactor()
    log.path = os.path.join(args.evidence_dir, "rbac-accept.log")
    with open(log.path, "a", encoding="utf-8") as handle:
        handle.write("\n===== console-rbac-accept run %s (base_url=%s admin=%s) =====\n"
                     % (utcnow(), args.base_url, args.admin_email))

    client = Client(args.base_url, args.timeout_seconds, args.evidence_dir, _RED)
    summary_path = os.path.join(args.evidence_dir, "summary.json")
    summary = {
        "tool": "scripts/console-rbac-accept.sh",
        "version": 1,
        "started_at": utcnow(),
        "base_url": args.base_url,
        "admin_email": args.admin_email,
        "checks": [],
        "product_bugs": [],
        "not_verified": [],
        "fixtures": None,
        "result": "RUNNING",
    }

    try:
        env = Env(args, client)
    except InfraFailure as exc:
        log("FAIL infra: %s" % _RED.text(str(exc)))
        return 2

    log("console-rbac-accept: base_url=%s admin=%s evidence=%s state=%s"
        % (args.base_url, args.admin_email, args.evidence_dir, args.state_file))

    setup = Check("setup", "fixture 创建与登录")
    try:
        env.ensure_fixtures(setup)
    except (InfraFailure, CheckFailure) as exc:
        summary["result"] = "FAIL"
        summary["finished_at"] = utcnow()
        summary["failure"] = _RED.text(str(exc))
        write_summary(summary_path, summary)
        log("FAIL setup: %s" % _RED.text(str(exc)))
        return 2 if isinstance(exc, InfraFailure) else 1
    summary["fixtures"] = {
        "run_id": env.run_id,
        "organizations": env.orgs,
        "users": {name: {"email": item["email"], "id": item["id"]} for name, item in env.users.items()},
    }
    setup.finished_at = utcnow()
    summary["checks"].append({
        "id": setup.cid, "title": setup.title, "status": "PASS",
        "started_at": setup.started_at, "finished_at": setup.finished_at,
        "details": setup.details, "notes": setup.notes, "failure": None, "skip_reason": None,
        "requests": setup.exchanges,
    })

    for cid, title, function, kind in CHECKS:
        check = Check(cid, title, kind)
        log("---- %s %s" % (cid, title))
        try:
            function(env, check)
        except SkipCheck as exc:
            check.status = "SKIP"
            check.skip_reason = _RED.text(str(exc))
        except CheckFailure as exc:
            check.status = "FAIL"
            check.failure = _RED.text(str(exc))
        except InfraFailure as exc:
            check.status = "FAIL"
            check.failure = _RED.text("infra: %s" % exc)
        except Exception as exc:  # noqa: BLE001 - the harness must report, not crash
            check.status = "FAIL"
            check.failure = _RED.text("unexpected error: %r" % (exc,))
        check.finished_at = utcnow()
        summary["checks"].append({
            "id": check.cid, "title": check.title, "status": check.status,
            "started_at": check.started_at, "finished_at": check.finished_at,
            "details": check.details, "notes": check.notes,
            "failure": check.failure, "skip_reason": check.skip_reason,
            "requests": check.exchanges,
        })
        if check.status == "SKIP":
            log("SKIP %s %s: %s" % (cid, title, check.skip_reason))
        elif check.status == "FAIL":
            summary["result"] = "FAIL"
            summary["finished_at"] = utcnow()
            summary["product_bugs"] = env.bugs
            write_summary(summary_path, summary)
            log("FAIL %s %s: %s" % (cid, title, check.failure))
            log("console-rbac-accept: 首个硬失败，停止；summary=%s" % summary_path)
            return 1
        else:
            log("PASS %s %s" % (cid, title))

    summary["result"] = "PASS"
    summary["finished_at"] = utcnow()
    summary["product_bugs"] = env.bugs
    summary["not_verified"] = [c["id"] for c in summary["checks"] if c["status"] == "SKIP"]
    write_summary(summary_path, summary)
    passed = sum(1 for c in summary["checks"] if c["status"] == "PASS")
    skipped = sum(1 for c in summary["checks"] if c["status"] == "SKIP")
    log("console-rbac-accept: %s（%d PASS，%d SKIP，%d product bug）；summary=%s"
        % (summary["result"], passed, skipped, len(env.bugs), summary_path))
    return 0


if __name__ == "__main__":
    sys.exit(main())
PYEOF

python_args=(
  --base-url "$base_url"
  --admin-email "$admin_email"
  --admin-password-file "$admin_password_file"
  --state-file "$state_file"
  --evidence-dir "$evidence_dir"
  --timeout-seconds "$timeout_seconds"
  --gateway-url "$gateway_url"
  --gateway-model "$gateway_model"
)
if [[ "$seed_local_test_credit" == "1" ]]; then
  python_args+=(--seed-local-test-credit)
fi
if [[ -n "$run_id" ]]; then
  python_args+=(--run-id "$run_id")
fi

rc=0
python3 "$helper" "${python_args[@]}" || rc=$?
printf 'console-rbac-accept: evidence dir: %s\n' "$evidence_dir"
exit "$rc"
