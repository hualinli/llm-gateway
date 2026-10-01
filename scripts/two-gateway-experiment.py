#!/usr/bin/env python3
"""T09 two-gateway experiment: identity, config, limits and wallet across instances.

What it proves (each check writes raw evidence under --output):

  a. two Gateway processes with distinct ports and instance ids both serve the
     same Control/PostgreSQL/Redis, and Control's /gateways view reports both
     with their own applied version;
  b. a key revoked in the Control snapshot is refused by BOTH gateways, and the
     end-to-end propagation latency (Control persist -> refusal) is measured as
     P50/P95/P99 rather than asserted;
  c. the last affordable balance is admitted exactly once across the two data
     planes (T06B reservation through two processes);
  d. a published per-org RPM is shared through Redis across instances: exhausting
     the bucket on gateway A makes gateway B refuse, which a per-instance bucket
     could not do;
  e. a stale instance (no successful control refresh within the lease) refuses
     NEW requests while an in-flight stream drains, and recovers when Control
     returns.

Isolation: like scripts/isolated-baseline.py, this runs in a private Docker
project name, probes free host ports and never touches the daily demo stack.

NOTE on migrations.Version: this task owns migration 000013 but must not bump
migrations.Version (the parent task does that). Every service calls
migrations.Check, which compares schema_migrations.version against the compiled
constant, so the experiment applies 000013 and then pins schema_migrations back
to the pre-T09 version. That is recorded in evidence.json under
"migration_version_workaround" and is a build-ordering artefact, not a schema
claim.
"""

import argparse
import hashlib
import json
import os
import pathlib
import platform
import random
import re
import shutil
import socket
import string
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

ROOT = pathlib.Path(__file__).resolve().parent.parent
PROJECT_PREFIX = "llm-gw-t09-"
PG_IMAGE = "postgres:16.15"
REDIS_IMAGE = "redis:7.4.11"

# The migration every service checks against until the parent task bumps it.
PINNED_MIGRATION_VERSION = 12

MODEL = "gpt-t09"


def log(message):
    print(f"[t09] {message}", flush=True)


def now_utc():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def probe_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        return sock.getsockname()[1]


def run(args, **kwargs):
    kwargs.setdefault("cwd", ROOT)
    kwargs.setdefault("capture_output", True)
    kwargs.setdefault("text", True)
    kwargs.setdefault("encoding", "utf-8")
    kwargs.setdefault("errors", "replace")
    return subprocess.run(args, **kwargs)


def must_run(args, what, **kwargs):
    proc = run(args, **kwargs)
    if proc.returncode != 0:
        raise RuntimeError(f"{what} failed ({' '.join(map(str, args))}): {proc.stderr.strip()}")
    return proc


def wait_tcp(port, timeout, what):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"timed out waiting for {what} on 127.0.0.1:{port}")


def http_json(url, method="GET", payload=None, timeout=10, headers=None):
    data = None
    headers = dict(headers or {})
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(request, timeout=timeout) as response:
        body = response.read().decode()
    return json.loads(body) if body.strip() else {}


def http_status(url, method="GET", timeout=10):
    try:
        request = urllib.request.Request(url, method=method)
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status
    except urllib.error.HTTPError as err:
        return err.code
    except OSError:
        return 0


def wait_json(url, timeout, what, ready=lambda body: True):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            body = http_json(url)
            if ready(body):
                return body
            last = body
        except Exception as err:  # noqa: BLE001 - readiness polling
            last = err
        time.sleep(0.2)
    raise RuntimeError(f"timed out waiting for {what}: last={last!r}")


class Process:
    """A child process whose stdout/stderr are captured to one log file."""

    def __init__(self, name, args, env, log_path, cwd=ROOT):
        self.name = name
        self.log_path = log_path
        log_path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = open(log_path, "w", encoding="utf-8")
        self.handle.write(f"$ {' '.join(args)}\n# env: {json.dumps(env, sort_keys=True)}\n\n")
        self.handle.flush()
        self.proc = subprocess.Popen(args, cwd=cwd, env=env, stdout=self.handle,
                                     stderr=subprocess.STDOUT, text=True)

    def stop(self, timeout=10):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=timeout)
        self.handle.close()

    def alive(self):
        return self.proc.poll() is None

    def output(self):
        # Read from the path rather than the handle: stop() closes the handle,
        # and the combined log is assembled after every process has stopped.
        try:
            return self.log_path.read_text(encoding="utf-8", errors="replace")
        except OSError as err:
            return f"<log unavailable: {err}>"


class SSEResult:
    def __init__(self, status, body, headers, total_seconds, done_seconds, error=None, ttfb=None):
        self.status = status
        self.body = body
        self.headers = headers
        self.total_seconds = total_seconds
        self.done_seconds = done_seconds
        self.error = error
        self.ttfb = ttfb

    def as_dict(self):
        return {
            "status": self.status,
            "bytes": len(self.body),
            "has_done": "[DONE]" in self.body,
            "total_seconds": round(self.total_seconds, 6),
            "done_seconds": None if self.done_seconds is None else round(self.done_seconds, 6),
            "ttfb_seconds": None if self.ttfb is None else round(self.ttfb, 6),
            "error": self.error,
            "body_tail": self.body[-120:],
        }


def sse_request(base_url, key, model=MODEL, max_tokens=64, timeout=30, stop_after_done=False):
    """POST one streaming chat completion and measure the wire.

    stop_after_done=True returns as soon as the end marker is read (measuring how
    fast the *body* arrives); False reads until the response actually ends
    (measuring when the chunked response is closed).
    """
    payload = json.dumps({
        "model": model,
        "stream": True,
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": max_tokens,
    }).encode()
    request = urllib.request.Request(base_url + "/v1/chat/completions", data=payload, headers={
        "Authorization": "Bearer " + key,
        "Content-Type": "application/json",
    }, method="POST")
    start = time.time()
    chunks = []
    ttfb = None
    done_at = None
    status = 0
    headers = {}
    error = None
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            status = response.status
            headers = dict(response.headers)
            while True:
                chunk = response.read(1)
                if not chunk:
                    break
                if ttfb is None:
                    ttfb = time.time() - start
                chunks.append(chunk)
                if done_at is None and b"".join(chunks[-8:]).find(b"[DONE]") >= 0:
                    done_at = time.time() - start
                    if stop_after_done:
                        break
    except urllib.error.HTTPError as err:
        status = err.code
        try:
            body = err.read().decode()
        except Exception:  # noqa: BLE001
            body = ""
        return SSEResult(status, body, dict(err.headers or {}), time.time() - start, None)
    except Exception as err:  # noqa: BLE001 - a timeout is data here
        error = f"{type(err).__name__}: {err}"
    body = b"".join(chunks).decode("utf-8", errors="replace")
    return SSEResult(status, body, headers, time.time() - start, done_at, error, ttfb)


def percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, int(round((len(ordered) - 1) * fraction)))
    return ordered[index]


class Experiment:
    def __init__(self, args):
        self.args = args
        self.output = pathlib.Path(args.output).resolve()
        self.output.mkdir(parents=True, exist_ok=True)
        self.evidence = {
            "started_at": now_utc(),
            "host": {"platform": platform.platform(), "python": sys.version.split()[0]},
            "checks": {},
            "artifacts": [],
        }
        self.project = PROJECT_PREFIX + "".join(random.choices(string.ascii_lowercase + string.digits, k=6))
        self.processes = []
        self.containers = []
        self.raw_key = "sk-t09-" + "".join(random.choices(string.ascii_lowercase + string.digits, k=24))
        self.revoked_key = "sk-t09r-" + "".join(random.choices(string.ascii_lowercase + string.digits, k=24))
        # A second organization owns the wallet race, so no check ever has to
        # delete an append-only receipt or ledger row to reset the balance.
        self.raw_key_b = "sk-t09b-" + "".join(random.choices(string.ascii_lowercase + string.digits, k=24))
        self.ports = {name: probe_port() for name in (
            "pg", "redis", "control_grpc", "control_http", "gateway_a", "gateway_b",
            "diag_a", "diag_b", "upstream_a")}
        self.org_id = None
        self.org_b_id = None
        self.user_id = None
        self.key_id = None
        self.key_b_id = None
        self.revoked_key_id = None
        self.desired_version = 0
        self.key_records = []
        self.log_paths = {}

    # --- infrastructure -----------------------------------------------------

    def artifact(self, name, text):
        path = self.output / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
        self.evidence["artifacts"].append(name)
        return path

    def docker(self, args, what):
        return must_run(["docker"] + args, what)

    def start_dependencies(self):
        pg_name = self.project + "-pg"
        redis_name = self.project + "-redis"
        self.docker([
            "run", "-d", "--rm", "--name", pg_name,
            "-p", f"127.0.0.1:{self.ports['pg']}:5432",
            "-e", "POSTGRES_DB=gateway_test", "-e", "POSTGRES_USER=gateway_test",
            "-e", "POSTGRES_PASSWORD=test_only_local",
            "--tmpfs", "/var/lib/postgresql/data",
            PG_IMAGE,
        ], "start postgres")
        self.containers.append(pg_name)
        self.docker([
            "run", "-d", "--rm", "--name", redis_name,
            "-p", f"127.0.0.1:{self.ports['redis']}:6379",
            REDIS_IMAGE, "redis-server", "--save", "", "--appendonly", "no",
        ], "start redis")
        self.containers.append(redis_name)
        wait_tcp(self.ports["pg"], 60, "postgres")
        wait_tcp(self.ports["redis"], 30, "redis")
        deadline = time.time() + 60
        while time.time() < deadline:
            proc = run(["docker", "exec", pg_name, "pg_isready", "-U", "gateway_test", "-d", "gateway_test"])
            if proc.returncode == 0:
                return
            time.sleep(0.3)
        raise RuntimeError("postgres never became ready")

    def psql(self, sql):
        proc = must_run([
            "docker", "exec", "-i", self.project + "-pg",
            "psql", "-v", "ON_ERROR_STOP=1", "-U", "gateway_test", "-d", "gateway_test",
            "-At", "-c", sql,
        ], "psql")
        return proc.stdout.strip()

    def psql_value(self, sql):
        """First result row of a statement. psql also prints the command tag
        ("INSERT 0 1") for non-SELECT statements, so scalar reads take the
        first line rather than the whole output."""
        output = self.psql(sql)
        lines = [line for line in output.splitlines() if line.strip()]
        return lines[0] if lines else ""

    @property
    def dsn(self):
        return (f"postgres://gateway_test:test_only_local@127.0.0.1:{self.ports['pg']}"
                "/gateway_test?sslmode=disable")

    def build(self):
        # Binaries are built outside the evidence directory: they are ~80 MB of
        # build output and must never be committed as evidence.
        self.build_dir = pathlib.Path(tempfile.mkdtemp(prefix=self.project + "-build-"))
        binary_dir = self.build_dir
        for name in ("control", "gateway", "upstream-mock", "migrate"):
            must_run(["go", "build", "-o", str(binary_dir / name), f"./cmd/{name}"],
                     f"build {name}", cwd=ROOT, timeout=600)
        return binary_dir

    def base_env(self):
        env = dict(os.environ)
        env.update({
            "GATEWAY_PROFILE": "demo",
            "POSTGRES_DSN": self.dsn,
            "REDIS_ADDR": f"127.0.0.1:{self.ports['redis']}",
        })
        return env

    def control_env(self):
        return {
            "CONTROL_GRPC_ADDR": f"127.0.0.1:{self.ports['control_grpc']}",
            "CONTROL_HTTP_ADDR": f"127.0.0.1:{self.ports['control_http']}",
            "CONTROL_REQUIRE_POSTGRES": "true",
            "CONTROL_ADMIN_TOKEN": self.args.admin_token,
        }

    def start_process(self, name, args, env_overrides=None):
        env = self.base_env()
        if env_overrides:
            env.update(env_overrides)
        path = self.output / "logs" / f"{name}.log"
        proc = Process(name, args, env, path)
        self.processes.append(proc)
        self.log_paths[name] = path
        return proc

    # --- setup --------------------------------------------------------------

    def apply_migrations(self):
        binary_dir = self.build_dir
        proc = run([str(binary_dir / "migrate"), "up"], env=self.base_env(), cwd=ROOT, timeout=300)
        self.artifact("migrate.log", proc.stdout + proc.stderr)
        if proc.returncode != 0:
            raise RuntimeError(f"migrate up failed: {proc.stderr}")
        version = self.psql_value("SELECT version FROM schema_migrations")
        # See the module docstring: 000013 is applied but the pinned constant is
        # not bumped in this task, so the services that call migrations.Check
        # would refuse to start. Pin the recorded version back and say so.
        self.psql(f"UPDATE schema_migrations SET version = {PINNED_MIGRATION_VERSION}, dirty = false")
        self.evidence["migration_version_workaround"] = {
            "applied_version": int(version),
            "pinned_version": PINNED_MIGRATION_VERSION,
            "why": "migrations.Version is owned by the parent task; services call migrations.Check and would refuse to start at 13",
        }
        tables = self.psql(
            "SELECT table_name FROM information_schema.tables "
            "WHERE table_schema='public' AND table_name IN "
            "('gateway_instances','gateway_instance_status') ORDER BY table_name")
        self.evidence["migration_version_workaround"]["t09_tables"] = tables.split()

    def seed(self):
        key_hash = hashlib.sha256(self.raw_key.encode()).hexdigest()
        revoked_hash = hashlib.sha256(self.revoked_key.encode()).hexdigest()
        sql = f"""
        INSERT INTO users (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
        VALUES ('t09@t09.test', 'hash', 'active', false, 1, now(), now()) RETURNING id;
        """
        self.user_id = int(self.psql_value(sql))
        self.org_id = int(self.psql_value(f"""
        INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
        VALUES ('enterprise', 't09', 'active', {self.user_id}, now(), now()) RETURNING id;
        """))
        self.psql(f"""
        INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
        VALUES ({self.org_id}, {self.user_id}, 'owner', 'active', now(), now());
        """)
        # The wallet-race organization: a second org, same owner, one key.
        self.org_b_id = int(self.psql_value(f"""
        INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
        VALUES ('enterprise', 't09-wallet-race', 'active', {self.user_id}, now(), now()) RETURNING id;
        """))
        self.psql(f"""
        INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
        VALUES ({self.org_b_id}, {self.user_id}, 'owner', 'active', now(), now());
        """)
        key_b_hash = hashlib.sha256(self.raw_key_b.encode()).hexdigest()
        self.key_id = "key_t09_" + "".join(random.choices(string.hexdigits.lower(), k=24))
        self.key_b_id = "key_t09b_" + "".join(random.choices(string.hexdigits.lower(), k=24))
        self.revoked_key_id = "key_t09r_" + "".join(random.choices(string.hexdigits.lower(), k=24))
        self.psql(f"""
        INSERT INTO api_keys (key_id, key_hash, key_prefix, org_id, owner_user_id, created_by,
                              status, policy_version, created_at)
        VALUES ('{self.key_id}', '{key_hash}', '{self.raw_key[:10]}', {self.org_id},
                {self.user_id}, {self.user_id}, 'active', 1, now()),
               ('{self.key_b_id}', '{key_b_hash}', '{self.raw_key_b[:10]}', {self.org_b_id},
                {self.user_id}, {self.user_id}, 'active', 1, now()),
               ('{self.revoked_key_id}', '{revoked_hash}', '{self.revoked_key[:11]}', {self.org_id},
                {self.user_id}, {self.user_id}, 'active', 1, now());
        """)
        # A price of 1 micro per token in both directions makes a receipt's charge
        # equal to its token count (same convention as the T06C test harness).
        self.psql(f"""
        INSERT INTO price_versions (model, currency, input_price_micro_per_million,
                                    output_price_micro_per_million, effective_at, created_at)
        VALUES ('{MODEL}', 'CNY', 1000000, 1000000, now() - interval '1 hour', now());
        """)
        # Fund generously up front: every check below runs with GATEWAY_BILLING=pg,
        # and a reservation is held until T07 settlement (which this runner does
        # not run). check_c resets and re-funds the wallet for the exact-race.
        self.set_balance(50_000_000)
        self.set_balance(0, self.org_b_id)
        self.evidence["seed"] = {
            "org_id": self.org_id, "org_b_id": self.org_b_id, "user_id": self.user_id,
            "key_id": self.key_id, "key_b_id": self.key_b_id,
            "revoked_key_id": self.revoked_key_id, "model": MODEL,
        }

    def live_key_records(self, status="active"):
        """The snapshot's key set: the live key, the wallet-race key and the
        (never revoked in the snapshot) second key used by the revoke check."""
        return [self.key_record(self.key_id, self.raw_key, status=status),
                self.key_record(self.key_b_id, self.raw_key_b),
                self.key_record(self.revoked_key_id, self.revoked_key)]

    def key_record(self, key_id, raw_key, status="active"):
        return {
            "key_id": key_id,
            "key_hash": hashlib.sha256(raw_key.encode()).hexdigest(),
            "key_prefix": raw_key[:10],
            "tenant_id": f"org:{self.org_id}",
            "status": status,
            "org_id": self.org_id,
            "owner_user_id": self.user_id,
        }

    @property
    def admin_headers(self):
        return {"Authorization": "Bearer " + self.args.admin_token}

    def control_json(self, path, method="GET", payload=None, timeout=30):
        return http_json(self.control_http() + path, method=method, payload=payload,
                         timeout=timeout, headers=self.admin_headers)

    def post_config(self, keys=None, org_policies=None, routes=None, providers=None):
        payload = {}
        if keys is not None:
            payload["key_records"] = keys
        if org_policies is not None:
            payload["org_policies"] = org_policies
        if routes is not None:
            payload["routes"] = routes
        if providers is not None:
            payload["providers"] = providers
        body = self.control_json("/config", method="POST", payload=payload, timeout=30)
        self.desired_version = body["desired_version"]
        return body

    def control_http(self):
        return f"http://127.0.0.1:{self.ports['control_http']}"

    def wait_control_http(self, timeout):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            try:
                # /healthz is plain text, so readiness is checked with the raw
                # status helper rather than the JSON one.
                if http_status(self.control_http() + "/healthz", timeout=5) == 200:
                    return
            except Exception as err:  # noqa: BLE001 - readiness polling
                last = err
            time.sleep(0.2)
        raise RuntimeError(f"timed out waiting for the control admin API: {last!r}")

    def wait_http_ok(self, url, timeout, what):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            status = http_status(url, timeout=5)
            if status == 200:
                return
            last = status
            time.sleep(0.2)
        raise RuntimeError(f"timed out waiting for {what} at {url}: last status={last}")

    def wait_control_instances(self, timeout, what, ready):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            try:
                body = self.control_json("/gateways")
                if ready(body):
                    return body
                last = body
            except Exception as err:  # noqa: BLE001
                last = err
            time.sleep(0.2)
        raise RuntimeError(f"timed out waiting for {what}: last={last!r}")

    def gateway(self, which):
        return f"http://127.0.0.1:{self.ports['gateway_' + which]}"

    def diag(self, which):
        return f"http://127.0.0.1:{self.ports['diag_' + which]}"

    def start_services(self):
        binary_dir = self.build_dir
        self.apply_migrations()
        self.seed()

        self.start_process("upstream", [
            str(binary_dir / "upstream-mock"),
            "-listen", f"127.0.0.1:{self.ports['upstream_a']}", "-name", "primary",
            "-ttft", "5ms", "-token-interval", "2ms", "-tokens", "8",
        ])
        self.start_process("control", [str(binary_dir / "control")], self.control_env())
        wait_tcp(self.ports["control_grpc"], 30, "control grpc")
        self.wait_control_http(30)

        self.post_config(
            keys=self.live_key_records(),
            routes={MODEL: ["primary"]},
            providers={"primary": {
                "id": "primary", "protocol": "openai-compatible",
                "url": f"http://127.0.0.1:{self.ports['upstream_a']}",
                "model": MODEL,
                "timeouts": {"attempt_timeout_ms": 90000},
            }},
        )
        self.evidence["setup_version"] = self.desired_version

        for which, instance in (("a", "gw-a"), ("b", "gw-b")):
            self.start_process(f"gateway_{which}", [str(binary_dir / "gateway")], {
                "GATEWAY_ADDR": f"127.0.0.1:{self.ports['gateway_' + which]}",
                "GATEWAY_DIAGNOSTICS_ADDR": f"127.0.0.1:{self.ports['diag_' + which]}",
                "GATEWAY_INSTANCE_ID": instance,
                "CONTROL_ADDR": f"127.0.0.1:{self.ports['control_grpc']}",
                "GATEWAY_ACCESS_GUARD": "pg",
                "GATEWAY_BILLING": "pg",
                "LIMITER_ON_REDIS_ERROR": "closed",
                "GATEWAY_MAX_ACTIVE_STREAMS": "64",
                "GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG": "32",
                "CONTROL_CONFIG_POLL_INTERVAL": "2s",
                "CONTROL_CONFIG_LEASE": "30s",
                "CONTROL_STATUS_REPORT_INTERVAL": "2s",
                "METERING_SPOOL_DIR": str(self.output / f"spool-{which}"),
            })
        for which in ("a", "b"):
            self.wait_http_ok(self.gateway(which) + "/healthz", 60, f"gateway {which} livez")
            self.wait_control_instances(60, f"gateway {which} first report",
                                        lambda body: any(
                                            target["instance_id"] == f"gw-{which}" and target["health"] == "active"
                                            for target in body["instances"]))

    # --- checks -------------------------------------------------------------

    def check_a_both_serve(self):
        served = {}
        for which in ("a", "b"):
            result = sse_request(self.gateway(which), self.raw_key)
            served[which] = result.as_dict()
        view = self.control_json("/gateways")
        self.evidence["checks"]["a_both_serve"] = {
            "gateway_a": served["a"],
            "gateway_b": served["b"],
            "control_view": view,
        }
        both_200 = served["a"]["status"] == 200 and served["b"]["status"] == 200
        both_done = served["a"]["has_done"] and served["b"]["has_done"]
        instances = {target["instance_id"]: target for target in view["instances"]}
        return {
            "pass": both_200 and both_done and len(instances) >= 2,
            "both_served": both_200 and both_done,
            "instances": sorted(instances),
            "applied_versions": {name: target["applied_version"] for name, target in instances.items()},
        }

    def check_b_revocation(self, rounds=None):
        rounds = rounds or self.args.revocation_rounds
        latencies = {"a": [], "b": []}
        samples = []
        for round_index in range(rounds):
            # Round 0 revokes on the normal snapshot; later rounds restore and
            # revoke again so the measurement is a distribution, not one sample.
            revoked = self.live_key_records(status="revoked")
            active = self.live_key_records()
            target = revoked if round_index % 2 == 0 else active
            expected = 401 if round_index % 2 == 0 else 200
            started = time.time()
            self.post_config(keys=target)
            published = time.time()
            per_gateway = {}
            for which in ("a", "b"):
                deadline = time.time() + 30
                observed = None
                while time.time() < deadline:
                    result = sse_request(self.gateway(which), self.raw_key, timeout=15)
                    if result.status == expected:
                        observed = time.time() - published
                        break
                    time.sleep(0.02)
                if observed is None:
                    per_gateway[which] = None
                else:
                    per_gateway[which] = observed
                    latencies[which].append(observed)
            samples.append({
                "round": round_index,
                "action": "revoke" if round_index % 2 == 0 else "restore",
                "expected_status": expected,
                "persist_seconds": published - started,
                "propagation_seconds": per_gateway,
            })
        # Leave the key active at the end.
        if rounds % 2 == 1:
            self.post_config(keys=self.live_key_records())
        combined = latencies["a"] + latencies["b"]
        summary = {
            "rounds": rounds,
            "samples": samples,
            "combined": {
                "n": len(combined),
                "p50": percentile(combined, 0.50),
                "p95": percentile(combined, 0.95),
                "p99": percentile(combined, 0.99),
                "max": max(combined) if combined else None,
            },
            "per_gateway": {which: {"n": len(values), "p50": percentile(values, 0.50),
                                    "p99": percentile(values, 0.99),
                                    "max": max(values) if values else None}
                            for which, values in latencies.items()},
            "target_seconds": 5.0,
        }
        self.evidence["checks"]["b_revocation_propagation"] = summary
        return {
            "pass": len(combined) == rounds * 2 and (summary["combined"]["p99"] or 99) <= 5.0,
            "p99_seconds": summary["combined"]["p99"],
            "max_seconds": summary["combined"]["max"],
            "samples": len(combined),
        }

    def measure_reservation(self, key, org_id):
        """Admit one request and read what the reservation actually held.

        billing_executions has no surrogate key (execution_id is the primary
        key), so the newest row for the organization is the one this request
        created. A request that was refused before admission must not have
        created a row at all.
        """
        before = int(self.psql_value(
            f"SELECT count(*) FROM billing_executions WHERE org_id = {org_id}"))
        result = sse_request(self.gateway("a"), key)
        after = int(self.psql_value(
            f"SELECT count(*) FROM billing_executions WHERE org_id = {org_id}"))
        if after == before:
            return result, None
        rows = self.psql_value(
            "SELECT reserved_micro FROM billing_executions WHERE org_id = "
            f"{org_id} ORDER BY created_at DESC, execution_id DESC LIMIT 1")
        return result, (int(rows) if rows else None)

    def check_c_last_balance(self):
        """The last affordable balance is admitted exactly once across two gateways.

        The race uses the second organization, so no append-only receipt or
        ledger row has to be deleted to reset the wallet between phases. Only
        the account projection is reset, which is stated in the evidence.
        """
        org = self.org_b_id
        key = self.raw_key_b

        # Phase 1: an unfunded wallet must refuse before any execution row exists.
        self.set_balance(0, org)
        probe, reserved = self.measure_reservation(key, org)
        if probe.status != 402 or reserved is not None:
            self.evidence["checks"]["c_last_balance"] = {
                "phase1": probe.as_dict(), "reserved_micro": reserved,
                "note": "an unfunded wallet must refuse before any execution row exists",
            }
            return {"pass": False, "reason": "phase 1 did not refuse an unfunded wallet"}

        # Phase 2: fund generously once to learn the reservation the admission
        # policy computes for this exact request shape.
        self.set_balance(10_000_000, org)
        probe, reserved = self.measure_reservation(key, org)
        if probe.status != 200 or not reserved:
            self.evidence["checks"]["c_last_balance"] = {
                "phase2_probe": probe.as_dict(), "reserved_micro": reserved}
            return {"pass": False, "reason": "could not measure the reservation size"}

        # Phase 3: exactly one reservation of available balance, two data planes.
        self.set_balance(reserved, org)
        marker = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f+00")
        results = {}
        barrier = threading.Barrier(2)

        def fire(which):
            barrier.wait()
            results[which] = sse_request(self.gateway(which), key, timeout=30)

        threads = [threading.Thread(target=fire, args=(which,)) for which in ("a", "b")]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()

        executions = int(self.psql_value(
            f"SELECT count(*) FROM billing_executions WHERE org_id = {org} AND created_at >= '{marker}'"))
        account = self.psql_value(
            f"SELECT posted_balance_micro, reserved_micro FROM billing_accounts WHERE org_id = {org}")
        admitted = sorted(which for which, result in results.items() if result.status == 200)
        refused = sorted(which for which, result in results.items() if result.status == 402)
        evidence = {
            "organization": "second organization (org_b) so nothing is deleted to reset the wallet",
            "reserved_micro": reserved,
            "funded_micro": reserved,
            "results": {which: result.as_dict() for which, result in results.items()},
            "admitted": admitted,
            "refused_402": refused,
            "execution_rows": executions,
            "account": account,
        }
        self.evidence["checks"]["c_last_balance"] = evidence
        self.set_balance(50_000_000, org)
        return {
            "pass": len(admitted) == 1 and len(refused) == 1 and executions == 1,
            "admitted": admitted, "refused": refused, "reserved_micro": reserved,
            "execution_rows": executions,
        }

    def set_balance(self, micro, org_id=None):
        org = self.org_id if org_id is None else org_id
        self.psql(f"""
        INSERT INTO billing_accounts (org_id, posted_balance_micro, reserved_micro, status, version,
                                      created_at, updated_at)
        VALUES ({org}, {micro}, 0, 'active', 1, now(), now())
        ON CONFLICT (org_id) DO UPDATE SET posted_balance_micro = {micro}, reserved_micro = 0,
            version = billing_accounts.version + 1, updated_at = now();
        """)

    def check_d_org_rpm(self):
        """A published org RPM is one shared bucket, not one bucket per instance."""
        rpm, burst = 60, 2
        self.post_config(org_policies={str(self.org_id): {"requests_per_minute": rpm, "burst": burst}})
        deadline = time.time() + 10
        applied = False
        while time.time() < deadline:
            view = self.control_json("/gateways")
            if all(target["applied_version"] == self.desired_version for target in view["instances"]):
                applied = True
                break
            time.sleep(0.2)
        self.redis_delete(f"ratelimit:org:{self.org_id}")

        # Direction 1: spend the whole burst on A, then B must refuse.
        first = [sse_request(self.gateway("a"), self.raw_key) for _ in range(burst)]
        from_b = sse_request(self.gateway("b"), self.raw_key)
        direction_one = {
            "gateway_a_statuses": [result.status for result in first],
            "gateway_b_status": from_b.status,
        }
        self.redis_delete(f"ratelimit:org:{self.org_id}")
        # Direction 2: the mirror image, so the result cannot be a per-gateway bucket.
        second = [sse_request(self.gateway("b"), self.raw_key) for _ in range(burst)]
        from_a = sse_request(self.gateway("a"), self.raw_key)
        direction_two = {
            "gateway_b_statuses": [result.status for result in second],
            "gateway_a_status": from_a.status,
        }
        self.redis_delete(f"ratelimit:org:{self.org_id}")
        self.post_config(org_policies={str(self.org_id): {"requests_per_minute": 100000, "burst": 100000}})

        evidence = {
            "policy": {"requests_per_minute": rpm, "burst": burst},
            "redis_key": f"ratelimit:org:{self.org_id}",
            "all_instances_applied": applied,
            "direction_a_exhausts_then_b": direction_one,
            "direction_b_exhausts_then_a": direction_two,
            "per_instance_alternative": {
                "would_allow": burst * 2,
                "note": "a per-instance bucket would let B serve a full burst after A spent one",
            },
        }
        self.evidence["checks"]["d_org_rpm_shared"] = evidence
        passed = (direction_one["gateway_a_statuses"] == [200] * burst and direction_one["gateway_b_status"] == 429
                  and direction_two["gateway_b_statuses"] == [200] * burst and direction_two["gateway_a_status"] == 429)
        return {"pass": passed, "direction_a_exhausts_then_b": direction_one,
                "direction_b_exhausts_then_a": direction_two}

    def gateway_metrics(self, which, names=(
            "process_resident_memory_bytes", "go_memstats_heap_alloc_bytes",
            "go_memstats_heap_inuse_bytes", "go_memstats_heap_objects")):
        text = urllib.request.urlopen(self.diag(which) + "/metrics", timeout=20).read().decode()
        out = {}
        for name in names:
            match = re.search(r"^" + name + r" (\S+)$", text, re.M)
            if match:
                out[name] = float(match.group(1))
        return out

    def goroutine_dump(self, which):
        url = self.diag(which) + "/debug/pprof/goroutine?debug=2"
        return urllib.request.urlopen(url, timeout=20).read().decode()

    def redis_delete(self, key):
        must_run(["docker", "exec", self.project + "-redis", "redis-cli", "DEL", key], "redis del")

    def open_slow_stream(self, which, interval_ms=2000, tokens=20, timeout=120):
        """Start one genuinely long SSE stream and return (response, first_bytes).

        The mock upstream honours x_mock_token_interval_ms / x_mock_tokens,
        which the Gateway forwards verbatim, so the stream is still producing
        events while the control lease expires. That is what makes "in-flight
        streams drain" a real observation instead of a race with a fast stream.
        """
        request = urllib.request.Request(self.gateway(which) + "/v1/chat/completions",
                                        data=json.dumps({
                                            "model": MODEL, "stream": True,
                                            "messages": [{"role": "user", "content": "hi"}],
                                            "max_tokens": 128,
                                            "x_mock_token_interval_ms": interval_ms,
                                            "x_mock_tokens": tokens,
                                        }).encode(),
                                        headers={"Authorization": "Bearer " + self.raw_key,
                                                 "Content-Type": "application/json"},
                                        method="POST")
        response = urllib.request.urlopen(request, timeout=timeout)
        return response, response.read(96)

    def check_e_stale_instance(self):
        """Control up -> long stream -> Control down past the lease."""
        self.set_balance(50_000_000)
        response, first_chunk = self.open_slow_stream("a")
        inflight_started = time.time()
        self.post_config(org_policies={str(self.org_id): {"requests_per_minute": 100000, "burst": 100000}})
        control = next(proc for proc in self.processes if proc.name == "control")
        control.stop()

        # Wait past the lease (30s) plus one poll interval.
        time.sleep(self.args.lease_wait)

        stale_status = sse_request(self.gateway("a"), self.raw_key, timeout=20).status
        metrics_text = urllib.request.urlopen(self.diag("a") + "/metrics", timeout=10).read().decode()

        # The in-flight stream must still drain: read the rest of the body now.
        rest = response.read()
        response.close()
        body = first_chunk + rest
        drained = b"[DONE]" in body

        # Restart Control: the same version must be re-applied and serving resumed.
        binary_dir = self.build_dir
        self.processes.remove(control)
        self.start_process("control", [str(binary_dir / "control")], self.control_env())
        recovered_status, recovered_seconds = None, None
        deadline = time.time() + 60
        while time.time() < deadline:
            status = sse_request(self.gateway("a"), self.raw_key, timeout=20).status
            if status == 200:
                recovered_status = status
                recovered_seconds = time.time() - inflight_started
                break
            time.sleep(0.2)

        def metric(name):
            match = re.search(r"^" + name + r" (\S+)$", metrics_text, re.M)
            return match.group(1) if match else None

        evidence = {
            "lease_configured_seconds": 30.0,
            "waited_seconds": self.args.lease_wait,
            "new_request_status_while_stale": stale_status,
            "inflight_stream_drained": drained,
            "inflight_bytes": len(body),
            "inflight_stream_had_done": b"[DONE]" in body,
            "metrics_while_stale": {
                "gateway_control_lease_expired_total": metric("gateway_control_lease_expired_total"),
                "gateway_control_stale": metric("gateway_control_stale"),
                "gateway_control_applied_config_version": metric("gateway_control_applied_config_version"),
            },
            "recovered_status": recovered_status,
            "recovered_after_seconds": recovered_seconds,
        }
        self.evidence["checks"]["e_stale_instance"] = evidence
        return {
            "pass": stale_status == 401 and drained and recovered_status == 200,
            "stale_status": stale_status, "drained": drained, "recovered": recovered_status == 200,
        }

    def check_f_propagation_watch(self):
        """A healthy-instance revocation should not wait for the poll interval."""
        # This is the same measurement as check b, reported separately with the
        # watch push active; the poll interval is 2s so a poll-only path would
        # show up as ~1-2s here instead of milliseconds.
        samples = []
        for round_index in range(4):
            revoke = round_index % 2 == 0
            status = "revoked" if revoke else "active"
            self.post_config(keys=self.live_key_records(status=status))
            published = time.time()
            expected = 401 if revoke else 200
            observed = {}
            # The response-end time is recorded next to the propagation latency:
            # this loop revokes/restores the snapshot (a Control hot reload)
            # while requests are in flight, which is the condition the recorded
            # "response does not end after [DONE]" observation suspected.
            ends = []
            for which in ("a", "b"):
                while time.time() - published < 20:
                    result = sse_request(self.gateway(which), self.raw_key, timeout=15)
                    ends.append(round(result.total_seconds, 6))
                    if result.status == expected:
                        observed[which] = time.time() - published
                        break
                    time.sleep(0.01)
            samples.append({"round": round_index, "expected": expected, "latency": observed,
                            "response_end_seconds": ends})
        if len(samples) % 2 == 1:
            self.post_config(keys=self.live_key_records())
        combined = [value for sample in samples for value in sample["latency"].values()]
        ends = [value for sample in samples for value in sample["response_end_seconds"]]
        self.evidence["checks"]["f_watch_push"] = {
            "samples": samples,
            "n": len(combined),
            "p50": percentile(combined, 0.50),
            "p99": percentile(combined, 0.99),
            "max": max(combined) if combined else None,
            "poll_interval_seconds": 2.0,
            "response_end_seconds": {
                "n": len(ends),
                "p50": percentile(ends, 0.50),
                "p99": percentile(ends, 0.99),
                "max": max(ends) if ends else None,
                "note": "when the chunked response actually ended, measured while the snapshot was being republished",
            },
        }
        return {"pass": bool(combined) and max(combined) < 2.0,
                "max_seconds": max(combined) if combined else None,
                "note": "below the 2s poll interval proves the Watch push, not polling"}

    def check_g_hang_probe(self):
        """The recorded 70-90s 'response does not end after [DONE]' probe."""
        self.set_balance(50_000_000)
        samples = []
        dumps = []
        for index in range(self.args.hang_rounds):
            result = sse_request(self.gateway("a"), self.raw_key, timeout=self.args.hang_timeout)
            samples.append(result.as_dict())
            if result.status != 200:
                break
            if result.total_seconds > self.args.hang_threshold:
                dump = self.goroutine_dump("a")
                name = f"hang-goroutines-round{index}.txt"
                self.artifact(name, dump)
                dumps.append(name)
        slow = [sample for sample in samples if not sample["has_done"] or sample["total_seconds"] > 5]
        self.evidence["checks"]["g_hang_probe"] = {
            "rounds": len(samples),
            "total_seconds": [sample["total_seconds"] for sample in samples],
            "done_seconds": [sample["done_seconds"] for sample in samples],
            "slow_or_unterminated": slow,
            "goroutine_dumps": dumps,
            "threshold_seconds": self.args.hang_threshold,
            "note": "total_seconds measures when the chunked response actually ended; done_seconds when [DONE] arrived",
        }
        return {"pass": not slow, "slow": slow, "rounds": len(samples)}

    def check_h_snapshot_size(self, key_counts=(1000, 10000)):
        """Control snapshot size, persist cost and Gateway memory at 1k/10k keys."""
        results = []
        memory = {"before": {which: self.gateway_metrics(which) for which in ("a", "b")}}
        for count in key_counts:
            records = []
            for index in range(count):
                raw = f"sk-t09-bulk-{count}-{index}"
                records.append({
                    "key_id": f"key_t09_bulk_{count}_{index}",
                    "key_hash": hashlib.sha256(raw.encode()).hexdigest(),
                    "key_prefix": f"sk-t09-{index % 97:02d}",
                    "tenant_id": f"org:{self.org_id}",
                    "status": "active",
                    "org_id": self.org_id,
                    "owner_user_id": self.user_id,
                })
            # Keep the two live keys usable for the other checks.
            records.extend(self.live_key_records())
            started = time.time()
            body = self.post_config(keys=records)
            persist_seconds = time.time() - started
            payload_bytes = len(json.dumps({"key_records": records}).encode())
            results.append({
                "keys": len(records),
                "request_bytes": payload_bytes,
                "persist_seconds": round(persist_seconds, 4),
                "desired_version": body["desired_version"],
            })
            # Wait until every instance reports the new version, then read the
            # process memory the snapshot costs each Gateway.
            self.wait_control_instances(60, f"both instances applied v{body['desired_version']}",
                                        lambda view, version=body["desired_version"]: all(
                                            target["applied_version"] == version
                                            for target in view["instances"]))
            time.sleep(1.0)
            memory[f"after_{len(records)}_keys"] = {which: self.gateway_metrics(which) for which in ("a", "b")}
        self.evidence["checks"]["h_snapshot_size"] = {
            "measurements": results,
            "gateway_memory": memory,
            "note": "request_bytes is the JSON admin payload; the stored payload is protojson of the same records; "
                    "memory is per Gateway process after it applied that snapshot",
        }
        # Restore the two live keys and let the gateways shrink again.
        self.post_config(keys=self.live_key_records())
        time.sleep(2.0)
        # Force a garbage collection before reading memory again: without it the
        # post-restore numbers show retained arenas, not live objects, and a
        # "the snapshot memory came back" claim would be unfounded. The stdlib
        # pprof handler runs runtime.GC() when the request carries ?gc=1.
        for which in ("a", "b"):
            try:
                urllib.request.urlopen(self.diag(which) + "/debug/pprof/heap?gc=1", timeout=30).read()
            except Exception:  # noqa: BLE001 - memory numbers are still reported
                pass
        time.sleep(1.0)
        self.evidence["checks"]["h_snapshot_size"]["gateway_memory"]["after_restore_after_gc"] = {
            which: self.gateway_metrics(which) for which in ("a", "b")}
        return {"pass": True, "measurements": results, "memory": memory}

    # --- driver -------------------------------------------------------------

    def run(self):
        try:
            self.start_dependencies()
            self.build()
            self.start_services()
            checks = {}
            order = [
                ("a", self.check_a_both_serve),
                ("f", self.check_f_propagation_watch),
                ("b", self.check_b_revocation),
                ("d", self.check_d_org_rpm),
                ("c", self.check_c_last_balance),
                ("g", self.check_g_hang_probe),
                ("h", self.check_h_snapshot_size),
                ("e", self.check_e_stale_instance),
            ]
            for name, function in order:
                log(f"check {name}: {function.__name__}")
                try:
                    checks[name] = function()
                except Exception as err:  # noqa: BLE001 - one failed check must not hide the rest
                    checks[name] = {"pass": False, "error": f"{type(err).__name__}: {err}"}
                log(f"check {name}: {checks[name]}")
            self.evidence["checks_summary"] = checks
            self.evidence["passed"] = all(
                isinstance(value, dict) and value.get("pass") for value in checks.values())
        finally:
            for proc in list(self.processes):
                proc.stop()
            self.dump_logs()
            for container in self.containers:
                run(["docker", "rm", "-f", container])
            build_dir = getattr(self, "build_dir", None)
            if build_dir is not None:
                shutil.rmtree(build_dir, ignore_errors=True)
            self.evidence["finished_at"] = now_utc()
            (self.output / "evidence.json").write_text(
                json.dumps(self.evidence, indent=2, sort_keys=True, default=str), encoding="utf-8")
        return self.evidence

    def dump_logs(self):
        combined = []
        for proc in self.processes:
            try:
                text = proc.output()
            except Exception as err:  # noqa: BLE001
                text = f"<log unavailable: {err}>"
            combined.append(f"===== {proc.name} ({proc.log_path.name}) =====\n{text}")
        self.artifact("process-logs.txt", "\n".join(combined))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--output", default="docs/verification/2026-10-01/t09-multi-instance",
                        help="directory for raw evidence")
    parser.add_argument("--revocation-rounds", type=int, default=20)
    parser.add_argument("--hang-rounds", type=int, default=12)
    parser.add_argument("--hang-timeout", type=float, default=20.0)
    parser.add_argument("--hang-threshold", type=float, default=5.0,
                        help="dump gateways goroutines when a probe exceeds this")
    parser.add_argument("--lease-wait", type=float, default=36.0)
    parser.add_argument("--admin-token", default="t09-control-token",
                        help="CONTROL_ADMIN_TOKEN for the experiment's own control plane")
    parser.add_argument("--skip", nargs="*", default=[], help="check names to skip")
    args = parser.parse_args()

    experiment = Experiment(args)
    log(f"project={experiment.project} ports={experiment.ports}")
    evidence = experiment.run()
    summary = evidence.get("checks_summary", {})
    for name, value in summary.items():
        status = "PASS" if isinstance(value, dict) and value.get("pass") else "FAIL"
        log(f"{status} {name}: {json.dumps(value, default=str)[:400]}")
    log(f"evidence: {experiment.output / 'evidence.json'}")
    return 0 if evidence.get("passed") else 1


if __name__ == "__main__":
    sys.exit(main())
