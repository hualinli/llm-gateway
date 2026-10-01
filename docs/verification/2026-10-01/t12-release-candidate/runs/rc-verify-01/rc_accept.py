#!/usr/bin/env python3
"""rc_accept.py — T12 release-candidate 的黑盒验收驱动（由 scripts/release-candidate.sh 生成）。

只用 HTTP + python3 标准库：不读数据库、不 import 仓库里的 Go 代码。
所有落盘内容先脱敏：口令、session、CSRF、API Key secret 一律以
"前缀…<redacted:N>" 形式出现，明文只存在于进程内存与 0600 的 identities.json。

phase:
  seed    价格版本、资金账户、成员与 billing_viewer，写 identities.json
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
import subprocess
import sys
import time
import urllib.error
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
    text = re.sub(r"(gw_session=)[^;\s\"]+", r"\1<redacted>", text)
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

    orgs = {m["organization"]["name"]: m["organization"]["id"] for m in body.get("organizations", [])}
    missing = [n for n in (args.org_a, args.org_b) if n not in orgs]
    if missing:
        rec.step("SEED-0", "两家 demo 企业存在", False,
                 ["缺少组织 %s；bootstrap 需要用 --demo-data" % missing])
        return rec.finish()
    org_a, org_b = orgs[args.org_a], orgs[args.org_b]

    identities = read_identities(args.identities)
    if identities is None:
        identities = {
            "run_id": args.run_id,
            "admin": {"email": args.admin_email},
            "org_a": {"id": org_a, "name": args.org_a},
            "org_b": {"id": org_b, "name": args.org_b},
            "effective_at": time.strftime("%Y-%m-%dT%H:%M:%SZ",
                                          time.gmtime(int(time.time() / 60) * 60 - 600)),
            "users": {},
            "keys": {},
        }
    identities["org_a"]["id"] = org_a
    identities["org_b"]["id"] = org_b

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

    # 资金账户：给企业 A 充值（Idempotency-Key 固定 → 重放不重复入账）
    adjust_key = "rc-credit-%s" % identities["run_id"]
    status, _, resp = admin.request(
        "POST", "/api/v1/admin/billing/accounts/%s/adjustments" % org_a,
        {"amount_micro": "100000000", "direction": "credit",
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

    # 身份：A 的 member、B 的 member、A 的 member（用于"移除成员"）、A 的 billing_viewer
    wanted = [
        ("member_a", args.org_a, "member"),
        ("member_b", args.org_b, "member"),
        ("member_c", args.org_a, "member"),
        ("viewer_a", args.org_a, "billing_viewer"),
    ]
    for label, org_name, role in wanted:
        existing = identities["users"].get(label)
        if existing:
            register_secret("pw:" + label, existing.get("password", ""))
            continue
        email = "rc-%s-%s@rc.test" % (label.replace("_", "-"), args.run_id)
        password = "rc-%s-%s" % (label, args.run_id)
        register_secret("pw:" + label, password)
        status, _, resp = admin.request("POST", "/api/v1/admin/users", {
            "email": email, "password": password, "platform_admin": False,
            "organizations": [identities[("org_a" if org_name == args.org_a else "org_b")]["id"]]})
        if status not in (201, 409):
            rec.step("SEED-3", "创建用户 %s" % label, False,
                     ["%s -> %s %s" % (email, status, resp)])
            return rec.finish()
        user_id = resp.get("id") if isinstance(resp, dict) else None
        if user_id is None:
            # 409：已存在，从 /admin/users 列表里找不到接口，直接复用旧 identity
            rec.step("SEED-3", "创建用户 %s" % label, False,
                     ["用户已存在但 identities.json 无记录，请先执行 down -v 重新开始"])
            return rec.finish()
        if role != "member":
            status, _, resp2 = admin.request(
                "PATCH", "/api/v1/orgs/%s/members/%s" % (identities["org_a"]["id"], user_id),
                {"role": role})
            if status not in (200, 204):
                rec.step("SEED-3", "设置 %s 角色 %s" % (label, role), False,
                         ["status=%s body=%s" % (status, resp2)])
                return rec.finish()
        identities["users"][label] = {"email": email, "password": password,
                                      "user_id": user_id, "role": role, "org": org_name}

    write_identities(args.identities, identities)
    rec.capture("identities-redacted", {"identities": args.identities},
                {"users": {k: {"email": v["email"], "user_id": v["user_id"], "role": v["role"]}
                           for k, v in identities["users"].items()}})
    rec.step("SEED-OK", "seed 完成（价格 / 资金 / 成员 / billing_viewer）", True, [
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

    # ---------------- S1 三个角色登录 ----------------
    roles = {}
    logins = []
    ok = True
    for label, expected_role in (("admin", None), ("member_a", "member"),
                                 ("member_b", "member"), ("viewer_a", "billing_viewer")):
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
    rec.capture("login-roles", {"roles": ["admin", "member_a", "member_b", "viewer_a"]},
                {"logins": logins})
    # 负例：错口令必须 401
    bad = Session(args.console)
    bad_status, _, bad_body = bad.login(args.admin_email, "definitely-not-the-password")
    neg_ok = bad_status == 401
    rec.step("S1", "三角色登录（+ 错误口令 401 负例）", ok and neg_ok, [
        "platform_admin=%s" % logins[0].get("status"),
        "member_a/%s -> %s" % (identities["users"]["member_a"]["email"], logins[1].get("status")),
        "member_b -> %s" % logins[2].get("status"),
        "billing_viewer -> %s (orgA role 应为 billing_viewer)" % logins[3].get("status"),
        "错误口令 -> %s（应为 401 %s）" % (bad_status, (bad_body or {}).get("error", {}).get("code")
                                          if isinstance(bad_body, dict) else ""),
        "session cookie flags (member_a): %s" % logins[1].get("cookie_flags"),
    ], observations={"login_negative_status": bad_status})

    if not ok:
        return rec.finish()

    admin, member_a, member_b, viewer_a = roles["admin"], roles["member_a"], roles["member_b"], roles["viewer_a"]

    # ---------------- S2 成员建 Key + 流式调用 ----------------
    status, _, key = member_a.request("POST", "/api/v1/orgs/%s/keys" % org_a, {})
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

    _, _, pre_account = viewer_a.request("GET", "/api/v1/orgs/%s/billing/account" % org_a)
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
        status, _, account = viewer_a.request("GET", "/api/v1/orgs/%s/billing/account" % org_a)
        status2, _, entries = viewer_a.request("GET", "/api/v1/orgs/%s/billing/entries?limit=20" % org_a)
        status3, _, usage = viewer_a.request("GET", "/api/v1/orgs/%s/usage?limit=20" % org_a)
        debit = [e for e in (entries or {}).get("items", [])
                 if e.get("kind") == "debit" and "billing_receipt:debit:" in str(e.get("business_id", ""))]
        rows = [u for u in (usage or {}).get("items", []) if u.get("model") == args.price_model]
        if debit and rows:
            settle_visible_after = round(time.time() - settle_started, 3)
            break
        if time.time() > settle_deadline:
            break
        time.sleep(0.25)
    rec.capture("org-a-charge", {"org": org_a, "actor": "billing_viewer"},
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
        "企业角色=member 读账户会被 403（负例见 responses）",
    ], observations={"available_micro_before": pre_available, "available_micro_after": available,
                     "settled_entries": len(debit), "usage_rows": len(rows),
                     "settle_visible_after_s": settle_visible_after})
    status_neg, _, neg = member_a.request("GET", "/api/v1/orgs/%s/billing/account" % org_a)
    rec.capture("member-billing-forbidden", {"actor": "member_a"}, {"status": status_neg, "body": neg})

    # ---------------- S4 余额不足在上游之前被拒 ----------------
    status, _, key_b = member_b.request("POST", "/api/v1/orgs/%s/keys" % org_b, {})
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
    status, _, key_c = member_c.request("POST", "/api/v1/orgs/%s/keys" % org_a, {})
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

    # ---------------- S7 平台管理员的异常与审计面 ----------------
    status, _, exceptions = admin.request("GET", "/api/v1/orgs/%s/billing/exceptions" % org_a)
    status2, _, audit = admin.request("GET", "/api/v1/orgs/%s/audit?limit=50" % org_a)
    status3, _, statement = admin.request("GET", "/api/v1/orgs/%s/billing/statements" % org_a)
    rec.capture("platform-surfaces", {"org": org_a, "actor": "platform_admin"},
                {"exceptions": exceptions, "audit": audit, "statement": statement})
    totals = (exceptions or {}).get("totals", {}) if isinstance(exceptions, dict) else {}
    actions = sorted({item.get("action") for item in (audit or {}).get("items", [])})
    has_settle = any(a and a.startswith("billing.settlement") for a in actions)
    surfaces_ok = (status == 200 and status2 == 200 and status3 == 200
                   and "open" in totals and has_settle and len(actions) >= 3)
    rec.step("S7", "平台管理员的资金异常 / 审计 / 月度账单面", surfaces_ok, [
        "GET /billing/exceptions -> %s totals=%s（%s）" % (
            status, totals, "空队列也是真实事实" if not (exceptions or {}).get("items") else "有异常行"),
        "GET /audit -> %s %d 条，动作=%s" % (status2, len((audit or {}).get("items", [])),
                                            ", ".join(actions[:6])),
        "GET /billing/statements -> %s month=%s debit_micro=%s" % (
            status3, (statement or {}).get("month"),
            ((statement or {}).get("totals") or {}).get("debit_micro")),
    ], observations={"exception_open": totals.get("open"), "audit_actions": actions[:12]})

    # ---------------- S8 Redis 故障：fail-closed 与恢复 ----------------
    if args.with_failure and args.compose_cmd:
        status, _, key_f = member_a.request("POST", "/api/v1/orgs/%s/keys" % org_a, {})
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
        status, _, key_g = member_a.request("POST", "/api/v1/orgs/%s/keys" % org_a, {})
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

    # 1 成员登录 + 建 Key + 流式调用
    t0 = time.time()
    member = Session(args.console)
    member.login(identities["users"]["member_a"]["email"], identities["users"]["member_a"]["password"])
    status, _, key = member.request("POST", "/api/v1/orgs/%s/keys" % org_a, {})
    secret = key["secret"] if isinstance(key, dict) else None
    register_secret("key:demo", secret)
    call = wait_gateway(args.gateway, secret, args.price_model, until_ok, timeout=60)[0]
    demo_print(1, "成员登录 → 创建 Key → 流式调用", """
curl -s -c jar -X POST %s/api/v1/auth/login -H 'Content-Type: application/json' \\
  -d '{"email":"%s","password":"<来自 identities.json>"}'
curl -s -b jar -X POST %s/api/v1/orgs/%s/keys -H "X-CSRF-Token: <login.csrf_token>" -d '{}'
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

    # 2 企业查费
    viewer = Session(args.console)
    viewer.login(identities["users"]["viewer_a"]["email"], identities["users"]["viewer_a"]["password"])
    _, _, account = viewer.request("GET", "/api/v1/orgs/%s/billing/account" % org_a)
    # 结算由 dispatcher 异步完成：有界等待它可见，并记录时延（不要在这之前下结论）。
    settle_started = time.time()
    settle_visible_after = None
    debit = []
    entries = usage = None
    for _ in range(60):
        _, _, entries = viewer.request("GET", "/api/v1/orgs/%s/billing/entries?limit=5" % org_a)
        _, _, usage = viewer.request("GET", "/api/v1/orgs/%s/usage?limit=5" % org_a)
        debit = [e for e in (entries or {}).get("items", [])
                 if e.get("kind") == "debit"
                 and "billing_receipt:debit:" in str(e.get("business_id", ""))]
        if debit:
            settle_visible_after = round(time.time() - settle_started, 3)
            break
        time.sleep(0.25)
    demo_print(2, "企业 billing_viewer 看到这笔收费", """
curl -s -b viewer.jar %s/api/v1/orgs/%s/billing/account
curl -s -b viewer.jar '%s/api/v1/orgs/%s/billing/entries?limit=5'
curl -s -b viewer.jar '%s/api/v1/orgs/%s/usage?limit=5'
""" % (args.console, org_a, args.console, org_a, args.console, org_a),
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
    _, _, key_b = member_b.request("POST", "/api/v1/orgs/%s/keys" % org_b, {})
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
        _, _, key_f = member.request("POST", "/api/v1/orgs/%s/keys" % org_a, {})
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
    demo_print(6, "平台管理员的异常队列与审计", """
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
