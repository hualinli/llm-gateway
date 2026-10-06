#!/usr/bin/env python3
"""T11 capacity/economy runner: isolated Compose project, capacity ladders, run-scoped reconciliation.

Why this runner exists next to scripts/isolated-baseline.py (T01A):

  * T01A measures *low-load* direct-vs-gateway with the Demo profile, where
    GATEWAY_BILLING/GATEWAY_ACCESS_GUARD are "off". T11 must measure the
    *billable* path: admission + durable receipt + settlement against
    PostgreSQL, one wallet vs ten wallets, one Gateway vs two.
  * Every round therefore runs with GATEWAY_BILLING=pg and
    GATEWAY_ACCESS_GUARD=pg against per-run organizations, keys, price version
    and funded wallets that this runner seeds itself.
  * A round is only usable for a capacity conclusion when the *generator* was
    not the limiter: drops > 0.1% of scheduled arrivals, or arrivals below 99%
    of the plan, mark the round invalid (cmd/loadtest publishes the same rule;
    this runner re-evaluates it across every generator of a round).

Isolation model (same principles as T01A, extended):

  * Compose project name   -> `llm-gw-cap-<run_id>`; namespaces containers,
    networks and named volumes, so `down -v` can only remove this run's state.
  * Host ports             -> one socket()-probed free loopback port per
    published port, applied with `ports: !override` (Compose >= 2.24.4).
  * Images                 -> pinned to `llm-gw-cap-<commit8>-<service>`, so a
    later `--no-build` run reuses exactly the images built from that commit.
  * Secrets                -> per-run random CONTROL_ADMIN_TOKEN /
    METERING_ADMIN_TOKEN, per-run random API keys; only prefixes are ever
    printed or written to the report, full values live in memory and in the
    0600 override file.
  * Limiter                -> GATEWAY_LIMITER_RATE/BURST=100000 for the
    capacity rounds: the Demo default (20 rps) would otherwise make every
    ladder a measurement of the token bucket. This is an explicit deviation,
    recorded in parameters.json and in the report.

The runner is fail-soft per round: a broken round is recorded as a failure and
the run continues. With --reuse-rounds, a round whose raw record already exists
in the output directory is reused instead of re-run.
"""

import argparse
import hashlib
import json
import math
import os
import pathlib
import platform
import random
import re
import secrets
import shlex
import socket
import string
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASE_COMPOSE_NAME = "docker-compose.yml"
PROJECT_PREFIX = "llm-gw-cap-"
PROFILE_MOCKS = "demo-mocks"
PROFILE_MULTI = "multi-instance"
MODEL = "default"
DSN_IN_CONTAINER = "postgres://gateway:gateway@postgres:5432/llm_gateway?sslmode=disable"
PRICE_EFFECTIVE_AT = "2026-01-01 00:00:00+00"

# Stream shape durations (cmd/loadtest/README.md),
# used only to size closed-loop -n and open-loop concurrency slots.
SHAPE_SECONDS = {"s": 0.125, "m": 2.08, "l": 10.28}
# A 50 KiB M stream at 1 KiB/s takes ~51s; at 10 KiB/s ~5s.
SLOW_SECONDS = {"10k": 5.5, "1k": 51.0, "stop": 32.0}

# (service, container_port, role): one probed free host port per entry.
PUBLISHED_PORTS = (
    ("control", 50051, "control_grpc"),
    ("control", 50052, "control_http"),
    ("metering", 8091, "metering"),
    ("console", 8092, "console"),
    ("gateway", 8090, "gateway"),
    ("gateway", 8094, "gateway_diag"),
    ("gateway-b", 8093, "gateway_b"),
    ("gateway-b", 8094, "gateway_b_diag"),
    ("mock-primary", 18080, "mock_primary"),
    ("mock-backup", 18081, "mock_backup"),
    ("postgres", 5432, "postgres"),
    ("redis", 6379, "redis"),
)

SERIES_UNLABELED = (
    "process_cpu_seconds_total",
    "process_resident_memory_bytes",
    "process_virtual_memory_bytes",
    "process_open_fds",
    "process_max_fds",
    "go_memstats_heap_inuse_bytes",
    "go_memstats_heap_alloc_bytes",
    "go_memstats_heap_objects",
    "go_memstats_heap_released_bytes",
    "go_memstats_sys_bytes",
    "go_goroutines",
    "go_threads",
    "go_gc_duration_seconds_count",
    "gateway_active_streams",
)
SERIES_LABELED_PREFIXES = (
    "gateway_access_guard_total",
    "gateway_requests_total",
    "gateway_active_streams_rejected_total",
    "limiter_limited_total",
    "limiter_allowed_total",
    "gateway_stream_incomplete_total",
    "metering_receipt_settled_total",
    "metering_receipt_dispatch_total",
    "metering_receipt_backlog",
    "metering_receipt_oldest_unsettled_age_seconds",
    "metering_receipt_exception_total",
    "metering_exception_backlog",
    "metering_usage_missing_total",
    "metering_usage_invalid_total",
    "metering_settlement_total",
    "metering_billing_review_required_open",
    "metering_billing_recovery_owner_unknown_open",
)

SAMPLE_INTERVAL = 1.0
DOCKER_STATS_INTERVAL = 3.0
PG_INTERVAL = 2.0
REDIS_INTERVAL = 2.0
RECONCILE_TIMEOUT = 240.0
MAX_COMMITTED_BYTES = 400_000
# These are small text summaries, not raw sample series: they stay in the
# repository copy whatever their size (index.json carries every round).
ALWAYS_COMMITTED = {"index.json", "summary.json", "reconciliation.json", "parameters.json",
                    "environment.json", "timeline.json", "plan.json", "report-tables.md",
                    "README.md", "compose-override.yml"}

# The Dockerfile runs `RUN go mod download` before `COPY . .`, so a build host
# that cannot reach proxy.golang.org fails there. When that happens the runner
# retries once with a generated copy of the same Dockerfile that only adds an
# explicit GOPROXY; the retry is recorded in environment.json (build_recipe).
FALLBACK_GOPROXY = "https://goproxy.cn,direct"
# `docker info --format {{.Architecture}}` says x86_64/aarch64 where Go says
# amd64/arm64; the pprof symbolization binary must be built for the container.
GOARCH_BY_DOCKER = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64",
                    "armv7l": "arm", "armhf": "arm", "i386": "386", "i686": "386"}
DEFAULT_BUILD_SERVICES = ("control", "gateway", "gateway-b", "metering", "console", "migrate",
                          "mock-backup", "mock-primary")


def module_proxy_failure(log_path):
    """True when the compose build log shows `go mod download` failing on network."""
    try:
        text = pathlib.Path(log_path).read_text(errors="replace")
    except OSError:
        return False
    if "go mod download" not in text:
        return False
    markers = ("i/o timeout", "dial tcp", "proxy.golang.org", "no such host",
               "connection refused", "TLS handshake timeout", "request canceled")
    return any(marker in text for marker in markers)


def write_goproxy_dockerfile(source, destination, goproxy):
    """Copy the repository Dockerfile, adding one ENV line for a reachable module proxy."""
    text = pathlib.Path(source).read_text()
    marker = "WORKDIR /src"
    if marker not in text:
        raise RuntimeError(f"{source} has no {marker!r} line to anchor the GOPROXY fallback")
    pathlib.Path(destination).write_text(text.replace(marker, f"ENV GOPROXY={goproxy}\n{marker}", 1))
    return destination


def log(message):
    print(f"[cap] {message}", flush=True)


def now_utc():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def run(args, **kwargs):
    kwargs.setdefault("cwd", ROOT)
    return subprocess.run(args, **kwargs)


def capture(args, timeout=180):
    try:
        proc = subprocess.run(args, cwd=ROOT, capture_output=True, text=True,
                              encoding="utf-8", errors="replace", timeout=timeout)
    except subprocess.TimeoutExpired:
        return 124, "", f"timeout after {timeout}s"
    return proc.returncode, proc.stdout, proc.stderr


def capture_out(args, timeout=180):
    code, out, err = capture(args, timeout)
    if code != 0:
        raise RuntimeError(f"{quote(args)} exited {code}: {err.strip()[:600]}")
    return out


def quote(args):
    return " ".join(shlex.quote(str(item)) for item in args)


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def sha256_text(text):
    if isinstance(text, str):
        text = text.encode()
    return hashlib.sha256(text).hexdigest()


def write_json(path, payload):
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, sort_keys=False) + "\n")
    return path


def redact(token, keep=6):
    return (token[:keep] + "\u2026") if token else ""


def rel(path):
    try:
        return str(pathlib.Path(path).resolve().relative_to(ROOT))
    except ValueError:
        return str(pathlib.Path(path).resolve())


def free_port(taken):
    for _ in range(80):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        if port in taken:
            continue
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as check:
            if check.connect_ex(("127.0.0.1", port)) == 0:
                continue
        taken.add(port)
        return port
    raise RuntimeError("could not allocate a free loopback port")


def http_json(url, method="GET", payload=None, timeout=15, headers=None):
    data = None
    request_headers = {"Accept": "application/json"}
    if headers:
        request_headers.update(headers)
    if payload is not None:
        data = json.dumps(payload).encode()
        request_headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=data, headers=request_headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read().decode(errors="replace")
            try:
                return response.status, json.loads(raw or "{}")
            except ValueError:
                return response.status, {"raw": raw[:2000]}
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode(errors="replace")
        try:
            body = json.loads(raw or "{}")
        except ValueError:
            body = {"raw": raw[:2000]}
        return exc.code, body
    except Exception as exc:  # noqa: BLE001 - the caller decides what a failure means
        return None, {"error": f"{type(exc).__name__}: {exc}"}


def http_text(url, timeout=10):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return response.read().decode(errors="replace")
    except Exception as exc:  # noqa: BLE001
        return None


def http_ok(url, timeout=5):
    try:
        request = urllib.request.Request(url, headers={"Accept": "*/*"})
        with urllib.request.urlopen(request, timeout=timeout) as response:
            response.read(256)
            return response.status
    except urllib.error.HTTPError as exc:
        return exc.code
    except Exception:  # noqa: BLE001
        return None


def wait_http_ok(url, timeout, what):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        status = http_ok(url)
        if status == 200:
            return
        last = status
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for {what} at {url}: last status={last}")


def git_state():
    commit = capture_out(["git", "rev-parse", "HEAD"]).strip()
    status = capture_out(["git", "status", "--porcelain"])
    tracked = capture_out(["git", "diff", "HEAD", "--stat"]).strip()
    untracked = [line[3:].strip() for line in status.splitlines() if line.startswith("??")]
    modified = [line for line in status.splitlines() if line.strip() and not line.startswith("??")]
    return {
        "commit": commit,
        "branch": capture_out(["git", "rev-parse", "--abbrev-ref", "HEAD"]).strip(),
        "tracked_dirty": bool(modified),
        "tracked_changes": modified[:80],
        "tracked_diff_stat": tracked.splitlines()[-1] if tracked else "",
        "untracked_files": untracked[:80],
        "untracked_count": len(untracked),
        "git_status_sha256": sha256_text(status),
        "git_diff_head_sha256": sha256_text(capture_out(["git", "diff", "HEAD"])),
    }


# --------------------------------------------------------------------------
# Metric parsing
# --------------------------------------------------------------------------

_SAMPLE_RE = re.compile(r"^(?P<name>[a-zA-Z_:][a-zA-Z0-9_:]*)(?P<labels>\{[^}]*\})?\s+(?P<value>\S+)$")


def parse_metrics(text):
    """Compact projection of one /metrics scrape.

    Returns (unlabeled, labeled): `unlabeled` maps a bare metric name to its
    value when the series has no labels; `labeled` maps `name{labels}` to its
    value for the curated families (counters and gauges alike, because several
    of the interesting metering series are gauges without labels).
    """
    unlabeled, labeled = {}, {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        match = _SAMPLE_RE.match(line.strip())
        if not match:
            continue
        name = match.group("name")
        labels = match.group("labels") or ""
        try:
            value = float(match.group("value"))
        except ValueError:
            continue
        if not labels:
            if name in SERIES_UNLABELED or any(
                    name.startswith(prefix) or name == prefix for prefix in SERIES_LABELED_PREFIXES):
                unlabeled[name] = value
            continue
        if any(name.startswith(prefix) or name == prefix for prefix in SERIES_LABELED_PREFIXES):
            labeled[f"{name}{labels}"] = value
    return unlabeled, labeled


def series_deltas(first, last):
    out = {}
    for key, value in (last or {}).items():
        base = (first or {}).get(key)
        out[key] = round(value - base, 6) if base is not None else None
    return out


def histogram_quantile(buckets, quantile):
    total = None
    for bound, count in buckets:
        if math.isinf(bound):
            total = count
    if not total:
        return None
    rank = quantile * total
    previous_bound, previous_count = 0.0, 0.0
    for bound, count in sorted(buckets, key=lambda item: item[0]):
        if count >= rank:
            if math.isinf(bound) or count == previous_count:
                return previous_bound
            span = bound - previous_bound
            return previous_bound + span * (rank - previous_count) / (count - previous_count)
        previous_bound, previous_count = bound, count
    return None


def scrape_histograms(text, families):
    """families: iterable of histogram family prefixes (without `_bucket`)."""
    series = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        match = _SAMPLE_RE.match(line.strip())
        if not match or not match.group("name").endswith("_bucket"):
            continue
        name = match.group("name")
        family = name[: -len("_bucket")]
        if family not in families:
            continue
        labels = match.group("labels") or ""
        le_match = re.search(r'le="([^"]+)"', labels)
        if not le_match:
            continue
        raw = le_match.group(1)
        bound = math.inf if raw in ("+Inf", "Inf") else float(raw)
        try:
            value = float(match.group("value"))
        except ValueError:
            continue
        key = re.sub(r'(^|,)le="[^"]+"', "", labels.strip("{}")).strip(",")
        series.setdefault(f"{family}{{{key}}}" if key else family, []).append((bound, value))
    return series


# --------------------------------------------------------------------------
# Stack
# --------------------------------------------------------------------------

class Stack:
    def __init__(self, args, output, work):
        self.args = args
        self.output = output
        self.work = work
        self.state = {"commands": {}, "warnings": [], "notes": [], "ports": {}, "urls": {}}
        self.control_token = secrets.token_urlsafe(24)
        self.metering_token = secrets.token_urlsafe(24)
        self.commit = None
        self.run_id = None
        self.project = None
        self.two_gateways = False
        self.override_path = work / "compose-override.yml"
        self.docker_config = work / ".docker"
        self.loadtest = work / "loadtest"
        self.images = {}
        self.build_dockerfile = None
        self.build_services = ()

    # --- compose plumbing -------------------------------------------------

    def profiles(self):
        return [PROFILE_MOCKS] + ([PROFILE_MULTI] if self.two_gateways else [])

    def compose(self, *extra, profiles=None):
        args = ["docker", "compose", "-p", self.project, "-f", BASE_COMPOSE_NAME, "-f", "docker-compose.mock.yml",
                "-f", rel(self.override_path)]
        for profile in (profiles if profiles is not None else self.profiles()):
            args += ["--profile", profile]
        return args + list(extra)

    def compose_env(self):
        return {
            **os.environ,
            "HOST_UID": str(os.getuid()),
            "HOST_GID": str(os.getgid()),
            "CONTROL_ADMIN_TOKEN": self.control_token,
            "METERING_ADMIN_TOKEN": self.metering_token,
        }

    def image_prefix(self):
        return self.args.image_prefix or f"llm-gw-cap-{self.commit[:8]}"

    def render_override(self, mode):
        gateway_env = {
            "GATEWAY_ACCESS_GUARD": "pg",
            "GATEWAY_BILLING": "pg",
            "POSTGRES_DSN": DSN_IN_CONTAINER,
            "GATEWAY_DIAGNOSTICS_ADDR": ":8094",
            "GATEWAY_LIMITER_RATE": "100000",
            "GATEWAY_LIMITER_BURST": "100000",
            "GATEWAY_MAX_ACTIVE_STREAMS": str(mode["streams"]),
            "GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG": str(mode.get("per_org") or 0),
            "GATEWAY_INSTANCE_ID": "gw-a",
            "CONTROL_CONFIG_POLL_INTERVAL": "2s",
        }
        gateway_b_env = dict(gateway_env)
        gateway_b_env["GATEWAY_INSTANCE_ID"] = "gw-b"
        env_by_service = {
            "control": {"CONTROL_ADMIN_TOKEN": self.control_token},
            "metering": {"METERING_ADMIN_TOKEN": self.metering_token},
            "gateway": gateway_env,
            "gateway-b": gateway_b_env,
        }
        cpus_by_service = {"gateway": mode.get("gateway_cpus", self.args.gateway_cpus)}
        if mode.get("two_gateways") and mode.get("gateway_b_cpus", self.args.gateway_cpus):
            cpus_by_service["gateway-b"] = mode.get("gateway_b_cpus", self.args.gateway_cpus)
        cpus_by_service = {name: value for name, value in cpus_by_service.items() if value}
        lines = [
            "# Generated by scripts/capacity-report.py -- do not edit by hand.",
            f"# run_id: {self.run_id}  commit: {self.commit}",
            "#",
            "# Host ports are free ports probed with socket(); `!override` replaces the",
            "# base `ports` list (Compose >= 2.24.4). Named volumes are project-scoped",
            "# by `-p`, so `down -v` can only remove this run's state.",
            "#",
            "# Images are pinned to this commit so a later --no-build run reuses exactly",
            "# the binaries built from it. Secrets below are per-run random values.",
        ]
        for note in self.state["notes"]:
            lines.append(f"# note: {note}")
        lines.append("services:")
        # `migrate` has no published port and no env override, but it must still
        # appear here: when the GOPROXY fallback is active its build recipe has to
        # change too, or the one-shot migration container keeps the stock
        # Dockerfile and the whole `up` fails on `go mod download`.
        services = sorted(set(env_by_service) | set(cpus_by_service)
                          | {name for name, _, _ in PUBLISHED_PORTS} | {"migrate"})
        for service in services:
            ports = [(self.state["ports"][role]["host_port"], container)
                     for name, container, role in PUBLISHED_PORTS if name == service]
            lines.append(f"  {service}:")
            if ports:
                lines.append("    ports: !override")
                for host_port, container_port in ports:
                    lines.append(f'      - "127.0.0.1:{host_port}:{container_port}"')
            if cpus_by_service.get(service):
                lines.append(f'    cpus: "{cpus_by_service[service]}"')
            if self.images.get(service):
                lines.append(f"    image: {self.images[service]}")
            if self.build_dockerfile and service in (self.build_services or DEFAULT_BUILD_SERVICES):
                lines.append("    build:")
                lines.append("      context: .")
                lines.append(f"      dockerfile: {json.dumps(self.build_dockerfile)}")
            if env_by_service.get(service):
                lines.append("    environment:")
                for key, value in env_by_service[service].items():
                    lines.append(f'      {key}: "{value}"')
        self.override_path.write_text("\n".join(lines) + "\n")
        os.chmod(self.override_path, 0o600)

    # --- lifecycle --------------------------------------------------------

    def prepare(self, git_start):
        self.commit = git_start["commit"]
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.run_id = f"{stamp}-{self.commit[:8]}"
        self.project = re.sub(r"[^a-z0-9-]", "", f"{PROJECT_PREFIX}{self.run_id}".lower())
        if not self.project.startswith(PROJECT_PREFIX):
            raise SystemExit(f"refusing project name {self.project!r}")
        taken = set()
        for service, container_port, role in PUBLISHED_PORTS:
            host_port = free_port(taken)
            self.state["ports"][role] = {
                "service": service, "container_port": container_port, "host_port": host_port,
            }
        ports = self.state["ports"]
        self.state["urls"] = {
            "control_http": f"http://127.0.0.1:{ports['control_http']['host_port']}",
            "gateway": f"http://127.0.0.1:{ports['gateway']['host_port']}",
            "gateway_b": f"http://127.0.0.1:{ports['gateway_b']['host_port']}",
            "gateway_metrics": f"http://127.0.0.1:{ports['gateway_diag']['host_port']}/metrics",
            "gateway_pprof": f"http://127.0.0.1:{ports['gateway_diag']['host_port']}/debug/pprof",
            "gateway_b_metrics": f"http://127.0.0.1:{ports['gateway_b_diag']['host_port']}/metrics",
            "gateway_b_pprof": f"http://127.0.0.1:{ports['gateway_b_diag']['host_port']}/debug/pprof",
            "metering": f"http://127.0.0.1:{ports['metering']['host_port']}",
            "console_metrics": f"http://127.0.0.1:{ports['console']['host_port']}/metrics",
            "mock_primary": f"http://127.0.0.1:{ports['mock_primary']['host_port']}",
            "mock_backup": f"http://127.0.0.1:{ports['mock_backup']['host_port']}",
            "postgres": f"127.0.0.1:{ports['postgres']['host_port']}",
            "redis": f"127.0.0.1:{ports['redis']['host_port']}",
        }
        prefix = self.image_prefix()
        self.images = {
            name: f"{prefix}-{name}:latest"
            for name in ("control", "gateway", "metering", "console", "migrate",
                         "mock-primary", "mock-backup")
        }
        # gateway-b is the same binary as gateway: pin it to the same image, or
        # `--no-build` would look for a <project>-gateway-b image that never exists.
        self.images["gateway-b"] = self.images["gateway"]
        self.docker_config.mkdir(parents=True, exist_ok=True)
        (self.docker_config / "config.json").write_text("{}\n")
        user_config = pathlib.Path(os.environ.get("DOCKER_CONFIG") or (pathlib.Path.home() / ".docker"))
        plugins = user_config / "cli-plugins"
        if plugins.is_dir():
            target = self.docker_config / "cli-plugins"
            target.mkdir(exist_ok=True)
            for plugin in plugins.iterdir():
                link = target / plugin.name
                if not link.exists():
                    link.symlink_to(plugin.resolve())
        os.environ["DOCKER_CONFIG"] = str(self.docker_config)

    def mode(self, streams=256, per_org=0, two_gateways=False, gateway_cpus=None, gateway_b_cpus=None):
        return {
            "streams": streams,
            "per_org": per_org,
            "two_gateways": two_gateways,
            "gateway_cpus": gateway_cpus if gateway_cpus is not None else self.args.gateway_cpus,
            "gateway_b_cpus": gateway_b_cpus,
        }

    def ensure_master_key(self):
        """The control service mounts ./data/provider-credential-master-key read-only.

        Docker creates a *directory* for a bind mount whose source is missing, and
        control then refuses to start ("is a directory"). Bootstrap the file the
        same way scripts/isolated-baseline.py does.
        """
        if "provider-credential-master-key" not in (ROOT / BASE_COMPOSE_NAME).read_text():
            return None
        path = ROOT / "data/provider-credential-master-key"
        if path.exists() and not path.is_file():
            raise RuntimeError(f"{path} exists but is not a regular file; remove it and re-run")
        if path.is_file():
            return {"state": "present", "path": rel(path)}
        log("bootstrapping the local provider credential master key")
        with (self.work / "master-key.log").open("w") as handle:
            run(["go", "run", "./cmd/democtl", "ensure-master-key"], stdout=handle,
                stderr=subprocess.STDOUT, check=True)
        self.state["commands"]["ensure_master_key"] = "go run ./cmd/democtl ensure-master-key"
        return {"state": "generated", "path": rel(path),
                "command": "go run ./cmd/democtl ensure-master-key"}

    def capture_logs(self):
        if not self.state.get("up_started"):
            return None
        path = self.work / "compose-logs.txt"
        with path.open("w") as handle:
            run(self.compose("logs", "--no-color", "--tail", "3000"), env=self.compose_env(),
                stdout=handle, stderr=subprocess.STDOUT)
        return rel(path)

    def ensure_images(self):
        if self.args.no_build:
            missing = [image for image in self.images.values()
                       if capture(["docker", "image", "inspect", image])[0] != 0]
            if missing:
                raise SystemExit(
                    "--no-build was given but these pinned images are missing:\n  "
                    + "\n  ".join(missing)
                    + "\nBuild them once with: python3 scripts/capacity-report.py --output "
                    + f"{rel(self.output)} --experiments up --keep")
            log(f"--no-build: reusing pinned images {self.image_prefix()}-*")
        else:
            log(f"building pinned images {self.image_prefix()}-*")

    def up(self, mode, initial=False):
        two_now = bool(mode.get("two_gateways"))
        self.render_override(mode)
        code, _, err = capture(self.compose("config", "-q", profiles=[PROFILE_MOCKS, PROFILE_MULTI]))
        if code != 0:
            raise RuntimeError(f"generated compose override does not validate: {err.strip()}")
        self.state["up_started"] = True
        if not initial and self.two_gateways and not two_now:
            log("stopping gateway-b (switching to one-Gateway mode)")
            run(self.compose("rm", "-sf", "gateway-b", profiles=[PROFILE_MOCKS, PROFILE_MULTI]),
                env=self.compose_env(), capture_output=True)
        self.two_gateways = two_now
        if initial:
            flags = ["up", "-d", "--pull", "never", "--wait",
                     "--no-build" if self.args.no_build else "--build"]
            services = []
        else:
            # A mode switch must not recreate PostgreSQL/Redis/Control/Metering:
            # it only needs the data-plane instances on the new caps and CPUs.
            flags = ["up", "-d", "--pull", "never", "--wait", "--no-build", "--force-recreate", "--no-deps"]
            services = ["gateway"] + (["gateway-b"] if two_now else [])
        label = "initial" if initial else f"mode-{int(time.time())}"
        log(f"compose up ({'building' if initial and not self.args.no_build else 'reusing images'}; "
            f"{'two gateways' if two_now else 'one gateway'}; {'full stack' if initial else 'gateway only'})")
        log_path = self.work / f"compose-up-{label}.log"
        with log_path.open("w") as handle:
            proc = run(self.compose(*(flags + services)), env=self.compose_env(),
                       stdout=handle, stderr=subprocess.STDOUT)
        self.state["commands"]["compose_up" if initial else f"compose_up_{label}"] = \
            quote(self.compose(*(flags + services)))
        if proc.returncode != 0 and initial and not self.args.no_build and module_proxy_failure(log_path):
            log(f"stock build could not reach the Go module proxy; retrying once with GOPROXY={FALLBACK_GOPROXY}")
            self.state["warnings"].append(
                "the build container could not reach proxy.golang.org for `go mod download`; the images were "
                f"built from a generated copy of Dockerfile with GOPROXY={FALLBACK_GOPROXY}")
            resolved, _ = self.resolve_config()
            build_names = [name for name, service in (resolved or {}).get("services", {}).items()
                           if service.get("build")]
            self.build_services = tuple(sorted(build_names or DEFAULT_BUILD_SERVICES))
            self.build_dockerfile = str(write_goproxy_dockerfile(
                ROOT / "Dockerfile", self.work / "Dockerfile.cap", FALLBACK_GOPROXY))
            self.render_override(mode)
            code, _, err = capture(self.compose("config", "-q", profiles=[PROFILE_MOCKS, PROFILE_MULTI]))
            if code != 0:
                raise RuntimeError(f"compose override with the generated Dockerfile does not validate: {err.strip()}")
            retry_log = self.work / "compose-up-retry.log"
            with retry_log.open("w") as handle:
                retry = run(self.compose(*(flags + services)), env=self.compose_env(),
                            stdout=handle, stderr=subprocess.STDOUT)
            self.state["commands"]["compose_up_retry"] = quote(self.compose(*(flags + services)))
            self.state["build_recipe"] = f"generated Dockerfile copy with GOPROXY={FALLBACK_GOPROXY}"
            proc = retry
            log_path = retry_log
        if proc.returncode != 0:
            tail = log_path.read_text(errors="replace")[-3000:]
            raise RuntimeError(f"docker compose up failed (exit {proc.returncode}):\n{tail}")
        self.state.setdefault("build_recipe", "stock Dockerfile" if not self.args.no_build else "reused pinned images")
        self.register_effective(mode)
        wait_http_ok(self.state["urls"]["gateway"] + "/livez", 90, "gateway")
        if two_now:
            wait_http_ok(self.state["urls"]["gateway_b"] + "/livez", 90, "gateway-b")

    def register_effective(self, mode):
        resolved, error = self.resolve_config()
        if resolved is None:
            self.state["warnings"].append(f"config --format json failed: {error}")
            return
        gateway_env = (resolved["services"].get("gateway") or {}).get("environment") or {}
        gateway = resolved["services"].get("gateway") or {}
        self.state.setdefault("effective_gateway_env", []).append({
            "mode": {key: value for key, value in mode.items() if key != "two_gateways"},
            "two_gateways": bool(mode.get("two_gateways")),
            "gateway_cpus": gateway.get("cpus"),
            "env": {key: gateway_env.get(key) for key in (
                "GATEWAY_BILLING", "GATEWAY_ACCESS_GUARD", "GATEWAY_MAX_ACTIVE_STREAMS",
                "GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG", "GATEWAY_LIMITER_RATE",
                "GATEWAY_DIAGNOSTICS_ADDR", "GATEWAY_INSTANCE_ID", "POSTGRES_DSN")},
        })

    def resolve_config(self):
        code, out, err = capture(self.compose("config", "--format", "json",
                                              profiles=[PROFILE_MOCKS, PROFILE_MULTI]))
        if code != 0:
            return None, err.strip()
        try:
            return json.loads(out), None
        except ValueError as exc:
            return None, str(exc)

    def down(self):
        if not self.project.startswith(PROJECT_PREFIX):
            return {"ok": False, "detail": f"refusing to tear down {self.project!r}"}
        log(f"tearing down {self.project}")
        with (self.work / "teardown.log").open("w") as handle:
            proc = run(self.compose("down", "-v", "--remove-orphans",
                                    profiles=[PROFILE_MOCKS, PROFILE_MULTI]),
                       env=self.compose_env(), stdout=handle, stderr=subprocess.STDOUT)
        verify = {"exit": proc.returncode}
        code, out, _ = capture(["docker", "ps", "-a", "--filter",
                                f"label=com.docker.compose.project={self.project}", "--format", "{{.Names}}"])
        verify["containers_remaining"] = [line for line in out.splitlines() if line.strip()]
        code, out, _ = capture(["docker", "volume", "ls", "--filter",
                                f"label=com.docker.compose.project={self.project}", "--format", "{{.Name}}"])
        verify["volumes_remaining"] = [line for line in out.splitlines() if line.strip()]
        verify["ok"] = (proc.returncode == 0 and not verify["containers_remaining"]
                        and not verify["volumes_remaining"])
        return verify

    def foreign_containers(self):
        """CPU/memory of containers this run does not own.

        A single-host capacity run is only as good as its isolation: other
        experiments on the same Docker VM compete for the same cores. Recording
        them makes the contention visible instead of implied.
        """
        code, out, _ = capture(["docker", "ps", "--format", "{{.Names}}"])
        names = [line.strip() for line in out.splitlines() if line.strip()
                 and not line.strip().startswith(self.project)]
        if not names:
            return []
        code, out, _ = capture(["docker", "stats", "--no-stream", "--format", "{{json .}}"] + names,
                               timeout=60)
        observed = []
        for line in out.splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            observed.append({"name": entry.get("Name"), "cpu_percent": entry.get("CPUPerc"),
                             "mem_usage": entry.get("MemUsage")})
        return observed

    def containers(self):
        code, out, _ = capture(["docker", "ps", "--filter",
                                f"label=com.docker.compose.project={self.project}",
                                "--format", "{{.Names}}"])
        return sorted(line.strip() for line in out.splitlines() if line.strip())

    def inspect(self, name, template):
        code, out, _ = capture(["docker", "inspect", "--format", template, name])
        return out.strip() if code == 0 else None

    # --- in-container helpers ---------------------------------------------

    def psql(self, sql, timeout=60):
        args = self.compose("exec", "-T", "postgres", "psql", "-v", "ON_ERROR_STOP=1",
                            "-U", "gateway", "-d", "llm_gateway", "-At", "-F", "|", "-c", sql)
        code, out, err = capture(args, timeout=timeout)
        if code == 0:
            return out
        return f"ERROR|{err.strip()[:400]}"

    def psql_rows(self, sql, timeout=60):
        rows = []
        for line in self.psql(sql, timeout=timeout).splitlines():
            if not line.strip():
                continue
            rows.append(line.rstrip("\n").split("|"))
        return rows

    def psql_value(self, sql, timeout=60):
        rows = self.psql_rows(sql, timeout=timeout)
        return rows[0][0] if rows else None

    def redis_info(self, section=""):
        args = self.compose("exec", "-T", "redis", "redis-cli", "--raw", "INFO")
        if section:
            args.append(section)
        code, out, _ = capture(args, timeout=20)
        return out if code == 0 else ""

    def redis_ping_rtt_ms(self):
        host, port = self.state["urls"]["redis"].split(":")
        try:
            with socket.create_connection((host, int(port)), timeout=2) as sock:
                started = time.perf_counter()
                sock.sendall(b"PING\r\n")
                data = sock.recv(64)
                elapsed = (time.perf_counter() - started) * 1000
                return round(elapsed, 3) if b"PONG" in data else None
        except Exception:  # noqa: BLE001
            return None


# --------------------------------------------------------------------------
# Seed + control configuration
# --------------------------------------------------------------------------

class Seed:
    """Per-run organizations, users, keys, price version and funded wallets.

    Re-seeding the same project is idempotent: organizations are looked up by
    name, the wallet is only created (and credited) once, and only API keys are
    replaced. That keeps `billing_accounts == SUM(billing_entries)` true across
    a `--keep` + `--reuse-rounds` re-run.
    """

    def __init__(self, stack, org_count_hot=1, org_count_multi=10, balance_micro=10 ** 12):
        self.stack = stack
        self.hot_orgs = []
        self.multi_orgs = []
        self.org_ids = []
        self.keys = {"hot": [], "multi": []}
        self.key_ids = {"hot": [], "multi": []}
        self.key_meta = []
        self.user_id = None
        self.balance_micro = balance_micro
        self.requested = {"hot": org_count_hot, "multi": org_count_multi}
        self.info = {}

    def placeholder(self):
        """Dry runs and plan printing need the shape of the seed, not real keys."""
        self.keys = {"hot": ["sk-t11-dry-run-hot"], "multi": [f"sk-t11-dry-run-{i}" for i in range(10)]}
        self.key_ids = {"hot": ["key_dry_hot"], "multi": [f"key_dry_{i}" for i in range(10)]}
        self.hot_orgs = [1]
        self.multi_orgs = list(range(2, 12))
        return self

    def key_for(self, group, index=0):
        keys = self.keys.get(group) or []
        if not keys:
            raise RuntimeError(f"no seeded key for group {group!r}")
        return keys[index % len(keys)]

    def org_for(self, group, index=0):
        orgs = self.hot_orgs if group == "hot" else self.multi_orgs
        return orgs[index % len(orgs)]

    def apply(self):
        stack = self.stack
        # A placeholder seed may have filled the key/org lists for plan printing;
        # real keys and org ids must start from empty or key_for() would hand out
        # the placeholder string.
        self.keys = {"hot": [], "multi": []}
        self.key_ids = {"hot": [], "multi": []}
        self.hot_orgs, self.multi_orgs, self.org_ids, self.key_meta = [], [], [], []
        email = "t11-capacity@t11.test"
        self.user_id = int(stack.psql_value(f"""
        INSERT INTO users (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
        VALUES ('{email}', 'not-a-login-hash', 'active', false, 1, now(), now())
        ON CONFLICT (email) DO UPDATE SET updated_at = now()
        RETURNING id;"""))
        for group, count in (("hot", self.requested["hot"]), ("multi", self.requested["multi"])):
            for index in range(count):
                name = f"t11-{group}-{index}"
                existing = stack.psql_value(
                    f"SELECT id FROM organizations WHERE name = '{name}' AND type = 'enterprise';")
                if existing:
                    org_id = int(existing)
                    created = False
                else:
                    org_id = int(stack.psql_value(f"""
                    INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
                    VALUES ('enterprise', '{name}', 'active', {self.user_id}, now(), now())
                    RETURNING id;"""))
                    stack.psql(f"""
                    INSERT INTO memberships (org_id, user_id, role, status, created_at, updated_at)
                    VALUES ({org_id}, {self.user_id}, 'owner', 'active', now(), now());""")
                    stack.psql(f"""
                    INSERT INTO billing_accounts
                      (org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
                    VALUES ({org_id}, 'CNY', {self.balance_micro}, 0, 'active', 1, now(), now());""")
                    stack.psql(f"""
                    INSERT INTO billing_entries
                      (org_id, kind, amount_micro, direction, business_id, payload_digest, reason, posted_at, created_at)
                    VALUES ({org_id}, 'credit', {self.balance_micro}, 'credit',
                            't11-seed-{name}', '{sha256_text(name)}',
                            'T11 capacity seed credit', now(), now());""")
                    created = True
                # Keys are not money facts: replacing them keeps the wallet intact.
                stack.psql(f"DELETE FROM api_keys WHERE org_id = {org_id};")
                raw_key = "sk-t11-" + "".join(random.choices(string.ascii_lowercase + string.digits, k=28))
                key_id = "key_t11_" + "".join(random.choices(string.hexdigits.lower(), k=22))
                key_hash = hashlib.sha256(raw_key.encode()).hexdigest()
                stack.psql(f"""
                INSERT INTO api_keys (key_id, key_hash, key_prefix, org_id, owner_user_id, created_by,
                                      status, policy_version, created_at)
                VALUES ('{key_id}', '{key_hash}', '{raw_key[:10]}', {org_id}, {self.user_id},
                        {self.user_id}, 'active', 1, now());""")
                self.keys[group].append(raw_key)
                self.key_ids[group].append(key_id)
                self.org_ids.append(org_id)
                (self.hot_orgs if group == "hot" else self.multi_orgs).append(org_id)
                self.key_meta.append({
                    "key_id": key_id, "key_hash": key_hash, "key_prefix": raw_key[:10],
                    "tenant_id": f"org:{org_id}", "status": "active", "org_id": org_id,
                    "owner_user_id": self.user_id,
                })
                if created:
                    self.info.setdefault("created_orgs", []).append(org_id)
        stack.psql(f"""
        INSERT INTO price_versions (model, currency, input_price_micro_per_million,
                                    output_price_micro_per_million, effective_at, created_at)
        VALUES ('{MODEL}', 'CNY', 1000000, 1000000, '{PRICE_EFFECTIVE_AT}', now())
        ON CONFLICT (model, effective_at) DO NOTHING;""")
        price_id = stack.psql_value(
            f"SELECT id FROM price_versions WHERE model = '{MODEL}' ORDER BY effective_at DESC LIMIT 1;")
        self.info.update({
            "user_id": self.user_id,
            "hot_orgs": self.hot_orgs,
            "multi_orgs": self.multi_orgs,
            "key_ids": self.key_ids,
            "key_prefixes": {group: [key[:10] for key in keys] for group, keys in self.keys.items()},
            "balance_micro_per_org": self.balance_micro,
            "price_version_id": int(price_id) if price_id else None,
            "price": {"model": MODEL, "input_micro_per_million": 1000000,
                      "output_micro_per_million": 1000000,
                      "effective_at": PRICE_EFFECTIVE_AT},
            "note": ("direct (mock) rounds do not create billing rows; only gateway rounds are "
                     "reconciled. A `t11-capacity@t11.test` user owns every org."),
        })
        return self.info

    def snapshot_records(self):
        return self.key_meta


def configure_control(stack, key_records, providers, routes, expected_gateways=1):
    headers = {"Authorization": "Bearer " + stack.control_token}
    base = stack.state["urls"]["control_http"]
    payload = {"key_records": key_records, "providers": providers, "routes": routes}
    status, body = http_json(base + "/config", method="POST", payload=payload, timeout=20, headers=headers)
    if status != 200:
        raise RuntimeError(f"control /config returned {status}: {json.dumps(body)[:500]}")
    desired = int(body.get("desired_version") or 0)
    if desired <= 0:
        raise RuntimeError(f"control /config returned no desired_version: {body}")
    deadline = time.time() + 60
    last = None
    while time.time() < deadline:
        code, view = http_json(base + "/gateways", headers=headers, timeout=10)
        if code == 200:
            last = view
            in_sync = 0
            # The T09 view calls the list `instances`; democtl still reads
            # `gateways`. Accept either so the wait does not depend on the alias.
            instances = (view or {}).get("instances") or (view or {}).get("gateways") or []
            for gateway in instances:
                if gateway.get("state") == "rejected":
                    raise RuntimeError(f"gateway rejected config v{desired}: {gateway.get('reject_reason')}")
                applied = int(gateway.get("applied_version") or 0)
                age = gateway.get("report_age_ms")
                fresh = age is None or (isinstance(age, (int, float)) and age < 30000)
                if gateway.get("state") == "applied" and applied >= desired and fresh:
                    in_sync += 1
            if in_sync >= expected_gateways:
                return {"desired_version": desired, "gateways_in_sync": in_sync, "view": view}
        time.sleep(0.25)
    raise RuntimeError(f"{expected_gateways} gateway(s) did not report applied_version >= {desired} "
                       f"within 60s: {json.dumps(last)[:600]}")


# --------------------------------------------------------------------------
# Samplers
# --------------------------------------------------------------------------

class Sampler(threading.Thread):
    def __init__(self, name, interval, collect, sink, stop):
        super().__init__(daemon=True)
        self.sampler_name = name
        self.interval = interval
        self.collect = collect
        self.sink = sink
        self.stop = stop

    def run(self):
        while not self.stop.is_set():
            started = time.time()
            try:
                sample = self.collect()
            except Exception as exc:  # noqa: BLE001 - a sampler must never kill a round
                sample = {"collect_error": f"{type(exc).__name__}: {exc}"}
            sample["t"] = round(started, 3)
            self.sink.append(sample)
            self.stop.wait(max(0.05, self.interval - (time.time() - started)))


def metrics_collector(url):
    def collect():
        text = http_text(url, timeout=4)
        if text is None:
            return {"error": "scrape failed"}
        unlabeled, labeled = parse_metrics(text)
        out = dict(unlabeled)
        out.update(labeled)
        return out
    return collect


def docker_stats_collector(stack):
    def collect():
        names = stack.containers()
        if not names:
            return {"containers": {}}
        code, out, err = capture(["docker", "stats", "--no-stream", "--format", "{{json .}}"] + names,
                                 timeout=40)
        if code != 0:
            return {"error": err.strip()[:200]}
        containers = {}
        for line in out.splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            containers[entry.get("Name", "?")] = {
                "cpu_percent": entry.get("CPUPerc"), "mem_usage": entry.get("MemUsage"),
                "mem_percent": entry.get("MemPerc"), "net_io": entry.get("NetIO"),
                "block_io": entry.get("BlockIO"), "pids": entry.get("PIDs"),
            }
        return {"containers": containers}
    return collect


PG_SAMPLE_SQL = """
SELECT tag, value FROM (
  SELECT 'backends' AS tag, count(*)::text AS value FROM pg_stat_activity WHERE datname = current_database()
  UNION ALL SELECT 'active', count(*)::text FROM pg_stat_activity WHERE datname = current_database() AND state = 'active'
  UNION ALL SELECT 'idle_in_tx', count(*)::text FROM pg_stat_activity WHERE datname = current_database() AND state = 'idle in transaction'
  UNION ALL SELECT 'waiting_locks', count(*)::text FROM pg_locks WHERE NOT granted
  UNION ALL SELECT 'wait_row', count(*)::text FROM pg_locks WHERE NOT granted AND locktype = 'transactionid'
  UNION ALL SELECT 'max_conn', current_setting('max_connections')
  UNION ALL SELECT 'wait_events', COALESCE(string_agg(DISTINCT COALESCE(wait_event_type,'-') || ':' || COALESCE(wait_event,'-'), ','), '-')
    FROM pg_stat_activity WHERE datname = current_database() AND state <> 'idle' AND wait_event IS NOT NULL
  UNION ALL SELECT 'xact_commit', COALESCE(sum(xact_commit),0)::text FROM pg_stat_database WHERE datname = current_database()
  UNION ALL SELECT 'xact_rollback', COALESCE(sum(xact_rollback),0)::text FROM pg_stat_database WHERE datname = current_database()
  UNION ALL SELECT 'blks_hit', COALESCE(sum(blks_hit),0)::text FROM pg_stat_database WHERE datname = current_database()
  UNION ALL SELECT 'blks_read', COALESCE(sum(blks_read),0)::text FROM pg_stat_database WHERE datname = current_database()
  UNION ALL SELECT 'tup_inserted', COALESCE(sum(tup_inserted),0)::text FROM pg_stat_database WHERE datname = current_database()
  UNION ALL SELECT 'tup_updated', COALESCE(sum(tup_updated),0)::text FROM pg_stat_database WHERE datname = current_database()
  UNION ALL SELECT 'open_executions', count(*)::text FROM billing_executions WHERE status IN ('reserved','executing')
  UNION ALL SELECT 'pending_receipts', count(*)::text FROM usage_receipts WHERE status = 'pending_dispatch'
  UNION ALL SELECT 'dispatched_receipts', count(*)::text FROM usage_receipts WHERE status = 'dispatched'
  UNION ALL SELECT 'settled_receipts', count(*)::text FROM usage_receipts WHERE status = 'settled'
  UNION ALL SELECT 'exception_receipts', count(*)::text FROM usage_receipts WHERE status = 'exception'
  UNION ALL SELECT 'unsettled_age_s', COALESCE(EXTRACT(EPOCH FROM now() - min(occurred_at) FILTER (WHERE status <> 'settled')), 0)::text FROM usage_receipts
  UNION ALL SELECT 'oldest_due_age_s', COALESCE(EXTRACT(EPOCH FROM now() - min(dispatch_after) FILTER (WHERE status = 'pending_dispatch')), 0)::text FROM usage_receipts
  UNION ALL SELECT 'review_required', count(*)::text FROM billing_executions WHERE status = 'review_required'
) sample;
"""


def pg_collector(stack):
    def collect():
        rows = stack.psql_rows(PG_SAMPLE_SQL, timeout=30)
        out = {}
        for row in rows:
            if len(row) >= 2 and row[0] != "ERROR":
                out[row[0]] = row[1]
            elif row and row[0] == "ERROR":
                out["error"] = row[1][:200] if len(row) > 1 else "psql error"
        return out or {"error": "no rows"}
    return collect


REDIS_FIELDS = ("instantaneous_ops_per_sec", "instantaneous_input_kbps", "instantaneous_output_kbps",
                "connected_clients", "blocked_clients", "used_memory", "used_memory_peak",
                "total_commands_processed", "keyspace_hits", "keyspace_misses",
                "total_net_input_bytes", "total_net_output_bytes", "aof_last_write_status")


def redis_collector(stack):
    def collect():
        text = stack.redis_info()
        out = {}
        for line in text.splitlines():
            if ":" not in line or line.startswith("#"):
                continue
            key, _, value = line.partition(":")
            if key in REDIS_FIELDS:
                out[key] = value
        for line in text.splitlines():
            if not line.startswith("latency_percentiles_usec_"):
                continue
            key, _, value = line.partition(":")
            out.setdefault("latency_percentiles_usec", {})[
                key.replace("latency_percentiles_usec_", "")] = value
        out["ping_rtt_ms"] = stack.redis_ping_rtt_ms()
        return out
    return collect


PPROF_SUFFIX = {"cpu": "cpu.pb.gz", "heap": "heap.pb.gz", "mutex": "mutex.pb.gz",
                "block": "block.pb.gz", "goroutine": "goroutine.txt"}


def pprof_capture(stack, out_dir, label, seconds=20, two_gateways=False):
    results = {}
    base = stack.state["urls"]["gateway_b_pprof" if two_gateways else "gateway_pprof"]
    out_dir.mkdir(parents=True, exist_ok=True)
    for kind, suffix in PPROF_SUFFIX.items():
        target = out_dir / f"{label}.{suffix}"
        if kind == "cpu":
            url = f"{base}/profile?seconds={seconds}"
        elif kind == "goroutine":
            url = f"{base}/goroutine?debug=1"
        else:
            url = f"{base}/{kind}?debug=0"
        try:
            with urllib.request.urlopen(url, timeout=seconds + 40) as response:
                data = response.read()
        except Exception as exc:  # noqa: BLE001
            results[kind] = {"error": f"{type(exc).__name__}: {exc}"}
            continue
        target.write_bytes(data)
        results[kind] = {"bytes": len(data), "sha256": sha256_text(data)[:16], "path": rel(target)}
    return results


# --------------------------------------------------------------------------
# Round execution
# --------------------------------------------------------------------------

def generator_argv(binary, spec, target, key, json_path, timeout):
    argv = [str(binary), "-target", target, "-mode", spec["mode"],
            "-c", str(spec["c"]), "-n", str(spec["n"]), "-json", str(json_path),
            "-timeout", f"{timeout}s", "-tolerate-drops"]
    if spec["mode"] in ("open", "fixed"):
        argv += ["-rate", str(spec["rate"])]
    if spec.get("queue") is not None:
        argv += ["-admission-queue", str(spec["queue"])]
    if spec.get("shape"):
        argv += ["-shape", spec["shape"]]
    if spec.get("large_event"):
        argv += ["-large-event", spec["large_event"]]
    if spec.get("slow_client") and spec["slow_client"] != "normal":
        argv += ["-slow-client", spec["slow_client"]]
    if spec.get("slow_duration"):
        argv += ["-slow-client-duration", f"{spec['slow_duration']}s"]
    if spec.get("cancel_after"):
        argv += ["-cancel-after", f"{spec['cancel_after']}s"]
    if key:
        # The key travels through the argument, never the environment: loadtest
        # reads -key with $GATEWAY_API_KEY as its default, and passing it here
        # keeps the process list free of other runs' keys. It is never logged:
        # only the report fields below are persisted.
        argv += ["-key", key]
    return argv


def summarise_generator(envelope, exit_code, wall_seconds, name):
    report = envelope.get("report", {})

    def lat(block):
        block = block or {}
        return {key: block.get(key) for key in ("Count", "P50", "P95", "P99", "Min", "Max", "Avg")}

    return {
        "generator": name,
        "exit_code": exit_code,
        "wall_seconds": round(wall_seconds, 3),
        "target": envelope.get("target"),
        "mode": envelope.get("mode"),
        "load_profile": envelope.get("load_profile"),
        "elapsed_seconds": envelope.get("elapsed_seconds"),
        "accounting": {key: report.get(key) for key in (
            "Scheduled", "Started", "Completed", "Rejected", "TransportError", "Incomplete",
            "Cancelled", "ParseError", "NotScheduled", "Abandoned", "LoadgenDropped",
            "DroppedNoFreeSlot", "DroppedQueueFull", "CancelExplicit", "CancelTimeout",
            "PeakActive", "ActiveAtEnd", "GoroutinesPeak")},
        "rates": {key: report.get(key) for key in (
            "PlannedStartRatePerSecond", "ArrivalRatePerSecond", "AchievedStartRatePerSecond",
            "SuccessPerSecond", "ArrivalWindowSeconds", "StartWindowSeconds", "RunSeconds")},
        "latency": {
            "first_event": lat(report.get("FirstEvent")),
            "first_content": lat(report.get("FirstContent")),
            "stream_duration": lat(report.get("StreamDuration")),
            "total_duration": lat(report.get("TotalDur")),
            "dispatch_lag": lat(report.get("DispatchLag")),
            "scheduled_to_terminal": lat(report.get("ScheduledToTerminal")),
        },
        "status": report.get("Status"),
        "avg_events": report.get("AvgEvents"),
        "avg_bytes_per_stream": report.get("AvgBytesPerStream"),
        "loadgen_dropped_percent": report.get("LoadgenDroppedPercent"),
        "capacity_round_valid": report.get("CapacityRoundValid"),
        "capacity_invalid_reasons": report.get("CapacityInvalidReasons"),
        "capacity_rule": report.get("CapacityRule"),
    }


def aggregate_round(generators, specs, planned_rate, arrival_ratio_floor=0.99, drop_ceiling=0.1):
    totals = {key: 0 for key in ("Scheduled", "Started", "Completed", "Rejected", "TransportError",
                                 "Incomplete", "Cancelled", "ParseError", "NotScheduled", "Abandoned",
                                 "LoadgenDropped", "DroppedNoFreeSlot", "DroppedQueueFull",
                                 "CancelExplicit", "CancelTimeout")}
    peaks, reasons = [], []
    achieved_window, started, arrived = 0.0, 0, 0.0
    for gen in generators:
        accounting = gen.get("accounting") or {}
        for key in totals:
            totals[key] += int(accounting.get(key) or 0)
        peaks.append(int(accounting.get("PeakActive") or 0))
        rates = gen.get("rates") or {}
        achieved_window = max(achieved_window, float(rates.get("ArrivalWindowSeconds") or 0))
        started += int(accounting.get("Started") or 0)
        arrived += float(rates.get("ArrivalRatePerSecond") or 0)
        for reason in gen.get("capacity_invalid_reasons") or []:
            reasons.append(f"{gen['generator']}: {reason}")
        if gen.get("exit_code") not in (0, 1):
            reasons.append(f"{gen['generator']}: loadtest exit {gen.get('exit_code')}")
    dropped_percent = (100.0 * totals["LoadgenDropped"] / totals["Scheduled"]) if totals["Scheduled"] else None
    arrival_rate = round(arrived, 4)
    open_loop = bool(specs) and all(spec.get("mode") == "open" for spec in specs)
    arrival_ratio = (arrival_rate / planned_rate) if (planned_rate and open_loop) else None
    if dropped_percent is not None and dropped_percent > drop_ceiling:
        reasons.append(f"aggregate loadgen_dropped {totals['LoadgenDropped']}/{totals['Scheduled']} "
                       f"= {dropped_percent:.3f}% > {drop_ceiling}%")
    if arrival_ratio is not None and arrival_ratio < arrival_ratio_floor:
        reasons.append(f"aggregate arrival rate {arrival_rate:.3f}/s is {arrival_ratio * 100:.2f}% of the "
                       f"{planned_rate:.3f}/s plan (< {arrival_ratio_floor * 100:.0f}%)")
    return {
        "scheduled": totals["Scheduled"], "started": totals["Started"],
        "completed": totals["Completed"], "rejected": totals["Rejected"],
        "transport_error": totals["TransportError"], "incomplete": totals["Incomplete"],
        "cancelled": totals["Cancelled"], "parse_error": totals["ParseError"],
        "not_scheduled": totals["NotScheduled"], "abandoned": totals["Abandoned"],
        "loadgen_dropped": totals["LoadgenDropped"],
        "dropped_no_free_slot": totals["DroppedNoFreeSlot"],
        "dropped_queue_full": totals["DroppedQueueFull"],
        "loadgen_dropped_percent": round(dropped_percent, 4) if dropped_percent is not None else None,
        "peak_active_sum": sum(peaks), "peak_active_max": max(peaks) if peaks else 0,
        "peak_active_per_generator": peaks,
        "planned_start_rate": round(planned_rate, 4) if planned_rate else 0.0,
        "achieved_arrival_rate": arrival_rate,
        "arrival_ratio": round(arrival_ratio, 5) if arrival_ratio is not None else None,
        "arrival_ratio_enforced": open_loop,
        "achieved_start_rate": round(started / achieved_window, 4) if achieved_window else None,
        "window_seconds": round(achieved_window, 3),
        "capacity_round_valid": not reasons,
        "capacity_invalid_reasons": reasons,
        "note": ("per-generator percentiles; no merged percentile" if len(generators) > 1
                 else "single generator: percentiles are exact for the round"),
    }


def latency_across(generators, key):
    values = []
    for gen in generators:
        block = ((gen.get("latency") or {}).get(key) or {})
        if block.get("Count"):
            values.append(block)
    return values


def run_round(ctx, spec):
    stack, output, work = ctx["stack"], ctx["output"], ctx["work"]
    round_dir = work / "raw" / spec["id"]
    round_dir.mkdir(parents=True, exist_ok=True)
    record_path = output / "raw" / f"{spec['id']}.json"
    if ctx["reuse"] and record_path.exists():
        try:
            existing = json.loads(record_path.read_text())
            if not existing.get("error"):
                log(f"reusing round {spec['id']}")
                return existing
        except ValueError:
            pass
    log(f"round {spec['id']}: {spec['label']} ({len(spec['generators'])} generator(s), "
        f"plan {spec['planned_rate']:.2f}/s, hold {spec['hold_seconds']}s)")
    stop = threading.Event()
    sink = {}
    threads = []
    collectors = [
        ("gateway_metrics", SAMPLE_INTERVAL, metrics_collector(stack.state["urls"]["gateway_metrics"])),
        ("metering_metrics", SAMPLE_INTERVAL, metrics_collector(stack.state["urls"]["metering"] + "/metrics")),
        ("console_metrics", SAMPLE_INTERVAL * 2, metrics_collector(stack.state["urls"]["console_metrics"])),
        ("docker_stats", DOCKER_STATS_INTERVAL, docker_stats_collector(stack)),
        ("pg", PG_INTERVAL, pg_collector(stack)),
        ("redis", REDIS_INTERVAL, redis_collector(stack)),
    ]
    if spec.get("two_gateways"):
        collectors.append(("gateway_b_metrics", SAMPLE_INTERVAL,
                           metrics_collector(stack.state["urls"]["gateway_b_metrics"])))
    for name, interval, collector in collectors:
        sink[name] = []
        threads.append(Sampler(name, interval, collector, sink[name], stop))
    pprof_result, pprof_thread = {}, None
    if spec.get("pprof"):
        def capture_profiles():
            time.sleep(spec.get("pprof_delay", 3))
            pprof_result.update(pprof_capture(stack, work / "profiles", spec["id"],
                                              seconds=spec.get("pprof_seconds", 20),
                                              two_gateways=spec.get("two_gateways", False)))
        pprof_thread = threading.Thread(target=capture_profiles, daemon=True)
    for generator in spec["generators"]:
        if str(generator.get("key", "")).startswith("sk-t11-dry-run"):
            raise RuntimeError(f"round {spec['id']} carries a dry-run placeholder key: the plan was built "
                               "before the run seeded its organizations")
    started = time.time()
    for thread in threads:
        thread.start()
    if pprof_thread:
        pprof_thread.start()
    procs = []
    for index, generator in enumerate(spec["generators"]):
        json_path = round_dir / f"gen{index}.json"
        log_path = round_dir / f"gen{index}.log"
        argv = generator_argv(stack.loadtest, generator, generator.get("target") or spec["target"],
                              generator.get("key", ""), json_path, generator.get("timeout", 180))
        handle = log_path.open("w")
        procs.append({"index": index, "spec": generator, "json": json_path, "log": log_path,
                      "handle": handle,
                      "proc": subprocess.Popen(argv, cwd=ROOT, stdout=handle, stderr=subprocess.STDOUT),
                      "started": time.time()})
    for entry in procs:
        entry["returncode"] = entry["proc"].wait()
        entry["finished"] = time.time()
        entry["handle"].close()
    wall = time.time() - started
    # One full, unprojected /metrics snapshot per round is the raw counter
    # evidence behind the compact series (and the source of the histogram
    # percentiles for the gateway's own latency measure).
    full_snapshots = {}
    for name, url in (("gateway", stack.state["urls"]["gateway_metrics"]),
                      ("metering", stack.state["urls"]["metering"] + "/metrics"),
                      ("gateway_b", stack.state["urls"]["gateway_b_metrics"] if spec.get("two_gateways") else None)):
        if not url:
            continue
        text = http_text(url, timeout=10)
        if text is None:
            continue
        path = round_dir / f"metrics-{name}.txt"
        path.write_text(text)
        histograms = scrape_histograms(text, {
            "gateway_first_event_seconds", "gateway_ttft_seconds", "gateway_latency_seconds",
            "metering_settlement_lag_seconds", "limiter_queue_wait_seconds"})
        full_snapshots[name] = {
            "path": rel(path), "bytes": len(text), "sha256": sha256_text(text)[:16],
            "histogram_percentiles": {
                key: {q: round(histogram_quantile(buckets, q), 6) if histogram_quantile(buckets, q) is not None else None
                      for q in (0.5, 0.95, 0.99)}
                for key, buckets in histograms.items()},
        }
    stop.set()
    for thread in threads:
        thread.join(timeout=15)
    if pprof_thread:
        pprof_thread.join(timeout=spec.get("pprof_seconds", 20) + 60)
    resources = {
        "round": spec["id"],
        "interval_seconds": {"gateway_metrics": SAMPLE_INTERVAL, "docker_stats": DOCKER_STATS_INTERVAL,
                             "pg": PG_INTERVAL, "redis": REDIS_INTERVAL},
        "series": sink,
        "sample_counts": {name: len(values) for name, values in sink.items()},
        "full_metrics_snapshots": full_snapshots,
    }
    resources_path = work / "resources" / f"{spec['id']}.json"
    write_json(resources_path, resources)
    generators = []
    for entry in procs:
        envelope = {}
        if entry["json"].exists():
            try:
                envelope = json.loads(entry["json"].read_text())
            except ValueError as exc:
                envelope = {"parse_error": str(exc)}
        generators.append(summarise_generator(envelope, entry["returncode"],
                                              entry["finished"] - entry["started"], f"gen{entry['index']}"))
    aggregate = aggregate_round(generators, spec["generators"], spec["planned_rate"])
    record = {
        "id": spec["id"], "experiment": spec["experiment"], "label": spec["label"],
        "warmup": bool(spec.get("warmup")), "side": spec.get("side"), "shape": spec.get("shape"),
        "mode": spec.get("mode"), "org_group": spec.get("org_group"), "pprof": bool(spec.get("pprof")),
        "generator_count": len(spec["generators"]), "two_gateways": bool(spec.get("two_gateways")),
        "mode_config": spec.get("mode_config"), "target": spec["target"],
        "hold_seconds": spec["hold_seconds"], "planned_rate": spec["planned_rate"],
        "started_unix": started, "wall_seconds": round(wall, 3), "finished_unix": time.time(),
        "aggregate": aggregate, "generators": generators,
        "resources_file": rel(resources_path),
        "resources_summary": summarise_resources(sink, full_snapshots),
        "error": None,
    }
    write_json(record_path, record)
    log(f"  -> completed={aggregate['completed']} rejected={aggregate['rejected']} "
        f"incomplete={aggregate['incomplete']} transport_error={aggregate['transport_error']} "
        f"dropped={aggregate['loadgen_dropped']} "
        f"start_rate={aggregate['achieved_start_rate']}/s plan={aggregate['planned_start_rate']}/s "
        f"valid={aggregate['capacity_round_valid']} wall={wall:.1f}s")
    return record


def summarise_resources(sink, full_snapshots):
    out = {"series_points": {name: len(values) for name, values in sink.items()},
           "full_metrics_snapshots": full_snapshots}
    gateway = sink.get("gateway_metrics") or []

    def values_of(name):
        return [sample.get(name) for sample in gateway if isinstance(sample.get(name), (int, float))]

    def peak(name):
        values = values_of(name)
        return round(max(values), 4) if values else None

    def last(name):
        values = values_of(name)
        return round(values[-1], 4) if values else None

    out["gateway"] = {
        "samples": len(gateway),
        "sample_errors": sum(1 for sample in gateway if sample.get("error")),
        "rss_peak_mib": mib(peak("process_resident_memory_bytes")),
        "rss_last_mib": mib(last("process_resident_memory_bytes")),
        "heap_inuse_peak_mib": mib(peak("go_memstats_heap_inuse_bytes")),
        "heap_inuse_last_mib": mib(last("go_memstats_heap_inuse_bytes")),
        "heap_alloc_peak_mib": mib(peak("go_memstats_heap_alloc_bytes")),
        "goroutines_peak": peak("go_goroutines"),
        "goroutines_last": last("go_goroutines"),
        "open_fds_peak": peak("process_open_fds"),
        "cpu_seconds_delta": delta_of(gateway, "process_cpu_seconds_total"),
        "gc_cycles_delta": delta_of(gateway, "go_gc_duration_seconds_count"),
        "active_streams_peak": peak("gateway_active_streams"),
        "active_streams_last": last("gateway_active_streams"),
        "series_first_last_delta": series_deltas(gateway[0] if gateway else {},
                                                 gateway[-1] if gateway else {}),
    }
    if sink.get("gateway_b_metrics"):
        other = sink["gateway_b_metrics"]
        out["gateway_b"] = {
            "samples": len(other),
            "rss_peak_mib": mib(max([s.get("process_resident_memory_bytes", 0) for s in other] or [0])),
            "cpu_seconds_delta": delta_of(other, "process_cpu_seconds_total"),
            "active_streams_peak": max([s.get("gateway_active_streams", 0) for s in other] or [0]),
        }
    docker = sink.get("docker_stats") or []
    container_peaks = {}
    for sample in docker:
        for name, entry in (sample.get("containers") or {}).items():
            mem = parse_bytes(entry.get("mem_usage"))
            cpu = parse_percent(entry.get("cpu_percent"))
            current = container_peaks.setdefault(name, {"mem_peak_bytes": 0, "cpu_peak_percent": 0.0})
            if mem:
                current["mem_peak_bytes"] = max(current["mem_peak_bytes"], mem)
            if cpu is not None:
                current["cpu_peak_percent"] = max(current["cpu_peak_percent"], cpu)
    out["containers"] = {name: {"mem_peak_mib": round(values["mem_peak_bytes"] / 1048576, 1),
                                "cpu_peak_percent": round(values["cpu_peak_percent"], 1)}
                         for name, values in sorted(container_peaks.items())}
    pg = sink.get("pg") or []
    out["pg"] = {"samples": len(pg)}
    for key in ("backends", "active", "idle_in_tx", "waiting_locks", "wait_row", "open_executions",
                "pending_receipts", "dispatched_receipts", "settled_receipts", "unsettled_age_s",
                "oldest_due_age_s", "review_required", "xact_commit", "xact_rollback", "tup_inserted",
                "tup_updated", "blks_hit", "blks_read"):
        numeric = []
        for sample in pg:
            try:
                numeric.append(float(sample[key]))
            except (KeyError, TypeError, ValueError):
                continue
        if numeric:
            out["pg"][key] = {"first": numeric[0], "last": numeric[-1], "max": max(numeric)}
    wait_events = sorted({sample.get("wait_events") for sample in pg
                          if sample.get("wait_events") not in (None, "", "-")})
    out["pg"]["wait_events_observed"] = wait_events[:12]
    out["pg"]["max_conn"] = pg[0].get("max_conn") if pg else None
    redis = sink.get("redis") or []
    out["redis"] = {"samples": len(redis)}
    for key in ("instantaneous_ops_per_sec", "connected_clients", "blocked_clients", "used_memory"):
        numeric = []
        for sample in redis:
            try:
                numeric.append(float(sample.get(key)))
            except (TypeError, ValueError):
                continue
        if numeric:
            out["redis"][key] = {"first": numeric[0], "last": numeric[-1], "max": max(numeric)}
    rtts = sorted(sample.get("ping_rtt_ms") for sample in redis
                  if isinstance(sample.get("ping_rtt_ms"), (int, float)))
    if rtts:
        out["redis"]["ping_rtt_ms"] = {
            "n": len(rtts), "p50": round(rtts[len(rtts) // 2], 3),
            "p95": round(rtts[min(len(rtts) - 1, int(0.95 * len(rtts)))], 3),
            "max": round(max(rtts), 3)}
    return out


def mib(value):
    return round(value / 1048576, 2) if value is not None else None


def delta_of(samples, name):
    values = [sample.get(name) for sample in samples if isinstance(sample.get(name), (int, float))]
    return round(values[-1] - values[0], 4) if len(values) > 1 else None


def parse_bytes(text):
    if not text:
        return None
    match = re.match(r"([0-9.]+)\s*([KMG]?i?B)", text.strip())
    if not match:
        return None
    factor = {"B": 1, "KiB": 1024, "MiB": 1024 ** 2, "GiB": 1024 ** 3,
              "KB": 1000, "MB": 1000 ** 2, "GB": 1000 ** 3}.get(match.group(2), 1)
    return int(float(match.group(1)) * factor)


def parse_percent(text):
    if not text:
        return None
    try:
        return float(str(text).strip().rstrip("%"))
    except ValueError:
        return None


# --------------------------------------------------------------------------
# Experiment plan
# --------------------------------------------------------------------------

def open_generator(rate, shape, hold, max_slots, duration_hint=None, extra=None):
    duration = duration_hint or SHAPE_SECONDS[shape]
    spec = {
        "mode": "open", "c": max(16, min(max_slots, math.ceil(rate * duration * 1.4))),
        "n": max(1, math.ceil(rate * hold)), "rate": rate, "shape": shape,
        "timeout": timeout_for(shape, (extra or {}).get("slow_client"),
                               (extra or {}).get("slow_duration")),
    }
    spec.update(extra or {})
    return spec


def closed_generator(concurrency, shape, hold, extra=None):
    spec = {
        "mode": "closed", "c": concurrency, "shape": shape,
        "n": max(1, math.ceil(concurrency / SHAPE_SECONDS[shape] * hold)),
        "timeout": timeout_for(shape, None, None),
    }
    spec.update(extra or {})
    return spec


def timeout_for(shape, slow_client, slow_duration):
    base = 60.0 + max(0.0, SHAPE_SECONDS.get(shape, 0.2) - 0.2)
    if slow_client and slow_client != "normal":
        rate = {"10k": 10240.0, "1k": 1024.0}.get(slow_client, 0.0)
        if rate:
            base = max(base, 60 + 512 * 100 / rate)
        if slow_client == "stop":
            base = max(base, 120 + (slow_duration or 0))
    if slow_duration:
        base = max(base, 60 + slow_duration)
    return int(base)


def observed_m_knee(records):
    """The best sustained M-shape start rate seen so far: the capacity knee."""
    rates = []
    for record in records:
        aggregate = record.get("aggregate") or {}
        if record.get("shape") == "m" and not record.get("error"):
            if aggregate.get("achieved_start_rate"):
                rates.append(aggregate["achieved_start_rate"])
    return max(rates) if rates else None


def calibrate_overload(plan, knee, cfg, slots):
    """Rewrite the overload/fallback rounds once the knee is known."""
    overload = round(1.2 * knee, 1)
    fallback = round(0.5 * knee, 1)
    for spec in plan:
        role = spec.get("dynamic_rate")
        if not role:
            continue
        rate = overload if role == "overload" else fallback
        generator = spec["generators"][0]
        generator["rate"] = rate
        generator["n"] = max(1, math.ceil(rate * spec["hold_seconds"]))
        generator["c"] = max(16, min(slots, math.ceil(rate * SHAPE_SECONDS["m"] * 1.4)))
        generator["timeout"] = timeout_for("m", None, None)
        spec["planned_rate"] = rate
        if role == "overload":
            spec["label"] = (f"B09 {rate:.1f}/s (120% of the observed {knee:.1f}/s M knee) "
                             f"for {spec['hold_seconds']}s")
        else:
            spec["label"] = (f"B09 fallback {rate:.1f}/s (50% of the observed {knee:.1f}/s M knee) "
                             f"for {spec['hold_seconds']}s")
    return {"knee_start_rate_per_second": round(knee, 3), "overload_rate": overload,
            "fallback_rate": fallback,
            "why": ("the knee is the best sustained M-shape start rate of the closed-loop M ladder "
                    "(and the warm-up closed rounds) observed earlier in this run")}


def split_across_targets(generators, targets):
    """Spread each generator's offered load over `targets` (one entry per Gateway).

    cmd/loadtest drives exactly one URL, so a two-Gateway round must run one
    generator per instance: otherwise every request lands on the first Gateway
    and the comparison measures one instance with the other one idle.
    """
    if len(targets) == 1:
        return [{**generator, "target": targets[0]} for generator in generators]
    out = []
    for generator in generators:
        for index, target in enumerate(targets):
            half = dict(generator)
            half["target"] = target
            if half.get("mode") == "open":
                half["rate"] = round(generator["rate"] / len(targets), 6)
                half["n"] = max(1, math.ceil(generator["n"] / len(targets)))
            else:
                share = generator["c"] // len(targets) + (1 if index < generator["c"] % len(targets) else 0)
                half["c"] = max(1, share)
                half["n"] = max(1, math.ceil(generator["n"] / len(targets)))
            out.append(half)
    return out


def round_spec(round_id, experiment, label, target, generators, planned_rate, hold, **extra):
    spec = {"id": round_id, "experiment": experiment, "label": label, "target": target,
            "generators": generators, "planned_rate": planned_rate, "hold_seconds": hold}
    spec.update(extra)
    return spec


def build_plan(cfg, stack, seed):
    urls = stack.state["urls"]
    step, warmup, rounds = cfg["step_seconds"], cfg["warmup_seconds"], cfg["rounds"]
    slots = cfg["max_slots"]
    gateway_target = urls["gateway"] + "/v1/chat/completions"
    gateway_b_target = urls["gateway_b"] + "/v1/chat/completions"
    direct_target = urls["mock_primary"] + "/v1/chat/completions"
    plans, warmups = [], []

    def hot(index=0):
        return {"key": seed.key_for("hot", index), "org": seed.org_for("hot", index)}

    def multi(index):
        return {"key": seed.key_for("multi", index), "org": seed.org_for("multi", index)}

    if cfg["smoke"]:
        tiny = max(6, min(step, 15))
        warmups.append(round_spec("smoke-direct-s", "smoke", "smoke: direct mock S 10/s",
                                  direct_target, [open_generator(10.0, "s", tiny, slots)], 10.0, tiny,
                                  side="direct", shape="s", mode="open"))
        warmups.append(round_spec("smoke-gateway-s", "smoke", "smoke: gateway S 10/s",
                                  gateway_target, [{**open_generator(10.0, "s", tiny, slots), **hot()}],
                                  10.0, tiny, side="gateway", shape="s", mode="open", org_group="hot"))
        warmups.append(round_spec("smoke-gateway-closed", "smoke", "smoke: gateway closed M c=8",
                                  gateway_target, [{**closed_generator(8, "m", tiny), **hot()}],
                                  8 / SHAPE_SECONDS["m"], tiny, side="gateway", shape="m",
                                  mode="closed", org_group="hot"))
        warmups.append(round_spec("smoke-gateway-slow", "smoke", "smoke: gateway 2/s at 1 KiB/s client",
                                  gateway_target,
                                  [{**open_generator(2.0, "m", tiny + 20, slots, duration_hint=SLOW_SECONDS["1k"],
                                                     extra={"slow_client": "1k"}), **hot()}],
                                  2.0, tiny + 20, side="gateway", shape="m", mode="open", org_group="hot",
                                  pprof=True, pprof_seconds=5))
        cfg["smoke"] = True
        return warmups

    # ---- B01: direct vs gateway, S/M/L, low load ----
    for side, target in (("direct", direct_target), ("gateway", gateway_target)):
        for shape, rate, repeats in (("s", 25.0, 3), ("m", 4.0, 2), ("l", 2.0, 1)):
            for repeat in range(1, min(rounds, repeats) + 1):
                generator = open_generator(rate, shape, step, slots)
                if side == "gateway":
                    generator.update(hot())
                plans.append(round_spec(
                    f"b01-{side}-{shape}-r{repeat}", "b01", f"B01 {side} {shape.upper()} {rate:.0f}/s r{repeat}",
                    target, [generator], rate, step, side=side, shape=shape, mode="open",
                    org_group="hot" if side == "gateway" else None))

    # ---- closed-loop concurrency ladder (M) ----
    for concurrency in (1, 16, 64, 128, 256):
        plans.append(round_spec(f"closed-c{concurrency}", "closed-ladder",
                                f"closed-loop ladder M c={concurrency}", gateway_target,
                                [{**closed_generator(concurrency, "m", step), **hot()}],
                                concurrency / SHAPE_SECONDS["m"], step, side="gateway", shape="m",
                                mode="closed", org_group="hot"))
    for repeat in (2, 3):
        plans.append(round_spec(f"closed-c256-r{repeat}", "closed-ladder",
                                f"closed-loop ladder M c=256 r{repeat}", gateway_target,
                                [{**closed_generator(256, "m", step), **hot()}],
                                256 / SHAPE_SECONDS["m"], step, side="gateway", shape="m",
                                mode="closed", org_group="hot"))

    # ---- open-loop rate ladders (S and L) ----
    for rate in (50.0, 100.0, 200.0, 300.0, 500.0):
        plans.append(round_spec(f"open-s-{int(rate)}", "open-ladder-s", f"open-loop S ladder {rate:.0f}/s",
                                gateway_target, [{**open_generator(rate, "s", step, slots), **hot()}],
                                rate, step, side="gateway", shape="s", mode="open", org_group="hot"))
    for rate in (2.0, 5.0, 10.0, 20.0, 30.0):
        plans.append(round_spec(f"open-l-{int(rate)}", "open-ladder-l", f"open-loop L ladder {rate:.0f}/s",
                                gateway_target, [{**open_generator(rate, "l", step, slots), **hot()}],
                                rate, step, side="gateway", shape="l", mode="open", org_group="hot"))

    # ---- one hot wallet vs ten wallets (closed M) ----
    for repeat in (1, 2):
        plans.append(round_spec(f"wallets-hot-r{repeat}", "wallets",
                                f"one hot wallet, closed M c=128 r{repeat}", gateway_target,
                                [{**closed_generator(128, "m", step), **hot()}],
                                128 / SHAPE_SECONDS["m"], step, side="gateway", shape="m",
                                mode="closed", org_group="hot"))
    for repeat in (1, 2):
        generators = [{**closed_generator(13, "m", step), **multi(index)} for index in range(10)]
        plans.append(round_spec(f"wallets-multi-r{repeat}", "wallets",
                                f"ten wallets, closed M c=13x10=130 r{repeat}", gateway_target,
                                generators, 13 * 10 / SHAPE_SECONDS["m"], step, side="gateway",
                                shape="m", mode="closed", org_group="multi"))

    # ---- B06 slow-client curve ----
    slow_points = (
        ("p0-normal", 50.0, None, 0.0, 0.0),
        ("p1-1pct-1k", 50.0, "1k", 0.5, 0.0),
        ("p2-10pct-1k", 20.0, "1k", 2.0, 0.0),
        ("p3-50pct-1k", 4.0, "1k", 2.0, 0.0),
        ("p4-100pct-10k", 10.0, "10k", 10.0, 0.0),
        ("p5-100pct-1k", 3.0, "1k", 3.0, 0.0),
        ("p6-100pct-stop", 3.0, "stop", 3.0, cfg["slow_stop_seconds"]),
    )
    for name, total_rate, slow_mode, slow_rate, stop_seconds in slow_points:
        generators = []
        hold = step + (stop_seconds or 0)
        if total_rate - slow_rate > 0:
            generators.append({**open_generator(total_rate - slow_rate, "m", hold, slots), **hot()})
        if slow_rate > 0:
            extra = {"slow_client": slow_mode}
            if stop_seconds:
                extra["slow_duration"] = stop_seconds
            generators.append({**open_generator(slow_rate, "m", hold, slots,
                                                duration_hint=SLOW_SECONDS.get(slow_mode), extra=extra), **hot()})
        plans.append(round_spec(f"slow-{name}", "slowclient",
                                f"B06 {name}: {total_rate:.1f}/s total, {slow_rate:.1f}/s {slow_mode}",
                                gateway_target, generators, total_rate, hold, side="gateway",
                                shape="m", mode="open", org_group="hot",
                                pprof=name in ("p0-normal", "p5-100pct-1k")))

    # ---- B09 overload then fallback ----
    if cfg["overload_rate"]:
        rate = float(cfg["overload_rate"])
        plans.append(round_spec("overload-120pct", "overload",
                                f"B09 {rate:.0f}/s for {cfg['overload_seconds']}s",
                                gateway_target,
                                [{**open_generator(rate, "m", cfg["overload_seconds"], slots), **hot()}],
                                rate, cfg["overload_seconds"], side="gateway", shape="m", mode="open",
                                org_group="hot", pprof=True, pprof_seconds=30, pprof_delay=10))
        fallback = rate / 2.4
        plans.append(round_spec("overload-fallback", "overload",
                                f"B09 fallback {fallback:.1f}/s for {cfg['fallback_seconds']}s",
                                gateway_target,
                                [{**open_generator(fallback, "m", cfg["fallback_seconds"], slots), **hot()}],
                                fallback, cfg["fallback_seconds"], side="gateway", shape="m", mode="open",
                                org_group="hot"))
    elif cfg["overload_dynamic"]:
        # The overload rate is calibrated at run time from the observed M-shape
        # knee (the best sustained start rate of the closed-loop M ladder), so
        # "120% of the knee" is a measurement, not a guess.
        plans.append(round_spec("overload-120pct", "overload", "B09 overload (rate resolved from the knee)",
                                gateway_target,
                                [{**open_generator(60.0, "m", cfg["overload_seconds"], slots), **hot()}],
                                60.0, cfg["overload_seconds"], side="gateway", shape="m", mode="open",
                                org_group="hot", pprof=True, pprof_seconds=30, pprof_delay=10,
                                dynamic_rate="overload"))
        plans.append(round_spec("overload-fallback", "overload", "B09 fallback (rate resolved from the knee)",
                                gateway_target,
                                [{**open_generator(25.0, "m", cfg["fallback_seconds"], slots), **hot()}],
                                25.0, cfg["fallback_seconds"], side="gateway", shape="m", mode="open",
                                org_group="hot", dynamic_rate="fallback"))

    # ---- one vs two gateways, matched total resources ----
    if cfg["include_gw1v2"]:
        one = {"streams": 512, "gateway_cpus": 8, "gateway_b_cpus": None, "two_gateways": False}
        two = {"streams": 256, "gateway_cpus": 4, "gateway_b_cpus": 4, "two_gateways": True}
        for name, mode_cfg in (("gw1", one), ("gw2", two)):
            targets = [gateway_target, gateway_b_target] if mode_cfg["two_gateways"] else [gateway_target]
            loads = (
                ("m-open-60", lambda: open_generator(60.0, "m", step, slots)),
                ("m-open-120", lambda: open_generator(120.0, "m", step, slots)),
                ("m-closed-256", lambda: closed_generator(256, "m", step)),
            )
            for load_name, builder in loads:
                generator = {**builder(), **hot()}
                generators = split_across_targets([generator], targets)
                plans.append(round_spec(
                    f"gw1v2-{name}-{load_name}", "gw1v2", f"{name} matched-resources {load_name}",
                    gateway_target, generators,
                    float(generator.get("rate") or 256 / SHAPE_SECONDS["m"]),
                    step, side="gateway", shape="m", mode=generator["mode"], org_group="hot",
                    two_gateways=mode_cfg["two_gateways"], mode_config=dict(mode_cfg)))
            # balanced many-org load on the same total resources; every org is
            # served by both instances so the wallet is genuinely shared.
            generators = [{**open_generator(40.0 / 10, "m", step, slots), **multi(index)} for index in range(10)]
            plans.append(round_spec(
                f"gw1v2-{name}-multi-org", "gw1v2", f"{name} ten orgs, M 4/s each (40/s total)",
                gateway_target, split_across_targets(generators, targets), 40.0, step, side="gateway",
                shape="m", mode="open", org_group="multi", two_gateways=mode_cfg["two_gateways"],
                mode_config=dict(mode_cfg)))
            # hot organization, per-org active-stream cap 32: 1 gateway -> 32, 2 -> 64.
            # Last in the block so the per-org cap costs one mode switch per config.
            generator = {**open_generator(40.0, "m", step, slots), **hot()}
            plans.append(round_spec(
                f"gw1v2-perorg-{name}", "gw1v2", f"{name} per-org cap 32, one hot org, M 40/s",
                gateway_target, split_across_targets([generator], targets), 40.0, step, side="gateway",
                shape="m", mode="open", org_group="hot", two_gateways=mode_cfg["two_gateways"],
                mode_config={**mode_cfg, "per_org": 32}))

    # ---- warm-ups (recorded, excluded from medians) ----
    if warmup > 0:
        warmups.append(round_spec("warmup-open-m-20", "warmup", f"warm-up M 20/s for {warmup}s (discarded)",
                                  gateway_target, [{**open_generator(20.0, "m", warmup, slots), **hot()}],
                                  20.0, warmup, side="gateway", shape="m", mode="open",
                                  org_group="hot", warmup=True))
        closed_hold = max(10, warmup // 2)
        warmups.append(round_spec("warmup-closed-c64", "warmup",
                                  f"warm-up M closed c=64 for {closed_hold}s (discarded)", gateway_target,
                                  [{**closed_generator(64, "m", closed_hold), **hot()}],
                                  64 / SHAPE_SECONDS["m"], closed_hold, side="gateway", shape="m",
                                  mode="closed", org_group="hot", warmup=True))
    return warmups + plans


# --------------------------------------------------------------------------
# Reconciliation
# --------------------------------------------------------------------------

def reconcile(ctx, seed, records):
    stack = ctx["stack"]
    orgs = ",".join(str(org) for org in seed.org_ids)
    rows = stack.psql_rows(f"""
    SELECT 'exec_total', count(*)::text FROM billing_executions WHERE org_id IN ({orgs})
    UNION ALL SELECT 'exec_' || status, count(*)::text FROM billing_executions WHERE org_id IN ({orgs}) GROUP BY status
    UNION ALL SELECT 'receipt_total', count(*)::text FROM usage_receipts WHERE org_id IN ({orgs})
    UNION ALL SELECT 'receipt_' || status, count(*)::text FROM usage_receipts WHERE org_id IN ({orgs}) GROUP BY status
    UNION ALL SELECT 'entries_total', count(*)::text FROM billing_entries WHERE org_id IN ({orgs})
    UNION ALL SELECT 'entries_' || kind, count(*)::text FROM billing_entries WHERE org_id IN ({orgs}) GROUP BY kind
    UNION ALL SELECT 'entry_sum', COALESCE(sum(CASE WHEN kind IN ('credit','debit','adjustment')
        THEN CASE WHEN direction='credit' THEN amount_micro ELSE -amount_micro END ELSE 0 END),0)::text
      FROM billing_entries WHERE org_id IN ({orgs})
    UNION ALL SELECT 'entry_reserved', COALESCE(sum(CASE WHEN kind IN ('reservation','release')
        THEN CASE WHEN direction='credit' THEN amount_micro ELSE -amount_micro END ELSE 0 END),0)::text
      FROM billing_entries WHERE org_id IN ({orgs})
    UNION ALL SELECT 'account_posted', COALESCE(sum(posted_balance_micro),0)::text FROM billing_accounts WHERE org_id IN ({orgs})
    UNION ALL SELECT 'account_reserved', COALESCE(sum(reserved_micro),0)::text FROM billing_accounts WHERE org_id IN ({orgs})
    UNION ALL SELECT 'accounts', count(*)::text FROM billing_accounts WHERE org_id IN ({orgs})
    UNION ALL SELECT 'debit_sum', COALESCE(sum(amount_micro),0)::text FROM billing_entries WHERE org_id IN ({orgs}) AND kind='debit'
    UNION ALL SELECT 'settled_charge_sum', COALESCE(sum(charge_micro),0)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND status='settled'
    UNION ALL SELECT 'charge_sum', COALESCE(sum(charge_micro),0)::text FROM usage_receipts WHERE org_id IN ({orgs})
    UNION ALL SELECT 'receipt_wo_exec', count(*)::text FROM usage_receipts r
        LEFT JOIN billing_executions e USING (execution_id) WHERE r.org_id IN ({orgs}) AND e.execution_id IS NULL
    UNION ALL SELECT 'exec_wo_receipt_ok', count(*)::text FROM billing_executions e
        WHERE e.org_id IN ({orgs}) AND e.receipt_recorded_at IS NOT NULL
          AND NOT EXISTS (SELECT 1 FROM usage_receipts r WHERE r.execution_id = e.execution_id)
    """, timeout=180)
    facts = {}
    for row in rows:
        if len(row) >= 2 and row[0] != "ERROR":
            try:
                facts[row[0]] = int(row[1])
            except ValueError:
                facts[row[0]] = row[1]
    for row in stack.psql_rows(f"""
    SELECT 'lag_p50_s', COALESCE(EXTRACT(EPOCH FROM percentile_cont(0.5) WITHIN GROUP (ORDER BY settled_at - occurred_at)),0)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND settled_at IS NOT NULL
    UNION ALL SELECT 'lag_p95_s', COALESCE(EXTRACT(EPOCH FROM percentile_cont(0.95) WITHIN GROUP (ORDER BY settled_at - occurred_at)),0)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND settled_at IS NOT NULL
    UNION ALL SELECT 'lag_p99_s', COALESCE(EXTRACT(EPOCH FROM percentile_cont(0.99) WITHIN GROUP (ORDER BY settled_at - occurred_at)),0)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND settled_at IS NOT NULL
    UNION ALL SELECT 'lag_max_s', COALESCE(EXTRACT(EPOCH FROM max(settled_at - occurred_at)),0)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND settled_at IS NOT NULL
    UNION ALL SELECT 'receipt_complete_to_settle_p99_s', COALESCE(EXTRACT(EPOCH FROM percentile_cont(0.99) WITHIN GROUP (ORDER BY settled_at - created_at)),0)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND settled_at IS NOT NULL
    UNION ALL SELECT 'unsettled', count(*)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND settled_at IS NULL
    UNION ALL SELECT 'zero_charge', count(*)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND charge_micro = 0
    UNION ALL SELECT 'exceptions', count(*)::text FROM billing_exceptions WHERE org_id IN ({orgs})
    UNION ALL SELECT 'exception_receipts', count(*)::text FROM usage_receipts WHERE org_id IN ({orgs}) AND status = 'exception'
    """, timeout=180):
        if len(row) >= 2 and row[0] != "ERROR":
            try:
                facts[row[0]] = round(float(row[1]), 6)
            except ValueError:
                facts[row[0]] = row[1]

    gateway_rounds = [record for record in records if record.get("side") == "gateway" and not record.get("error")]
    gateway_completed = sum((record.get("aggregate") or {}).get("completed", 0) for record in gateway_rounds)
    gateway_started = sum((record.get("aggregate") or {}).get("started", 0) for record in gateway_rounds)
    checks = []

    def check(name, ok, detail):
        checks.append({"check": name, "ok": bool(ok), "detail": detail})

    check("usage_receipts == completed gateway streams",
          facts.get("receipt_total") == gateway_completed,
          f"receipts={facts.get('receipt_total')} loadtest completed={gateway_completed} "
          f"(started={gateway_started}) over {len(gateway_rounds)} gateway rounds incl. warm-ups")
    check("every receipt has its execution row", facts.get("receipt_wo_exec") == 0,
          f"receipt_wo_exec={facts.get('receipt_wo_exec')}")
    check("every receipt-recorded execution has a receipt", facts.get("exec_wo_receipt_ok") == 0,
          f"exec_wo_receipt_ok={facts.get('exec_wo_receipt_ok')}")
    check("debit entries == settled receipts",
          facts.get("entries_debit") == facts.get("receipt_settled"),
          f"debits={facts.get('entries_debit')} settled={facts.get('receipt_settled')} "
          f"pending={facts.get('receipt_pending_dispatch')} dispatched={facts.get('receipt_dispatched')} "
          f"exception={facts.get('receipt_exception')}")
    check("debit sum == settled receipt charge sum", facts.get("debit_sum") == facts.get("settled_charge_sum"),
          f"debit_sum={facts.get('debit_sum')} settled_charge_sum={facts.get('settled_charge_sum')} "
          f"unsettled={facts.get('unsettled')}")
    check("account projection == ledger recompute",
          facts.get("account_posted") == facts.get("entry_sum")
          and facts.get("account_reserved") == facts.get("entry_reserved"),
          f"posted projection={facts.get('account_posted')} ledger={facts.get('entry_sum')}; "
          f"reserved projection={facts.get('account_reserved')} ledger={facts.get('entry_reserved')}")
    check("reserved is back to zero after settlement", facts.get("account_reserved") == 0,
          f"reserved={facts.get('account_reserved')}")
    check("no execution left reserved/executing",
          facts.get("exec_reserved", 0) == 0 and facts.get("exec_executing", 0) == 0,
          f"reserved={facts.get('exec_reserved')} executing={facts.get('exec_executing')} "
          f"review_required={facts.get('exec_review_required')}")
    check("every round balances started = completed+rejected+transport+incomplete+cancelled+parse",
          all(record["aggregate"]["started"] ==
              record["aggregate"]["completed"] + record["aggregate"]["rejected"]
              + record["aggregate"]["transport_error"] + record["aggregate"]["incomplete"]
              + record["aggregate"]["cancelled"] + record["aggregate"]["parse_error"]
              for record in records if not record.get("error") and record.get("aggregate")),
          f"{len([r for r in records if not r.get('error')])} rounds checked")
    return {
        "orgs": seed.org_ids, "hot_orgs": seed.hot_orgs, "multi_orgs": seed.multi_orgs,
        "postgres_facts": facts,
        "loadtest_gateway_rounds": len(gateway_rounds),
        "loadtest_completed_streams": gateway_completed,
        "loadtest_started_streams": gateway_started,
        "checks": checks,
        "ok": all(item["ok"] for item in checks),
        "scope_note": ("scoped by org_id to the organizations this run created: these counts are the run's own "
                       "executions/receipts/debits, never the historical ledger totals"),
    }


def wait_reconcile(stack, timeout=RECONCILE_TIMEOUT):
    deadline = time.time() + timeout
    url = stack.state["urls"]["metering"] + "/reconcile"
    headers = {"Authorization": "Bearer " + stack.metering_token}
    attempts, last = 0, {}
    while True:
        attempts += 1
        status, body = http_json(url, timeout=90, headers=headers)
        last = {"http": status, "body": body}
        report = (body or {}).get("report") or {}
        if status == 200 and (body or {}).get("ok") and report.get("pending_messages") == 0:
            return {"ok": True, "attempts": attempts, "response": body}
        if time.time() >= deadline:
            return {"ok": False, "attempts": attempts, "detail": f"after {attempts} polls", "response": last}
        time.sleep(3)


# --------------------------------------------------------------------------
# Reporting
# --------------------------------------------------------------------------

def median(values):
    values = sorted(value for value in values if value is not None)
    if not values:
        return None
    middle = len(values) // 2
    if len(values) % 2:
        return round(values[middle], 4)
    return round((values[middle - 1] + values[middle]) / 2, 4)


def value_range(values):
    values = [value for value in values if value is not None]
    if not values:
        return None
    return [round(min(values), 4), round(max(values), 4)]


def summarize_experiments(records):
    rows = []
    for record in records:
        aggregate = record.get("aggregate") or {}
        generators = record.get("generators") or []
        lat = {name: latency_across(generators, name) for name in
               ("first_event", "first_content", "dispatch_lag", "scheduled_to_terminal",
                "stream_duration", "total_duration")}

        def weighted_p95(name):
            weighted, total = 0.0, 0
            for block in lat[name]:
                if block.get("Count") and block.get("P95") is not None:
                    weighted += block["P95"] * block["Count"]
                    total += block["Count"]
            return round(weighted / total / 1e6, 3) if total else None

        def p50(name):
            blocks = [block for block in lat[name] if block.get("Count")]
            if not blocks:
                return None
            if len(blocks) == 1:
                return round((blocks[0]["P50"] or 0) / 1e6, 3)
            return round(median([(block["P50"] or 0) / 1e6 for block in blocks]), 3)

        rows.append({
            "id": record.get("id"), "experiment": record.get("experiment"),
            "label": record.get("label"), "warmup": record.get("warmup"),
            "valid": aggregate.get("capacity_round_valid"),
            "invalid_reasons": aggregate.get("capacity_invalid_reasons"),
            "scheduled": aggregate.get("scheduled"), "started": aggregate.get("started"),
            "completed": aggregate.get("completed"), "rejected": aggregate.get("rejected"),
            "transport_error": aggregate.get("transport_error"),
            "incomplete": aggregate.get("incomplete"), "cancelled": aggregate.get("cancelled"),
            "loadgen_dropped": aggregate.get("loadgen_dropped"),
            "loadgen_dropped_percent": aggregate.get("loadgen_dropped_percent"),
            "peak_active_sum": aggregate.get("peak_active_sum"),
            "planned_start_rate": aggregate.get("planned_start_rate"),
            "achieved_arrival_rate": aggregate.get("achieved_arrival_rate"),
            "achieved_start_rate": aggregate.get("achieved_start_rate"),
            "arrival_ratio": aggregate.get("arrival_ratio"),
            "arrival_ratio_enforced": aggregate.get("arrival_ratio_enforced"),
            "window_seconds": aggregate.get("window_seconds"),
            "wall_seconds": record.get("wall_seconds"),
            "first_event_p50_ms": p50("first_event"), "first_event_p95_ms": weighted_p95("first_event"),
            "first_content_p50_ms": p50("first_content"),
            "first_content_p95_ms": weighted_p95("first_content"),
            "dispatch_lag_p95_ms": weighted_p95("dispatch_lag"),
            "scheduled_to_terminal_p95_ms": weighted_p95("scheduled_to_terminal"),
            "stream_duration_p50_ms": p50("stream_duration"),
            "resources": record.get("resources_summary"),
            "two_gateways": record.get("two_gateways"),
            "pprof": record.get("pprof"),
            "error": record.get("error"),
            "generators": [{
                "generator": gen.get("generator"), "exit_code": gen.get("exit_code"),
                "c": (gen.get("load_profile") or {}).get("concurrency"),
                "n": (gen.get("load_profile") or {}).get("total_arrivals"),
                "queue": (gen.get("load_profile") or {}).get("admission_queue"),
                "mode": gen.get("mode"),
                "rate": (gen.get("load_profile") or {}).get("planned_rate_per_second"),
                "shape": (gen.get("load_profile") or {}).get("shape"),
                "slow_client": (gen.get("load_profile") or {}).get("slow_client"),
                "status": gen.get("status"),
                "first_content_p50_ms": ns_to_ms(((gen.get("latency") or {}).get("first_content") or {}).get("P50")),
                "first_content_p95_ms": ns_to_ms(((gen.get("latency") or {}).get("first_content") or {}).get("P95")),
                "dispatch_lag_p95_ms": ns_to_ms(((gen.get("latency") or {}).get("dispatch_lag") or {}).get("P95")),
            } for gen in generators],
        })
    experiments = {}
    for row in rows:
        if row["warmup"] or row["error"]:
            continue
        entry = experiments.setdefault(row["experiment"], {"rounds": [], "valid_rounds": [], "invalid_rounds": [],
                                                           "two_gateways": sorted({bool(r["two_gateways"]) for r in rows
                                                                                   if r["experiment"] == row["experiment"]})})
        entry["rounds"].append(row["id"])
        (entry["valid_rounds"] if row["valid"] else entry["invalid_rounds"]).append(row["id"])
    for name, entry in experiments.items():
        selected = [row for row in rows if row["experiment"] == name and row["valid"]
                    and not row["warmup"] and not row["error"]]
        entry["metrics"] = {}
        for metric in ("achieved_start_rate", "achieved_arrival_rate", "completed", "peak_active_sum",
                       "first_event_p50_ms", "first_event_p95_ms", "first_content_p50_ms",
                       "first_content_p95_ms", "dispatch_lag_p95_ms", "scheduled_to_terminal_p95_ms",
                       "wall_seconds"):
            values = [row.get(metric) for row in selected]
            entry["metrics"][metric] = {"n": len([value for value in values if value is not None]),
                                        "median": median(values), "range": value_range(values)}
    return rows, experiments


def ns_to_ms(value):
    return round(value / 1e6, 3) if value else None


def markdown_tables(rows, experiments):
    lines = ["<!-- generated by scripts/capacity-report.py; do not edit by hand -->", "",
             "### All rounds", "",
             "| round | exp | gw | plan /s | arrival /s | start /s | sched | started | completed | rejected | transport | incomplete | dropped | drop % | peak active | first_content p50/p95 ms | dispatch p95 ms | valid |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for row in rows:
        lines.append("| {id}{warm} | {exp} | {gw} | {plan} | {arr} | {start} | {sched} | {started} | {completed} | {rejected} | {transport} | {incomplete} | {dropped} | {droppct} | {peak} | {fc} | {dl} | {valid} |".format(
            id=row["id"], warm=" (warm-up)" if row["warmup"] else "", exp=row["experiment"],
            gw="2" if row["two_gateways"] else "1",
            plan=fmt(row["planned_start_rate"]), arr=fmt(row["achieved_arrival_rate"]),
            start=fmt(row["achieved_start_rate"]), sched=row["scheduled"], started=row["started"],
            completed=row["completed"], rejected=row["rejected"], transport=row["transport_error"],
            incomplete=row["incomplete"], dropped=row["loadgen_dropped"],
            droppct=fmt(row["loadgen_dropped_percent"], 3), peak=row["peak_active_sum"],
            fc=f'{fmt(row["first_content_p50_ms"])}/{fmt(row["first_content_p95_ms"])}',
            dl=fmt(row["dispatch_lag_p95_ms"]),
            valid="yes" if row["valid"] else "**no**"))
    lines += ["", "### Per experiment (valid rounds only)", "",
              "| experiment | rounds | valid | invalid | start rate median [min..max] | arrival rate median [min..max] | completed median [min..max] | peak active median | first_content p50 median | first_content p95 median | dispatch p95 median |",
              "|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, entry in sorted(experiments.items()):
        metrics = entry["metrics"]
        lines.append("| {name} | {rounds} | {valid} | {invalid} | {sr} | {ar} | {cp} | {pa} | {fc50} | {fc95} | {dl} |".format(
            name=name, rounds=len(entry["rounds"]), valid=len(entry["valid_rounds"]),
            invalid=len(entry["invalid_rounds"]),
            sr=fmt_median_range(metrics["achieved_start_rate"]),
            ar=fmt_median_range(metrics["achieved_arrival_rate"]),
            cp=fmt_median_range(metrics["completed"]),
            pa=fmt_median_range(metrics["peak_active_sum"]),
            fc50=fmt_median_range(metrics["first_content_p50_ms"]),
            fc95=fmt_median_range(metrics["first_content_p95_ms"]),
            dl=fmt_median_range(metrics["dispatch_lag_p95_ms"])))
    return "\n".join(lines) + "\n"


def fmt(value, digits=3):
    if value is None:
        return "-"
    if isinstance(value, float):
        return f"{value:.{digits}f}"
    return str(value)


def fmt_median_range(entry):
    if not entry or entry.get("median") is None:
        return "-"
    low, high = entry.get("range") or [None, None]
    return f"{fmt(entry['median'])} [{fmt(low)}..{fmt(high)}]"


def analyze_profiles(work, output, gateway_binary):
    """Turn the collected pprof profiles into committed text evidence."""
    source = work / "profiles"
    target = output / "profiles"
    target.mkdir(parents=True, exist_ok=True)
    written = []
    for profile in sorted(source.glob("*")):
        name = profile.name
        if name.endswith(".txt"):
            text = profile.read_text(errors="replace")
            lines = text.splitlines()
            trimmed = "\n".join(lines[:400])
            if len(lines) > 400:
                trimmed += f"\n... [{len(lines) - 400} more lines in {rel(profile)}]"
            (target / name).write_text(trimmed + "\n")
            written.append(name)
            continue
        if gateway_binary.exists():
            code, out, err = capture(["go", "tool", "pprof", "-top", "-nodecount=25",
                                      str(gateway_binary), str(profile)], timeout=180)
            body = out if code == 0 else f"pprof failed (exit {code}): {err.strip()[:400]}"
        else:
            body = "go tool pprof unavailable: no gateway binary was built for symbolization"
        out_name = name.replace(".pb.gz", ".top.txt")
        (target / out_name).write_text(
            f"# source profile: {rel(profile)}\n# binary: {rel(gateway_binary)}\n\n{body}\n")
        written.append(out_name)
    return written


def publish(ctx, rows, experiments, seed, recon, git_start, extra):
    output, work = ctx["output"], ctx["work"]
    for sub in ("raw", "resources", "profiles"):
        (output / sub).mkdir(parents=True, exist_ok=True)
    write_json(output / "index.json", {
        "run_id": ctx["stack"].run_id, "project": ctx["stack"].project,
        "commit": ctx["stack"].commit, "rounds": rows, "experiments": experiments})
    write_json(output / "summary.json", {
        "run_id": ctx["stack"].run_id, "generated_at": now_utc(),
        "rounds": len(rows), "invalid_rounds": [row["id"] for row in rows if not row["valid"]],
        "failed_rounds": [row["id"] for row in rows if row["error"]],
        "experiments": experiments})
    write_json(output / "reconciliation.json", recon)
    (output / "report-tables.md").write_text(markdown_tables(rows, experiments))
    write_json(output / "timeline.json", {"run_id": ctx["stack"].run_id, "phases": ctx["timeline"]})
    write_json(output / "parameters.json", ctx["parameters"])
    write_json(output / "plan.json", ctx["plan_summary"])
    # compact per-round resource summaries; the full series stays in the work dir
    for record in ctx["records"]:
        summary = record.get("resources_summary") or {}
        write_json(output / "resources" / f"{record['id']}.json", {
            "round": record["id"], "experiment": record.get("experiment"),
            "series_file": record.get("resources_file"), "summary": summary})
    profiles = analyze_profiles(work, output, work / "bin" / "gateway")
    environment = {
        **extra, "run_id": ctx["stack"].run_id, "project": ctx["stack"].project,
        "output_dir": rel(output), "work_dir": rel(work),
        "commit": git_start["commit"], "branch": git_start["branch"],
        "git_start": {key: git_start[key] for key in
                      ("tracked_dirty", "tracked_changes", "tracked_diff_stat", "untracked_count",
                       "untracked_files", "git_status_sha256", "git_diff_head_sha256")},
        "git_end": ctx.get("git_end"), "source_changed_during_run": ctx.get("source_changed_during_run"),
        "isolated": {
            "compose_project": ctx["stack"].project, "ports": ctx["stack"].state["ports"],
            "urls": ctx["stack"].state["urls"], "named_volumes": "project-scoped by -p",
            "containers": ctx["stack"].containers(),
            "images": [{"service": name, "image": image} for name, image in ctx["stack"].images.items()],
            "tokens": {"control_admin_token_prefix": redact(ctx["stack"].control_token),
                       "metering_admin_token_prefix": redact(ctx["stack"].metering_token),
                       "api_key_prefixes": {group: [key[:10] for key in keys]
                                            for group, keys in seed.keys.items()}},
            "seed": seed.info,
        },
        "commands": ctx["stack"].state["commands"],
        "effective_gateway_env": ctx["stack"].state.get("effective_gateway_env"),
        "build_recipe": ctx["stack"].state.get("build_recipe"),
        "generated_dockerfile": rel(ctx["stack"].build_dockerfile) if ctx["stack"].build_dockerfile else None,
        "warnings": ctx["stack"].state["warnings"], "error": ctx.get("error"),
        "compose_logs": ctx.get("compose_logs"),
        "foreign_containers": ctx.get("foreign_containers"),
        "teardown": ctx.get("teardown"), "profiles": profiles,
        "reconciliation_endpoint": (recon or {}).get("metering_endpoint"),
    }
    write_json(output / "environment.json", environment)
    entries = []
    for root, label in ((output, "report"), (work, "work")):
        for path in sorted(root.rglob("*")):
            # A symlink (the run-local DOCKER_CONFIG mirrors the user's
            # cli-plugins) is not this run's artifact: never follow it.
            if path.is_symlink() or not path.is_file() or path.name.endswith(".pyc"):
                continue
            size = path.stat().st_size
            always = root == output and path.name in ALWAYS_COMMITTED
            committed = (root == output and path.name != "manifest.json"
                         and (always or size <= MAX_COMMITTED_BYTES))
            entries.append({
                "path": rel(path), "scope": label, "bytes": size, "sha256": sha256_file(path),
                "committed": committed,
                "note": None if committed else (
                    "kept in the gitignored working directory (build artifact, secret, or over the "
                    "committed-size budget); regenerate with the recorded command"),
            })
    manifest = {"run_id": ctx["stack"].run_id, "generated_at": now_utc(),
                "commit": git_start["commit"], "root": rel(output), "files": entries,
                "committed_bytes": sum(entry["bytes"] for entry in entries if entry["committed"]),
                "total_bytes": sum(entry["bytes"] for entry in entries)}
    write_json(output / "manifest.json", manifest)
    return manifest


# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------

def republish(output, work):
    """Rebuild the committed manifest and the profile text from artifacts on disk.

    The raw series, binaries and profiles live in --work-dir (gitignored); the
    report copy is derived from them. `--republish` re-derives it without
    touching Docker, so a report edit (README, numbers) can refresh manifest.json
    and the sha256 set without re-running a single round.
    """
    environment_path = output / "environment.json"
    environment = json.loads(environment_path.read_text()) if environment_path.exists() else {}
    commit = environment.get("commit", "")
    run_id = environment.get("run_id", "")
    analyze_profiles(work, output, work / "bin" / "gateway")
    for record_path in sorted((output / "raw").glob("*.json")):
        record = json.loads(record_path.read_text())
        write_json(output / "resources" / f"{record['id']}.json", {
            "round": record["id"], "experiment": record.get("experiment"),
            "series_file": record.get("resources_file"), "summary": record.get("resources_summary") or {}})
    entries = []
    for root, label in ((output, "report"), (work, "work")):
        for path in sorted(root.rglob("*")):
            if path.is_symlink() or not path.is_file() or path.name.endswith(".pyc"):
                continue
            size = path.stat().st_size
            always = root == output and path.name in ALWAYS_COMMITTED
            committed = (root == output and path.name != "manifest.json"
                         and (always or size <= MAX_COMMITTED_BYTES))
            entries.append({"path": rel(path), "scope": label, "bytes": size,
                            "sha256": sha256_file(path), "committed": committed,
                            "note": None if committed else (
                                "kept in the gitignored working directory (build artifact, secret, or over "
                                "the committed-size budget); regenerate with the recorded command")})
    manifest = {"run_id": run_id, "generated_at": now_utc(), "commit": commit,
                "root": rel(output), "files": entries,
                "committed_bytes": sum(entry["bytes"] for entry in entries if entry["committed"]),
                "total_bytes": sum(entry["bytes"] for entry in entries)}
    write_json(output / "manifest.json", manifest)
    log(f"republished {rel(output)}: {len(entries)} files, {manifest['committed_bytes']} committed bytes")
    return manifest


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description="T11 capacity runner: isolated Compose project, billing-on load matrix, run-scoped reconciliation.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=("usage: python3 scripts/capacity-report.py --output DIR [--experiments LIST] [--no-build] [--keep]\n"
                "       python3 scripts/capacity-report.py --output data/verification/t11-capacity \\\n"
                "         --work-dir data/benchmarks/t11-capacity --experiments smoke --step-seconds 12"))
    parser.add_argument("--output", required=True, help="report directory (small artifacts are copied here)")
    parser.add_argument("--work-dir", default="data/benchmarks/t11-capacity",
                        help="raw/binary/profile working directory (gitignored)")
    parser.add_argument("--experiments", default="all",
                        help="comma list: all|smoke|up|b01|closed-ladder|open-ladder-s|open-ladder-l|"
                             "gw1v2|wallets|slowclient|overload")
    parser.add_argument("--no-build", action="store_true", help="reuse the pinned images instead of building")
    parser.add_argument("--keep", action="store_true", help="leave the isolated stack running")
    parser.add_argument("--allow-dirty", action="store_true",
                        help="proceed when tracked files differ from HEAD (measurements then do not come from that commit)")
    parser.add_argument("--dry-run", action="store_true",
                        help="render/validate the override and print the plan; touch no Docker state")
    parser.add_argument("--reuse-rounds", action="store_true",
                        help="skip rounds whose raw record already exists in the output directory")
    parser.add_argument("--step-seconds", type=int, default=60, help="holding time per ladder step")
    parser.add_argument("--warmup-seconds", type=int, default=30, help="warm-up duration (0 disables)")
    parser.add_argument("--rounds", type=int, default=3, help="maximum repeats where the matrix allows")
    parser.add_argument("--slow-stop-seconds", type=int, default=30, help="full-stop read phase for the stop point")
    parser.add_argument("--overload-rate", type=float, default=0.0,
                        help="open-loop M rate for the overload phase (0 disables the overload experiment)")
    parser.add_argument("--overload-seconds", type=int, default=180)
    parser.add_argument("--fallback-seconds", type=int, default=180)
    parser.add_argument("--streams", type=int, default=256, help="GATEWAY_MAX_ACTIVE_STREAMS in one-Gateway mode")
    parser.add_argument("--gateway-cpus", type=float, default=4.0, help="compose cpus limit for the gateway")
    parser.add_argument("--max-slots", type=int, default=512, help="generator concurrency slot ceiling")
    parser.add_argument("--image-prefix", default=None, help="pinned image prefix (default llm-gw-cap-<commit8>)")
    parser.add_argument("--include-gw1v2", action="store_true", help="force the one-vs-two-Gateway matrix")
    parser.add_argument("--reconcile-timeout", type=int, default=int(RECONCILE_TIMEOUT))
    parser.add_argument("--republish", action="store_true",
                        help="rebuild manifest.json and profiles/*.txt from the artifacts already on disk; "
                             "runs no Docker and no round")
    parser.add_argument("--key-file", default=None,
                        help="optional: write the run's first hot API key (mode 0600) to this path for manual "
                             "reproduction; never point it inside --output")
    return parser.parse_args(argv)


def plan_summary(plan):
    return [{"id": spec["id"], "experiment": spec["experiment"], "label": spec["label"],
             "target": spec["target"], "planned_rate": spec["planned_rate"],
             "hold_seconds": spec["hold_seconds"], "generators": spec["generators"],
             "two_gateways": spec.get("two_gateways"), "mode_config": spec.get("mode_config"),
             "warmup": bool(spec.get("warmup"))} for spec in plan]


def count_rounds(plan):
    counts = {}
    for spec in plan:
        counts[spec["experiment"]] = counts.get(spec["experiment"], 0) + 1
    return counts


def docker_environment():
    out = {}
    out["server_version"] = capture(["docker", "version", "--format", "{{.Server.Version}}"])[1].strip()
    out["compose_version"] = capture(["docker", "compose", "version", "--short"])[1].strip()
    info = capture(["docker", "info", "--format",
                    "{{.NCPU}}|{{.MemTotal}}|{{.OperatingSystem}}|{{.Driver}}|{{.Architecture}}"])[1].strip()
    parts = (info.split("|") + [""] * 5)[:5]
    out.update({"cpus": parts[0], "memory_bytes": parts[1] if parts[1].isdigit() else None,
                "operating_system": parts[2], "storage_driver": parts[3], "architecture": parts[4]})
    out["go"] = capture(["go", "version"])[1].strip()
    return out


def main(argv=None):
    args = parse_args(argv)
    output = pathlib.Path(args.output).resolve()
    work = pathlib.Path(args.work_dir).resolve()
    work.mkdir(parents=True, exist_ok=True)
    if args.republish:
        # Republishing derives the manifest from artifacts already on disk; it
        # measures nothing, so it must not be blocked by a dirty worktree.
        republish(output, work)
        return 0
    git_start = git_state()
    if git_start["tracked_dirty"] and not args.allow_dirty:
        raise SystemExit(
            "refusing to measure: tracked files differ from HEAD "
            f"({len(git_start['tracked_changes'])} paths)\n  "
            + "\n  ".join(git_start["tracked_changes"][:10])
            + "\ncommit them, or pass --allow-dirty to record that this run is not reproducible from a fixed commit")
    stack = Stack(args, output, work)
    stack.prepare(git_start)
    requests = {name.strip() for name in args.experiments.split(",") if name.strip()}
    if "all" in requests:
        requests = {"b01", "closed-ladder", "open-ladder-s", "open-ladder-l", "gw1v2", "wallets",
                    "slowclient", "overload"}
    smoke = "smoke" in requests
    up_only = requests == {"up"}
    cfg = {
        "step_seconds": max(5, args.step_seconds), "warmup_seconds": 0 if smoke else max(0, args.warmup_seconds),
        "rounds": max(1, args.rounds), "max_slots": max(16, args.max_slots),
        "slow_stop_seconds": args.slow_stop_seconds, "include_gw1v2": "gw1v2" in requests or args.include_gw1v2,
        "overload_rate": args.overload_rate,
        "overload_dynamic": (args.overload_rate == 0 and ("overload" in requests or "all" in args.experiments)),
        "overload_seconds": args.overload_seconds,
        "fallback_seconds": args.fallback_seconds, "smoke": smoke,
    }
    seed = Seed(stack, balance_micro=10 ** 12).placeholder()
    ctx = {"stack": stack, "output": output, "work": work, "reuse": args.reuse_rounds,
           "timeline": [], "parameters": {}, "plan": [], "plan_summary": [], "records": [],
           "error": None}
    results = []
    recon = None
    rows, experiment_summary = [], {}
    try:
        stack.ensure_images() if not args.dry_run else None
        if smoke:
            selected_experiments = {"smoke"}
        elif up_only:
            selected_experiments = set()
        else:
            selected_experiments = requests
        ctx["plan"] = []
        ctx["parameters"] = {
            "run_id": stack.run_id, "commit": git_start["commit"],
            "experiments": sorted(selected_experiments),
            "shortened_for_single_host": {
                "step_seconds": cfg["step_seconds"], "warmup_seconds": cfg["warmup_seconds"],
                "rounds_cap": cfg["rounds"],
                "note": ("the acceptance spec asks for a 60s warm-up, 5min per ladder step, >=3 rounds per step "
                         "and a 60min soak; this run uses the values above on a single Docker Desktop host"),
            },
            "slow_stop_seconds": cfg["slow_stop_seconds"],
            "overload": {"rate": cfg["overload_rate"], "seconds": cfg["overload_seconds"],
                         "fallback_seconds": cfg["fallback_seconds"]},
            "gateway": {"max_active_streams": args.streams, "cpus": args.gateway_cpus,
                        "limiter": {"rate": 100000, "burst": 100000,
                                    "why": "the Demo default of 20 rps would make every ladder a limiter measurement"}},
            "generator": {"binary": rel(stack.loadtest), "max_slots": cfg["max_slots"],
                          "validity_rule": "drops > 0.1% of scheduled arrivals or arrivals < 99% of the plan "
                                           "(open-loop rounds) invalidate the round for capacity conclusions"},
            "seed": {"hot_orgs": 1, "multi_orgs": 10, "balance_micro_per_org": 10 ** 12,
                     "price_micro_per_million_token": 1000000, "model": MODEL},
            "rounds_planned": 0,
        }
        if args.dry_run:
            ctx["plan"] = build_plan(cfg, stack, seed)
            ctx["plan_summary"] = plan_summary(ctx["plan"])
            ctx["parameters"]["rounds_planned"] = len(ctx["plan"])
            stack.render_override(stack.mode(streams=args.streams, two_gateways=cfg["include_gw1v2"]))
            code, _, err = capture(stack.compose("config", "-q", profiles=[PROFILE_MOCKS, PROFILE_MULTI]))
            if code != 0:
                raise RuntimeError(f"generated override does not validate: {err.strip()}")
            log(f"dry run: {len(ctx['plan'])} rounds planned; override validated at {rel(stack.override_path)}")
            for name, count in sorted(count_rounds(ctx["plan"]).items()):
                log(f"  {name}: {count} rounds")
            write_json(output / "plan.json", ctx["plan_summary"])
            return 0

        # ------------------------------------------------------------ up + seed
        ctx["timeline"].append({"phase": "compose_up", "at": now_utc()})
        master_key = stack.ensure_master_key()
        ctx["parameters"]["master_key"] = master_key
        stack.up(stack.mode(streams=args.streams, per_org=0, two_gateways=False), initial=True)
        ctx["timeline"].append({"phase": "stack_ready", "at": now_utc(), "containers": stack.containers()})
        ctx["foreign_containers"] = {"at_stack_ready": stack.foreign_containers()}
        log("building the host load generator and the Linux gateway binary (pprof symbols)")
        run(["go", "build", "-o", str(stack.loadtest), "./cmd/loadtest"], cwd=ROOT, check=True)
        docker_arch = capture(["docker", "info", "--format", "{{.Architecture}}"])[1].strip() or platform.machine()
        arch = GOARCH_BY_DOCKER.get(docker_arch, docker_arch)
        ctx["parameters"]["container_platform"] = {"docker_architecture": docker_arch, "goarch": arch,
                                                  "host_machine": platform.machine()}
        (work / "bin").mkdir(parents=True, exist_ok=True)
        run(["go", "build", "-trimpath", "-o", str(work / "bin" / "gateway"), "./cmd/gateway"],
            cwd=ROOT, env={**os.environ, "GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"}, check=True)
        log("seeding per-run organizations, keys, price version and wallets")
        seed.apply()
        if args.key_file:
            key_path = pathlib.Path(args.key_file).resolve()
            if str(key_path).startswith(str(output)):
                raise RuntimeError("--key-file must not be inside --output: raw keys never enter the report")
            key_path.write_text(seed.key_for("hot", 0) + "\n")
            os.chmod(key_path, 0o600)
            log(f"wrote the first hot API key to {key_path} (0600, outside the report)")
        providers = {
            "primary": {"id": "primary", "protocol": "openai-compatible",
                        "url": "http://mock-primary:18080", "model": MODEL,
                        "timeouts": {"attempt_timeout_ms": 120000}},
            "backup": {"id": "backup", "protocol": "openai-compatible",
                       "url": "http://mock-backup:18081", "model": MODEL},
        }
        applied = configure_control(stack, seed.snapshot_records(), providers, {MODEL: ["primary"]},
                                    expected_gateways=1)
        ctx["timeline"].append({"phase": "configured", "at": now_utc(),
                                "desired_version": applied["desired_version"]})
        log(f"control snapshot applied: desired_version={applied['desired_version']}")
        # The plan carries real per-organization keys, so it must be built after
        # the seed exists (build_plan() resolves seed.key_for(...) eagerly).
        plan = build_plan(cfg, stack, seed)
        ctx["plan"] = [spec for spec in plan
                       if spec["experiment"] in selected_experiments or spec["experiment"] == "warmup"]
        ctx["plan_summary"] = plan_summary(ctx["plan"])
        ctx["parameters"]["rounds_planned"] = len(ctx["plan"])
        log(f"plan: {len(ctx['plan'])} rounds "
            f"({', '.join(f'{name}={count}' for name, count in sorted(count_rounds(ctx['plan']).items()))})")
        if up_only:
            log("stack is up, seeded and configured; --experiments up only (teardown only without --keep)")
            ctx["parameters"]["rounds_planned"] = 0
            return 0

        # --------------------------------------------------------------- rounds
        current = {"streams": args.streams, "per_org": 0, "two_gateways": False,
                   "gateway_cpus": args.gateway_cpus, "gateway_b_cpus": None}
        for spec in ctx["plan"]:
            if spec.get("dynamic_rate") and not ctx.get("knee"):
                knee = observed_m_knee(results)
                if knee:
                    ctx["knee"] = calibrate_overload(ctx["plan"], knee, cfg, cfg["max_slots"])
                    ctx["parameters"]["overload"] = {**ctx["parameters"]["overload"], **ctx["knee"]}
                    log(f"overload calibrated: {ctx['knee']}")
            # A round without an explicit mode_config runs in the default mode;
            # it never inherits whatever mode the previous experiment switched to.
            requested = spec.get("mode_config") or {}
            desired = {
                "streams": requested.get("streams", args.streams),
                "per_org": requested.get("per_org", 0),
                "two_gateways": bool(requested.get("two_gateways", False)),
                "gateway_cpus": requested.get("gateway_cpus", args.gateway_cpus),
                "gateway_b_cpus": requested.get("gateway_b_cpus"),
            }
            spec["mode_config"] = dict(desired)
            if desired != current:
                log(f"switching stack mode: {current} -> {desired}")
                stack.up(stack.mode(**desired))
                configure_control(stack, seed.snapshot_records(), providers, {MODEL: ["primary"]},
                                  expected_gateways=2 if desired["two_gateways"] else 1)
                current = desired
                ctx["timeline"].append({"phase": "mode_switch", "at": now_utc(), "mode": desired})
            run_spec = dict(spec)
            run_spec["two_gateways"] = desired["two_gateways"]
            try:
                record = run_round(ctx, run_spec)
            except Exception as exc:  # noqa: BLE001 - one broken round must not kill the run
                log(f"round {spec['id']} FAILED: {type(exc).__name__}: {exc}")
                record = {"id": spec["id"], "experiment": spec["experiment"], "label": spec["label"],
                          "warmup": bool(spec.get("warmup")), "side": spec.get("side"),
                          "aggregate": {}, "generators": [], "resources_summary": {},
                          "error": f"{type(exc).__name__}: {exc}"}
            results.append(record)
        ctx["records"] = results

        # ------------------------------------------------------ settle + check
        ctx["timeline"].append({"phase": "settling", "at": now_utc()})
        log("waiting for receipt settlement")
        deadline = time.time() + args.reconcile_timeout
        pending = None
        while time.time() < deadline:
            pending = stack.psql_value(
                "SELECT count(*) FROM usage_receipts WHERE status IN ('pending_dispatch','dispatched')")
            if pending in ("0", 0):
                break
            time.sleep(2)
        log(f"unsettled (pending+dispatched) receipts: {pending}")
        endpoint = wait_reconcile(stack, timeout=args.reconcile_timeout)
        ctx["timeline"].append({"phase": "reconciled", "at": now_utc(), "ok": endpoint["ok"]})
        recon = reconcile(ctx, seed, results)
        recon["metering_endpoint"] = {
            "ok": endpoint["ok"], "attempts": endpoint.get("attempts"),
            "pending_messages": ((endpoint.get("response") or {}).get("report") or {}).get("pending_messages"),
            "consistent": ((endpoint.get("response") or {}).get("report") or {}).get("consistent"),
            "detail": endpoint.get("detail"),
            "unsettled_receipts_before_endpoint": pending,
        }
        rows, experiment_summary = summarize_experiments(results)
        ctx.setdefault("foreign_containers", {})["at_end"] = stack.foreign_containers()
        ctx["timeline"].append({"phase": "reported", "at": now_utc()})
    except Exception as exc:  # noqa: BLE001 - artefacts are still written
        ctx["error"] = f"{type(exc).__name__}: {exc}"
        log(f"ERROR: {ctx['error']}")
    finally:
        ctx["records"] = results
        if not args.dry_run and stack.state.get("up_started"):
            ctx["compose_logs"] = stack.capture_logs()
        if args.keep:
            ctx["teardown"] = {"skipped": True, "reason": "--keep",
                               "manual_command": quote(stack.compose(
                                   "down", "-v", "--remove-orphans",
                                   profiles=[PROFILE_MOCKS, PROFILE_MULTI]))}
        elif not args.dry_run and stack.state.get("up_started"):
            ctx["teardown"] = stack.down()
            log(f"teardown: {ctx['teardown']}")
        git_end = git_state()
        ctx["git_end"] = {key: git_end[key] for key in
                          ("tracked_dirty", "tracked_changes", "untracked_count",
                           "git_status_sha256", "git_diff_head_sha256")}
        ctx["source_changed_during_run"] = git_end["git_status_sha256"] != git_start["git_status_sha256"]
        if not args.dry_run:
            if not rows:
                rows, experiment_summary = summarize_experiments(results)
            if recon is None:
                recon = {"ok": False, "note": "reconciliation did not run (the run failed before it)"}
            manifest = publish(ctx, rows, experiment_summary, seed, recon, git_start, {
                "host": {"platform": platform.platform(), "machine": platform.machine(),
                         "python": platform.python_version(), "cpu_count": os.cpu_count(),
                         "kernel": platform.release()},
                "docker": docker_environment(),
                "tested_surface": "GATEWAY_BILLING=pg + GATEWAY_ACCESS_GUARD=pg (the billable path)",
                "notes": stack.state["notes"],
            })
            log(f"report written to {rel(output)} ({len(rows)} rounds, "
                f"{manifest['committed_bytes']} committed bytes)")
    if ctx["error"]:
        log(f"run FAILED: {ctx['error']}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
