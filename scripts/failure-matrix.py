#!/usr/bin/env python3
"""T10 isolated failure runner: inject faults at exact money-path windows.

What it is
----------
An isolated, single-machine runner for the failure/correctness half of T10
(docs/plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md §5, F01..F19). It starts a
private stack, seeds one organization per scenario, injects a fault at a named
window with the T10 failpoints (internal/testsupport/failpoint), and classifies
the outcome from PostgreSQL, Redis and the process logs.

Isolation (no interference with the daily demo stack)
-----------------------------------------------------
* PostgreSQL and Redis live in a private Compose project
  ``llm-gw-t10-fail-<runid>`` started from ``docker-compose.test.yml`` plus a
  generated override that publishes probed free host ports with ``!override``.
  ``docker-compose.test.yml`` uses tmpfs for both data directories, so there is
  no named volume to collide with, and ``down -v`` can only ever touch this
  project's containers. The teardown asserts the project name prefix before it
  runs anything destructive.
* Control, Gateway(s), Metering and the mock upstreams are host processes with
  probed free ports, run-scoped instance ids and run-scoped spool directories.
* Every scenario owns its own organization, key and wallet, so nothing has to be
  deleted or reset between scenarios and no append-only receipt/ledger row is
  ever mutated.
* The API key is generated in-process and passed to the Gateway only through the
  client request header; it never appears in argv, in a process list, or in the
  evidence.

Failpoints
----------
The binaries are built with ``-tags failpoint``. Every failpoint window is
documented in internal/testsupport/failpoint/doc.go; a process armed with a
``hold`` window writes ``FAILPOINT {"event":"hit",...}`` to stderr and blocks,
and this runner waits for that exact marker before it sends SIGKILL. That is
what makes "kill -9 at the receipt-insert window" deterministic instead of a
race against the wall clock.

Usage
-----
    python3 scripts/failure-matrix.py --list
    python3 scripts/failure-matrix.py --scenarios F01,F07
    python3 scripts/failure-matrix.py            # every implemented scenario
    python3 scripts/failure-matrix.py --output docs/verification/2026-10-01/t10-failures
"""

import argparse
import hashlib
import http.client
import http.server
import json
import os
import pathlib
import platform
import random
import re
import shutil
import signal
import socket
import string
import subprocess
import sys
import tempfile
import threading
import time
import traceback
import urllib.error
import urllib.request
from datetime import datetime, timezone

ROOT = pathlib.Path(__file__).resolve().parent.parent
PROJECT_PREFIX = "llm-gw-t10-fail-"
COMPOSE_FILE = ROOT / "docker-compose.test.yml"

# The gateway model used by most scenarios. It is priced, routed and served by
# the normal mock upstream.
MODEL_OK = "t10-ok"
MODEL_FAIL503 = "t10-fail503"
MODEL_FAIL429 = "t10-fail429"
MODEL_HANG = "t10-hang"
MODEL_CUT = "t10-cut"
MODEL_COUNT = "t10-count"
ALL_MODELS = (MODEL_OK, MODEL_FAIL503, MODEL_FAIL429, MODEL_HANG, MODEL_CUT, MODEL_COUNT)

# 1 micro per token in both directions, so a receipt's charge_micro equals its
# token count and every assertion in this file can be checked by eye.
PRICE_MICRO_PER_MILLION = 1_000_000

ADMIN_TOKEN = "t10-failure-matrix-admin-token"

# Redis names from internal/metering (receipts.go / consumer.go).
RECEIPT_STREAM = "usage:events:receipts"
SETTLEMENT_GROUP = "settlement-group"

FAILPOINT_RE = re.compile(r"FAILPOINT (\{.*\})")


def log(message):
    print(f"[t10-fail] {message}", flush=True)


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
        raise RuntimeError(f"{what} failed ({' '.join(map(str, args))}): {(proc.stderr or '').strip()}")
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


def http_status(url, timeout=10, headers=None):
    try:
        request = urllib.request.Request(url, headers=dict(headers or {}))
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status
    except urllib.error.HTTPError as err:
        return err.code
    except OSError:
        return 0


def wait_until(predicate, timeout, what, interval=0.2):
    """Poll predicate() until it returns a truthy value; return it or raise."""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            last = predicate()
            if last:
                return last
        except Exception as err:  # noqa: BLE001 - readiness polling
            last = err
        time.sleep(interval)
    raise RuntimeError(f"timed out waiting for {what}: last={last!r}")


# ---------------------------------------------------------------------------
# processes
# ---------------------------------------------------------------------------


class Process:
    """A host process whose stdout+stderr are captured into one log file."""

    def __init__(self, name, args, env, log_path, cwd=ROOT):
        self.name = name
        self.args = list(args)
        self.env = dict(env)
        self.log_path = pathlib.Path(log_path)
        self.log_path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = open(self.log_path, "w", encoding="utf-8")
        self.handle.write(f"$ {' '.join(self.args)}\n# cwd: {cwd}\n# env: {json.dumps(safe_env(self.env), sort_keys=True)}\n\n")
        self.handle.flush()
        self.proc = subprocess.Popen(self.args, cwd=cwd, env=self.env, stdout=self.handle,
                                     stderr=subprocess.STDOUT, text=True, start_new_session=False)
        self.killed_at = None

    @property
    def pid(self):
        return self.proc.pid

    def alive(self):
        return self.proc.poll() is None

    def kill9(self, reason=""):
        """SIGKILL this process — the failure the whole runner is built around."""
        if self.proc.poll() is None:
            os.kill(self.proc.pid, signal.SIGKILL)
            self.proc.wait(timeout=15)
        self.killed_at = now_utc()
        self.kill_reason = reason
        self.handle.flush()
        return self.killed_at

    def stop(self, timeout=10):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=timeout)
        self.close()

    def close(self):
        try:
            self.handle.close()
        except Exception:  # noqa: BLE001
            pass

    def text(self):
        try:
            return self.log_path.read_text(encoding="utf-8", errors="replace")
        except OSError as err:
            return f"<log unavailable: {err}>"

    def exit_code(self):
        return self.proc.poll()

    def failpoints(self):
        """Every FAILPOINT marker this process emitted, in order."""
        out = []
        for line in self.text().splitlines():
            match = FAILPOINT_RE.search(line)
            if not match:
                continue
            try:
                out.append(json.loads(match.group(1)))
            except json.JSONDecodeError:
                continue
        return out

    def wait_failpoint(self, name, timeout=60):
        """Block until this process logs the hit marker for `name`."""
        def check():
            for event in self.failpoints():
                if event.get("name") == name and event.get("event") == "hit":
                    return event
            if not self.alive():
                return {"event": "process-died", "name": name,
                        "exit_code": self.exit_code()}
            return None
        return wait_until(check, timeout, f"failpoint {name} in {self.name}")


SECRET_ENV_KEYS = ("POSTGRES_DSN", "GATEWAY_FAILPOINTS")


def safe_env(env):
    """Env as evidence: values kept, DSN password redacted."""
    out = {}
    for key, value in env.items():
        if key in ("POSTGRES_DSN",):
            out[key] = re.sub(r"://([^:]+):[^@]+@", r"://\1:***@", str(value))
        else:
            out[key] = value
    return out


# ---------------------------------------------------------------------------
# SSE client
# ---------------------------------------------------------------------------


class SSEResult:
    def __init__(self):
        self.status = 0
        self.headers = {}
        self.body = ""
        self.error = None
        self.started_at = time.time()
        self.first_event_seconds = None
        self.first_content_seconds = None
        self.done_seconds = None
        self.closed_seconds = None
        self.events = []
        self.cancelled_by_client = False
        self.classification = "unknown"

    def as_dict(self):
        return {
            "status": self.status,
            "bytes": len(self.body),
            "events": len(self.events),
            "has_done": "[DONE]" in self.body,
            "first_event_seconds": round(self.first_event_seconds, 6) if self.first_event_seconds else None,
            "first_content_seconds": round(self.first_content_seconds, 6) if self.first_content_seconds else None,
            "done_seconds": round(self.done_seconds, 6) if self.done_seconds else None,
            "closed_seconds": round(self.closed_seconds, 6) if self.closed_seconds else None,
            "cancelled_by_client": self.cancelled_by_client,
            "classification": self.classification,
            "error": self.error,
            "body_head": self.body[:200],
            "body_tail": self.body[-200:],
        }


class SSEStream:
    """A raw HTTP/1.1 streaming client with explicit read modes.

    urllib buffers and hides the socket, which makes "cancel the client"
    approximate. http.client exposes the connection, so a cancel can be a real
    connection teardown (the Gateway then sees a write error on its next flush,
    exactly like a browser tab closing), and a slow reader can be paced.
    """

    def __init__(self, host, port, path, key, payload, timeout=60):
        self.host = host
        self.port = port
        self.path = path
        self.key = key
        self.payload = payload
        self.timeout = timeout
        self.conn = None
        self.response = None
        self.result = SSEResult()
        self._buffer = b""

    def open(self):
        body = json.dumps(self.payload).encode()
        self.conn = http.client.HTTPConnection(self.host, self.port, timeout=self.timeout)
        self.conn.connect()
        self.conn.sock.settimeout(self.timeout)
        self.conn.request("POST", self.path, body=body, headers={
            "Authorization": "Bearer " + self.key,
            "Content-Type": "application/json",
            "Content-Length": str(len(body)),
            "Accept": "text/event-stream",
        })
        self.response = self.conn.getresponse()
        self.result.status = self.response.status
        self.result.headers = dict(self.response.getheaders())
        return self

    # -- read modes ---------------------------------------------------------

    def read_events(self, stop_at_done=True, max_bytes=None, deadline=None, chunk_size=1):
        """Read and classify SSE frames until [DONE], EOF or a bound.

        chunk_size defaults to 1 so TTFT and the first-content instant are
        measured on the byte that carried them; the oversized-event probes pass
        a large chunk because a megabyte one byte at a time is a client-side
        bottleneck, not a gateway measurement.
        """
        start = self.result.started_at
        while True:
            if deadline is not None and time.time() > deadline:
                self.result.error = "read deadline reached"
                break
            try:
                chunk = self.response.read(chunk_size)
            except Exception as err:  # noqa: BLE001 - a broken read is data
                self.result.error = f"{type(err).__name__}: {err}"
                break
            if not chunk:
                break
            if self.result.first_event_seconds is None:
                self.result.first_event_seconds = time.time() - start
            self._buffer += chunk
            if max_bytes is not None and len(self._buffer) >= max_bytes:
                break
            while b"\n\n" in self._buffer:
                raw, self._buffer = self._buffer.split(b"\n\n", 1)
                self._consume_event(raw, start)
            if stop_at_done and "[DONE]" in self.result.body:
                break
        self._flush_buffer()
        self.result.closed_seconds = time.time() - start
        self._classify()
        return self.result

    def _flush_buffer(self):
        """Keep a non-SSE body (a JSON error envelope) reachable.

        _consume_event only fires on a complete `\n\n`-terminated frame, so an
        error response would otherwise be invisible in the evidence.
        """
        if self._buffer:
            self.result.body += self._buffer.decode("utf-8", errors="replace")
            self._buffer = b""

    def _consume_event(self, raw, start):
        text = raw.decode("utf-8", errors="replace")
        self.result.body += text + "\n\n"
        self.result.events.append(text)
        data_lines = [line[5:].strip() for line in text.splitlines() if line.startswith("data:")]
        if not data_lines:
            return
        data = "\n".join(data_lines)
        if data == "[DONE]":
            if self.result.done_seconds is None:
                self.result.done_seconds = time.time() - start
            return
        try:
            payload = json.loads(data)
        except json.JSONDecodeError:
            return
        for choice in payload.get("choices") or []:
            delta = choice.get("delta") or {}
            if delta.get("content"):
                if self.result.first_content_seconds is None:
                    self.result.first_content_seconds = time.time() - start
                break

    def read_slow(self, bytes_per_second, budget_seconds, stop_at_done=False):
        """Read at a fixed rate: the slow-client half of §5/F03 and §5/B/C."""
        start = self.result.started_at
        deadline = start + budget_seconds
        interval = 0.02
        per_interval = max(1, int(bytes_per_second * interval))
        while time.time() < deadline:
            try:
                chunk = self.response.read(per_interval)
            except Exception as err:  # noqa: BLE001
                self.result.error = f"{type(err).__name__}: {err}"
                break
            if not chunk:
                break
            if self.result.first_event_seconds is None:
                self.result.first_event_seconds = time.time() - start
            self._buffer += chunk
            while b"\n\n" in self._buffer:
                raw, self._buffer = self._buffer.split(b"\n\n", 1)
                self._consume_event(raw, start)
            if stop_at_done and "[DONE]" in self.result.body:
                break
            time.sleep(interval)
        self._flush_buffer()
        self.result.closed_seconds = time.time() - start
        self._classify()
        return self.result

    def read_then_stall(self, prime_seconds=0.5, stall_seconds=20):
        """Read for a moment, then stop reading without closing (a stalled
        client). The socket stays open, so the Gateway's write blocks."""
        start = self.result.started_at
        deadline = start + prime_seconds
        while time.time() < deadline:
            try:
                chunk = self.response.read(1)
            except Exception as err:  # noqa: BLE001
                self.result.error = f"{type(err).__name__}: {err}"
                break
            if not chunk:
                break
            if self.result.first_event_seconds is None:
                self.result.first_event_seconds = time.time() - start
            self._buffer += chunk
            while b"\n\n" in self._buffer:
                raw, self._buffer = self._buffer.split(b"\n\n", 1)
                self._consume_event(raw, start)
        self.stalled_for = stall_seconds
        return self

    def release_stall(self, seconds):
        """After a stall, either resume reading or abort the connection."""
        time.sleep(seconds)
        self.result.closed_seconds = time.time() - self.result.started_at
        self._classify()
        return self.result

    def cancel(self):
        """Abrupt client cancel: SO_LINGER 0 + close sends RST, so the Gateway
        learns about it on its next write instead of at EOF."""
        self.result.cancelled_by_client = True
        try:
            if self.conn is not None and self.conn.sock is not None:
                self.conn.sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER,
                                          b"\x01\x00\x00\x00\x00\x00\x00\x00")
                self.conn.sock.close()
        except Exception:  # noqa: BLE001
            pass
        try:
            if self.conn is not None:
                self.conn.close()
        except Exception:  # noqa: BLE001
            pass
        self.result.closed_seconds = time.time() - self.result.started_at
        self._classify()
        return self.result

    def close(self):
        try:
            if self.conn is not None:
                self.conn.close()
        except Exception:  # noqa: BLE001
            pass

    def _classify(self):
        r = self.result
        if r.cancelled_by_client:
            r.classification = "cancelled"
        elif r.status and r.status >= 400:
            r.classification = "refused"
        elif "[DONE]" in r.body:
            r.classification = "complete"
        elif r.error:
            r.classification = "transport_error"
        elif r.status == 200:
            r.classification = "incomplete"
        else:
            r.classification = "unknown"


def sse_request(host, port, key, payload, timeout=60, stop_at_done=True):
    """One complete (or truncated) streaming request, read to the end."""
    stream = SSEStream(host, port, "/v1/chat/completions", key, payload, timeout=timeout).open()
    try:
        return stream.read_events(stop_at_done=stop_at_done)
    finally:
        stream.close()


class BackgroundRequest:
    """A streaming request running on its own thread.

    Required for the admission windows: the Gateway does not write a status line
    until admission finishes, so a synchronous open() would block for the whole
    failpoint hold instead of letting the runner kill the process at the window.
    """

    def __init__(self, host, port, key, payload, timeout=60):
        self.host = host
        self.port = port
        self.key = key
        self.payload = payload
        self.timeout = timeout
        self.stream = None
        self.result = None
        self.error = None
        self.opened = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _run(self):
        try:
            self.stream = SSEStream(self.host, self.port, "/v1/chat/completions",
                                    self.key, self.payload, timeout=self.timeout).open()
            self.opened.set()
            self.result = self.stream.read_events(deadline=time.time() + self.timeout)
        except Exception as err:  # noqa: BLE001 - a killed connection is the point
            self.error = f"{type(err).__name__}: {err}"
        finally:
            self.opened.set()

    def start(self):
        self.thread.start()
        return self

    def join(self, timeout=60):
        self.thread.join(timeout)
        return self.as_dict()

    def as_dict(self):
        return {
            "opened": self.opened.is_set(),
            "error": self.error,
            "stream": self.result.as_dict() if self.result is not None else None,
        }


def chat_payload(model=MODEL_OK, content="hi", max_tokens=32, **extra):
    payload = {
        "model": model,
        "stream": True,
        "messages": [{"role": "user", "content": content}],
        "max_tokens": max_tokens,
    }
    payload.update(extra)
    return payload


# ---------------------------------------------------------------------------
# a counting upstream: hard evidence for "the upstream was never called"
# ---------------------------------------------------------------------------


class CountingUpstream:
    """A minimal OpenAI-compatible SSE upstream that counts what it received.

    The shipped `cmd/upstream-mock` deliberately exposes no counters, so a
    scenario that must prove "no upstream call happened" (F09 admission, F11 last
    balance) needs an upstream that can say so. This one also records aborted
    streams, which is F03's "the upstream stopped" evidence.
    """

    def __init__(self, name, ttft=0.005, token_interval=0.01, tokens=4, mode="normal"):
        self.name = name
        self.ttft = ttft
        self.token_interval = token_interval
        self.tokens = tokens
        self.mode = mode  # normal | cut | hang
        self.port = probe_port()
        self.lock = threading.Lock()
        self.requests = 0
        self.completed = 0
        self.aborted = 0
        self.started_streams = 0
        self.active = 0
        self._server = None
        self._thread = None

    def snapshot(self):
        with self.lock:
            return {"requests": self.requests, "started_streams": self.started_streams,
                    "completed": self.completed, "aborted": self.aborted, "active": self.active}

    def start(self):
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):  # silence the default stderr spam
                pass

            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b"{}"
                try:
                    body = json.loads(raw.decode() or "{}")
                except json.JSONDecodeError:
                    body = {}
                tokens = outer.tokens
                interval = outer.token_interval
                if isinstance(body.get("x_mock_tokens"), int) and body["x_mock_tokens"] >= 0:
                    tokens = min(body["x_mock_tokens"], 10_000)
                if isinstance(body.get("x_mock_token_interval_ms"), int) and body["x_mock_token_interval_ms"] >= 0:
                    interval = min(body["x_mock_token_interval_ms"], 600_000) / 1000.0
                with outer.lock:
                    outer.requests += 1
                    outer.started_streams += 1
                    outer.active += 1
                try:
                    self._stream(tokens, interval)
                finally:
                    with outer.lock:
                        outer.active -= 1

            def _stream(self, tokens, interval):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                try:
                    self.wfile.flush()
                except OSError:
                    self._abort()
                    return
                if outer.mode == "hang":
                    time.sleep(3600)
                    return
                time.sleep(outer.ttft)
                limit = tokens if outer.mode != "cut" else max(1, tokens // 2)
                for i in range(limit):
                    event = json.dumps({
                        "id": f"chatcmpl-count-{outer.name}",
                        "object": "chat.completion.chunk",
                        "choices": [{"index": 0, "delta": {"content": f"{outer.name}-{i} "},
                                     "finish_reason": None}],
                    })
                    if not self._write(f"data: {event}\n\n"):
                        return
                    time.sleep(interval)
                if outer.mode == "cut":
                    self._done()
                    return
                usage = json.dumps({
                    "id": f"chatcmpl-count-{outer.name}",
                    "object": "chat.completion.chunk",
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                    "usage": {"prompt_tokens": 12, "completion_tokens": limit, "total_tokens": 12 + limit},
                })
                if not self._write(f"data: {usage}\n\n"):
                    return
                if not self._write("data: [DONE]\n\n"):
                    return
                with outer.lock:
                    outer.completed += 1

            def _write(self, text):
                try:
                    payload = text.encode()
                    self.wfile.write(f"{len(payload):X}\r\n".encode() + payload + b"\r\n")
                    self.wfile.flush()
                    return True
                except OSError:
                    self._abort()
                    return False

            def _done(self):
                try:
                    self.wfile.write(b"0\r\n\r\n")
                    self.wfile.flush()
                    with outer.lock:
                        outer.completed += 1
                except OSError:
                    self._abort()

            def _abort(self):
                with outer.lock:
                    outer.aborted += 1

        class QuietServer(http.server.ThreadingHTTPServer):
            # A client cancelling mid-write is a scenario input, not an error:
            # keep socketserver from printing a traceback for it.
            def handle_error(self, request, client_address):
                pass

        self._server = QuietServer(("127.0.0.1", self.port), Handler)
        self._server.daemon_threads = True
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        wait_tcp(self.port, 10, f"counting upstream {self.name}")
        return self

    def stop(self):
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()


# ---------------------------------------------------------------------------
# the stack
# ---------------------------------------------------------------------------


class ScenarioOrg:
    def __init__(self, scenario, user_id, org_id, key_id, raw_key):
        self.scenario = scenario
        self.user_id = user_id
        self.org_id = org_id
        self.key_id = key_id
        self.raw_key = raw_key


class Stack:
    """The private, run-scoped stack the scenarios share."""

    def __init__(self, args, output):
        self.args = args
        self.output = pathlib.Path(output).resolve()
        self.output.mkdir(parents=True, exist_ok=True)
        run_suffix = "".join(random.choices(string.ascii_lowercase + string.digits, k=8))
        self.project = PROJECT_PREFIX + run_suffix
        self.run_id = run_suffix
        self.ports = {name: probe_port() for name in (
            "pg", "redis", "control_grpc", "control_http", "gateway_a", "gateway_b",
            "diag_a", "diag_b", "metering", "metering_diag", "mock_ok", "mock_503",
            "mock_429", "mock_hang", "mock_cut")}
        self.processes = {}
        self.upstreams = []
        self.orgs = {}
        self.desired_version = None
        self.build_dir = None
        self.override_path = None
        self.evidence = {
            "run_id": self.run_id,
            "started_at": now_utc(),
            "project": self.project,
            "ports": self.ports,
            "host": {
                "platform": platform.platform(),
                "python": sys.version.split()[0],
                "cpus": os.cpu_count(),
            },
        }

    # -- infrastructure -----------------------------------------------------

    def docker(self, args, what):
        return must_run(["docker"] + args, what)

    def compose(self, *args, what="compose"):
        cmd = ["docker", "compose", "-f", str(COMPOSE_FILE)]
        if self.override_path is not None:
            cmd += ["-f", str(self.override_path)]
        cmd += ["-p", self.project] + list(args)
        return must_run(cmd, what)

    def write_override(self):
        path = self.output / "compose-override.yml"
        path.write_text(
            "# Generated by scripts/failure-matrix.py -- do not edit.\n"
            f"# run_id: {self.run_id}\n"
            "#\n"
            "# Host-port isolation: both ports are free ports probed at run time, and\n"
            "# `!override` replaces the base `ports` list (Compose >= 2.24.4).\n"
            "#\n"
            "# Storage isolation: the base docker-compose.test.yml keeps both data\n"
            "# directories on tmpfs, whose contents are DESTROYED by `docker compose\n"
            "# stop` + `start` (Docker recreates the mount), and a tmpfs mount at the\n"
            "# same path also shadows a volume. The outage scenarios stop and restart\n"
            "# the dependency containers, so this override resets the tmpfs and gives\n"
            "# each service a project-scoped named volume instead: `-p <project>` turns\n"
            "# `pgdata` into `<project>_pgdata`, which no other stack can name, and\n"
            "# `docker compose -p <project> down -v` is the only thing that deletes it.\n"
            "services:\n"
            "  postgres:\n"
            "    tmpfs: !reset null\n"
            "    ports: !override\n"
            f'      - "127.0.0.1:{self.ports["pg"]}:5432"\n'
            "    volumes: !override\n"
            "      - pgdata:/var/lib/postgresql/data\n"
            "  redis:\n"
            "    tmpfs: !reset null\n"
            "    ports: !override\n"
            f'      - "127.0.0.1:{self.ports["redis"]}:6379"\n'
            "    volumes: !override\n"
            "      - redisdata:/data\n"
            "volumes:\n"
            "  pgdata:\n"
            "  redisdata:\n",
            encoding="utf-8",
        )
        self.override_path = path

    def start_dependencies(self):
        """Bring up the private dependencies, retrying on a port race.

        A probed free port can be taken by another process between the probe and
        the bind; that is a property of the host, not of this run, so the runner
        re-probes and retries instead of failing the whole matrix.
        """
        last = None
        for attempt in range(3):
            self.write_override()
            try:
                self.compose("up", "-d", "--wait", "postgres", "redis", what="start postgres+redis")
                break
            except RuntimeError as err:
                last = err
                if "address already in use" not in str(err):
                    raise
                log(f"dependency port collision on attempt {attempt + 1}; re-probing ports")
                run(["docker", "compose", "-f", str(COMPOSE_FILE), "-f", str(self.override_path),
                     "-p", self.project, "down", "-v", "--remove-orphans"])
                for name in ("pg", "redis"):
                    self.ports[name] = probe_port()
        else:
            raise last
        wait_tcp(self.ports["pg"], 60, "postgres")
        wait_tcp(self.ports["redis"], 30, "redis")
        self.wait_postgres_ready()
        self.wait_redis_ready()

    def wait_postgres_ready(self, timeout=90):
        """TCP is not readiness: Docker's port forwarder accepts a connection
        before the postmaster speaks the protocol, so a client that only waits
        for the socket sees `unexpected EOF`. Probe with a real statement."""
        container = self.pg_container()

        def ready():
            proc = run(["docker", "exec", container, "psql", "-U", "gateway_test",
                        "-d", "gateway_test", "-At", "-c", "SELECT 1"])
            return proc.returncode == 0 and proc.stdout.strip() == "1"
        return wait_until(ready, timeout, "postgres to accept statements", interval=0.3)

    def wait_redis_ready(self, timeout=60):
        def ready():
            proc = run(["docker", "exec", f"{self.project}-redis-1", "redis-cli", "PING"])
            return proc.returncode == 0 and "PONG" in proc.stdout
        return wait_until(ready, timeout, "redis to answer PING", interval=0.3)

    def ensure_dependencies(self):
        """Make sure postgres and redis are up and actually serving."""
        for service in ("postgres", "redis"):
            self.compose("start", service, what=f"ensure {service}")
        wait_tcp(self.ports["pg"], 60, "postgres")
        wait_tcp(self.ports["redis"], 30, "redis")
        self.wait_postgres_ready()
        self.wait_redis_ready()

    def stop_dependency(self, name):
        """Stop one dependency container: the Redis/PG outage injection."""
        self.compose("stop", name, what=f"stop {name}")

    def start_dependency(self, name):
        self.compose("start", name, what=f"start {name}")
        wait_tcp(self.ports["pg" if name == "postgres" else "redis"], 60, name)
        if name == "postgres":
            self.wait_postgres_ready()
        else:
            self.wait_redis_ready()

    def restart_dependency(self, name):
        self.stop_dependency(name)
        self.start_dependency(name)

    def teardown(self):
        for process in list(self.processes.values()):
            try:
                process.stop()
            except Exception:  # noqa: BLE001
                pass
        for upstream in self.upstreams:
            try:
                upstream.stop()
            except Exception:  # noqa: BLE001
                pass
        if not self.project.startswith(PROJECT_PREFIX):
            raise RuntimeError(f"refusing to tear down a project this runner does not own: {self.project!r}")
        # The override must be part of the teardown config too: `down -v` only
        # removes the volumes the CURRENT config declares, and the named volumes
        # this run created are declared there, not in the base file.
        proc = run(["docker", "compose", "-f", str(COMPOSE_FILE), "-f", str(self.override_path),
                    "-p", self.project, "down", "-v", "--remove-orphans"])
        # Belt and braces: the project name is the only handle on this run's
        # state, and nothing outside the prefix may ever be touched.
        volumes = [v for v in run(["docker", "volume", "ls", "-q"]).stdout.split()
                   if v.startswith(self.project + "_")]
        for volume in volumes:
            if not volume.startswith(PROJECT_PREFIX):
                raise RuntimeError(f"refusing to remove volume outside this runner's prefix: {volume!r}")
            run(["docker", "volume", "rm", volume])
        leftovers = run(["docker", "ps", "-a", "--filter", f"label=com.docker.compose.project={self.project}",
                         "--format", "{{.Names}}"]).stdout.strip()
        return {"down_stdout": proc.stdout, "down_stderr": proc.stderr,
                "leftover_containers": leftovers.split() if leftovers else [],
                "removed_volumes": volumes}

    # -- database -----------------------------------------------------------

    def psql(self, sql, database="gateway_test", user="gateway_test"):
        proc = must_run([
            "docker", "exec", "-i", f"{self.project}-postgres-1",
            "psql", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", database, "-At", "-c", sql,
        ], "psql")
        return proc.stdout.strip()

    def psql_try(self, sql):
        """psql that returns (ok, output_or_error) instead of raising: several
        F13/F19 assertions are about a statement being REFUSED."""
        proc = run([
            "docker", "exec", "-i", f"{self.project}-postgres-1",
            "psql", "-v", "ON_ERROR_STOP=1", "-U", "gateway_test", "-d", "gateway_test", "-At", "-c", sql,
        ])
        return proc.returncode == 0, (proc.stdout.strip() if proc.returncode == 0 else (proc.stderr or "").strip())

    def pg_container(self):
        """The deterministic Compose name first; the label is only a fallback.

        Called during startup, where `docker ps` (without -a) can briefly return
        nothing before the container is created.
        """
        direct = f"{self.project}-postgres-1"
        if run(["docker", "inspect", "-f", "{{.State.Status}}", direct]).returncode == 0:
            return direct
        names = run(["docker", "ps", "-a", "--filter", f"label=com.docker.compose.project={self.project}",
                     "--format", "{{.Names}} {{.Service}}"]).stdout.strip().splitlines()
        for line in names:
            parts = line.split()
            if len(parts) == 2 and parts[1] == "postgres":
                return parts[0]
        raise RuntimeError(f"no postgres container in project {self.project}: {names}")

    def psql_value(self, sql):
        output = self.psql(sql)
        lines = [line for line in output.splitlines() if line.strip()]
        return lines[0] if lines else ""

    def psql_rows(self, sql):
        return [line.split("|") for line in self.psql(sql).splitlines() if line.strip()]

    @property
    def dsn(self):
        return (f"postgres://gateway_test:test_only_local@127.0.0.1:{self.ports['pg']}"
                "/gateway_test?sslmode=disable")

    def redis_cli(self, *args):
        return must_run(["docker", "exec", f"{self.project}-redis-1", "redis-cli", *args],
                        "redis-cli").stdout.strip()

    # -- build --------------------------------------------------------------

    def build(self):
        self.build_dir = pathlib.Path(tempfile.mkdtemp(prefix=self.project + "-build-"))
        for name in ("control", "gateway", "upstream-mock", "metering", "migrate"):
            must_run(["go", "build", "-tags", "failpoint", "-o", str(self.build_dir / name), f"./cmd/{name}"],
                     f"build {name}", cwd=ROOT, timeout=900)
        return self.build_dir

    def base_env(self):
        env = dict(os.environ)
        env.update({
            "GATEWAY_PROFILE": "demo",
            "POSTGRES_DSN": self.dsn,
            "REDIS_ADDR": f"127.0.0.1:{self.ports['redis']}",
        })
        return env

    def start_process(self, name, args, env_overrides=None, replace=True):
        if replace and name in self.processes:
            self.processes[name].stop()
        env = self.base_env()
        if env_overrides:
            env.update(env_overrides)
        path = self.output / "logs" / f"{name}.log"
        process = Process(name, args, env, path)
        self.processes[name] = process
        return process

    def process(self, name):
        return self.processes.get(name)

    # -- seeding ------------------------------------------------------------

    def apply_migrations(self):
        proc = run([str(self.build_dir / "migrate"), "up"], env=self.base_env(), cwd=ROOT, timeout=300)
        (self.output / "raw").mkdir(parents=True, exist_ok=True)
        (self.output / "raw" / "migrate.log").write_text(proc.stdout + proc.stderr, encoding="utf-8")
        if proc.returncode != 0:
            raise RuntimeError(f"migrate up failed: {proc.stderr}")
        version = self.psql_value("SELECT version FROM schema_migrations")
        self.evidence["schema_version"] = int(version)
        return version

    def seed_org(self, scenario, balance_micro=50_000_000):
        key_id = f"key_{scenario.lower()}_" + "".join(random.choices(string.hexdigits.lower(), k=20))
        raw_key = f"sk-t10-{scenario.lower()}-" + "".join(random.choices(string.ascii_lowercase + string.digits, k=24))
        email = f"{scenario.lower()}@t10.test"
        user_id = int(self.psql_value(f"""
        INSERT INTO users (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
        VALUES ('{email}', 'hash', 'active', false, 1, now(), now()) RETURNING id;
        """))
        org_id = int(self.psql_value(f"""
        INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
        VALUES ('enterprise', 't10-{scenario.lower()}', 'active', {user_id}, now(), now()) RETURNING id;
        """))
        self.psql(f"""
        INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
        VALUES ({org_id}, {user_id}, 'owner', 'active', now(), now());
        """)
        key_hash = hashlib.sha256(raw_key.encode()).hexdigest()
        self.psql(f"""
        INSERT INTO api_keys (key_id, key_hash, key_prefix, org_id, owner_user_id, created_by,
                              status, policy_version, created_at)
        VALUES ('{key_id}', '{key_hash}', '{raw_key[:10]}', {org_id}, {user_id}, {user_id}, 'active', 1, now());
        """)
        self.set_balance(balance_micro, org_id)
        org = ScenarioOrg(scenario, user_id, org_id, key_id, raw_key)
        self.orgs[scenario] = org
        return org

    def seed_prices(self):
        for model in ALL_MODELS:
            self.psql(f"""
            INSERT INTO price_versions (model, currency, input_price_micro_per_million,
                                        output_price_micro_per_million, effective_at, created_at)
            VALUES ('{model}', 'CNY', {PRICE_MICRO_PER_MILLION}, {PRICE_MICRO_PER_MILLION},
                    now() - interval '1 hour', now());
            """)

    def set_balance(self, micro, org_id):
        self.psql(f"""
        INSERT INTO billing_accounts (org_id, posted_balance_micro, reserved_micro, status, version,
                                      created_at, updated_at)
        VALUES ({org_id}, {micro}, 0, 'active', 1, now(), now())
        ON CONFLICT (org_id) DO UPDATE SET posted_balance_micro = {micro}, reserved_micro = 0,
            version = billing_accounts.version + 1, updated_at = now();
        """)

    def key_records(self):
        return [{
            "key_id": org.key_id,
            "key_hash": hashlib.sha256(org.raw_key.encode()).hexdigest(),
            "key_prefix": org.raw_key[:10],
            "tenant_id": f"org:{org.org_id}",
            "status": "active",
            "org_id": org.org_id,
            "owner_user_id": org.user_id,
        } for org in self.orgs.values()]

    # -- services -----------------------------------------------------------

    def start_control(self):
        self.start_process("control", [str(self.build_dir / "control")], {
            "CONTROL_GRPC_ADDR": f"127.0.0.1:{self.ports['control_grpc']}",
            "CONTROL_HTTP_ADDR": f"127.0.0.1:{self.ports['control_http']}",
            "CONTROL_REQUIRE_POSTGRES": "true",
            "CONTROL_ADMIN_TOKEN": ADMIN_TOKEN,
        })
        wait_tcp(self.ports["control_grpc"], 30, "control grpc")
        wait_until(lambda: http_status(self.control_http() + "/healthz", timeout=5) == 200, 30, "control http")

    def control_http(self):
        return f"http://127.0.0.1:{self.ports['control_http']}"

    def post_config(self, keys, routes, providers):
        return http_json(self.control_http() + "/config", method="POST", payload={
            "key_records": keys, "routes": routes, "providers": providers,
        }, timeout=30, headers={"Authorization": "Bearer " + ADMIN_TOKEN})

    def start_mocks(self):
        self.upstreams = [CountingUpstream("count", tokens=6, token_interval=0.01).start()]
        self.counting = self.upstreams[0]
        specs = {
            "mock_ok": (self.ports["mock_ok"], ["-name", "ok", "-ttft", "5ms", "-token-interval", "5ms", "-tokens", "6"]),
            "mock_503": (self.ports["mock_503"], ["-name", "fail503", "-status", "503"]),
            "mock_429": (self.ports["mock_429"], ["-name", "fail429", "-status", "429"]),
            "mock_hang": (self.ports["mock_hang"], ["-name", "hang", "-hang"]),
            "mock_cut": (self.ports["mock_cut"], ["-name", "cut", "-ttft", "5ms", "-token-interval", "5ms",
                                                  "-tokens", "6", "-fail-after", "2"]),
        }
        for name, (port, flags) in specs.items():
            self.start_process(name, [str(self.build_dir / "upstream-mock"), "-listen", f"127.0.0.1:{port}"] + flags)
        for name, (port, _) in specs.items():
            wait_tcp(port, 15, name)

    def publish_config(self):
        routes = {
            MODEL_OK: ["p_ok"],
            MODEL_FAIL503: ["p_503", "p_ok"],
            MODEL_FAIL429: ["p_429", "p_ok"],
            MODEL_HANG: ["p_hang", "p_ok"],
            MODEL_CUT: ["p_cut"],
            MODEL_COUNT: ["p_count"],
        }
        providers = {
            "p_ok": {"id": "p_ok", "protocol": "openai-compatible",
                     "url": f"http://127.0.0.1:{self.ports['mock_ok']}", "model": MODEL_OK,
                     "timeouts": {"attempt_timeout_ms": 30000}},
            "p_503": {"id": "p_503", "protocol": "openai-compatible",
                      "url": f"http://127.0.0.1:{self.ports['mock_503']}", "model": MODEL_FAIL503,
                      "timeouts": {"attempt_timeout_ms": 5000}},
            "p_429": {"id": "p_429", "protocol": "openai-compatible",
                      "url": f"http://127.0.0.1:{self.ports['mock_429']}", "model": MODEL_FAIL429,
                      "timeouts": {"attempt_timeout_ms": 5000}},
            "p_hang": {"id": "p_hang", "protocol": "openai-compatible",
                       "url": f"http://127.0.0.1:{self.ports['mock_hang']}", "model": MODEL_HANG,
                       "timeouts": {"attempt_timeout_ms": 1500}},
            "p_cut": {"id": "p_cut", "protocol": "openai-compatible",
                      "url": f"http://127.0.0.1:{self.ports['mock_cut']}", "model": MODEL_CUT,
                      "timeouts": {"attempt_timeout_ms": 30000}},
            "p_count": {"id": "p_count", "protocol": "openai-compatible",
                        "url": f"http://127.0.0.1:{self.counting.port}", "model": MODEL_COUNT,
                        "timeouts": {"attempt_timeout_ms": 30000}},
        }
        body = self.post_config(self.key_records(), routes, providers)
        self.desired_version = body.get("desired_version")
        return body

    def gateway_env(self, which="a", extra=None):
        env = {
            "GATEWAY_ADDR": f"127.0.0.1:{self.ports['gateway_' + which]}",
            "GATEWAY_DIAGNOSTICS_ADDR": f"127.0.0.1:{self.ports['diag_' + which]}",
            "GATEWAY_INSTANCE_ID": f"gw-{which}",
            "CONTROL_ADDR": f"127.0.0.1:{self.ports['control_grpc']}",
            "GATEWAY_ACCESS_GUARD": "pg",
            "GATEWAY_BILLING": "pg",
            "LIMITER_ON_REDIS_ERROR": "closed",
            "GATEWAY_LIMITER_RATE": "100000",
            "GATEWAY_LIMITER_BURST": "100000",
            "GATEWAY_MAX_ACTIVE_STREAMS": "64",
            "GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG": "32",
            "CONTROL_CONFIG_POLL_INTERVAL": "1s",
            "CONTROL_CONFIG_LEASE": "30s",
            "CONTROL_STATUS_REPORT_INTERVAL": "1s",
            "GATEWAY_INSTANCE_HEARTBEAT_INTERVAL": "1s",
            "GATEWAY_BILLING_ADMISSION_TIMEOUT": "5s",
            "GATEWAY_BILLING_RECEIPT_TIMEOUT": "5s",
            "GATEWAY_BILLING_RECEIPT_VERIFY_TIMEOUT": "3s",
            "GATEWAY_BILLING_DECISION_TIMEOUT": "3s",
            "GATEWAY_FINALIZE_TIMEOUT": "8s",
            "GATEWAY_FAILPOINT_HOLD": "90s",
            "METERING_SPOOL_DIR": str(self.output / f"spool-{which}"),
        }
        if extra:
            env.update(extra)
        return env

    def start_gateway(self, which="a", failpoints=None, instance_id=None, extra_env=None):
        extra = {}
        if failpoints:
            extra["GATEWAY_FAILPOINTS"] = failpoints
        if instance_id:
            extra["GATEWAY_INSTANCE_ID"] = instance_id
        if extra_env:
            extra.update(extra_env)
        env = self.gateway_env(which, extra)
        process = self.start_process(f"gateway_{which}", [str(self.build_dir / "gateway")], env)
        wait_until(lambda: http_status(self.gateway(which) + "/healthz", timeout=5) == 200, 60,
                   f"gateway {which} healthz")
        if self.desired_version:
            # The key snapshot arrives over the control watch, so a gateway that
            # is merely listening may still answer 401 for a key the scenario is
            # about to use.
            instance = env.get("GATEWAY_INSTANCE_ID", f"gw-{which}")
            wait_until(lambda: any(
                target.get("instance_id") == instance
                and target.get("health") == "active"
                and target.get("applied_version") == self.desired_version
                for target in http_json(self.control_http() + "/gateways",
                                        headers={"Authorization": "Bearer " + ADMIN_TOKEN},
                                        timeout=10).get("instances", [])),
                60, f"gateway {which} to apply v{self.desired_version}")
        return process

    def gateway(self, which="a"):
        return f"http://127.0.0.1:{self.ports['gateway_' + which]}"

    def diag(self, which="a"):
        return f"http://127.0.0.1:{self.ports['diag_' + which]}"

    def start_metering(self, failpoints=None, recovery=None, settlement="on", wait_ready=True):
        extra = {
            "METERING_ADDR": f"127.0.0.1:{self.ports['metering']}",
            "METERING_DIAGNOSTICS_ADDR": f"127.0.0.1:{self.ports['metering_diag']}",
            "METERING_ADMIN_TOKEN": ADMIN_TOKEN,
            "METERING_REQUIRE_POSTGRES": "true",
            "METERING_SETTLEMENT": settlement,
            "METERING_SETTLEMENT_POLL": "250ms",
            "METERING_SETTLEMENT_REDISPATCH": "2s",
            "METERING_SETTLEMENT_BACKLOG_INTERVAL": "2s",
            "METERING_RECOVERY": recovery or "on",
            "METERING_RECOVERY_INTERVAL": "1s",
            "METERING_RECOVERY_LEASE": "11s",
            "METERING_RECOVERY_MAX_EXECUTION": "3s",
            "METERING_RECOVERY_GRACE": "1s",
            "METERING_RECOVERY_TIMEOUT": "10s",
            "METERING_RECOVERY_BATCH": "32",
            "GATEWAY_FAILPOINT_HOLD": "90s",
            "METERING_SPOOL_DIR": str(self.output / "spool-metering"),
        }
        if failpoints:
            extra["GATEWAY_FAILPOINTS"] = failpoints
        process = self.start_process("metering", [str(self.build_dir / "metering")], extra)
        if wait_ready:
            wait_until(lambda: http_status(f"http://127.0.0.1:{self.ports['metering']}/readyz", timeout=5) == 200,
                       60, "metering readyz")
        return process

    def kill_at(self, process, failpoint_name, timeout=60):
        """Wait for a process to be stopped AT a window, then SIGKILL it."""
        event = process.wait_failpoint(failpoint_name, timeout=timeout)
        if event.get("event") != "hit":
            raise RuntimeError(f"{process.name} never reached {failpoint_name}: {event}")
        killed = process.kill9(reason=f"failpoint {failpoint_name}")
        return {"failpoint": event, "killed_at": killed}

    # -- observations -------------------------------------------------------

    def metrics(self, which="a"):
        text = urllib.request.urlopen(self.diag(which) + "/metrics", timeout=20).read().decode()
        return parse_metrics(text)

    def org_state(self, org):
        executions = self.psql_rows(
            f"SELECT execution_id, status, terminal_reason, reserved_micro, gateway_id "
            f"FROM billing_executions WHERE org_id = {org.org_id} ORDER BY created_at, execution_id")
        receipts = self.psql_rows(
            f"SELECT execution_id, event_id, status, charge_micro, total_tokens "
            f"FROM usage_receipts WHERE org_id = {org.org_id} ORDER BY created_at, execution_id")
        entries = self.psql_rows(
            f"SELECT kind, direction, amount_micro, business_id FROM billing_entries "
            f"WHERE org_id = {org.org_id} ORDER BY id")
        account = self.psql_value(
            f"SELECT posted_balance_micro, reserved_micro, version FROM billing_accounts WHERE org_id = {org.org_id}")
        audits = self.psql_rows(
            f"SELECT action, target_id, outcome FROM console_audit_events WHERE actor_org_id = {org.org_id} "
            f"ORDER BY id")
        return {
            "executions": [dict(zip(("execution_id", "status", "terminal_reason", "reserved_micro", "gateway_id"), row))
                           for row in executions],
            "receipts": [dict(zip(("execution_id", "event_id", "status", "charge_micro", "total_tokens"), row))
                         for row in receipts],
            "entries": [dict(zip(("kind", "direction", "amount_micro", "business_id"), row)) for row in entries],
            "account": account,
            "audits": [dict(zip(("action", "target_id", "outcome"), row)) for row in audits],
        }

    def write_json(self, relative, payload):
        path = self.output / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(payload, indent=2, sort_keys=True, default=str), encoding="utf-8")
        return path


def parse_metrics(text):
    out = {}
    for line in text.splitlines():
        if line.startswith("#") or not line.strip():
            continue
        parts = line.rsplit(" ", 1)
        if len(parts) != 2:
            continue
        name = parts[0].split("{")[0]
        try:
            value = float(parts[1])
        except ValueError:
            continue
        out[name] = out.get(name, 0.0) + value
    return out


def assert_that(assertions, name, ok, detail):
    assertions.append({"assertion": name, "ok": bool(ok), "detail": detail})
    return bool(ok)


def verdict(assertions):
    return "PASS" if all(a["ok"] for a in assertions) else "FAIL"


# ---------------------------------------------------------------------------
# scenarios
# ---------------------------------------------------------------------------


# The Result the currently running scenario is filling in. main() reads it when a
# scenario raises, so a crash still leaves the evidence collected so far instead
# of an empty record.
CURRENT_RESULT = None


class Result:
    def __init__(self, sid, title):
        global CURRENT_RESULT
        CURRENT_RESULT = self
        self.sid = sid
        self.title = title
        self.assertions = []
        self.evidence = {}
        self.notes = []
        self.partial_reason = None

    def check(self, name, ok, detail):
        self.assertions.append({"assertion": name, "ok": bool(ok), "detail": detail})
        return bool(ok)

    def note(self, text):
        self.notes.append(text)

    def partial(self, reason):
        self.partial_reason = reason

    def as_dict(self):
        if not self.assertions:
            status = "NOT_EXECUTED"
        elif all(a["ok"] for a in self.assertions):
            status = "PARTIAL" if self.partial_reason else "PASS"
        else:
            status = "FAIL"
        return {
            "id": self.sid,
            "title": self.title,
            "status": status,
            "partial_reason": self.partial_reason,
            "assertions": self.assertions,
            "notes": self.notes,
            "evidence": self.evidence,
        }


def delta(before, after, list_key):
    return len(after[list_key]) - len(before[list_key])


def entries_of(state, kind):
    return [e for e in state["entries"] if e["kind"] == kind]


def count_audit(state, action):
    """How many audit rows of one action this organization has. Audit rows are
    the durable evidence a release or an abandonment leaves behind, so the
    scenarios count them per action instead of pattern-matching a log."""
    return len([row for row in state["audits"] if row["action"] == action])


def per_execution_entries(state):
    """business_id -> entries, so "at most one of each kind per execution" can
    be checked without depending on settlement timing."""
    out = {}
    for entry in state["entries"]:
        execution_id = entry["business_id"].rsplit(":", 1)[-1]
        out.setdefault(execution_id, []).append(entry)
    return out


def kinds_per_execution(state, kind):
    return {execution_id: len([e for e in entries if e["kind"] == kind])
            for execution_id, entries in per_execution_entries(state).items()}


def account_tuple(state):
    parts = state["account"].split("|") if state["account"] else []
    if len(parts) != 3:
        return {"posted_balance_micro": None, "reserved_micro": None, "version": None}
    return {"posted_balance_micro": int(parts[0]), "reserved_micro": int(parts[1]), "version": int(parts[2])}


def gw(which="a"):
    return "127.0.0.1", which


def request(stack, org, model=MODEL_OK, which="a", timeout=60, **extra):
    return sse_request("127.0.0.1", stack.ports["gateway_" + which], org.raw_key,
                       chat_payload(model=model, **extra), timeout=timeout)


# --- F01 -------------------------------------------------------------------


def run_f01(stack):
    r = Result("F01", "a failure before the first event fails over safely, with exactly one customer charge")
    org = stack.orgs["F01"]
    before = stack.org_state(org)
    retry_before = stack.metrics("a").get("provider_retry_total", 0.0)
    cases = {}
    for label, model in (("upstream_503", MODEL_FAIL503), ("upstream_429", MODEL_FAIL429),
                         ("upstream_timeout", MODEL_HANG)):
        result = request(stack, org, model=model, timeout=90)
        cases[label] = {"model": model, **result.as_dict()}
    after = stack.org_state(org)
    retry_after = stack.metrics("a").get("provider_retry_total", 0.0)
    r.evidence = {
        "cases": cases,
        "executions_created": delta(before, after, "executions"),
        "receipts_created": delta(before, after, "receipts"),
        "reservation_entries_created": len(entries_of(after, "reservation")) - len(entries_of(before, "reservation")),
        "release_entries_created": len(entries_of(after, "release")) - len(entries_of(before, "release")),
        "account": account_tuple(after),
        "provider_retry_delta": retry_after - retry_before,
        "receipts": after["receipts"],
        "executions": after["executions"],
    }
    for label, payload in cases.items():
        r.check(f"{label}: the client still got a complete stream",
                payload["status"] == 200 and payload["has_done"],
                {"status": payload["status"], "has_done": payload["has_done"]})
    r.check("the backup provider served every failed-over request",
            all("ok-1" in c.get("body_head", "") for c in cases.values()),
            {label: c.get("body_head") for label, c in cases.items()})
    r.check("exactly one execution per request (no attempt became its own execution)",
            delta(before, after, "executions") == 3,
            {"executions_created": delta(before, after, "executions")})
    r.check("exactly one durable receipt per execution",
            delta(before, after, "receipts") == 3,
            [x["status"] for x in after["receipts"]])
    r.check("exactly one reservation entry per request",
            len(entries_of(after, "reservation")) - len(entries_of(before, "reservation")) == 3,
            entries_of(after, "reservation"))
    reservations = kinds_per_execution(after, "reservation")
    debits = kinds_per_execution(after, "debit")
    releases = kinds_per_execution(after, "release")
    r.evidence["per_execution_entries"] = {"reservation": reservations, "debit": debits, "release": releases}
    consistent = all(debits.get(execution_id, 0) == releases.get(execution_id, 0)
                     and debits.get(execution_id, 0) <= 1
                     for execution_id in reservations)
    r.check("every execution was reserved exactly once and charged at most once",
            len(reservations) == 3 and all(v == 1 for v in reservations.values()) and consistent,
            {"reservation": reservations, "debit": debits, "release": releases})
    r.check("the provider retry counter shows the failover actually happened",
            retry_after - retry_before == 3, {"delta": retry_after - retry_before})
    r.partial("'per-attempt upstream cost' has no implementation in this revision: billing_entries records the "
              "customer charge only, so the second half of F01's third clause ('attempt cost is independent') "
              "cannot be verified from this system's facts.")
    return r


# --- F02 -------------------------------------------------------------------


def run_f02(stack):
    r = Result("F02", "a mid-stream break is incomplete, is never transparently replayed, and its reservation is released by durable evidence, not by a blind TTL")
    org = stack.orgs["F02"]
    before = stack.org_state(org)
    incomplete_before = stack.metrics("a").get("gateway_stream_incomplete_total", 0.0)
    abandoned_before = stack.metrics("a").get("gateway_billing_attempt_abandoned_total", 0.0)
    result = request(stack, org, model=MODEL_CUT, timeout=60)
    incomplete_after = stack.metrics("a").get("gateway_stream_incomplete_total", 0.0)
    abandoned_after = stack.metrics("a").get("gateway_billing_attempt_abandoned_total", 0.0)
    immediate = stack.org_state(org)
    r.evidence = {
        "stream": result.as_dict(),
        "executions_created": delta(before, immediate, "executions"),
        "incomplete_metric_delta": incomplete_after - incomplete_before,
        "abandoned_metric_delta": abandoned_after - abandoned_before,
        "executions": immediate["executions"],
        "receipts": immediate["receipts"],
        "account": account_tuple(immediate),
    }
    r.check("the broken stream is not a complete stream", result.status == 200 and not result.as_dict()["has_done"],
            result.as_dict())
    r.check("the client saw exactly the events the upstream produced before the break",
            len(result.events) == 2, {"events": len(result.events)})
    r.check("gateway_stream_incomplete_total counts the break",
            incomplete_after - incomplete_before >= 1, {"delta": incomplete_after - incomplete_before})
    r.check("the break created exactly one execution and no second attempt",
            delta(before, immediate, "executions") == 1, immediate["executions"])
    r.check("no durable receipt backs the broken stream", len(immediate["receipts"]) == 0, immediate["receipts"])

    # F02-1 fix (option b: durable evidence, never a blind TTL). The owning
    # process observed the attempt end without a durable decision, so it writes
    # the abandonment marker. The reservation is *not* returned here: T07B's
    # recovery still owns the release, its window, its receipt guard and its
    # audit row — the marker only removes the owner-liveness requirement.
    execution_id = immediate["executions"][0]["execution_id"]
    marker = str(immediate["executions"][0]["terminal_reason"] or "")
    r.check("the attempt ended with a durable abandonment marker (no status change, no money movement)",
            marker.startswith("abandoned:") and immediate["executions"][0]["status"] == "executing",
            immediate["executions"])
    r.check("the abandonment is visible as a metric",
            abandoned_after - abandoned_before >= 1, {"delta": abandoned_after - abandoned_before})
    r.check("the immediate state still holds the reservation (the marker is not a release)",
            not entries_of(immediate, "release") and not entries_of(immediate, "debit")
            and account_tuple(immediate)["reserved_micro"]
            == int(immediate["executions"][0]["reserved_micro"]),
            {"entries": immediate["entries"], "account": account_tuple(immediate)})
    r.check("the abandonment itself is audited, with the reason",
            count_audit(immediate, "billing.execution.abandoned") == 1,
            immediate["audits"])

    # Now wait past the configured recovery window (3s + 1s grace) with the
    # owning gateway (gw-a) still heartbeating: the marked execution must be
    # released, with the recorded checks and the recovery audit row.
    released = wait_until(lambda: (lambda s: s if (s["executions"] and s["executions"][0]["status"] == "released")
                                   else None)(stack.org_state(org)), 90, "the marked reservation to be released")
    if released is None:
        r.check("the marked reservation is released after the recovery window", False, stack.org_state(org))
        return r
    recovery_audit = stack.psql_rows(
        f"SELECT detail->>'abandonment_reason', detail->>'owner_liveness_waived', "
        f"detail->>'owner_alive', detail->>'manual_override' "
        f"FROM console_audit_events WHERE action = 'billing.execution.recovered' "
        f"AND target_id = '{execution_id}'")
    window = {"executions": released["executions"], "entries": released["entries"],
              "account": account_tuple(released), "audits": released["audits"],
              "recovery_window_seconds": 4, "recovery_audit": recovery_audit,
              "gateway_instance": stack.psql_rows(
                  "SELECT instance_id, last_seen_at::text FROM gateway_instances ORDER BY instance_id"),
              "metering_recovery_lease_seconds": 11}
    r.evidence["after_recovery_window"] = window
    r.check("the reservation is released after the window and the funds are returned exactly once",
            account_tuple(released)["reserved_micro"] == 0
            and len(entries_of(released, "release")) == 1,
            {"account": account_tuple(released), "entries": released["entries"]})
    r.check("the release is the audited recovery row (billing.execution.recovered)",
            count_audit(released, "billing.execution.recovered") == 1, released["audits"])
    r.check("the release names the abandonment and records the waived liveness check",
            "abandoned (" in str(released["executions"][0]["terminal_reason"] or "")
            and recovery_audit
            and recovery_audit[0][0]
            and recovery_audit[0][1] == "true" and recovery_audit[0][2] == "true"
            and recovery_audit[0][3] == "false",
            {"terminal_reason": released["executions"][0]["terminal_reason"], "audit": recovery_audit})
    r.check("no chargeable receipt exists for the released execution",
            not released["receipts"], released["receipts"])

    # (2) The uncertain case must still hold. A finalization whose outcome is
    # unknown (here: the receipt INSERT is forced to fail) must NOT be marked
    # abandoned, so the reservation stays held past the same window: the human
    # review / T07B path owns it, not a timer.
    org_uncertain = stack.orgs["F02-uncertain"]
    stack.start_gateway("a", failpoints="receipt.before_insert=error", instance_id="gw-a")
    uncertain = request(stack, org_uncertain, model=MODEL_OK, timeout=60)
    uncertain_immediate = stack.org_state(org_uncertain)
    time.sleep(8)
    uncertain_later = stack.org_state(org_uncertain)
    r.evidence["uncertain_finalization"] = {
        "stream": uncertain.as_dict(),
        "immediate": {"executions": uncertain_immediate["executions"],
                      "receipts": uncertain_immediate["receipts"]},
        "after_window": {"executions": uncertain_later["executions"], "entries": uncertain_later["entries"],
                         "account": account_tuple(uncertain_later), "audits": uncertain_later["audits"]},
        "note": "the gateway is armed with receipt.before_insert=error: FinalizeReceipt fails before "
                "the receipt row exists, so the call site cannot know whether a chargeable fact "
                "exists. It must keep the hold.",
    }
    r.check("uncertain finalization: the client is not given a clean end", not uncertain.as_dict()["has_done"],
            uncertain.as_dict())
    r.check("uncertain finalization: no receipt and no abandonment marker were written",
            not uncertain_later["receipts"]
            and not str(uncertain_later["executions"][0]["terminal_reason"] or "").startswith("abandoned:")
            and uncertain_later["executions"][0]["status"] == "executing",
            uncertain_later["executions"])
    r.check("uncertain finalization: the reservation is still held after the recovery window",
            account_tuple(uncertain_later)["reserved_micro"] > 0
            and account_tuple(uncertain_later)["reserved_micro"]
            == account_tuple(uncertain_immediate)["reserved_micro"]
            and not entries_of(uncertain_later, "release")
            and count_audit(uncertain_later, "billing.execution.recovered") == 0,
            {"account": account_tuple(uncertain_later), "entries": uncertain_later["entries"],
             "audits": uncertain_later["audits"]})

    # (3) A late receipt after the release is still refused. The marker is
    # injected with SQL here (the gateway's own writer is case 1) so the runner
    # can hold an attempt open while the janitor releases it: the terminal usage
    # event then arrives for an execution whose funds are already returned. The
    # store must refuse the chargeable receipt, the gateway must withhold [DONE]
    # and record the late usage as a durable anomaly, and no money may move.
    org_late = stack.orgs["F02-late"]
    late_request = BackgroundRequest(
        "127.0.0.1", stack.ports["gateway_a"], org_late.raw_key,
        chat_payload(model=MODEL_OK, x_mock_tokens=30, x_mock_token_interval_ms=400), timeout=90).start()
    late_execution = wait_until(lambda: (lambda s: s[0][0] if s else None)(
        stack.psql_rows(f"SELECT execution_id FROM billing_executions WHERE org_id = {org_late.org_id} "
                        f"AND status IN ('reserved','executing')")), 30, "the late-receipt attempt to be admitted")
    if late_execution is not None:
        stack.psql(f"UPDATE billing_executions SET terminal_reason = 'abandoned:runner_late_receipt_probe', "
                   f"updated_at = now() WHERE execution_id = '{late_execution}'")
        late_released = wait_until(lambda: (lambda s: s if (s["executions"]
                                                            and s["executions"][0]["status"] == "released")
                                            else None)(stack.org_state(org_late)), 60, "the probe to be released")
    else:
        late_released = None
    late_client = late_request.join(90)
    late_state = stack.org_state(org_late)
    late_anomalies = stack.psql_rows(
        f"SELECT action, coalesce(detail->>'reason','') FROM console_audit_events "
        f"WHERE target_id = '{late_execution}' AND action NOT LIKE 'billing.execution.%'")
    gateway_log = stack.process("gateway_a").text() if stack.process("gateway_a") else ""
    r.evidence["late_receipt_after_release"] = {
        "execution_id": late_execution,
        "marker_injected_by": "SQL (the gateway's own marker writer is case 1)",
        "client": late_client,
        "released_before_the_terminal_event": late_released is not None,
        "executions": late_state["executions"],
        "receipts": late_state["receipts"],
        "entries": late_state["entries"],
        "account": account_tuple(late_state),
        "anomalies": late_anomalies,
        "gateway_refusal_log": [line for line in gateway_log.splitlines()
                                if "late usage" in line or "finalization refused" in line][-3:],
    }
    r.check("late receipt: the execution was released before its terminal usage event arrived",
            late_released is not None and late_released["executions"][0]["status"] == "released",
            late_released["executions"] if late_released else None)
    r.check("late receipt: the released execution refuses the chargeable receipt (no receipt row)",
            not late_state["receipts"], late_state["receipts"])
    r.check("late receipt: no money moved for the released execution (exactly one release, no debit)",
            len(entries_of(late_state, "release")) == 1 and not entries_of(late_state, "debit")
            and account_tuple(late_state)["reserved_micro"] == 0,
            {"entries": late_state["entries"], "account": account_tuple(late_state)})
    r.check("late receipt: the client is not given a clean end for an unchargeable execution",
            not (late_client.get("stream") or {}).get("has_done"), late_client)
    r.check("late receipt: the refusal is a durable, queryable anomaly and the gateway logged it",
            any(row[0] == "billing.settlement.exception" for row in late_anomalies)
            and any("late usage" in line for line in gateway_log.splitlines()),
            {"anomalies": late_anomalies,
             "log": [line for line in gateway_log.splitlines() if "finalization refused" in line][-2:]})
    return r


# --- F03 -------------------------------------------------------------------


def run_f03(stack):
    r = Result("F03", "client cancel (normal and slow reader) stops the upstream and reclaims resources without a new attempt")
    org = stack.orgs["F03"]
    baseline = stack.metrics("a")
    upstream_before = stack.counting.snapshot()
    before = stack.org_state(org)

    # (a) normal reader cancels as soon as the first content delta arrived.
    stream = SSEStream("127.0.0.1", stack.ports["gateway_a"], "/v1/chat/completions", org.raw_key,
                       chat_payload(model=MODEL_COUNT, x_mock_tokens=400,
                                    x_mock_token_interval_ms=20), timeout=60).open()
    deadline = time.time() + 20
    while stream.result.first_content_seconds is None and time.time() < deadline:
        try:
            chunk = stream.response.read(1)
        except Exception as err:  # noqa: BLE001
            stream.result.error = str(err)
            break
        if not chunk:
            break
        stream._buffer += chunk
        while b"\n\n" in stream._buffer:
            raw, stream._buffer = stream._buffer.split(b"\n\n", 1)
            stream._consume_event(raw, stream.result.started_at)
    normal = stream.cancel()

    # (b) slow reader: 1 KiB/s for a few seconds, then the same abrupt cancel.
    slow_stream = SSEStream("127.0.0.1", stack.ports["gateway_a"], "/v1/chat/completions", org.raw_key,
                            chat_payload(model=MODEL_COUNT, x_mock_tokens=400,
                                         x_mock_token_interval_ms=20), timeout=60).open()
    slow = slow_stream.read_slow(1024, budget_seconds=4)
    slow = slow_stream.cancel()

    # Resource reclamation: the active-stream gauge and the goroutine count must
    # come back within the 5s target of F03.
    reclaimed = {}
    reclaim_deadline = time.time() + 5
    while time.time() < reclaim_deadline:
        metrics = stack.metrics("a")
        reclaimed = {"active_streams": metrics.get("gateway_active_streams", 0.0),
                     "go_goroutines": metrics.get("go_goroutines", 0.0)}
        if reclaimed["active_streams"] == 0:
            break
        time.sleep(0.2)
    after = stack.org_state(org)
    # F02-1: a client cancel is one of the confirmed terminal paths, so both
    # cancels must leave the durable abandonment marker (the reservation is then
    # returned by T07B's window, never by the cancel itself). The assertion is
    # on the durable evidence, so it stays true whether or not the janitor has
    # already released the execution by the time this read runs.
    marked = wait_until(lambda: (lambda s: s if (
        count_audit(s, "billing.execution.abandoned") == 2
        and all("abandoned" in str(row["terminal_reason"] or "") for row in s["executions"])) else None)(
            stack.org_state(org)), 20, "both cancels to leave the durable abandonment marker")
    upstream_after = stack.counting.snapshot()
    r.evidence = {
        "normal_client": normal.as_dict(),
        "slow_client": slow.as_dict(),
        "reclaimed_within_5s": reclaimed,
        "baseline_goroutines": baseline.get("go_goroutines", 0.0),
        "executions_created": delta(before, after, "executions"),
        "executions": (marked or after)["executions"],
        "abandonment_audits": count_audit(marked or after, "billing.execution.abandoned"),
        "receipts": after["receipts"],
        "upstream_before": upstream_before,
        "upstream_after": upstream_after,
    }
    r.check("the normal reader's cancel was observed as a cancelled stream",
            normal.classification == "cancelled", normal.as_dict())
    r.check("the slow reader (1 KiB/s) also reached the cancel path",
            slow.classification == "cancelled", slow.as_dict())
    r.check("the upstream stopped producing: its in-flight count returned to zero",
            upstream_after["active"] == 0, upstream_after)
    r.check("no new attempt was created by a cancel: exactly two executions for two cancels",
            delta(before, after, "executions") == 2, after["executions"])
    r.check("no receipt was committed for a cancelled stream",
            delta(before, after, "receipts") == 0, after["receipts"])
    r.check("gateway_active_streams returned to zero within 5s of the cancels",
            reclaimed.get("active_streams") == 0, reclaimed)
    r.check("goroutines did not grow without bound across the cancels",
            reclaimed.get("go_goroutines", 0) <= baseline.get("go_goroutines", 0) + 10,
            {"baseline": baseline.get("go_goroutines", 0.0), "after": reclaimed.get("go_goroutines", 0.0)})
    r.check("each cancel left the durable abandonment marker (F02-1: evidence, not a blind TTL)",
            marked is not None, r.evidence["executions"])
    return r


# --- F04 -------------------------------------------------------------------


def run_f04(stack):
    r = Result("F04", "missing, negative and self-contradicting usage all become a PostgreSQL exception, never a zero charge")
    org = stack.orgs["F04"]
    before = stack.org_state(org)
    cases = {}
    for label in ("missing", "negative", "mismatch"):
        result = request(stack, org, model=MODEL_OK, timeout=60, x_mock_usage=label)
        cases[label] = result.as_dict()
    after = stack.org_state(org)
    exceptions = stack.psql_rows(
        f"SELECT execution_id, status, coalesce(last_error,''), charge_micro FROM usage_receipts "
        f"WHERE org_id = {org.org_id} ORDER BY created_at")
    r.evidence = {
        "streams": cases,
        "executions": after["executions"],
        "receipts": exceptions,
        "entries": after["entries"],
        "account": account_tuple(after),
        "incomplete_metric": stack.metrics("a").get("gateway_stream_incomplete_total", 0.0),
    }
    for label, payload in cases.items():
        r.check(f"{label}: the stream ended without [DONE] (no clean end without a durable decision)",
                payload["status"] == 200 and not payload["has_done"], payload)
    r.check("exactly one execution per refused boundary",
            delta(before, after, "executions") == 3, after["executions"])
    r.check("every refused boundary left a durable PostgreSQL exception receipt",
            len(exceptions) == 3 and all(row[1] == "exception" for row in exceptions), exceptions)
    r.check("every exception receipt carries a reason (never a silent zero)",
            all(row[2] for row in exceptions), [row[2] for row in exceptions])
    r.check("the execution is review_required, not settled and not released",
            all(row["status"] == "review_required" for row in after["executions"]), after["executions"])
    r.check("no money moved for any refused boundary (no debit, no release)",
            len(entries_of(after, "debit")) == len(entries_of(before, "debit"))
            and len(entries_of(after, "release")) == len(entries_of(before, "release")),
            after["entries"])
    r.check("the three reasons are distinguishable and name the actual defect",
            len({row[2] for row in exceptions}) == 3, [row[2] for row in exceptions])
    return r


# --- F05 -------------------------------------------------------------------


def run_f05(stack):
    r = Result("F05", "a kill between the settlement commit and the Redis ACK redelivers and still debits exactly once")
    org = stack.orgs["F05"]

    # Stop metering so the receipt this request produces stays undispatched until
    # the armed consumer picks it up.
    stack.process("metering").stop()
    stream = request(stack, org, model=MODEL_OK, timeout=60)
    pending = stack.org_state(org)
    r.evidence["stream"] = stream.as_dict()
    r.evidence["receipt_before_redelivery"] = pending["receipts"]
    r.check("the request completed with a durable receipt",
            stream.as_dict()["has_done"] and len(pending["receipts"]) == 1, pending["receipts"])

    stack.start_metering(failpoints="settlement.ack_before=hold")
    stack.kill_at(stack.process("metering"), "settlement.ack_before", timeout=90)
    committed = stack.org_state(org)
    pending_count = stack.redis_cli("XPENDING", RECEIPT_STREAM, SETTLEMENT_GROUP)
    r.evidence["after_kill"] = {
        "receipts": committed["receipts"],
        "entries": committed["entries"],
        "account": account_tuple(committed),
        "xpending": pending_count,
    }
    r.check("the settlement transaction committed before the kill",
            len(committed["receipts"]) == 1 and committed["receipts"][0]["status"] == "settled",
            committed["receipts"])
    r.check("exactly one debit was posted by the committed settlement",
            len(entries_of(committed, "debit")) == 1, committed["entries"])
    r.check("the message is still pending in the consumer group (the ACK never happened)",
            pending_count.splitlines()[0].strip() not in ("", "0"), pending_count)

    stack.start_metering()
    converged = wait_until(
        lambda: (lambda s: s if s["receipts"] and s["receipts"][0]["status"] == "settled"
                 and stack.redis_cli("XPENDING", RECEIPT_STREAM, SETTLEMENT_GROUP).splitlines()[0].strip() == "0"
                 else None)(stack.org_state(org)),
        timeout=120, what="redelivery to converge and be acknowledged")
    r.evidence["after_redelivery"] = {
        "receipts": converged["receipts"],
        "entries": converged["entries"],
        "account": account_tuple(converged),
        "xpending": stack.redis_cli("XPENDING", RECEIPT_STREAM, SETTLEMENT_GROUP),
        "settlement_metrics": {k: v for k, v in stack.metrics("a").items() if k.startswith("metering_settlement")},
    }
    consumer_log = stack.process("metering").text()
    r.check("the redelivered message was a replay, not a second debit",
            len(entries_of(converged, "debit")) == 1, converged["entries"])
    r.check("the redelivery was acknowledged after the replay",
            stack.redis_cli("XPENDING", RECEIPT_STREAM, SETTLEMENT_GROUP).splitlines()[0].strip() == "0",
            stack.redis_cli("XPENDING", RECEIPT_STREAM, SETTLEMENT_GROUP))
    r.check("the consumer recorded the redelivery as a replay",
            "settlement replay" in consumer_log, [line for line in consumer_log.splitlines()
                                                   if "settlement replay" in line][-2:])
    r.check("available/reserved agree with the ledger after convergence",
            account_tuple(converged)["reserved_micro"] == 0, account_tuple(converged))
    return r


# --- F06 + R01 (recovery release windows) ----------------------------------


def _stuck_reserved_execution(stack, org, model=MODEL_COUNT, port="gateway_a"):
    """Kill the gateway at admission.after_reserve, leaving a `reserved`
    execution whose funds are held and whose owner is about to disappear."""
    stack.start_gateway("a", failpoints="admission.after_reserve=hold")
    pending = BackgroundRequest("127.0.0.1", stack.ports[port], org.raw_key,
                                chat_payload(model=model), timeout=60).start()
    stack.kill_at(stack.process("gateway_a"), "admission.after_reserve", timeout=60)
    client = pending.join(30)
    state = stack.org_state(org)
    return client, state


def run_f06(stack):
    r = Result("F06", "a kill after admission leaves a recoverable execution and no unbounded hold")
    sub = {}
    for label, failpoint, model, expect_status in (
        ("before_reserve", "admission.before_reserve", MODEL_COUNT, "no-row"),
        ("after_reserve", "admission.after_reserve", MODEL_COUNT, "reserved"),
        ("after_mark_executing", "admission.after_mark_executing", MODEL_COUNT, "executing"),
    ):
        org = stack.orgs[f"F06-{label}"]
        upstream_before = stack.counting.snapshot()
        stack.start_gateway("a", failpoints=f"{failpoint}=hold")
        pending = BackgroundRequest("127.0.0.1", stack.ports["gateway_a"], org.raw_key,
                                    chat_payload(model=model), timeout=60).start()
        marker = stack.kill_at(stack.process("gateway_a"), failpoint, timeout=60)
        client = pending.join(30)
        state = stack.org_state(org)
        upstream_after = stack.counting.snapshot()
        sub[label] = {
            "failpoint": marker,
            "client": client,
            "executions": state["executions"],
            "receipts": state["receipts"],
            "entries": state["entries"],
            "account": account_tuple(state),
            "upstream_requests_delta": upstream_after["requests"] - upstream_before["requests"],
        }
    r.evidence = {"sub_cases": sub}

    r.check("kill before reserve: no execution row and no held funds exist",
            not sub["before_reserve"]["executions"]
            and sub["before_reserve"]["account"]["reserved_micro"] == 0,
            sub["before_reserve"])
    r.check("kill before reserve: the request never reached an upstream",
            sub["before_reserve"]["upstream_requests_delta"] == 0, sub["before_reserve"])
    r.check("kill after reserve: the execution is a recoverable `reserved` row holding its reservation",
            len(sub["after_reserve"]["executions"]) == 1
            and sub["after_reserve"]["executions"][0]["status"] == "reserved"
            and int(sub["after_reserve"]["executions"][0]["reserved_micro"]) > 0
            and sub["after_reserve"]["account"]["reserved_micro"] > 0,
            sub["after_reserve"])
    r.check("kill after reserve: no receipt and no charge exist for the abandoned admission",
            not sub["after_reserve"]["receipts"] and not entries_of(
                {"entries": sub["after_reserve"]["entries"]}, "debit"),
            sub["after_reserve"])
    r.check("kill after reserve: the request never reached an upstream",
            sub["after_reserve"]["upstream_requests_delta"] == 0, sub["after_reserve"])
    r.check("kill after mark-executing: the execution is `executing` with its reservation held",
            len(sub["after_mark_executing"]["executions"]) == 1
            and sub["after_mark_executing"]["executions"][0]["status"] == "executing"
            and sub["after_mark_executing"]["account"]["reserved_micro"] > 0,
            sub["after_mark_executing"])
    r.check("kill after mark-executing: still no upstream call and no receipt",
            sub["after_mark_executing"]["upstream_requests_delta"] == 0
            and not sub["after_mark_executing"]["receipts"],
            sub["after_mark_executing"])

    # The reservation must not be released by a blind timer while the owner is
    # alive: restart the gateway (new started_at > execution.created_at) and let
    # the janitor prove death before it releases.
    org = stack.orgs["F06-after_reserve"]
    stack.start_gateway("a")
    time.sleep(4)
    while_alive = stack.org_state(org)
    r.evidence["before_owner_death"] = {"executions": while_alive["executions"],
                                        "account": account_tuple(while_alive)}
    r.check("with the owner restarting, the janitor releases the abandoned reservation",
            wait_until(lambda: (lambda s: s if s["executions"] and s["executions"][0]["status"] == "released"
                                else None)(stack.org_state(org)), 90, "recovery release") is not None,
            stack.org_state(org)["executions"])
    released = stack.org_state(org)
    r.evidence["after_release"] = {"executions": released["executions"], "entries": released["entries"],
                                   "account": account_tuple(released), "audits": released["audits"]}
    r.check("the release returned the funds exactly once",
            account_tuple(released)["reserved_micro"] == 0
            and len(entries_of(released, "release")) == 1,
            {"account": account_tuple(released), "entries": released["entries"]})
    r.check("the release is audited",
            any(row["action"] == "billing.execution.recovered" for row in released["audits"]),
            released["audits"])
    return r


def run_r01(stack):
    r = Result("R01", "recovery release: kill before and after COMMIT (T07B section 7 window)")
    org = stack.orgs["R01"]
    stack.start_metering()  # the janitor runs with the metering process
    _, stuck = _stuck_reserved_execution(stack, org)
    r.evidence["stuck"] = {"executions": stuck["executions"], "account": account_tuple(stuck)}
    r.check("the abandoned execution starts as a `reserved` row holding funds",
            len(stuck["executions"]) == 1 and stuck["executions"][0]["status"] == "reserved",
            stuck["executions"])

    stack.start_gateway("a")  # new started_at: the old owner is provably gone
    stack.start_metering(failpoints="recovery.before_commit=hold")
    stack.kill_at(stack.process("metering"), "recovery.before_commit", timeout=120)
    before = stack.org_state(org)
    r.evidence["after_kill_before_commit"] = {
        "executions": before["executions"], "entries": before["entries"],
        "account": account_tuple(before), "audits": before["audits"],
    }
    r.check("a kill before the release COMMIT leaves the execution untouched",
            before["executions"][0]["status"] == "reserved"
            and len(entries_of(before, "release")) == 0
            and account_tuple(before)["reserved_micro"] == account_tuple(stuck)["reserved_micro"],
            {"executions": before["executions"], "account": account_tuple(before)})

    stack.start_metering()
    released = wait_until(lambda: (lambda s: s if s["executions"] and s["executions"][0]["status"] == "released"
                                   else None)(stack.org_state(org)), 120, "release after restart")
    r.evidence["after_commit"] = {
        "executions": released["executions"], "entries": released["entries"],
        "account": account_tuple(released), "audits": released["audits"],
    }
    r.check("after a clean restart the release commits exactly once",
            len(entries_of(released, "release")) == 1
            and account_tuple(released)["reserved_micro"] == 0,
            {"entries": released["entries"], "account": account_tuple(released)})
    r.check("the release wrote its audit row with the liveness evidence",
            any(row["action"] == "billing.execution.recovered" and row["outcome"] == "success"
                for row in released["audits"]),
            released["audits"])
    return r


# --- F07 / F18 -------------------------------------------------------------


def run_f07(stack):
    r = Result("F07/F18", "kills around the receipt transaction: no [DONE] without a receipt, and a committed receipt is charged")
    sub = {}
    for label, failpoint, model in (
        ("before_insert", "receipt.before_insert", MODEL_COUNT),
        ("after_insert_before_commit", "receipt.after_insert_before_commit", MODEL_COUNT),
        ("after_commit_before_done", "receipt.after_commit_before_done", MODEL_COUNT),
    ):
        org = stack.orgs[f"F07-{label}"]
        upstream_before = stack.counting.snapshot()
        stack.start_gateway("a", failpoints=f"{failpoint}=hold")
        stream = SSEStream("127.0.0.1", stack.ports["gateway_a"], "/v1/chat/completions", org.raw_key,
                           chat_payload(model=model), timeout=60).open()
        # Read whatever reached the client before the kill.
        try:
            stream.response.read(1)
        except Exception:  # noqa: BLE001
            pass
        marker = stack.kill_at(stack.process("gateway_a"), failpoint, timeout=60)
        try:
            # The process is gone, so this reads whatever the client already had
            # and then hits EOF: exactly what a real client would have seen.
            stream.read_events(deadline=time.time() + 10)
        except Exception:  # noqa: BLE001
            pass
        client_body = stream.result
        state = stack.org_state(org)
        upstream_after = stack.counting.snapshot()
        sub[label] = {
            "failpoint": marker,
            "client": client_body.as_dict(),
            "executions": state["executions"],
            "receipts": state["receipts"],
            "receipt_detail": stack.psql_rows(
                f"SELECT execution_id, status, charge_micro, coalesce(last_error,'') FROM usage_receipts "
                f"WHERE org_id = {org.org_id}"),
            "entries": state["entries"],
            "account": account_tuple(state),
            "audits": state["audits"],
            "upstream_started": upstream_after["started_streams"] - upstream_before["started_streams"],
        }
        stream.close()
    r.evidence = {"sub_cases": sub}

    r.check("before insert: no receipt and no [DONE]",
            not sub["before_insert"]["receipts"]
            and not sub["before_insert"]["client"]["has_done"],
            sub["before_insert"])
    r.check("before insert: the generation really happened (the upstream was called)",
            sub["before_insert"]["upstream_started"] == 1, sub["before_insert"])
    r.check("before insert: the execution stays recoverable with its reservation held",
            sub["before_insert"]["executions"][0]["status"] in ("reserved", "executing")
            and sub["before_insert"]["account"]["reserved_micro"] > 0,
            sub["before_insert"])

    r.check("after insert before COMMIT: the kill rolled the whole receipt back",
            not sub["after_insert_before_commit"]["receipts"], sub["after_insert_before_commit"])
    r.check("after insert before COMMIT: no [DONE] and no charge reached the client or the ledger",
            not sub["after_insert_before_commit"]["client"]["has_done"]
            and not entries_of({"entries": sub["after_insert_before_commit"]["entries"]}, "debit"),
            sub["after_insert_before_commit"])

    r.check("after COMMIT before [DONE]: the durable receipt exists",
            len(sub["after_commit_before_done"]["receipts"]) == 1
            and sub["after_commit_before_done"]["receipts"][0]["status"] in ("pending_dispatch", "dispatched", "settled"),
            sub["after_commit_before_done"])
    r.check("after COMMIT before [DONE]: the client did NOT receive a clean end",
            not sub["after_commit_before_done"]["client"]["has_done"],
            sub["after_commit_before_done"]["client"])
    r.check("after COMMIT before [DONE]: the execution is receipt_recorded or settled, never released",
            sub["after_commit_before_done"]["executions"][0]["status"] in ("receipt_recorded", "settled"),
            sub["after_commit_before_done"]["executions"])

    # F18: the receipt is the charge of record even though delivery failed. Let
    # the settlement pipeline finish the job and prove exactly one debit.
    org = stack.orgs["F07-after_commit_before_done"]
    stack.start_gateway("a")
    settled = wait_until(lambda: (lambda s: s if s["receipts"] and s["receipts"][0]["status"] == "settled"
                                  else None)(stack.org_state(org)), 120, "settlement of the committed receipt")
    r.evidence["after_commit_before_done_settled"] = {
        "receipts": settled["receipts"], "entries": settled["entries"],
        "account": account_tuple(settled),
    }
    r.check("F18: the receipt committed before the delivery failure is still charged exactly once",
            len(entries_of(settled, "debit")) == 1, settled["entries"])
    r.check("F18: the charge equals the receipt, not a refund or a release",
            len(entries_of(settled, "release")) == 1  # the reservation release that settlement posts
            and account_tuple(settled)["reserved_micro"] == 0,
            {"entries": settled["entries"], "account": account_tuple(settled)})
    return r


# --- F08 -------------------------------------------------------------------


def run_f08(stack):
    r = Result("F08", "a Redis outage fails the limiter closed, preserves receipts, and PG drives the redispatch")
    org = stack.orgs["F08"]
    stack.process("metering").stop()
    stream = request(stack, org, model=MODEL_OK, timeout=60)
    held = stack.org_state(org)
    r.evidence["stream_before_outage"] = stream.as_dict()
    r.check("a completed stream has its receipt in PostgreSQL before the outage",
            len(held["receipts"]) == 1 and held["receipts"][0]["status"] == "pending_dispatch",
            held["receipts"])

    before_outage_reserved = account_tuple(held)["reserved_micro"]
    stack.stop_dependency("redis")
    refused = request(stack, org, model=MODEL_OK, timeout=30)
    after_refusal = stack.org_state(org)
    new_executions = [e for e in after_refusal["executions"]
                      if e["execution_id"] not in {x["execution_id"] for x in held["executions"]}]
    r.evidence["during_outage"] = {
        "new_request": refused.as_dict(),
        "executions": after_refusal["executions"],
        "new_executions": new_executions,
        "receipts": after_refusal["receipts"],
        "account": account_tuple(after_refusal),
        "reserved_before_outage": before_outage_reserved,
    }
    r.check("new requests are refused while Redis is down (limiter fails closed, not open)",
            refused.status in (429, 500, 503), refused.as_dict())
    r.check("the refusal held no funds: every execution it created ended without a charge",
            all(e["status"] == "failed_no_charge" for e in new_executions)
            and account_tuple(after_refusal)["reserved_micro"] == before_outage_reserved,
            {"new_executions": new_executions, "account": account_tuple(after_refusal)})
    r.check("the in-flight receipt survived the outage untouched",
            len(after_refusal["receipts"]) == 1
            and after_refusal["receipts"][0]["status"] == "pending_dispatch",
            after_refusal["receipts"])
    if new_executions:
        r.note("F08-2: the billing admission runs OUTSIDE the limiter in the middleware chain, so a "
               "fail-closed limiter refusal still writes an execution row plus a reservation and a release "
               "entry per refused request. No money is held, but a Redis outage produces PostgreSQL churn "
               "on the money path.")

    stack.start_dependency("redis")
    stack.start_metering()
    converged = wait_until(lambda: (lambda s: s if s["receipts"] and s["receipts"][0]["status"] == "settled"
                                    else None)(stack.org_state(org)), 120, "PG-driven redispatch after the outage")
    r.evidence["after_outage"] = {
        "receipts": converged["receipts"], "entries": converged["entries"],
        "account": account_tuple(converged),
    }
    r.check("the dispatcher re-published from PostgreSQL after Redis returned",
            len(entries_of(converged, "debit")) == 1, converged["entries"])

    # Transport loss: destroy the consumer group and delete the stream. The
    # durable receipt is the fact; the group heals and the dispatcher re-offers.
    org2 = stack.orgs["F08-transport"]
    stream2 = request(stack, org2, model=MODEL_OK, timeout=60)
    wait_until(lambda: (lambda s: s if s["receipts"] and s["receipts"][0]["status"] in ("dispatched", "settled")
                        else None)(stack.org_state(org2)), 60, "first delivery of the transport-loss receipt")
    stack.redis_cli("XGROUP", "DESTROY", RECEIPT_STREAM, SETTLEMENT_GROUP)
    stack.redis_cli("DEL", RECEIPT_STREAM)
    lost = stack.org_state(org2)
    r.evidence["transport_loss"] = {
        "stream": stream2.as_dict(), "receipts": lost["receipts"],
        "entries": lost["entries"], "group_after_destroy": stack.redis_cli("EXISTS", RECEIPT_STREAM),
    }
    r.check("losing the transport did not lose the durable receipt",
            len(lost["receipts"]) == 1, lost["receipts"])
    healed = wait_until(lambda: (lambda s: s if s["receipts"] and s["receipts"][0]["status"] == "settled"
                                 else None)(stack.org_state(org2)), 120, "self-heal after transport loss")
    r.evidence["transport_loss_converged"] = {
        "receipts": healed["receipts"], "entries": healed["entries"],
        "group_exists": stack.redis_cli("EXISTS", RECEIPT_STREAM),
    }
    r.check("the group healed and the receipt settled exactly once after transport loss",
            len(entries_of(healed, "debit")) == 1 and stack.redis_cli("EXISTS", RECEIPT_STREAM) == "1",
            {"entries": healed["entries"], "stream_exists": stack.redis_cli("EXISTS", RECEIPT_STREAM)})
    return r


# --- F09 -------------------------------------------------------------------


def run_f09(stack):
    r = Result("F09", "a PostgreSQL outage at admission, finalize and settlement is refused or deferred, never faked")
    org = stack.orgs["F09"]

    # (a) admission. Two probes, because the request path has two PostgreSQL
    # consumers before the upstream and each must fail closed on its own:
    #   a1: the access guard (the first PG consumer) refuses the request;
    #   a2: with the guard disabled, the billing admission is the layer that
    #       must refuse with billing_unavailable.
    upstream_before = stack.counting.snapshot()
    stack.stop_dependency("postgres")
    refused = request(stack, org, model=MODEL_COUNT, timeout=30)
    upstream_after = stack.counting.snapshot()
    stack.start_dependency("postgres")
    admission_state = stack.org_state(org)
    r.evidence["admission_outage_guard"] = {
        "stream": refused.as_dict(),
        "executions": admission_state["executions"],
        "upstream_requests_delta": upstream_after["requests"] - upstream_before["requests"],
    }

    org_guard_off = stack.orgs["F09-guard-off"]
    stack.start_gateway("a", extra_env={"GATEWAY_ACCESS_GUARD": "off"})
    upstream_before = stack.counting.snapshot()
    stack.stop_dependency("postgres")
    refused_billing = request(stack, org_guard_off, model=MODEL_COUNT, timeout=30)
    upstream_after = stack.counting.snapshot()
    stack.start_dependency("postgres")
    billing_state = stack.org_state(org_guard_off)
    stack.start_gateway("a")
    r.evidence["admission_outage"] = {
        "stream": refused_billing.as_dict(),
        "executions": billing_state["executions"],
        "upstream_requests_delta": upstream_after["requests"] - upstream_before["requests"],
    }

    r.check("admission: the access guard refuses with a named 503 before anything else runs",
            refused.status == 503 and "unavailable" in refused.body, refused.as_dict())
    r.check("admission: with the guard off, the billing admission refuses with 503 billing_unavailable",
            refused_billing.status == 503 and "billing_unavailable" in refused_billing.body,
            refused_billing.as_dict())
    r.check("admission: neither refusal created an execution row or a reservation",
            not admission_state["executions"] and not billing_state["executions"],
            {"guard_on": admission_state["executions"], "guard_off": billing_state["executions"]})
    r.check("admission: the upstream was never called (fail closed before the money path)",
            r.evidence["admission_outage"]["upstream_requests_delta"] == 0
            and r.evidence["admission_outage_guard"]["upstream_requests_delta"] == 0,
            {"guard_on": r.evidence["admission_outage_guard"]["upstream_requests_delta"],
             "guard_off": r.evidence["admission_outage"]["upstream_requests_delta"]})

    # (b) finalize: hold admission, stop PG while the request is in flight, let
    # the hold expire so the upstream call and the receipt write both happen.
    org_b = stack.orgs["F09-finalize"]
    incomplete_before = stack.metrics("a").get("gateway_stream_incomplete_total", 0.0)
    # A short hold, so the runner can stop PostgreSQL while the admission is
    # frozen and the request then continues into the upstream call and the
    # receipt write on its own.
    stack.start_gateway("a", failpoints="admission.after_mark_executing=hold",
                        instance_id="gw-a", extra_env={"GATEWAY_FAILPOINT_HOLD": "6s"})
    holder = {}
    thread = threading.Thread(target=lambda: holder.update(
        {"stream": request(stack, org_b, model=MODEL_OK, timeout=60).as_dict()}))
    thread.start()
    stack.process("gateway_a").wait_failpoint("admission.after_mark_executing", timeout=30)
    stack.stop_dependency("postgres")
    thread.join(timeout=90)
    try:
        incomplete_after = stack.metrics("a").get("gateway_stream_incomplete_total", 0.0)
    except Exception:  # noqa: BLE001
        incomplete_after = None
    finalize_state = stack.org_state(org_b) if stack.start_dependency("postgres") is None else None
    r.evidence["finalize_outage"] = {
        "stream": holder.get("stream"),
        "incomplete_metric_delta": (incomplete_after - incomplete_before) if incomplete_after is not None else None,
        "executions": (finalize_state or {}).get("executions"),
        "receipts": (finalize_state or {}).get("receipts"),
    }
    r.check("finalize: the client did not receive a clean end while PostgreSQL was down",
            (holder.get("stream") or {}).get("has_done") is False, holder.get("stream"))
    r.check("finalize: the failure is visible as an incomplete stream",
            incomplete_after is not None and incomplete_after - incomplete_before >= 1,
            {"delta": (incomplete_after - incomplete_before) if incomplete_after is not None else None})
    r.check("finalize: no receipt was faked into existence",
            not (finalize_state or {}).get("receipts"), (finalize_state or {}).get("receipts"))
    r.check("finalize: the execution stays recoverable with its reservation held",
            (finalize_state or {}).get("executions")
            and (finalize_state or {})["executions"][0]["status"] in ("reserved", "executing"),
            (finalize_state or {}).get("executions"))

    # (c) settlement: a receipt that cannot be settled while PostgreSQL is down
    # must stay durable and be re-driven once PostgreSQL returns.
    org_c = stack.orgs["F09-settle"]
    stack.process("metering").stop()
    stream_c = request(stack, org_c, model=MODEL_OK, timeout=60)
    pending_before = stack.psql_rows(
        f"SELECT execution_id, status FROM usage_receipts WHERE org_id = {org_c.org_id}")
    stack.stop_dependency("postgres")
    # The settlement pipeline needs its ledger to exist at startup: with
    # PostgreSQL down it refuses to start at all rather than run without the
    # billing facts. That refusal is recorded as the fail-closed evidence.
    refused_start = stack.start_metering(wait_ready=False)
    time.sleep(3)
    refused_start_alive = refused_start.alive()
    refused_start_log = [line for line in refused_start.text().splitlines()
                         if "ledger" in line or "postgres" in line][-3:]
    stack.start_dependency("postgres")
    stack.start_metering()
    converged = wait_until(lambda: (lambda s: s if s["receipts"] and s["receipts"][0]["status"] == "settled"
                                    else None)(stack.org_state(org_c)), 150, "settlement after the PG outage")
    r.evidence["settle_outage"] = {
        "stream": stream_c.as_dict(),
        "receipt_before_outage": pending_before,
        "metering_refused_to_start_without_postgres": {
            "still_alive": refused_start_alive,
            "exit_code": refused_start.exit_code(),
            "log": refused_start_log,
        },
        "receipts": converged["receipts"],
        "entries": converged["entries"],
        "account": account_tuple(converged),
    }
    r.check("settle: the receipt was durable and unsettled before the outage",
            len(pending_before) == 1 and pending_before[0][1] == "pending_dispatch", pending_before)
    r.check("settle: the settlement pipeline refuses to run without its ledger instead of settling blind",
            not refused_start_alive, {"exit_code": refused_start.exit_code(), "log": refused_start_log})
    r.check("settle: exactly one receipt after convergence",
            len(converged["receipts"]) == 1, converged["receipts"])
    r.check("settle: it settled exactly once after PostgreSQL returned",
            len(entries_of(converged, "debit")) == 1, converged["entries"])
    return r


# --- F11 -------------------------------------------------------------------


def run_f11(stack):
    r = Result("F11", "two gateways racing for the last affordable balance admit exactly one request")
    org = stack.orgs["F11"]
    stack.start_gateway("b")
    # Measure the reservation the policy computes for this exact request shape.
    stack.set_balance(10_000_000, org.org_id)
    probe = request(stack, org, model=MODEL_OK, timeout=60)
    reserved = int(stack.psql_value(
        f"SELECT reserved_micro FROM billing_executions WHERE org_id = {org.org_id} "
        f"ORDER BY created_at DESC, execution_id DESC LIMIT 1"))
    r.evidence["probe"] = {"stream": probe.as_dict(), "reserved_micro": reserved}
    r.check("the probe measured a positive reservation", reserved > 0, reserved)

    before = stack.org_state(org)
    upstream_before = stack.counting.snapshot()
    stack.set_balance(reserved, org.org_id)
    results = {}
    barrier = threading.Barrier(2)

    def fire(which):
        barrier.wait()
        results[which] = request(stack, org, model=MODEL_COUNT, which=which, timeout=60).as_dict()

    threads = [threading.Thread(target=fire, args=(which,)) for which in ("a", "b")]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    after = stack.org_state(org)
    upstream_after = stack.counting.snapshot()
    admitted = sorted(k for k, v in results.items() if v["status"] == 200)
    refused = sorted(k for k, v in results.items() if v["status"] == 402)
    created = [e for e in after["executions"] if e["execution_id"] not in
               {x["execution_id"] for x in before["executions"]}]
    r.evidence["race"] = {
        "results": results, "admitted": admitted, "refused_402": refused,
        "funded_micro": reserved, "reserved_micro": reserved,
        "executions_created": created,
        "account": account_tuple(after),
        "upstream_requests_delta": upstream_after["requests"] - upstream_before["requests"],
    }
    r.check("exactly one gateway was admitted", len(admitted) == 1, results)
    r.check("the other gateway was refused with 402 before any upstream call",
            len(refused) == 1, results)
    r.check("exactly one execution row exists for the race",
            len(created) == 1, created)
    r.check("only the admitted request reached an upstream",
            upstream_after["requests"] - upstream_before["requests"] == 1,
            {"delta": upstream_after["requests"] - upstream_before["requests"]})
    created_ids = {e["execution_id"] for e in created}
    reservations = {k: v for k, v in kinds_per_execution(after, "reservation").items() if k in created_ids}
    debits = {k: v for k, v in kinds_per_execution(after, "debit").items() if k in created_ids}
    instant_reserved = account_tuple(after)["reserved_micro"]
    r.evidence["per_execution_entries"] = {"reservation": reservations, "debit": debits}
    r.check("the ledger holds exactly one reservation for the race, never two",
            list(reservations.values()) == [1] and all(v <= 1 for v in debits.values()),
            {"reservation": reservations, "debit": debits})
    r.check("the account never held more than the single affordable reservation",
            instant_reserved in (0, reserved), {"reserved_micro": instant_reserved, "one_reservation": reserved})
    return r


# --- F13 -------------------------------------------------------------------


def run_f13(stack):
    r = Result("F13", "same event id with a different payload and the same idempotency key with a different amount are conflicts")
    org = stack.orgs["F13"]
    stream = request(stack, org, model=MODEL_OK, timeout=60)
    receipts = stack.psql_rows(
        f"SELECT execution_id, event_id FROM usage_receipts WHERE org_id = {org.org_id}")
    r.evidence["stream"] = stream.as_dict()
    r.check("the scenario produced exactly one receipt to conflict with", len(receipts) == 1, receipts)
    if len(receipts) != 1:
        return r
    execution_id, event_id = receipts[0]

    columns = ("execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens, "
               "total_tokens, price_version_id, charge_micro, occurred_at, digest, status, "
               "dispatch_after, claim_until, attempts, last_error, created_at, settled_at")
    duplicate_sql = f"""
    INSERT INTO usage_receipts ({columns})
    SELECT 'exec_f13_duplicate', event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
           total_tokens, price_version_id, charge_micro, occurred_at,
           repeat('a', 64), status, dispatch_after, claim_until, attempts, last_error, created_at, settled_at
    FROM usage_receipts WHERE execution_id = '{execution_id}';
    """
    ok, output = stack.psql_try(duplicate_sql)
    count = int(stack.psql_value(
        f"SELECT count(*) FROM usage_receipts WHERE org_id = {org.org_id}"))
    r.evidence["duplicate_event_insert"] = {
        "sql": " ".join(duplicate_sql.split()),
        "succeeded": ok,
        "database_response": output,
        "receipts_for_org": count,
        "unique_constraint": stack.psql_value(
            "SELECT conname FROM pg_constraint WHERE conname = 'usage_receipts_event_id_key'"),
    }
    r.check("the database refuses a second receipt that reuses a billed event id",
            not ok, output)
    r.check("the refusal names the event-id uniqueness rule, not a silent DO NOTHING",
            "usage_receipts_event_id_key" in output or "duplicate key" in output, output)
    r.check("the refused insert moved nothing", count == 1, count)

    # Same execution id, different payload: the store-level contract.
    conflict = stack.psql_rows(
        "SELECT execution_id, status, reserved_micro FROM billing_executions "
        f"WHERE org_id = {org.org_id}")
    r.evidence["same_execution_id_different_payload"] = {
        "covered_by": "internal/billing/execution_pg_integration_test.go:"
                      "TestReserveIsIdempotentPerExecutionID (same execution id, changed payload -> "
                      "ErrIdempotencyConflict, no second reservation)",
        "rows": conflict,
        "note": "an HTTP client cannot choose its execution id (the guard mints a random one per "
                "request), so this half of F13 is verified at the store level where the identity is "
                "an input.",
    }
    r.evidence["same_adjustment_key_different_amount"] = {
        "covered_by": "internal/billing/adjustment_conflict_pg_integration_test.go "
                      "(same business id + kind, different amount -> ErrIdempotencyConflict, one entry)",
    }
    return r


# --- F19 -------------------------------------------------------------------


def run_f19(stack):
    r = Result("F19", "a COMMIT whose response is lost is resolved by business id: no duplicate reservation, receipt or debit")
    sub = {}

    # (a) admission COMMIT response lost
    org = stack.orgs["F19-admission"]
    upstream_before = stack.counting.snapshot()
    stack.start_gateway("a", failpoints="admission.commit_response_lost=error")
    stream = request(stack, org, model=MODEL_COUNT, timeout=60)
    state = stack.org_state(org)
    upstream_after = stack.counting.snapshot()
    sub["admission"] = {
        "stream": stream.as_dict(), "executions": state["executions"],
        "entries": state["entries"], "receipts": state["receipts"],
        "account": account_tuple(state),
        "upstream_requests_delta": upstream_after["requests"] - upstream_before["requests"],
    }

    # (b) receipt COMMIT response lost
    org_b = stack.orgs["F19-receipt"]
    stack.start_gateway("a", failpoints="receipt.commit_response_lost=error")
    stream_b = request(stack, org_b, model=MODEL_OK, timeout=60)
    state_b = stack.org_state(org_b)
    sub["receipt"] = {
        "stream": stream_b.as_dict(), "executions": state_b["executions"],
        "receipts": state_b["receipts"], "entries": state_b["entries"],
        "account": account_tuple(state_b),
        "resolution_log": [line for line in stack.process("gateway_a").text().splitlines()
                           if "COMMIT outcome was unknown" in line],
    }

    # (c) settlement COMMIT response lost
    org_c = stack.orgs["F19-settlement"]
    stack.start_gateway("a")
    stack.start_metering(failpoints="settlement.commit_response_lost=error")
    stream_c = request(stack, org_c, model=MODEL_OK, timeout=60)
    converged = wait_until(lambda: (lambda s: s if s["receipts"] and s["receipts"][0]["status"] == "settled"
                                    else None)(stack.org_state(org_c)), 120, "settlement convergence")
    sub["settlement"] = {
        "stream": stream_c.as_dict(), "receipts": converged["receipts"],
        "entries": converged["entries"], "account": account_tuple(converged),
        "resolution_log": [line for line in stack.process("metering").text().splitlines()
                           if "unknown" in line.lower() or "re-query" in line.lower()][-5:],
    }
    stack.start_metering()
    r.evidence = sub

    r.check("admission: the lost COMMIT response is a refusal (fail closed), not a fake success",
            sub["admission"]["stream"]["status"] == 503, sub["admission"]["stream"])
    r.check("admission: the committed reservation exists exactly once",
            len(sub["admission"]["executions"]) == 1
            and len(entries_of({"entries": sub["admission"]["entries"]}, "reservation")) == 1,
            sub["admission"])
    r.check("admission: the refused request never reached an upstream",
            sub["admission"]["upstream_requests_delta"] == 0, sub["admission"])
    r.check("admission: nothing was released or charged for the unknown-commit execution",
            not entries_of({"entries": sub["admission"]["entries"]}, "release")
            and not entries_of({"entries": sub["admission"]["entries"]}, "debit"),
            sub["admission"]["entries"])

    r.check("receipt: the re-query resolved the unknown COMMIT and released [DONE]",
            sub["receipt"]["stream"]["has_done"], sub["receipt"]["stream"])
    r.check("receipt: exactly one receipt exists (no second charge)",
            len(sub["receipt"]["receipts"]) == 1, sub["receipt"]["receipts"])
    r.check("receipt: the gateway logged the re-query resolution",
            any("COMMIT outcome was unknown" in line for line in sub["receipt"]["resolution_log"]),
            sub["receipt"]["resolution_log"])

    r.check("settlement: the settled receipt converged despite the lost COMMIT response",
            sub["settlement"]["receipts"] and sub["settlement"]["receipts"][0]["status"] == "settled",
            sub["settlement"]["receipts"])
    r.check("settlement: exactly one debit was posted",
            len(entries_of({"entries": sub["settlement"]["entries"]}, "debit")) == 1,
            sub["settlement"]["entries"])
    r.check("settlement: the account projection returned to zero reserved",
            sub["settlement"]["account"]["reserved_micro"] == 0,
            sub["settlement"]["account"])
    return r


# --- resource and robustness curves ----------------------------------------


RESOURCE_METRICS = (
    "process_resident_memory_bytes",
    "go_memstats_heap_alloc_bytes",
    "go_memstats_heap_inuse_bytes",
    "go_goroutines",
    "process_open_fds",
    "gateway_active_streams",
    "gateway_active_streams_rejected_total",
    "gateway_stream_incomplete_total",
)


class Sampler:
    """Reads the gateway's own metrics on a fixed interval while a phase runs."""

    def __init__(self, stack, which="a", interval=0.5):
        self.stack = stack
        self.which = which
        self.interval = interval
        self.samples = []
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, daemon=True)

    def _loop(self):
        while not self._stop.is_set():
            try:
                metrics = self.stack.metrics(self.which)
                self.samples.append({"at": round(time.time() - self.started_at, 3),
                                     **{name: metrics.get(name) for name in RESOURCE_METRICS}})
            except Exception as err:  # noqa: BLE001
                self.samples.append({"at": round(time.time() - self.started_at, 3), "error": str(err)})
            self._stop.wait(self.interval)

    def __enter__(self):
        self.started_at = time.time()
        self._thread.start()
        return self

    def __exit__(self, *exc):
        self._stop.set()
        self._thread.join(timeout=10)
        return False

    def shape(self):
        """First-half vs second-half mean per metric: the unbounded-growth test."""
        out = {}
        for name in RESOURCE_METRICS:
            values = [s[name] for s in self.samples if s.get(name) is not None]
            if not values:
                out[name] = None
                continue
            half = max(1, len(values) // 2)
            first = sum(values[:half]) / len(values[:half])
            second = sum(values[half:]) / len(values[half:]) if values[half:] else first
            out[name] = {
                "n": len(values),
                "min": round(min(values), 1),
                "max": round(max(values), 1),
                "first": round(values[0], 1),
                "last": round(values[-1], 1),
                "first_half_mean": round(first, 1),
                "second_half_mean": round(second, 1),
                "growth_ratio": None if first == 0 else round(second / first, 3),
            }
        return out


def run_res(stack):
    r = Result("RES", "resource curves: slow and stalled clients, oversized events, per-instance stream caps")
    org = stack.orgs["RES"]

    def idle_sample(seconds=3.0, interval=0.5):
        with Sampler(stack, interval=interval) as sampler:
            time.sleep(seconds)
        return sampler.shape()

    r.evidence["baseline"] = idle_sample()

    # --- slow clients at 1 KiB/s ------------------------------------------
    slow_streams = []
    for _ in range(4):
        stream = SSEStream("127.0.0.1", stack.ports["gateway_a"], "/v1/chat/completions", org.raw_key,
                           chat_payload(model=MODEL_COUNT, x_mock_tokens=400,
                                        x_mock_token_interval_ms=20), timeout=120).open()
        slow_streams.append(stream)
    threads = [threading.Thread(target=lambda s=s: s.read_slow(1024, budget_seconds=20), daemon=True)
               for s in slow_streams]
    with Sampler(stack) as sampler:
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=40)
    r.evidence["slow_clients_1kib"] = sampler.shape()
    for stream in slow_streams:
        stream.cancel()
    reclaimed = wait_until(lambda: (lambda m: m if m.get("gateway_active_streams", 0.0) == 0 else None)(
        stack.metrics("a")), 20, "slow-client streams to be reclaimed")
    r.evidence["slow_clients_reclaimed"] = {"active_streams": reclaimed.get("gateway_active_streams")}

    # --- a fully stalled client with a large stream ------------------------
    stalled = SSEStream("127.0.0.1", stack.ports["gateway_a"], "/v1/chat/completions", org.raw_key,
                        chat_payload(model=MODEL_COUNT, x_mock_tokens=200,
                                     x_mock_event_line_bytes=65536,
                                     x_mock_token_interval_ms=0), timeout=180).open()
    stalled.read_then_stall(prime_seconds=1.0)
    with Sampler(stack) as sampler:
        time.sleep(25)
    r.evidence["stalled_client_64kib_events"] = sampler.shape()
    stalled.cancel()
    wait_until(lambda: (lambda m: m if m.get("gateway_active_streams", 0.0) == 0 else None)(
        stack.metrics("a")), 30, "stalled stream to be reclaimed")

    # --- oversized single events ------------------------------------------
    # MODEL_OK is served by cmd/upstream-mock, which honours
    # x_mock_event_line_bytes; the counting upstream used elsewhere in this file
    # does not pad events.
    oversized = {}
    for label, line_bytes in (("1MiB", 1 << 20), ("2MiB_over_the_1MiB_line_cap", 2 << 20)):
        before = stack.metrics("a").get("gateway_stream_incomplete_total", 0.0)
        state_before = stack.org_state(org)
        stream = SSEStream("127.0.0.1", stack.ports["gateway_a"], "/v1/chat/completions", org.raw_key,
                           chat_payload(model=MODEL_OK, x_mock_tokens=2,
                                        x_mock_event_line_bytes=line_bytes), timeout=120).open()
        result = stream.read_events(deadline=time.time() + 60, chunk_size=1 << 16)
        stream.close()
        after = stack.metrics("a").get("gateway_stream_incomplete_total", 0.0)
        state_after = stack.org_state(org)
        created = [e for e in state_after["executions"]
                   if e["execution_id"] not in {x["execution_id"] for x in state_before["executions"]}]
        oversized[label] = {
            "requested_line_bytes": line_bytes,
            "stream": result.as_dict(),
            "incomplete_delta": after - before,
            "executions_created": created,
            "receipts_created": [x for x in state_after["receipts"]
                                 if x["execution_id"] in {e["execution_id"] for e in created}],
        }
    r.evidence["oversized_events"] = oversized
    r.check("a 1 MiB event line is forwarded whole and the stream still completes",
            oversized["1MiB"]["stream"]["has_done"]
            and oversized["1MiB"]["stream"]["bytes"] > (1 << 20),
            {"has_done": oversized["1MiB"]["stream"]["has_done"],
             "bytes": oversized["1MiB"]["stream"]["bytes"]})
    r.check("an event above the 1 MiB line cap is refused with a bounded, classified error "
            "(no truncated stream, no [DONE], no charge)",
            not oversized["2MiB_over_the_1MiB_line_cap"]["stream"]["has_done"]
            and oversized["2MiB_over_the_1MiB_line_cap"]["stream"]["status"] in (502, 503)
            and not oversized["2MiB_over_the_1MiB_line_cap"]["receipts_created"],
            oversized["2MiB_over_the_1MiB_line_cap"])

    # --- per-instance stream caps -----------------------------------------
    # Two documented, deliberately per-instance caps (T09): the global active
    # stream cap inside the proxy and the per-organization cap in the limiter.
    # They are measured separately because the organization cap sits INSIDE the
    # global one and answers 429 first when both are in reach.
    def measure_cap(global_cap, org_cap, attempts, label):
        stack.start_gateway("a", extra_env={
            "GATEWAY_MAX_ACTIVE_STREAMS": str(global_cap),
            "GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG": str(org_cap),
        })
        held = []
        statuses = []
        with Sampler(stack, interval=1.0) as sampler:
            for _ in range(attempts):
                stream = SSEStream("127.0.0.1", stack.ports["gateway_a"], "/v1/chat/completions",
                                   org.raw_key,
                                   chat_payload(model=MODEL_COUNT, x_mock_tokens=200,
                                                x_mock_token_interval_ms=200), timeout=180)
                try:
                    stream.open()
                    statuses.append(stream.result.status)
                    held.append(stream)
                except Exception as err:  # noqa: BLE001 - a refusal is data
                    statuses.append(f"{type(err).__name__}: {err}")
            sampled = sampler.shape()
        counted = {}
        for status in statuses:
            key = str(status)
            counted[key] = counted.get(key, 0) + 1
        for stream in held:
            stream.cancel()
        recovered = wait_until(lambda: (lambda m: m if m.get("gateway_active_streams", 0.0) == 0 else None)(
            stack.metrics("a")), 30, f"{label} streams to be reclaimed")
        stack.start_gateway("a")
        return {"label": label, "global_cap": global_cap, "org_cap": org_cap,
                "attempts": attempts, "statuses": counted, "sampled": sampled,
                "recovered": {"active_streams": recovered.get("gateway_active_streams"),
                              "goroutines": recovered.get("go_goroutines"),
                              "rss": recovered.get("process_resident_memory_bytes"),
                              "open_fds": recovered.get("process_open_fds")}}

    global_cap = measure_cap(16, 0, 20, "global")
    r.evidence["global_stream_cap"] = global_cap
    r.evidence["after_cap_release"] = global_cap["recovered"]
    org_cap = measure_cap(0, 4, 6, "per_organization")
    r.evidence["org_stream_cap"] = org_cap

    r.check("the per-instance global stream cap is enforced with 503",
            global_cap["statuses"].get("200") == 16 and global_cap["statuses"].get("503", 0) >= 1,
            global_cap["statuses"])
    r.check("the per-organization stream cap is enforced with 429 and does not leak into the global cap",
            org_cap["statuses"].get("200") == 4 and org_cap["statuses"].get("429", 0) >= 1,
            org_cap["statuses"])
    r.check("every held stream is reclaimed after the clients go away",
            r.evidence["after_cap_release"]["active_streams"] == 0, r.evidence["after_cap_release"])
    r.check("no resource grows without bound while the clients stall",
            all((shape or {}).get("growth_ratio") is None or (shape or {})["growth_ratio"] < 2.0
                for name, shape in r.evidence["stalled_client_64kib_events"].items()
                if name in ("process_resident_memory_bytes", "go_memstats_heap_alloc_bytes", "go_goroutines")),
            {name: r.evidence["stalled_client_64kib_events"][name]
             for name in ("process_resident_memory_bytes", "go_memstats_heap_alloc_bytes",
                          "go_goroutines", "process_open_fds")})
    r.check("no file descriptor leak across the phases",
            (r.evidence["after_cap_release"].get("open_fds") or 0)
            <= (r.evidence["baseline"].get("process_open_fds") or {}).get("max", 0) + 32,
            {"baseline": r.evidence["baseline"].get("process_open_fds"),
             "after": r.evidence["after_cap_release"].get("open_fds")})
    r.partial("the curves are a 20-25s observation on one macOS host, not the 300s Linux sampling "
              "window of BENCHMARK §3.2: they show the SHAPE (no unbounded growth, reclamation after "
              "release) and are not capacity numbers.")
    return r


# ---------------------------------------------------------------------------
# registry, gates and main
# ---------------------------------------------------------------------------

SCENARIOS = {
    "F01": (run_f01, ["F01"]),
    "F02": (run_f02, ["F02", "F02-uncertain", "F02-late"]),
    "F03": (run_f03, ["F03"]),
    "F04": (run_f04, ["F04"]),
    "F05": (run_f05, ["F05"]),
    "F06": (run_f06, ["F06-before_reserve", "F06-after_reserve", "F06-after_mark_executing"]),
    "R01": (run_r01, ["R01"]),
    "F07": (run_f07, ["F07-before_insert", "F07-after_insert_before_commit", "F07-after_commit_before_done"]),
    "F08": (run_f08, ["F08", "F08-transport"]),
    "F09": (run_f09, ["F09", "F09-guard-off", "F09-finalize", "F09-settle"]),
    "F11": (run_f11, ["F11"]),
    "F13": (run_f13, ["F13"]),
    "F19": (run_f19, ["F19-admission", "F19-receipt", "F19-settlement"]),
    "RES": (run_res, ["RES"]),
}

# Scenarios the acceptance spec lists but this half of T10 does not own, or that
# this runner deliberately does not execute. They are reported by name so the
# final table is complete instead of silently short.
NOT_EXECUTED = {
    "F10": "control-plane loss/restart/heartbeat lease: T09's matrix (scripts/two-gateway-experiment.py check_e).",
    "F12": "price change / key revoke / member removal during a long stream: T09's revocation matrix.",
    "F14": "pending backlog past 32 pages and a long failing prefix: T07C/T09 reclaim fairness evidence.",
    "F15": "restore a PostgreSQL backup into an isolated environment: the backup/restore half of T10.",
    "F16": "no token / wrong org / id enumeration / role forgery / CSRF: the security half of T10.",
    "F17": "legacy spool full disk / corrupt line / kill -9: the compatibility path, not the billing fact.",
    "F20": "reservation recovery vs a late finalize: covered by the store-level guards "
           "(internal/billing/recovery_pg_integration_test.go, TestRecoveryBlocksOnAnUncommittedFinalizeWindow) "
           "and by the late-usage anomaly path; not driven end to end by this runner.",
}


def run_build_gates(stack):
    """Deliverable-1 gates: the two builds really differ, and production refuses.

    These run before any scenario because they are the property that makes every
    other result trustworthy: if a production binary could arm a failpoint, none
    of the kill windows below would mean anything.
    """
    gates = {}
    tagged = stack.build_dir / "gateway"          # built with -tags failpoint
    untagged = stack.build_dir / "gateway-notag"
    build = run(["go", "build", "-o", str(untagged), "./cmd/gateway"], cwd=ROOT, timeout=600)
    gates["untagged_build"] = {"ok": build.returncode == 0, "stderr": (build.stderr or "")[-2000:]}
    if build.returncode != 0:
        return gates

    env = dict(os.environ)
    env.update({"GATEWAY_PROFILE": "prod", "GATEWAY_FAILPOINTS": "admission.after_reserve=kill"})

    def launch(binary, name):
        proc = run([str(binary)], env=env, cwd=ROOT, timeout=60)
        output = (proc.stdout or "") + (proc.stderr or "")
        return {"exit_code": proc.returncode, "output_tail": output[-2000:],
                "mentions_failpoints": "GATEWAY_FAILPOINTS" in output,
                "started_serving": "gateway listening" in output}

    gates["tagged_prod_refusal"] = launch(tagged, "tagged")
    gates["tagged_prod_refusal"]["ok"] = (
        gates["tagged_prod_refusal"]["exit_code"] != 0
        and gates["tagged_prod_refusal"]["mentions_failpoints"])
    gates["untagged_prod_refusal"] = launch(untagged, "untagged")
    gates["untagged_prod_refusal"]["ok"] = (
        gates["untagged_prod_refusal"]["exit_code"] != 0
        and gates["untagged_prod_refusal"]["mentions_failpoints"])

    # And the tagged binary must really arm when the profile is not production.
    demo_env = dict(os.environ)
    demo_env.update({
        "GATEWAY_PROFILE": "demo",
        "GATEWAY_FAILPOINTS": "admission.before_reserve=hold",
        "GATEWAY_ADDR": f"127.0.0.1:{probe_port()}",
        "GATEWAY_DIAGNOSTICS_ADDR": f"127.0.0.1:{probe_port()}",
    })
    proc = subprocess.Popen([str(tagged)], env=demo_env, cwd=ROOT, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True)
    time.sleep(3)
    proc.terminate()
    try:
        out, _ = proc.communicate(timeout=15)
    except subprocess.TimeoutExpired:
        proc.kill()
        out, _ = proc.communicate()
    gates["tagged_demo_arms"] = {
        "armed": "failpoints ARMED in this process" in out,
        "output_tail": out[-2000:],
    }
    gates["tagged_demo_arms"]["ok"] = gates["tagged_demo_arms"]["armed"]
    return gates


def ensure_baseline(stack):
    """Every scenario starts from a live, failpoint-free baseline.

    A scenario that injects a dependency outage may fail halfway through, so the
    restore is unconditional and runs before anything else: an outage must never
    leak into the next scenario's measurements.
    """
    stack.ensure_dependencies()
    gateway = stack.process("gateway_a")
    if gateway is None or not gateway.alive():
        stack.start_gateway("a")
    if stack.process("gateway_a").failpoints():
        stack.start_gateway("a")
    metering = stack.process("metering")
    if metering is None or not metering.alive():
        stack.start_metering()
    if stack.process("metering").failpoints():
        stack.start_metering()


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--output", default=str(ROOT / "docs/verification/2026-10-01/t10-failures"),
                        help="evidence directory")
    parser.add_argument("--scenarios", default=",".join(SCENARIOS),
                        help="comma-separated scenario ids (default: all implemented)")
    parser.add_argument("--list", action="store_true", help="list the scenarios and exit")
    parser.add_argument("--keep", action="store_true", help="do not tear the stack down (debugging)")
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    if args.list:
        for sid, (_, orgs) in SCENARIOS.items():
            print(f"{sid}\t{SCENARIOS[sid][0].__doc__ or ''}")
        print("\nDeclared but not executed by this runner:")
        for sid, why in NOT_EXECUTED.items():
            print(f"{sid}\t{why}")
        return 0

    selected = [s.strip() for s in args.scenarios.split(",") if s.strip()]
    unknown = [s for s in selected if s not in SCENARIOS]
    if unknown:
        raise SystemExit(f"unknown scenario(s) {unknown}; known: {list(SCENARIOS)}")

    output = pathlib.Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    stack = Stack(args, output)
    report = {
        "task": "T10 failure matrix",
        "started_at": now_utc(),
        "run_id": stack.run_id,
        "project": stack.project,
        "scenarios_requested": selected,
        "not_executed": NOT_EXECUTED,
        "gates": {},
        "scenarios": {},
        "environment": stack.evidence,
    }
    status = 1
    try:
        log(f"building failpoint-tagged binaries into {stack.build_dir or '<tmp>'}")
        stack.build()
        report["build"] = {"binaries": sorted(p.name for p in stack.build_dir.iterdir())}
        log("checking the failpoint build gates")
        report["gates"] = run_build_gates(stack)
        failed_gates = [name for name, gate in report["gates"].items() if not gate.get("ok")]
        if failed_gates:
            log(f"FAIL: failpoint build gates did not pass: {failed_gates}")

        log("starting the private postgres/redis project")
        stack.start_dependencies()
        stack.apply_migrations()
        for sid in selected:
            for org_name in SCENARIOS[sid][1]:
                stack.seed_org(org_name)
        stack.seed_prices()
        log("starting control, mock upstreams and the shared metering/gateway baseline")
        stack.start_control()
        stack.start_mocks()
        config = stack.publish_config()
        report["control_config"] = config
        stack.start_metering()
        stack.start_gateway("a")
        stack.evidence["gateway_armed_log"] = [
            line for line in stack.process("gateway_a").text().splitlines() if "failpoint" in line.lower()]

        for sid in selected:
            runner = SCENARIOS[sid][0]
            log(f"=== {sid} ===")
            ensure_baseline(stack)
            started = time.time()
            try:
                result = runner(stack).as_dict()
            except Exception as err:  # noqa: BLE001 - a failing scenario must not kill the run
                partial = CURRENT_RESULT if (CURRENT_RESULT and CURRENT_RESULT.sid == sid) else None
                result = partial.as_dict() if partial is not None else {
                    "id": sid, "title": "", "status": "ERROR",
                    "assertions": [], "notes": [], "evidence": {}}
                result["status"] = "ERROR"
                result["error"] = f"{type(err).__name__}: {err}"
                result["traceback"] = traceback.format_exc()
            result["seconds"] = round(time.time() - started, 2)
            report["scenarios"][sid] = result
            log(f"=== {sid}: {result['status']} ({result['seconds']}s) ===")
            stack.write_json(f"scenarios/{sid}.json", result)
    finally:
        if not args.keep:
            teardown = stack.teardown()
            report["teardown"] = teardown
        report["finished_at"] = now_utc()

    results = report["scenarios"]
    report["summary"] = {
        "executed": len(results),
        "pass": sum(1 for r in results.values() if r["status"] == "PASS"),
        "partial": sum(1 for r in results.values() if r["status"] == "PARTIAL"),
        "fail": sum(1 for r in results.values() if r["status"] == "FAIL"),
        "error": sum(1 for r in results.values() if r["status"] == "ERROR"),
    }
    stack.write_json("summary.json", report)
    print(json.dumps({"run_id": stack.run_id, "output": str(output), "summary": report["summary"],
                      "statuses": {sid: r["status"] for sid, r in results.items()},
                      "gates": {name: gate.get("ok") for name, gate in report["gates"].items()}},
                     indent=2, default=str))
    status = 0 if report["summary"]["fail"] == 0 and report["summary"]["error"] == 0 else 1
    return status


if __name__ == "__main__":
    sys.exit(main())
