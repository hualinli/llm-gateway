#!/usr/bin/env bash
#
# console-accept.sh — T02 Console HTTP 验收脚本（黑盒，curl + python3 stdlib）。
#
# 假设 Console 已经在 --base-url 上运行（本脚本不启动、不停止、不迁移任何东西）：
#   * 只发 HTTP 请求，不读数据库、不改产品代码、不依赖仓库内 Go 代码；
#   * HTTP 只用 curl，解析/断言只用 python3 标准库；
#   * 密码、session token、CSRF token 在输出与证据文件里一律只以
#     "前缀…<redacted:N>" 形式出现，明文既不打印也不落盘。
#
# 契约来源：docs/decisions.md、
# docs/openapi/console-v1.yaml（T02 冻结子集）。
#
# 退出码：0 = 全部硬检查通过（可能含非致命 OBSERVED 发现）；1 = 首个硬失败；2 = 用法/环境错误。
#
# 用法见 --help。
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
script_name="$(basename "$0")"

base_url="http://127.0.0.1:8092"
admin_email=""
admin_password_file=""
evidence_dir=""
insecure_cookies_ok=0
fail_on_contract_divergence=0
login_burst=5
max_throttle_attempts=12
throttle_settle_seconds=62
timeout_seconds=15

usage() {
  cat <<'USAGE'
用法:
  scripts/console-accept.sh --admin-email EMAIL --admin-password-file PATH [选项]

必需:
  --admin-email EMAIL            平台管理员邮箱（bootstrap 创建的那个）
  --admin-password-file PATH     平台管理员密码文件（内容不会被打印；只读，不做权限校验）

选项:
  --base-url URL                 目标 Console 地址（默认 http://127.0.0.1:8092）
  --evidence-dir DIR             证据目录（默认 data/console-accept/<UTC 时间戳>）
  --insecure-cookies-ok          声明目标是明文 http：要求 Set-Cookie 不带 Secure 属性；
                                 不传该开关时不要求 Secure（只记录 observation）
  --fail-on-contract-divergence  把非致命的契约偏差探测（check 12）当成硬失败
  --login-burst N                预期登录突发上限，默认 5（只用于报告与偏差提示）
  --max-throttle-attempts N      节流检查最多尝试次数，默认 12
  --throttle-settle-seconds N    节流检查前等待秒数，保证令牌桶回满，默认 62（0 = 不等待）
  --timeout-seconds N            单个 curl 请求超时，默认 15
  -h, --help                     显示本帮助

检查项（ADR-0002 §4）:
  1  GET  /livez                 → 200；GET /readyz → 200
  2  POST /api/v1/auth/login      → 200 + gw_session(HttpOnly/SameSite=Lax) + csrf_token + user.id 字符串 + 个人组织 owner
  3  GET  /api/v1/me              → 200（同用户）；无 cookie → 401 unauthorized；篡改 cookie → 401
  4  开户与 CSRF                   → 旧 POST /organizations 为 405；激活码管理校验缺失/错误 CSRF 403
  5  GET  /api/v1/organizations   → 200 含个人组织与新组织；limit=1000 被截断不报错；limit=0/abc → 400 invalid_request
  6  加入申请与跨企业隔离         → 获批成员 GET 组织 A 200；未加入组织 B 404
  7  POST /api/v1/auth/logout     → 204；旧 cookie 再用 → 401
  8  公开注册个人账户               → 创建个人 owner/session；重复邮箱 409；非平台管理员不能创建后台账号
  9  POST /api/v1/admin/users/{id}/status → 200 且旧会话失效 401；非法 status 400；未知用户 404
  10 登录节流                     → 连续错误密码最终 429 rate_limited + Retry-After
  11 错误信封                     → 每个 4xx/5xx 都是 T01A 信封 + X-Content-Type-Options: nosniff
  12 节流 key 契约探测（非致命）   → 换一个 email 是否仍然 429（ADR 要求按 email+IP 计量）
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
    --evidence-dir) evidence_dir="${2:-}"; shift 2 ;;
    --evidence-dir=*) evidence_dir="${1#*=}"; shift ;;
    --insecure-cookies-ok) insecure_cookies_ok=1; shift ;;
    --fail-on-contract-divergence) fail_on_contract_divergence=1; shift ;;
    --login-burst) login_burst="${2:-}"; shift 2 ;;
    --login-burst=*) login_burst="${1#*=}"; shift ;;
    --max-throttle-attempts) max_throttle_attempts="${2:-}"; shift 2 ;;
    --max-throttle-attempts=*) max_throttle_attempts="${1#*=}"; shift ;;
    --throttle-settle-seconds) throttle_settle_seconds="${2:-}"; shift 2 ;;
    --throttle-settle-seconds=*) throttle_settle_seconds="${1#*=}"; shift ;;
    --timeout-seconds) timeout_seconds="${2:-}"; shift 2 ;;
    --timeout-seconds=*) timeout_seconds="${1#*=}"; shift ;;
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
for pair in "login-burst:$login_burst" "max-throttle-attempts:$max_throttle_attempts" \
            "throttle-settle-seconds:$throttle_settle_seconds" "timeout-seconds:$timeout_seconds"; do
  name="${pair%%:*}"; value="${pair#*:}"
  case "$value" in
    ''|*[!0-9]*) die_usage "--$name 必须是非负整数: $value" ;;
  esac
done

command -v curl >/dev/null 2>&1 || { printf '%s: 需要 curl\n' "$script_name" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { printf '%s: 需要 python3\n' "$script_name" >&2; exit 2; }

if [[ -z "$evidence_dir" ]]; then
  evidence_dir="data/console-accept/$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$evidence_dir"
evidence_dir="$(cd "$evidence_dir" && pwd)"

# 非机密运行元数据，便于证据目录自解释。
{
  printf 'script=%s\n' "$script_name"
  printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'base_url=%s\n' "$base_url"
  printf 'admin_email=%s\n' "$admin_email"
  printf 'admin_password_file=%s\n' "$admin_password_file"
  printf 'insecure_cookies_ok=%s\n' "$insecure_cookies_ok"
  printf 'login_burst=%s\n' "$login_burst"
  printf 'max_throttle_attempts=%s\n' "$max_throttle_attempts"
  printf 'throttle_settle_seconds=%s\n' "$throttle_settle_seconds"
  printf 'timeout_seconds=%s\n' "$timeout_seconds"
  printf 'script_sha256=%s\n' "$(shasum -a 256 "$script_dir/$script_name" 2>/dev/null | awk '{print $1}')"
} > "$evidence_dir/run.env"

# python 驱动写在临时目录：它只是实现细节，不属于证据。
helper_dir=""
cleanup_helper() { [[ -n "$helper_dir" ]] && rm -rf "$helper_dir"; }
trap cleanup_helper EXIT
if ! helper_dir="$(mktemp -d "${TMPDIR:-/tmp}/console-accept.XXXXXX" 2>/dev/null)"; then
  helper_dir="$evidence_dir/.harness"
  mkdir -p "$helper_dir"
fi
helper="$helper_dir/accept_lib.py"

cat > "$helper" <<'PYEOF'
#!/usr/bin/env python3
"""Console (T02 / ADR-0002 §4) black-box HTTP acceptance checks.

The driver owns every assertion; HTTP is done by shelling out to curl (the only
network client allowed) and JSON parsing uses the standard library only.
"""

import argparse
import json
import os
import re
import secrets
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone

ERROR_CODES = {
    "invalid_request", "unauthorized", "forbidden", "not_found", "conflict",
    "method_not_allowed", "rate_limited", "insufficient_balance",
    "config_pending", "billing_unavailable", "upstream_unavailable",
    "unavailable", "internal_error",
}
MAX_STORED_BODY = 8192


class CheckFailure(Exception):
    pass


class InfraFailure(Exception):
    pass


def utcnow():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


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
        head = value[:4]
        return "%s…<redacted:%d>" % (head, len(value))

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
    def __init__(self, method, url, status, headers, body, elapsed):
        self.method = method
        self.url = url
        self.status = status
        self.headers = headers  # list[(name, value)] preserving duplicates
        self.body = body
        self.elapsed = elapsed

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
            out[name] = (raw, attrs, value)
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
        self.exchanges = []
        self.sequence = 0

    def note(self, text):
        line = redact(text)
        self.notes.append(line)
        log("    note: " + line)

    def require(self, condition, message):
        if not condition:
            raise CheckFailure(message)


# The active redactor is global so module-level helpers can use it.
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
        self.scratch = tempfile.mkdtemp(prefix="console-accept-http.")
        self.exchanges = []

    # -- transport ---------------------------------------------------------
    def call(self, check, label, method, path, *, query=None, body=None,
             session=None, csrf=CSRF_DEFAULT, extra_headers=None, raw_body=None,
             request_id=None):
        check.sequence += 1
        seq = check.sequence
        url = self.base_url + path
        if query:
            url += "?" + query
        request_id = request_id or "acc-%s-%02d" % (check.cid, seq)

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
            "curl", "--silent", "--show-error", "--max-time", str(self.timeout),
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

        try:
            proc = subprocess.run(cmd, input=stdin, stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, timeout=self.timeout + 10)
        except subprocess.TimeoutExpired:
            raise InfraFailure("curl 超时: %s %s" % (method, url))
        if proc.returncode != 0:
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
            raw = handle.read()
        decoded = raw.decode("utf-8", "replace")

        response = Response(method, url, status, headers_list, decoded, elapsed)
        error_code = None
        if decoded.strip().startswith("{"):
            try:
                parsed = json.loads(decoded)
                if isinstance(parsed, dict) and isinstance(parsed.get("error"), dict):
                    error_code = parsed["error"].get("code")
            except Exception:
                error_code = None
        exchange = {
            "check": check.cid,
            "seq": seq,
            "label": label,
            "method": method,
            "path": path + (("?" + query) if query else ""),
            "url": url,
            "request_id_sent": request_id,
            "request_headers": {k: self.red.text(v) for k, v in headers.items()},
            "request_body": self.red.text(payload) if payload is not None else None,
            "status": status,
            "error_code": error_code,
            "response_headers": [(k, self.red.text(v)) for k, v in headers_list],
            "response_body": self.red.text(decoded),
            "elapsed_seconds": round(elapsed, 4),
            "at": utcnow(),
        }
        check.exchanges.append(exchange)
        self.exchanges.append(exchange)
        self._write_raw(check, seq, label, method, path, headers, payload, status,
                        headers_list, decoded)
        log("    %s %s -> %d (%.0f ms)" % (method, url.replace(self.base_url, ""), status, elapsed * 1000))
        return response

    def _write_raw(self, check, seq, label, method, path, headers, payload, status,
                   response_headers, response_body):
        slug = re.sub(r"[^A-Za-z0-9]+", "-", label).strip("-").lower() or "call"
        base = os.path.join(self.evidence_dir, "%s-%02d-%s" % (check.cid, seq, slug))
        req_lines = ["%s %s HTTP/1.1" % (method, path)]
        for key, value in headers.items():
            req_lines.append("%s: %s" % (key, value))
        req_text = "\n".join(req_lines)
        if payload is not None:
            req_text += "\n\n" + payload
        with open(base + ".req", "w", encoding="utf-8") as handle:
            handle.write(self.red.text(req_text) + "\n")
        resp_lines = ["HTTP %d" % status]
        for key, value in response_headers:
            resp_lines.append("%s: %s" % (key, value))
        body = response_body
        if len(body) > MAX_STORED_BODY:
            body = body[:MAX_STORED_BODY] + "\n…<truncated>"
        with open(base + ".resp", "w", encoding="utf-8") as handle:
            handle.write(self.red.text("\n".join(resp_lines) + "\n\n" + body) + "\n")
        check.details.setdefault("raw_files", []).append(os.path.basename(base) + ".resp")


class Env:
    """Everything the individual checks share."""

    def __init__(self, args, client, red):
        self.args = args
        self.client = client
        self.red = red
        self.admin_email = args.admin_email
        self.admin_password = read_password(args.admin_password_file)
        self.red.add(self.admin_password)
        self.state = {}
        self.bugs = []
        self.checks = []

    def login(self, check, email, password, *, attempts=3, tag="login"):
        """POST /api/v1/auth/login, retrying once if the shared login bucket is full."""
        for attempt in range(1, attempts + 1):
            response = self.client.call(check, "%s-%s" % (tag, email.split("@")[0]),
                                        "POST", "/api/v1/auth/login",
                                        body={"email": email, "password": password})
            if response.status == 429 and attempt < attempts:
                wait = int(response.header("Retry-After") or 60)
                check.note("登录 429（IP 级共享令牌桶已空）；等待 %ds 后重试第 %d 次" % (wait, attempt + 1))
                time.sleep(min(max(wait, 1), 70))
                continue
            return response
        raise CheckFailure("login retries exhausted")


def read_password(path):
    with open(path, "rb") as handle:
        raw = handle.read().decode("utf-8", "replace")
    value = re.sub(r"[\r\n]+$", "", raw)
    if not value.strip():
        raise InfraFailure("密码文件为空: %s" % path)
    return value


def session_of(check, response, label):
    body = response.json() or {}
    cookies = cookies_of(response)
    if "gw_session" not in cookies:
        raise CheckFailure("%s: 响应没有 gw_session cookie" % label)
    user = body.get("user") or {}
    session = {
        "token": cookies["gw_session"][0],
        "csrf": body.get("csrf_token") or cookies.get("gw_csrf", ("", {}, ""))[0],
        "user_id": user.get("id"),
        "email": user.get("email"),
    }
    # 会话建立后立刻登记 secret：之后任何日志/证据里都只会出现前缀。
    _RED.add(session["token"])
    _RED.add(session["csrf"])
    return session


def flip(value):
    if not value:
        return "x"
    index = len(value) // 2
    char = value[index]
    replacement = "A" if char != "A" else "B"
    return value[:index] + replacement + value[index + 1:]


def find_cookie(response, name):
    return cookies_of(response).get(name)


def assert_error(check, response, status, code, label):
    check.require(response.status == status,
                  "%s: status=%d, want %d (body=%s)" % (label, response.status, status, response.body[:300]))
    err = response.error()
    check.require(err is not None, "%s: 响应不是 T01A 错误信封: %s" % (label, response.body[:300]))
    check.require(err.get("code") == code,
                  "%s: error.code=%r, want %r" % (label, err.get("code"), code))


# --------------------------------------------------------------------------
# checks
# --------------------------------------------------------------------------

def check01(env, check):
    check.details["description"] = "存活/就绪探针在业务 listener 上"
    live = env.client.call(check, "livez", "GET", "/livez")
    check.require(live.status == 200, "GET /livez status=%d, want 200" % live.status)
    check.require(live.body.strip() == "ok", "GET /livez body=%r, want ok" % live.body.strip())
    ready = env.client.call(check, "readyz", "GET", "/readyz")
    check.require(ready.status == 200, "GET /readyz status=%d, want 200 (body=%s)" % (ready.status, ready.body.strip()))
    check.details["livez_status"] = live.status
    check.details["readyz_status"] = ready.status


def check02(env, check):
    response = env.login(check, env.admin_email, env.admin_password, tag="login-admin")
    check.require(response.status == 200,
                  "POST /api/v1/auth/login status=%d, want 200 (body=%s)" % (response.status, response.body[:300]))
    body = response.json() or {}
    check.require(isinstance(body.get("user"), dict), "login 响应缺少 user 对象")

    cookie = find_cookie(response, "gw_session")
    check.require(cookie is not None, "login 响应缺少 Set-Cookie: gw_session")
    raw, attrs, header_value = cookie
    check.require(raw != "", "gw_session cookie 值为空")
    check.require("httponly" in attrs, "gw_session 缺少 HttpOnly: %s" % header_value)
    samesite = attrs.get("samesite", "")
    check.require(samesite.lower() == "lax", "gw_session SameSite=%r, want Lax" % samesite)
    check.require(attrs.get("path") == "/", "gw_session Path=%r, want /" % attrs.get("path"))
    secure = "secure" in attrs
    check.details["gw_session_attributes"] = {
        "http_only": True, "same_site": samesite, "path": attrs.get("path"),
        "secure": secure, "max_age": attrs.get("max-age"),
    }
    if env.args.insecure_cookies_ok:
        check.require(not secure, "声明了 --insecure-cookies-ok（明文 http），但 gw_session 带 Secure 属性")
    elif secure:
        check.note("gw_session 带 Secure（未声明 --insecure-cookies-ok，仅记录，不作为失败）")

    csrf_cookie = find_cookie(response, "gw_csrf")
    check.require(csrf_cookie is not None, "login 响应缺少 Set-Cookie: gw_csrf 伴随 cookie")
    check.require("httponly" in csrf_cookie[1], "gw_csrf 缺少 HttpOnly")

    user = body.get("user") or {}
    user_id = user.get("id")
    check.require(isinstance(user_id, str) and user_id.isdigit(),
                  "user.id=%r，契约要求 64 位 id 用 JSON 字符串" % (user_id,))
    check.require(user.get("email") == env.admin_email, "user.email=%r" % user.get("email"))
    check.require(user.get("status") == "active", "user.status=%r, want active" % user.get("status"))
    check.require(user.get("platform_admin") is True, "平台管理员 user.platform_admin 应为 true")

    csrf_token = body.get("csrf_token")
    check.require(isinstance(csrf_token, str) and len(csrf_token) >= 16, "csrf_token 缺失或过短")
    check.require(csrf_cookie[0] == csrf_token, "gw_csrf cookie 与 body.csrf_token 不一致")

    organizations = body.get("organizations")
    check.require(isinstance(organizations, list) and organizations, "organizations 为空")
    personal = [m for m in organizations
                if isinstance(m, dict) and (m.get("organization") or {}).get("type") == "personal"]
    check.require(len(personal) == 1, "个人组织应恰好 1 个，实际 %d 个" % len(personal))
    check.require(personal[0].get("role") == "owner", "个人组织角色=%r, want owner" % personal[0].get("role"))
    org_id = (personal[0].get("organization") or {}).get("id")
    check.require(isinstance(org_id, str) and org_id.isdigit(), "organization.id=%r，应为 JSON 字符串" % (org_id,))

    env.state["admin_session"] = session_of(check, response, "login")
    env.state["personal_org_id"] = org_id
    check.details["user_id"] = user_id
    check.details["personal_org_id"] = org_id
    check.details["csrf_token_redacted"] = env.red.text(csrf_token)
    check.note("个人组织 id=%s role=owner；csrf=%s" % (org_id, env.red.text(csrf_token)))


def check03(env, check):
    admin = env.state["admin_session"]
    me = env.client.call(check, "me-with-session", "GET", "/api/v1/me", session=admin)
    check.require(me.status == 200, "GET /api/v1/me status=%d, want 200 (body=%s)" % (me.status, me.body[:300]))
    body = me.json() or {}
    check.require((body.get("user") or {}).get("id") == admin["user_id"],
                  "GET /api/v1/me 返回的 user.id=%r，与登录不一致" % (body.get("user") or {}).get("id"))
    check.require(body.get("csrf_token") == admin["csrf"],
                  "GET /api/v1/me 未回显会话绑定的 csrf_token")

    anon = env.client.call(check, "me-no-cookie", "GET", "/api/v1/me")
    assert_error(check, anon, 401, "unauthorized", "GET /api/v1/me（无 cookie）")

    tampered = dict(admin)
    tampered["token"] = flip(admin["token"])
    bad = env.client.call(check, "me-tampered-cookie", "GET", "/api/v1/me", session=tampered)
    assert_error(check, bad, 401, "unauthorized", "GET /api/v1/me（篡改 cookie）")
    check.require(tampered["token"] != admin["token"], "篡改后的 token 必须与原值不同")

    cleared = find_cookie(bad, "gw_session")
    if cleared is not None:
        check.note("被拒绝的 cookie 会被清理（Set-Cookie gw_session Max-Age=%s）" % cleared[1].get("max-age"))
    check.details["me_status"] = me.status
    check.details["anonymous_status"] = anon.status
    check.details["tampered_status"] = bad.status


def check04(env, check):
    admin = env.state["admin_session"]
    body = {"enterprise_name": ""}

    missing = env.client.call(check, "activation-no-csrf", "POST", "/api/v1/admin/activation-codes",
                              body=body, session=admin, csrf=None)
    assert_error(check, missing, 403, "forbidden", "POST /api/v1/admin/activation-codes（无 X-CSRF-Token）")

    wrong = env.client.call(check, "activation-wrong-csrf", "POST", "/api/v1/admin/activation-codes",
                            body=body, session=admin, csrf=flip(admin["csrf"]))
    assert_error(check, wrong, 403, "forbidden", "POST /api/v1/admin/activation-codes（错误 X-CSRF-Token）")

    valid_csrf = env.client.call(check, "activation-valid-csrf-invalid-input", "POST", "/api/v1/admin/activation-codes",
                              body=body, session=admin, csrf=admin["csrf"])
    assert_error(check, valid_csrf, 400, "invalid_request",
                 "有效 CSRF 下的空开户码企业名称（请求应进入参数校验）")

    legacy = env.client.call(check, "legacy-self-service-organization", "POST", "/api/v1/organizations",
                             body={"name": "must-not-be-created"}, session=admin, csrf=admin["csrf"])
    assert_error(check, legacy, 405, "method_not_allowed", "已停用的 POST /api/v1/organizations")

    org_name = "accept-owner-%s" % secrets.token_hex(4)
    created = env.client.call(check, "platform-create-enterprise", "POST", "/api/v1/admin/organizations",
                              body={"name": org_name, "owner_email": env.admin_email},
                              session=admin, csrf=admin["csrf"])
    check.require(created.status == 201,
                  "平台开户 status=%d, want 201 (body=%s)" % (created.status, created.body[:300]))
    membership = created.json() or {}
    organization = membership.get("organization") or {}
    check.require(isinstance(organization.get("id"), str) and organization["id"].isdigit(),
                  "平台开户响应缺少字符串 organization.id: %r" % organization.get("id"))
    check.require(organization.get("type") == "enterprise" and organization.get("name") == org_name,
                  "平台开户返回错误企业: %r" % organization)
    check.require(membership.get("role") == "owner" and membership.get("status") == "active",
                  "owner_email 指定的平台账号应取得 owner membership，实际 role/status=%r/%r"
                  % (membership.get("role"), membership.get("status")))
    env.state["new_org_id"] = organization["id"]
    env.state["new_org_name"] = org_name
    check.details.update({"new_org_id": organization["id"], "new_org_name": org_name})
    check.note("企业只能由平台创建或通过有效开户码兑换；旧自助开户路径已关闭。")


def check05(env, check):
    admin = env.state["admin_session"]
    page = env.client.call(check, "list-default", "GET", "/api/v1/organizations", session=admin)
    check.require(page.status == 200, "GET /api/v1/organizations status=%d, want 200 (body=%s)" % (page.status, page.body[:300]))
    payload = page.json() or {}
    items = payload.get("items")
    check.require(isinstance(items, list) and items, "organizations items 为空")
    check.require("next_cursor" in payload, "organizations 响应缺少 next_cursor")
    by_id = {}
    for item in items:
        organization = item.get("organization") or {}
        by_id[organization.get("id")] = item.get("role")
    check.require(by_id.get(env.state["personal_org_id"]) == "owner",
                  "列表缺少个人组织（id=%s）或角色不是 owner" % env.state["personal_org_id"])
    check.require(by_id.get(env.state["new_org_id"]) == "owner",
                  "列表缺少刚由平台创建的企业（id=%s）或角色不是 owner" % env.state["new_org_id"])
    same_name = [item for item in items
                 if (item.get("organization") or {}).get("name") == env.state["new_org_name"]]
    check.require(len(same_name) == 1, "刚创建的企业在列表中出现 %d 次" % len(same_name))

    clamped = env.client.call(check, "list-limit-1000", "GET", "/api/v1/organizations?limit=1000", session=admin)
    check.require(clamped.status == 200, "limit=1000 status=%d, want 200（上限应被截断而不是报错）" % clamped.status)
    clamped_items = (clamped.json() or {}).get("items") or []
    check.require(len(clamped_items) <= 100, "limit=1000 返回 %d 条，超过 MAX 100" % len(clamped_items))

    zero = env.client.call(check, "list-limit-0", "GET", "/api/v1/organizations?limit=0", session=admin)
    assert_error(check, zero, 400, "invalid_request", "limit=0")
    alpha = env.client.call(check, "list-limit-abc", "GET", "/api/v1/organizations?limit=abc", session=admin)
    assert_error(check, alpha, 400, "invalid_request", "limit=abc")

    first = env.client.call(check, "list-page-1", "GET", "/api/v1/organizations?limit=1", session=admin)
    check.require(first.status == 200, "limit=1 status=%d, want 200" % first.status)
    first_payload = first.json() or {}
    first_items = first_payload.get("items") or []
    check.require(len(first_items) == 1, "limit=1 返回 %d 条" % len(first_items))
    cursor = first_payload.get("next_cursor")
    check.require(isinstance(cursor, str) and cursor.isdigit(), "limit=1 的 next_cursor=%r，应为 id 字符串" % (cursor,))
    check.require(cursor == (first_items[0].get("organization") or {}).get("id"),
                  "next_cursor 应等于本页最后一条 id")
    second = env.client.call(check, "list-page-2", "GET", "/api/v1/organizations?limit=1&cursor=%s" % cursor, session=admin)
    check.require(second.status == 200, "limit=1&cursor=... status=%d, want 200" % second.status)
    second_items = (second.json() or {}).get("items") or []
    check.require(len(second_items) == 1, "第二页返回 %d 条" % len(second_items))
    second_id = (second_items[0].get("organization") or {}).get("id") or "0"
    check.require(int(second_id) > int(cursor), "第二页首条 id=%s 未大于 cursor=%s" % (second_id, cursor))

    check.details.update({
        "items_on_default_page": len(items),
        "limit_1000_status": clamped.status,
        "limit_1000_items": len(clamped_items),
        "limit_0_status": zero.status,
        "limit_abc_status": alpha.status,
        "pagination_cursor": cursor,
    })
    check.note("账户目录可分页读取；limit=0/limit=abc → 400 invalid_request。")


def check06(env, check):
    admin = env.state["admin_session"]
    stamp = secrets.token_hex(4)
    org_ids = {}
    org_names = {}
    for label in ("A", "B"):
        name = "accept-%s-%s" % (label.lower(), stamp)
        created = env.client.call(check, "create-isolation-org-" + label, "POST",
                                  "/api/v1/admin/organizations",
                                  body={"name": name, "owner_email": env.admin_email},
                                  session=admin, csrf=admin["csrf"])
        check.require(created.status == 201,
                      "平台创建隔离企业 %s status=%d (body=%s)" % (label, created.status, created.body[:300]))
        organization = (created.json() or {}).get("organization") or {}
        check.require(isinstance(organization.get("id"), str) and organization["id"].isdigit(),
                      "新企业 %s 的 organization.id=%r" % (label, organization.get("id")))
        org_ids[label] = organization["id"]
        org_names[label] = name
    org_a, org_b = org_ids["A"], org_ids["B"]
    env.state["org_a"], env.state["org_b"] = org_a, org_b
    env.state["org_b_name"] = org_names["B"]

    applicant_email = "accept-applicant-%s@accept.test" % stamp
    applicant_password = "Acc3pt-%s-%s" % (stamp, secrets.token_urlsafe(12))
    env.red.add(applicant_password)
    login = env.client.call(check, "register-join-applicant", "POST", "/api/v1/auth/register",
                            body={"email": applicant_email, "password": applicant_password})
    check.require(login.status == 201,
                  "加入申请人自助注册 status=%d, want 201 (body=%s)" % (login.status, login.body[:300]))
    applicant = session_of(check, login, "join applicant register")
    env.state["nonadmin_session"] = applicant
    demo_user = (login.json() or {}).get("user") or {}
    check.require(demo_user.get("platform_admin") is False, "注册用户不应是平台管理员")

    application = env.client.call(check, "submit-join-request", "POST",
                                   "/api/v1/orgs/%s/join-requests" % org_a,
                                   body={"display_name": "Acceptance Applicant"}, session=applicant,
                                   csrf=applicant["csrf"])
    check.require(application.status == 201,
                  "加入申请 status=%d, want 201 (body=%s)" % (application.status, application.body[:300]))
    request_id = (application.json() or {}).get("id")
    check.require(isinstance(request_id, str) and request_id.isdigit(),
                  "加入申请 id=%r，应为 JSON 字符串" % request_id)
    decision = env.client.call(check, "approve-join-request", "POST",
                               "/api/v1/orgs/%s/join-requests/%s/decision" % (org_a, request_id),
                               body={"status": "approved", "monthly_limit_micro": "100000000"},
                               session=admin, csrf=admin["csrf"])
    check.require(decision.status == 200,
                  "企业负责人审批加入申请 status=%d, want 200 (body=%s)" % (decision.status, decision.body[:300]))
    refreshed = env.client.call(check, "me-after-approval", "GET", "/api/v1/me", session=applicant)
    check.require(refreshed.status == 200, "审批后读取 /me status=%d" % refreshed.status)
    membership = next((item for item in (refreshed.json() or {}).get("organizations", [])
                       if (item.get("organization") or {}).get("id") == org_a), None)
    check.require(membership is not None and membership.get("role") == "member"
                  and membership.get("status") == "active",
                  "审批后没有获得 A 企业普通成员关系")

    forbidden = env.client.call(check, "get-org-b", "GET", "/api/v1/organizations/%s" % org_b, session=applicant)
    assert_error(check, forbidden, 404, "not_found", "GET /api/v1/organizations/{org_b}")
    # 不泄露存在性的判据：非成员读一个真实组织，与读一个完全不存在的组织，
    # 响应必须逐字段一致（同样的 code/message），且不能回显对方的 id/name。
    unknown_org = env.client.call(check, "get-unknown-org", "GET",
                                  "/api/v1/organizations/9223372036854775807", session=applicant)
    assert_error(check, unknown_org, 404, "not_found", "GET /api/v1/organizations/{未知 org}")
    existing_err = forbidden.error() or {}
    unknown_err = unknown_org.error() or {}
    check.require(existing_err.get("message") == unknown_err.get("message"),
                  "非成员与不存在组织的 404 message 不同：%r vs %r（泄露存在性）"
                  % (existing_err.get("message"), unknown_err.get("message")))
    compact = re.sub(r"\s+", "", forbidden.body)
    check.require(('"id":"%s"' % org_b) not in compact and org_names["B"] not in forbidden.body,
                  "404 响应泄露了组织 B 的 id/name: %s" % forbidden.body[:200])

    allowed = env.client.call(check, "get-org-a", "GET", "/api/v1/organizations/%s" % org_a, session=applicant)
    check.require(allowed.status == 200, "GET /api/v1/organizations/{org_a} status=%d, want 200" % allowed.status)
    got = ((allowed.json() or {}).get("organization") or {}).get("id")
    check.require(got == org_a, "组织 A 返回 id=%r, want %s" % (got, org_a))

    applicant_list = env.client.call(check, "applicant-list-orgs", "GET", "/api/v1/organizations?limit=100", session=applicant)
    check.require(applicant_list.status == 200, "申请人列组织 status=%d" % applicant_list.status)
    applicant_ids = [(item.get("organization") or {}).get("id")
                     for item in (applicant_list.json() or {}).get("items") or []]
    check.require(org_a in applicant_ids, "申请人列表缺少获批企业 A")
    check.require(org_b not in applicant_ids, "申请人列表出现了未加入的企业 B")

    check.details.update({"applicant_user": applicant_email, "org_a": org_a, "org_b": org_b,
                          "org_b_status": forbidden.status, "org_a_status": allowed.status})
    check.note("公开注册用户申请获批后，org_a=%s → 200，org_b=%s → 404 not_found（不泄露存在性）" % (org_a, org_b))


def check07(env, check):
    admin = env.state["admin_session"]
    logout = env.client.call(check, "logout", "POST", "/api/v1/auth/logout", session=admin, csrf=admin["csrf"])
    check.require(logout.status == 204, "POST /api/v1/auth/logout status=%d, want 204 (body=%s)" % (logout.status, logout.body[:200]))
    check.require(logout.body.strip() == "", "204 响应不应有 body: %r" % logout.body[:120])
    cleared = find_cookie(logout, "gw_session")
    if cleared is not None:
        check.note("登出同时过期 cookie：gw_session Max-Age=%s" % cleared[1].get("max-age"))

    replay = env.client.call(check, "me-after-logout", "GET", "/api/v1/me", session=admin)
    assert_error(check, replay, 401, "unauthorized", "登出后复用旧 cookie 访问 /api/v1/me")
    check.details["logout_status"] = logout.status
    check.details["replay_status"] = replay.status


def check08(env, check):
    login = env.login(check, env.admin_email, env.admin_password, tag="login-admin-2")
    check.require(login.status == 200, "管理员二次登录 status=%d, want 200 (body=%s)" % (login.status, login.body[:300]))
    admin = session_of(check, login, "admin login #2")
    env.state["admin_session"] = admin

    stamp = secrets.token_hex(4)
    victim_email = env.state.setdefault("victim_email", "accept-victim-%s@accept.test" % stamp)
    victim_password = env.state.setdefault("victim_password", "V1ctim-%s-%s" % (secrets.token_urlsafe(12), stamp))
    env.red.add(victim_password)

    created = env.client.call(check, "public-register", "POST", "/api/v1/auth/register",
                              body={"email": victim_email, "password": victim_password})
    check.require(created.status == 201,
                  "POST /api/v1/auth/register status=%d, want 201 (body=%s)" % (created.status, created.body[:300]))
    user = created.json() or {}
    registered_user = user.get("user") or {}
    check.require(isinstance(registered_user.get("id"), str) and registered_user["id"].isdigit(),
                  "新注册 user.id=%r，应为 JSON 字符串" % registered_user.get("id"))
    check.require(registered_user.get("email") == victim_email, "注册 user.email=%r" % registered_user.get("email"))
    check.require(registered_user.get("status") == "active", "注册 user.status=%r, want active" % registered_user.get("status"))
    check.require(registered_user.get("platform_admin") is False, "自助注册用户默认不应是平台管理员")
    personal = [item for item in user.get("organizations", [])
                if (item.get("organization") or {}).get("type") == "personal"]
    check.require(len(personal) == 1 and personal[0].get("role") == "owner",
                  "公开注册应创建且仅创建一个个人 owner 账户")
    session_of(check, created, "self-register")
    env.state["victim_id"] = registered_user["id"]
    env.state["victim_email"] = victim_email

    duplicate = env.client.call(check, "duplicate-register", "POST", "/api/v1/auth/register",
                                body={"email": victim_email, "password": victim_password})
    assert_error(check, duplicate, 409, "conflict", "重复注册相同邮箱")

    nonadmin = env.state.get("nonadmin_session")
    check.require(nonadmin is not None, "check 8 需要 check 6 建立的普通用户会话")
    evil_email = "accept-evil-%s@accept.test" % stamp
    evil_password = "Ev1l-%s" % secrets.token_urlsafe(12)
    env.red.add(evil_password)
    denied = env.client.call(check, "nonadmin-create-user", "POST", "/api/v1/admin/users",
                             body={"email": evil_email, "password": evil_password},
                             session=nonadmin, csrf=nonadmin["csrf"])
    assert_error(check, denied, 403, "forbidden", "非平台管理员 POST /api/v1/admin/users")

    check.details.update({"victim_user_id": registered_user["id"], "victim_email": victim_email,
                          "nonadmin_status": denied.status})
    check.note("公开注册创建了个人 owner 账户；重复邮箱为 409；非平台管理员的后台建用户请求为 403。")


def check09(env, check):
    admin = env.state["admin_session"]
    victim_email = env.state["victim_email"]
    victim_password = env.state["victim_password"]
    victim_id = env.state["victim_id"]

    victim_login = env.login(check, victim_email, victim_password, tag="login-victim")
    check.require(victim_login.status == 200,
                  "受控用户登录 status=%d, want 200 (body=%s)" % (victim_login.status, victim_login.body[:300]))
    victim = session_of(check, victim_login, "victim login")

    disabled = env.client.call(check, "disable-user", "POST", "/api/v1/admin/users/%s/status" % victim_id,
                               body={"status": "disabled"}, session=admin, csrf=admin["csrf"])
    check.require(disabled.status == 200,
                  "POST /api/v1/admin/users/{id}/status status=%d, want 200 (body=%s)" % (disabled.status, disabled.body[:300]))
    check.require((disabled.json() or {}).get("status") == "disabled",
                  "禁用响应 status=%r, want disabled" % (disabled.json() or {}).get("status"))

    stale = env.client.call(check, "me-after-disable", "GET", "/api/v1/me", session=victim)
    assert_error(check, stale, 401, "unauthorized", "被禁用用户的旧 cookie 访问 /api/v1/me")

    invalid = env.client.call(check, "status-invalid-value",
                              "POST", "/api/v1/admin/users/%s/status" % victim_id,
                              body={"status": "paused"}, session=admin, csrf=admin["csrf"])
    assert_error(check, invalid, 400, "invalid_request", "status=paused")

    unknown = env.client.call(check, "status-unknown-user",
                              "POST", "/api/v1/admin/users/9223372036854775807/status",
                              body={"status": "disabled"}, session=admin, csrf=admin["csrf"])
    assert_error(check, unknown, 404, "not_found", "未知 user id")

    bad_path = env.client.call(check, "status-non-numeric-id",
                               "POST", "/api/v1/admin/users/not-a-number/status",
                               body={"status": "disabled"}, session=admin, csrf=admin["csrf"])
    assert_error(check, bad_path, 400, "invalid_request", "非数字 user id")

    disabled_login = env.client.call(check, "login-disabled-user", "POST", "/api/v1/auth/login",
                                     body={"email": victim_email, "password": victim_password})
    assert_error(check, disabled_login, 403, "forbidden", "被禁用用户用正确密码登录")

    check.details.update({
        "victim_user_id": victim_id,
        "disable_status": disabled.status,
        "stale_session_status": stale.status,
        "invalid_status_code": invalid.code(),
        "unknown_user_code": unknown.code(),
    })
    check.note("禁用 → 200；旧会话 401；status=paused → 400 invalid_request；未知 id → 404 not_found")


def check10(env, check):
    settle = env.args.throttle_settle_seconds
    if settle > 0:
        check.note("等待 %ds 让登录令牌桶回满，以便测量“burst 之后第几次被 429”" % settle)
        time.sleep(settle)
    throwaway = "accept-throttle-%s@accept.test" % secrets.token_hex(4)
    attempts = []
    throttled_at = None
    for index in range(1, env.args.max_throttle_attempts + 1):
        password = "wrong-%d-%s" % (index, secrets.token_hex(6))
        env.red.add(password)
        response = env.client.call(check, "wrong-password-%02d" % index, "POST", "/api/v1/auth/login",
                                   body={"email": throwaway, "password": password})
        attempts.append({"attempt": index, "status": response.status, "code": response.code(),
                         "retry_after": response.header("Retry-After")})
        if response.status == 429:
            throttled_at = index
            check.require(response.code() == "rate_limited",
                          "第 %d 次尝试返回 429 但 code=%r" % (index, response.code()))
            retry_after = response.header("Retry-After")
            check.require(retry_after is not None and retry_after.isdigit() and int(retry_after) >= 1,
                          "429 缺少合法的 Retry-After: %r" % (retry_after,))
            break
        check.require(response.status == 401 and response.code() == "unauthorized",
                      "第 %d 次尝试 status=%d code=%r，节流前应始终是 401 unauthorized"
                      % (index, response.status, response.code()))
    check.require(throttled_at is not None,
                  "%d 次错误密码都没有触发 429 rate_limited" % env.args.max_throttle_attempts)
    check.details.update({
        "throwaway_email": throwaway,
        "attempts_to_429": throttled_at,
        "configured_burst": env.args.login_burst,
        "expected_attempts_with_full_bucket": env.args.login_burst + 1,
        "attempts": attempts,
    })
    if throttled_at != env.args.login_burst + 1:
        check.note("观测到第 %d 次触发 429（满桶预期 burst+1=%d；令牌桶是 IP 级共享，先前的登录也会消耗令牌）"
                   % (throttled_at, env.args.login_burst + 1))
    check.note("dedicated email=%s：第 %d 次错误密码进入 429 rate_limited（burst=%d，Retry-After=%s）"
               % (throwaway, throttled_at, env.args.login_burst, attempts[-1]["retry_after"]))


def check11(env, check):
    probes = [
        ("not-found-route", "GET", "/api/v1/definitely-not-a-route", None, 404, "not_found"),
        ("method-not-allowed", "DELETE", "/api/v1/me", None, 405, "method_not_allowed"),
        ("malformed-login-body", "POST", "/api/v1/auth/login", "{", 400, "invalid_request"),
    ]
    for label, method, path, raw_body, status, code in probes:
        response = env.client.call(check, label, method, path, raw_body=raw_body)
        assert_error(check, response, status, code, "%s %s" % (method, path))

    checked = 0
    failures = []
    for exchange in env.client.exchanges:
        if exchange["status"] < 400:
            continue
        checked += 1
        headers = {key.lower(): value for key, value in exchange["response_headers"]}
        nosniff = headers.get("x-content-type-options", "")
        if nosniff != "nosniff":
            failures.append("%s %s: X-Content-Type-Options=%r" % (exchange["method"], exchange["path"], nosniff))
        content_type = headers.get("content-type", "").split(";")[0].strip().lower()
        if content_type != "application/json":
            failures.append("%s %s: Content-Type=%r" % (exchange["method"], exchange["path"], content_type))
        body = exchange["response_body"]
        try:
            payload = json.loads(body)
        except Exception:
            failures.append("%s %s: body 不是 JSON" % (exchange["method"], exchange["path"]))
            continue
        error = payload.get("error") if isinstance(payload, dict) else None
        if not isinstance(error, dict):
            failures.append("%s %s: 缺少 error 信封" % (exchange["method"], exchange["path"]))
            continue
        if set(error) != {"code", "message", "request_id"}:
            failures.append("%s %s: error 字段集合=%s" % (exchange["method"], exchange["path"], sorted(error)))
        if error.get("code") not in ERROR_CODES:
            failures.append("%s %s: code=%r 不在规范词表" % (exchange["method"], exchange["path"], error.get("code")))
        if not isinstance(error.get("message"), str):
            failures.append("%s %s: message 不是字符串" % (exchange["method"], exchange["path"]))
        sent = exchange.get("request_id_sent")
        if error.get("request_id") != sent:
            failures.append("%s %s: error.request_id=%r, 发出去的 X-Request-ID=%r"
                            % (exchange["method"], exchange["path"], error.get("request_id"), sent))
        echoed = headers.get("x-request-id")
        if echoed != sent:
            failures.append("%s %s: 响应 X-Request-ID=%r, want %r" % (exchange["method"], exchange["path"], echoed, sent))
    check.require(not failures, "%d/%d 个错误响应违反信封/nosniff 契约: %s"
                  % (len(failures), checked, "; ".join(failures[:5])))
    check.require(checked >= 6, "错误响应样本只有 %d 个，覆盖不足" % checked)
    check.details["error_responses_checked"] = checked
    check.note("扫过 %d 个 4xx/5xx 响应：信封字段集合、code 词表、request_id 回显、nosniff、application/json 全部满足" % checked)


def check12(env, check):
    """ADR-0002 §2.5 要求按 email+IP 计量；这里换一个 email 立即再登录一次。"""
    check.kind = "observation"
    response = env.client.call(check, "other-email-login", "POST", "/api/v1/auth/login",
                               body={"email": env.admin_email, "password": env.admin_password})
    check.details["other_email"] = env.admin_email
    check.details["other_email_status"] = response.status
    check.details["other_email_code"] = response.code()
    if response.status == 429:
        check.status = "OBSERVED"
        check.details["contract_divergence"] = "login throttle bucket keyed by client IP only"
        env.bugs.append({
            "id": "BUG-1",
            "title": "登录节流按客户端 IP 计量，而不是 ADR-0002 §2.5 要求的 email+IP",
            "endpoint": "POST /api/v1/auth/login",
            "expected": "换一个 email 后应立即拥有自己的 5 次/分钟突发额度（不同 email 不应共享桶）",
            "actual": "同一 IP 在 throwaway email 被 429 后，另一个（正确密码的）email 立刻也收到 429 rate_limited",
            "repro": ("先对 accept-throttle-<rand>@accept.test 连续错误密码触发 429，"
                      "再立刻用管理员邮箱 + 正确密码 POST /api/v1/auth/login → 429"),
            "code_path": "internal/console/handlers.go: s.iam.Login(ctx, email, req.Password, c.ClientIP())；"
                         "internal/iam/service.go: key := strings.TrimSpace(clientKey)（clientKey 已只是 IP）",
            "impact": "一个 NAT/出口 IP 上的 5 次失败登录会让该 IP 上所有账号在剩余窗口内无法登录（含正确密码），"
                      "违反 ADR §2.5 的 email+IP 语义，也构成跨账号拒绝服务。",
        })
        log("    BUG: 换 email 仍然 429 → 节流 key 实际是 client IP（ADR 要求 email+IP）")
    else:
        check.status = "PASS"
        check.details["contract_divergence"] = None


CHECKS = [
    ("check01", "GET /livez 探针", check01, "check"),
    ("check02", "POST /api/v1/auth/login 会话与 cookie", check02, "check"),
    ("check03", "GET /api/v1/me 无/篡改 cookie 负例", check03, "check"),
    ("check04", "开户入口与CSRF保护", check04, "check"),
    ("check05", "GET /api/v1/organizations 列表与 limit", check05, "check"),
    ("check06", "注册、加入审批与跨企业隔离", check06, "check"),
    ("check07", "POST /api/v1/auth/logout 会话撤销", check07, "check"),
    ("check08", "公开注册个人账户与后台用户保护", check08, "check"),
    ("check09", "POST /api/v1/admin/users/{id}/status", check09, "check"),
    ("check10", "登录节流 429 rate_limited", check10, "check"),
    ("check11", "错误信封与 nosniff 全量扫描", check11, "check"),
    ("check12", "节流 key 契约探测（非致命）", check12, "observation"),
]


def write_summary(path, data):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(data, handle, indent=2, ensure_ascii=False)
        handle.write("\n")
    os.replace(tmp, path)


def main():
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--admin-email", required=True)
    parser.add_argument("--admin-password-file", required=True)
    parser.add_argument("--evidence-dir", required=True)
    parser.add_argument("--insecure-cookies-ok", action="store_true")
    parser.add_argument("--fail-on-contract-divergence", action="store_true")
    parser.add_argument("--login-burst", type=int, default=5)
    parser.add_argument("--max-throttle-attempts", type=int, default=12)
    parser.add_argument("--throttle-settle-seconds", type=int, default=62)
    parser.add_argument("--timeout-seconds", type=int, default=15)
    args, unknown = parser.parse_known_args()
    if unknown:
        print("console-accept: 未知内部参数 %s" % unknown, file=sys.stderr)
        return 2

    global _RED
    _RED = Redactor()
    log.path = os.path.join(args.evidence_dir, "accept.log")
    with open(log.path, "a", encoding="utf-8") as handle:
        handle.write("\n===== console-accept run %s (base_url=%s admin=%s) =====\n"
                     % (utcnow(), args.base_url, args.admin_email))

    client = Client(args.base_url, args.timeout_seconds, args.evidence_dir, _RED)
    try:
        env = Env(args, client, _RED)
    except InfraFailure as exc:
        log("FAIL infra: %s" % _RED.text(str(exc)))
        return 2
    summary_path = os.path.join(args.evidence_dir, "summary.json")
    summary = {
        "tool": "scripts/console-accept.sh",
        "version": 1,
        "started_at": utcnow(),
        "base_url": args.base_url,
        "admin_email": args.admin_email,
        "insecure_cookies_ok": bool(args.insecure_cookies_ok),
        "login_burst": args.login_burst,
        "checks": [],
        "product_bugs": [],
        "result": "RUNNING",
    }

    log("console-accept: base_url=%s admin=%s evidence=%s" % (args.base_url, args.admin_email, args.evidence_dir))
    if args.throttle_settle_seconds > 0:
        log("console-accept: check10 前会等待 %ds 让登录令牌桶回满" % args.throttle_settle_seconds)

    try:
        for cid, title, function, kind in CHECKS:
            check = Check(cid, title, kind)
            log("---- %s %s" % (cid, title))
            try:
                function(env, check)
                if check.kind == "observation" and check.status == "PASS":
                    check.status = "OBSERVED"
                elif check.status == "PASS":
                    check.status = "PASS"
            except CheckFailure as exc:
                check.status = "FAIL"
                check.failure = _RED.text(str(exc))
            except InfraFailure as exc:
                check.status = "FAIL"
                check.failure = _RED.text(str(exc))
            except Exception as exc:  # noqa: BLE001 - the harness must report, not crash
                check.status = "FAIL"
                check.failure = _RED.text("unexpected error: %r" % (exc,))
            check.finished_at = utcnow()
            entry = {
                "id": check.cid, "title": check.title, "kind": check.kind,
                "status": check.status, "started_at": check.started_at,
                "finished_at": check.finished_at, "details": check.details,
                "notes": check.notes, "failure": check.failure,
                "requests": [{
                    "method": item["method"], "path": item["path"], "status": item["status"],
                    "error_code": item["error_code"], "request_id": item["request_id_sent"],
                } for item in check.exchanges],
            }
            summary["checks"].append(entry)
            if check.status == "FAIL":
                summary["result"] = "FAIL"
                summary["finished_at"] = utcnow()
                summary["product_bugs"] = env.bugs
                write_summary(summary_path, summary)
                log("FAIL %s %s: %s" % (check.cid, check.title, check.failure))
                log("console-accept: 首个硬失败，停止；summary=%s" % summary_path)
                return 1
            suffix = ""
            if check.status == "OBSERVED" and check.details.get("contract_divergence"):
                suffix = " [contract divergence: %s]" % check.details["contract_divergence"]
            log("%s %s %s%s" % (check.status, check.cid, check.title, suffix))
    except InfraFailure as exc:
        summary["result"] = "FAIL"
        summary["finished_at"] = utcnow()
        summary["product_bugs"] = env.bugs
        write_summary(summary_path, summary)
        log("FAIL infra: %s" % _RED.text(str(exc)))
        return 1

    divergence = any(c["status"] == "OBSERVED" and c["details"].get("contract_divergence")
                     for c in summary["checks"])
    if divergence and args.fail_on_contract_divergence:
        summary["result"] = "FAIL"
    else:
        summary["result"] = "PASS"
    summary["finished_at"] = utcnow()
    summary["product_bugs"] = env.bugs
    write_summary(summary_path, summary)
    passed = sum(1 for c in summary["checks"] if c["status"] == "PASS")
    observed = sum(1 for c in summary["checks"] if c["status"] == "OBSERVED")
    log("console-accept: %s（%d checks PASS，%d OBSERVED，%d product bug）；summary=%s"
        % (summary["result"], passed, observed, len(env.bugs), summary_path))
    return 0 if summary["result"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
PYEOF

python_args=(
  --base-url "$base_url"
  --admin-email "$admin_email"
  --admin-password-file "$admin_password_file"
  --evidence-dir "$evidence_dir"
  --login-burst "$login_burst"
  --max-throttle-attempts "$max_throttle_attempts"
  --throttle-settle-seconds "$throttle_settle_seconds"
  --timeout-seconds "$timeout_seconds"
)
if [[ "$insecure_cookies_ok" == "1" ]]; then
  python_args+=(--insecure-cookies-ok)
fi
if [[ "$fail_on_contract_divergence" == "1" ]]; then
  python_args+=(--fail-on-contract-divergence)
fi

rc=0
python3 "$helper" "${python_args[@]}" || rc=$?
printf 'console-accept: evidence dir: %s\n' "$evidence_dir"
exit "$rc"
