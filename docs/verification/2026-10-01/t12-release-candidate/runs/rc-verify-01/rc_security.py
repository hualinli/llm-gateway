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
    safe = re.sub(r"gw_session=[^;]+", "gw_session=<redacted>", joined)
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
