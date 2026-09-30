#!/usr/bin/env python3
"""Isolated low-load baseline: private Compose project, probed ports, per-run artefacts.

Why this runner exists next to scripts/benchmark-local.py:

  * benchmark-local.py drives the *daily* Compose project (the default project name
    is derived from the checkout directory) and its fixed host ports 8090/8091/
    18080/18081. That collides with a running daily stack, with demo-accept and
    with concurrent agent runs, and it mutates the daily Demo configuration.
  * This runner derives a private project name from the HEAD commit plus a UTC
    timestamp, republishes every service on a freshly probed free port, and keeps
    every artifact under --output.

Isolation model:

  * Compose project name  -> namespaces containers, networks and *named volumes*.
    Volumes such as pg-data/redis-data/metering-spool are automatically prefixed
    with the project name, so `docker compose -p <project> ... down -v` can only
    ever remove this run's state. No volume override is needed.
  * Host ports            -> one free port is probed per published port with
    socket(); the base-file defaults (8090/8091/50051/50052/5433/6379) are never
    bound by this project. `ports: !override` (Compose >= 2.24.4) *replaces* the
    base list instead of appending to it.
  * Per-run secrets       -> random CONTROL_ADMIN_TOKEN / METERING_ADMIN_TOKEN are
    written into the override file (mode 0600) and never printed in full.
  * Limiter               -> the isolated gateway runs with a deliberately huge
    token bucket so the Demo default (20 rps) cannot distort the comparison.

This is a W1 *low-load* baseline, not a capacity test. See README.md in the output
directory for the measured numbers and their validity limits.
"""

import argparse
import hashlib
import json
import os
import pathlib
import platform
import re
import secrets
import shlex
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASE_COMPOSE_NAME = "docker-compose.yml"
BASE_COMPOSE = ROOT / BASE_COMPOSE_NAME
PROFILE = "demo-mocks"
PROJECT_PREFIX = "llm-gw-iso-"
ROUNDS = 3
SAMPLE_INTERVAL_SECONDS = 0.5
RECONCILE_TIMEOUT_SECONDS = 60
METRICS_NAMES = (
    "process_cpu_seconds_total",
    "process_resident_memory_bytes",
    "go_memstats_heap_alloc_bytes",
    "go_gc_duration_seconds_count",
)

# (service, container_port, role): exactly one free host port is probed per entry.
# `control` exposes two ports (gRPC + HTTP), so it gets two probes.
PUBLISHED_PORTS = (
    ("control", 50051, "control_grpc"),
    ("control", 50052, "control_http"),
    ("metering", 8091, "metering"),
    ("gateway", 8090, "gateway"),
    ("mock-primary", 18080, "mock_primary"),
    ("mock-backup", 18081, "mock_backup"),
    ("postgres", 5432, "postgres"),
    ("redis", 6379, "redis"),
)

# Low-load W1 matrix. (case id, mode, concurrency, requests, arrival rate)
LOAD_CASES = (
    ("closed-c1-n60", "closed", 1, 60, 0),
    ("closed-c4-n120", "closed", 4, 120, 0),
    ("fixed-c8-n120-rate20", "fixed", 8, 120, 20),
)

LIMITER_ENV = {"GATEWAY_LIMITER_RATE": "100000", "GATEWAY_LIMITER_BURST": "100000"}

# The Dockerfile runs `RUN go mod download` before `COPY . .`, so a build host that
# cannot reach proxy.golang.org fails there. When that happens the runner retries
# once with a generated copy of the same Dockerfile that only adds an explicit
# GOPROXY; the retry is recorded in environment.json (`build_recipe`).
FALLBACK_GOPROXY = "https://goproxy.cn,direct"
DEFAULT_BUILD_SERVICES = ("control", "gateway", "metering", "migrate", "mock-backup", "mock-primary")


def log(message):
    print(f"[iso] {message}", flush=True)


def run(args, **kwargs):
    kwargs.setdefault("cwd", ROOT)
    return subprocess.run(args, **kwargs)


def capture(args):
    """Run a command, return (returncode, stdout, stderr) without raising."""
    proc = subprocess.run(args, cwd=ROOT, capture_output=True, text=True,
                          encoding="utf-8", errors="replace")
    return proc.returncode, proc.stdout, proc.stderr


def capture_out(args):
    code, out, err = capture(args)
    if code != 0:
        raise RuntimeError(f"{' '.join(args)} exited {code}: {err.strip()}")
    return out


def quote(args):
    return " ".join(shlex.quote(str(item)) for item in args)


def sha256_hex(data):
    if isinstance(data, str):
        data = data.encode()
    return hashlib.sha256(data).hexdigest()


def write_json(path, payload):
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, sort_keys=False) + "\n")


def redact(token, keep=6):
    if not token:
        return ""
    return token[:keep] + "\u2026"


def rel_or_abs(path):
    path = pathlib.Path(path).resolve()
    try:
        return str(path.relative_to(ROOT))
    except ValueError:
        return str(path)


def free_port(taken):
    """Probe one free loopback port by binding to port 0 (one probe per published port)."""
    for _ in range(50):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        if port in taken:
            continue
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as check:
            if check.connect_ex(("127.0.0.1", port)) == 0:
                continue  # somebody grabbed it between bind and close
        taken.add(port)
        return port
    raise RuntimeError("could not allocate a free loopback port")


def host_memory_bytes():
    code, out, _ = capture(["sysctl", "-n", "hw.memsize"])
    if code == 0 and out.strip().isdigit():
        return int(out.strip())
    try:
        with open("/proc/meminfo") as handle:
            for line in handle:
                if line.startswith("MemTotal:"):
                    return int(line.split()[1]) * 1024
    except OSError:
        pass
    return None


def git_state():
    commit = capture_out(["git", "rev-parse", "HEAD"]).strip()
    status = capture_out(["git", "status", "--short"])
    diff_unstaged = capture_out(["git", "diff"])
    diff_all = capture_out(["git", "diff", "HEAD"])
    files = [line for line in status.splitlines() if line.strip()]
    return {
        "commit": commit,
        "dirty": bool(files),
        "dirty_file_count": len(files),
        "dirty_files": files[:200],
        "git_status_sha256": sha256_hex(status),
        "git_diff_sha256": sha256_hex(diff_unstaged),
        "git_diff_head_sha256": sha256_hex(diff_all),
    }


def http_json(url, token=None, timeout=10):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            return response.status, json.loads(raw.decode() or "{}")
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            body = json.loads(raw.decode() or "{}")
        except ValueError:
            body = {"raw": raw.decode(errors="replace")[:500]}
        return exc.code, body
    except Exception as exc:  # connection refused while the stack is still starting
        return None, {"error": str(exc)}


def sample_metrics(url):
    try:
        with urllib.request.urlopen(url, timeout=2) as response:
            lines = response.read().decode().splitlines()
        return {
            key: float(value)
            for line in lines
            if line and not line.startswith("#") and "{" not in line
            for key, value in [line.split()[:2]]
            if key in METRICS_NAMES
        }
    except Exception as exc:
        return {"sample_error": str(exc)}


def render_override(ports_by_service, env_by_service, run_id, commit, notes,
                    build_dockerfile=None, build_services=()):
    lines = [
        "# Generated by scripts/isolated-baseline.py -- do not edit by hand.",
        f"# run_id: {run_id}",
        f"# commit: {commit}",
        "#",
        "# Host-port isolation: every published port below is a free port probed with",
        "# socket() at run time; the base-file defaults (8090/8091/50051/50052/5433/",
        "# 6379) are never bound by this project. `!override` replaces the base `ports`",
        "# list instead of appending to it (requires Compose >= 2.24.4).",
        "#",
        "# Volumes need no override: Compose scopes named volumes to the project name",
        "# (`docker compose -p <project>`), so pg-data/redis-data/metering-spool become",
        "# <project>_pg-data etc. and `down -v` can only remove this run's state.",
        "#",
        "# Secrets below are per-run random values; this file is written mode 0600.",
    ]
    lines.extend(f"# note: {note}" for note in notes)
    if build_dockerfile:
        lines.append("#")
        lines.append("# `build.dockerfile` below points at a generated copy of the repository")
        lines.append("# Dockerfile that only adds an explicit GOPROXY: this host cannot reach")
        lines.append("# proxy.golang.org from inside the build container. It is byte-identical to")
        lines.append(f"# the repository Dockerfile otherwise (see {pathlib.Path(build_dockerfile).name}).")
    lines.append("services:")
    for service in sorted(set(ports_by_service) | set(env_by_service) | set(build_services)):
        lines.append(f"  {service}:")
        if service in ports_by_service:
            lines.append("    ports: !override")
            for host_port, container_port in ports_by_service[service]:
                lines.append(f'      - "127.0.0.1:{host_port}:{container_port}"')
        if service in env_by_service:
            lines.append("    environment:")
            for key, value in env_by_service[service].items():
                lines.append(f'      {key}: "{value}"')
        if build_dockerfile and service in build_services:
            lines.append("    build:")
            lines.append(f"      dockerfile: {json.dumps(build_dockerfile)}")
    return "\n".join(lines) + "\n"


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
    patched = text.replace(marker, f'ENV GOPROXY={goproxy}\n{marker}', 1)
    pathlib.Path(destination).write_text(patched)
    return destination


def parse_mock_behaviour(config, service):
    """Read the effective mock flags out of the resolved compose config."""
    defaults = {
        "mock-primary": {"ttft": "20ms", "token_interval": "15ms", "tokens": 8},
        "mock-backup": {"ttft": "80ms", "token_interval": "15ms", "tokens": 8},
    }[service]
    command = (config.get("services", {}).get(service) or {}).get("command") or []
    if isinstance(command, str):
        command = command.split()
    behaviour = dict(defaults)
    for index, item in enumerate(command):
        if item == "-ttft" and index + 1 < len(command):
            behaviour["ttft"] = command[index + 1]
        elif item == "-token-interval" and index + 1 < len(command):
            behaviour["token_interval"] = command[index + 1]
        elif item == "-tokens" and index + 1 < len(command):
            behaviour["tokens"] = int(command[index + 1])
        elif item == "-name" and index + 1 < len(command):
            behaviour["name"] = command[index + 1]
    return behaviour


def resolve_config(compose, env):
    code, out, err = capture(compose + ["config", "--format", "json"])
    if code != 0:
        return None, err.strip()
    try:
        return json.loads(out), None
    except ValueError as exc:
        return None, f"config --format json was not JSON: {exc}"


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description="Isolated low-load baseline (direct vs gateway) on a private Compose project.",
    )
    parser.add_argument("--output", required=True, help="output directory, e.g. data/benchmarks/isolated-<run_id>")
    parser.add_argument("--no-build", action="store_true", help="reuse locally built images")
    parser.add_argument("--keep", action="store_true", help="do not tear the stack down at the end")
    parser.add_argument("--allow-dirty", action="store_true", help="proceed even if the worktree is dirty (measurements then do not come from a fixed commit)")
    parser.add_argument("--dry-run", action="store_true", help="do everything except `docker compose up` and the load runs; validate the generated override")
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    output = pathlib.Path(args.output).resolve()
    if output.exists() and any(output.iterdir()) and not (output / "environment.json").exists():
        raise SystemExit(f"{output} already exists and is not an isolated-baseline output directory; refusing to mix artifacts")

    state = {
        "started_unix": time.time(),
        "commit": None,
        "dirty": None,
        "dirty_file_count": None,
        "run_id": None,
        "project": None,
        "ports": {},
        "urls": {},
        "commands": {},
        "invalid_reasons": [],
        "warnings": [],
        "runs": [],
        "dry_run": args.dry_run,
        "keep": args.keep,
        "no_build": args.no_build,
        "allow_dirty": args.allow_dirty,
        "error": None,
        "up_started": False,
        "teardown": {"skipped": True, "reason": "not attempted"},
        "source_changed_during_run": False,
        "reconciliation": None,
        "api_key_prefix": None,
        "images": [],
        "volumes": [],
        "notes": [],
    }

    # ---------------------------------------------------------------- 1. guard
    git_start = git_state()
    commit = git_start["commit"]
    if git_start["dirty"] and not args.allow_dirty:
        raise SystemExit(
            "refusing to measure a dirty worktree: commit the tree first, or pass --allow-dirty "
            f"to accept that this run is not reproducible from a fixed commit "
            f"({git_start['dirty_file_count']} modified/untracked paths)"
        )
    if git_start["dirty"]:
        state["warnings"].append(
            f"worktree dirty ({git_start['dirty_file_count']} paths); measurements are not from a fixed commit"
        )
    for sub in ("raw", "resources"):
        (output / sub).mkdir(parents=True, exist_ok=True)
    # Run-local Docker config: an empty config.json means no credential helper is
    # ever consulted (the macOS helper errors out in this sandbox), and buildx
    # state lands under the output directory instead of ~/.docker. CLI plugins
    # (docker-compose, docker-buildx) are discovered under $DOCKER_CONFIG, so they
    # are mirrored from the user config directory.
    user_docker_config = pathlib.Path(os.environ.get("DOCKER_CONFIG") or (pathlib.Path.home() / ".docker"))
    docker_config = output / ".docker"
    docker_config.mkdir(parents=True, exist_ok=True)
    (docker_config / "config.json").write_text("{}\n")
    plugins_source = user_docker_config / "cli-plugins"
    if plugins_source.is_dir():
        plugins_target = docker_config / "cli-plugins"
        plugins_target.mkdir(exist_ok=True)
        for plugin in plugins_source.iterdir():
            link = plugins_target / plugin.name
            if not link.exists():
                link.symlink_to(plugin.resolve())
    os.environ["DOCKER_CONFIG"] = str(docker_config)

    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_id = f"{stamp}-{commit[:8]}"
    project = re.sub(r"[^a-z0-9-]", "", f"{PROJECT_PREFIX}{run_id}".lower())
    if not project.startswith(PROJECT_PREFIX):
        raise SystemExit(f"refusing project name {project!r}: must start with {PROJECT_PREFIX!r}")
    control_token = secrets.token_urlsafe(24)
    metering_token = secrets.token_urlsafe(24)
    state["run_id"] = run_id
    state["project"] = project
    state["commit"] = commit
    state["dirty"] = git_start["dirty"]
    state["dirty_file_count"] = git_start["dirty_file_count"]
    state["tokens"] = {
        "control_admin_token_prefix": redact(control_token),
        "metering_admin_token_prefix": redact(metering_token),
        "note": "per-run random tokens; full values only in compose-override.yml (mode 0600)",
    }

    log(f"run_id={run_id} project={project} output={output}")
    if not args.dry_run:
        log("NOT a capacity test: 3 low-load cases x 3 rounds, single host")

    # ------------------------------------------------------- 2. isolation plan
    taken = set()
    by_service = {}
    for service, container_port, role in PUBLISHED_PORTS:
        port = free_port(taken)
        state["ports"][role] = {"service": service, "container_port": container_port, "host_port": port}
        by_service.setdefault(service, []).append((port, container_port))

    def render():
        env_overrides = {
            "control": {"CONTROL_ADMIN_TOKEN": control_token},
            "metering": {"METERING_ADMIN_TOKEN": metering_token},
            "gateway": dict(LIMITER_ENV),
        }
        return render_override(by_service, env_overrides, run_id, commit, state["notes"],
                               state.get("build_dockerfile"), state.get("build_services", ()))

    override_path = output / "compose-override.yml"
    override_path.write_text(render())
    os.chmod(override_path, 0o600)
    override_arg = rel_or_abs(override_path)

    compose = [
        "docker", "compose",
        "-p", project,
        "-f", BASE_COMPOSE_NAME,
        "-f", override_arg,
        "--profile", PROFILE,
    ]
    compose_env = {
        **os.environ,
        "HOST_UID": str(os.getuid()),
        "HOST_GID": str(os.getgid()),
        "CONTROL_ADMIN_TOKEN": control_token,
        "METERING_ADMIN_TOKEN": metering_token,
    }
    state["commands"]["compose_config_validate"] = quote(compose + ["config", "-q"])
    up_flags = ["up", "-d", "--pull", "never", "--no-build" if args.no_build else "--build", "--wait"]
    state["commands"]["compose_up"] = quote(compose + up_flags)
    state["commands"]["compose_down"] = quote(compose + ["down", "-v", "--remove-orphans"])
    state["commands"]["compose_logs"] = quote(compose + ["logs", "--no-color", "--tail", "5000"])
    state["compose_env"] = {
        "HOST_UID": compose_env["HOST_UID"],
        "HOST_GID": compose_env["HOST_GID"],
        "CONTROL_ADMIN_TOKEN": redact(control_token),
        "METERING_ADMIN_TOKEN": redact(metering_token),
    }

    code, out, err = capture(compose + ["config", "-q"])
    if code != 0:
        raise RuntimeError(f"generated compose override does not validate: {err.strip()}")
    log(f"compose override validated: {override_arg}")

    resolved, resolve_error = resolve_config(compose, compose_env)
    if resolved is None:
        state["warnings"].append(f"could not read `config --format json` ({resolve_error}); skipping effective-config assertions")
    else:
        # If the gateway moves /metrics to a separate diagnostics listener, publish it
        # too so the resource sampler keeps working (T01A parallel workstream).
        gateway_env = (resolved.get("services", {}).get("gateway") or {}).get("environment") or {}
        diag = str(gateway_env.get("GATEWAY_DIAGNOSTICS_ADDR") or "").strip()
        match = re.search(r":(\d+)$", diag)
        if diag and match:
            diag_port = int(match.group(1))
            host_diag = free_port(taken)
            by_service.setdefault("gateway", []).append((host_diag, diag_port))
            state["ports"]["gateway_diagnostics"] = {
                "service": "gateway", "container_port": diag_port, "host_port": host_diag,
            }
            state["notes"].append(f"gateway diagnostics listener {diag} republished on 127.0.0.1:{host_diag}")
            override_path.write_text(render())
            os.chmod(override_path, 0o600)
            code, out, err = capture(compose + ["config", "-q"])
            if code != 0:
                raise RuntimeError(f"generated compose override does not validate after diagnostics port: {err.strip()}")
            resolved, resolve_error = resolve_config(compose, compose_env)
            if resolved is None:
                raise RuntimeError(f"could not re-read `config --format json`: {resolve_error}")

    effective_ports = {}
    if resolved is not None:
        for service, container_port, role in PUBLISHED_PORTS:
            published = None
            for entry in (resolved.get("services", {}).get(service) or {}).get("ports") or []:
                if int(entry.get("target", 0)) == container_port:
                    published = int(entry.get("published"))
            expected = state["ports"][role]["host_port"]
            if published != expected:
                raise RuntimeError(f"{service}:{container_port} resolved to host port {published}, expected {expected}")
            effective_ports[role] = published
        gateway_env = (resolved.get("services", {}).get("gateway") or {}).get("environment") or {}
        if gateway_env.get("GATEWAY_LIMITER_RATE") != LIMITER_ENV["GATEWAY_LIMITER_RATE"]:
            raise RuntimeError("gateway limiter override did not apply to the resolved config")
        state["resolved_service_names"] = sorted(resolved.get("services", {}).keys())
        state["mock_behaviour"] = {
            "primary": parse_mock_behaviour(resolved, "mock-primary"),
            "backup": parse_mock_behaviour(resolved, "mock-backup"),
        }
    else:
        state["mock_behaviour"] = {
            "primary": parse_mock_behaviour({}, "mock-primary"),
            "backup": parse_mock_behaviour({}, "mock-backup"),
        }

    urls = {
        "control_http": f"http://127.0.0.1:{state['ports']['control_http']['host_port']}",
        "gateway": f"http://127.0.0.1:{state['ports']['gateway']['host_port']}",
        "metering": f"http://127.0.0.1:{state['ports']['metering']['host_port']}",
        "mock_primary": f"http://127.0.0.1:{state['ports']['mock_primary']['host_port']}",
        "mock_backup": f"http://127.0.0.1:{state['ports']['mock_backup']['host_port']}",
        "metrics": f"http://127.0.0.1:{state['ports'].get('gateway_diagnostics', state['ports']['gateway'])['host_port']}/metrics",
    }
    state["urls"] = urls

    state["parameters"] = {
        "run_id": run_id,
        "project": project,
        "low_load_baseline": True,
        "rounds": ROUNDS,
        "sample_interval_seconds": SAMPLE_INTERVAL_SECONDS,
        "cases": [
            {"id": case_id, "mode": mode, "concurrency": concurrency, "requests": requests, "rate_qps": rate}
            for case_id, mode, concurrency, requests, rate in LOAD_CASES
        ],
        "sides": {
            "direct": {"target": urls["mock_primary"] + "/v1/chat/completions", "api_key": "none (upstream mock has no auth)"},
            "gateway": {"target": urls["gateway"] + "/v1/chat/completions", "api_key": "per-run tenant-iso key from <output>/api-key"},
        },
        "loadtest_request": {"model": "default", "stream": True, "messages": [{"role": "user", "content": "hi"}]},
        "mock_behaviour": state["mock_behaviour"],
        "limiter": {**LIMITER_ENV, "why": "overrides the Demo default GATEWAY_LIMITER_RATE=20 so the rate limit cannot distort the comparison"},
        "metering_auth": "per-run METERING_ADMIN_TOKEN; /reconcile polled with Bearer",
        "postgres_redis": "fresh project-scoped volumes (empty schema + empty Redis) for every run",
    }
    write_json(output / "parameters.json", state["parameters"])

    if args.dry_run:
        log("dry run: generated override, ports and artefacts; skipping master key, go build, compose up, load runs, reconciliation and teardown")
        print()
        print(f"  project        : {project}")
        print(f"  output         : {output}")
        print(f"  override       : {override_arg}")
        print(f"  compose up     : {state['commands']['compose_up']}")
        print(f"  compose down   : {state['commands']['compose_down']}")
        for role, info in state["ports"].items():
            print(f"  port {role:<19} 127.0.0.1:{info['host_port']} -> {info['service']}:{info['container_port']}")
        print(f"  control http   : {urls['control_http']}")
        print(f"  gateway        : {urls['gateway']}")
        print(f"  metering       : {urls['metering']}")
        print(f"  metrics        : {urls['metrics']}")
        print(f"  api key file   : {output / 'api-key'} (direct uses no key)")
        print(f"  raw results    : {output / 'raw'}/<side>-<case>-r<round>.json")
        print(f"  resources      : {output / 'resources'}/gateway-<case>-r<round>.json")
        print(f"  reconciliation : {output / 'reconciliation.json'}")
        state["teardown"] = {"skipped": True, "reason": "--dry-run"}
        write_json(output / "environment.json", build_environment(state, args, output, git_start))
        write_json(output / "index.json", build_index(state, output, valid=True))
        write_readme(output, state, args)
        log("dry run OK (nothing was created in Docker)")
        return 0

    # ----------------------------------------------------------- 3. start stack
    environment = build_environment(state, args, output, git_start)
    write_json(output / "environment.json", environment)

    try:
        if "provider-credential-master-key" in BASE_COMPOSE.read_text():
            key_path = ROOT / "data/provider-credential-master-key"
            if key_path.exists() and not key_path.is_file():
                raise RuntimeError(f"{key_path} exists but is not a regular file; remove it and re-run")
            if not key_path.is_file():
                log("bootstrapping local provider credential master key")
                with (output / "master-key.log").open("w") as handle:
                    run(["go", "run", "./cmd/democtl", "ensure-master-key"], stdout=handle, stderr=subprocess.STDOUT, check=True)

        log("building ./cmd/loadtest")
        build_cmd = ["go", "build", "-o", str(output / "loadtest"), "./cmd/loadtest"]
        state["commands"]["loadtest_build"] = quote(build_cmd)
        run(build_cmd, check=True)

        log(f"compose up ({'reusing images' if args.no_build else 'building images'})")
        state["up_started"] = True
        with (output / "compose-up.log").open("w") as handle:
            proc = run(compose + up_flags, env=compose_env, stdout=handle, stderr=subprocess.STDOUT)
            state["compose_up_exit"] = proc.returncode
        state.setdefault("build_recipe", "stock Dockerfile" if not args.no_build else "reused images (--no-build)")
        if proc.returncode != 0 and not args.no_build and module_proxy_failure(output / "compose-up.log"):
            # The Dockerfile downloads modules before COPY . .; if the build host
            # cannot reach proxy.golang.org, retry once with a generated copy of
            # the same Dockerfile that only pins a reachable GOPROXY.
            log(f"stock build could not reach the Go module proxy; retrying once with GOPROXY={FALLBACK_GOPROXY}")
            state["warnings"].append(
                "stock `go mod download` could not reach proxy.golang.org from the build container; "
                f"the images were rebuilt from a generated copy of Dockerfile with GOPROXY={FALLBACK_GOPROXY}"
            )
            state["build_recipe"] = f"generated Dockerfile copy with GOPROXY={FALLBACK_GOPROXY}"
            build_names = [name for name, service in (resolved or {}).get("services", {}).items() if service.get("build")]
            state["build_services"] = sorted(build_names or DEFAULT_BUILD_SERVICES)
            dockerfile = write_goproxy_dockerfile(ROOT / "Dockerfile", output / "Dockerfile.iso", FALLBACK_GOPROXY)
            state["build_dockerfile"] = str(dockerfile)
            state["commands"]["build_dockerfile"] = rel_or_abs(dockerfile)
            override_path.write_text(render())
            os.chmod(override_path, 0o600)
            code, _, err = capture(compose + ["config", "-q"])
            if code != 0:
                raise RuntimeError(f"compose override with the generated Dockerfile does not validate: {err.strip()}")
            state["commands"]["compose_up_retry"] = quote(compose + up_flags)
            with (output / "compose-up-retry.log").open("w") as handle:
                proc = run(compose + up_flags, env=compose_env, stdout=handle, stderr=subprocess.STDOUT)
            state["compose_up_exit"] = proc.returncode
        if proc.returncode != 0:
            raise RuntimeError(f"docker compose up failed (exit {proc.returncode}); see {output / 'compose-up.log'}")

        collect_inventory(state, compose, compose_env, project)

        # ------------------------------------------------------- 4. configure
        configure_cmd = [
            "go", "run", "./cmd/democtl", "configure",
            "-control", urls["control_http"],
            "-gateway", urls["gateway"],
            "-metering", urls["metering"],
            "-admin-token", control_token,
            "-primary-url", "http://mock-primary:18080",
            "-backup-url", "http://mock-backup:18081",
            "-tenant", "tenant-iso",
            "-key-file", str(output / "api-key"),
        ]
        recorded_configure = list(configure_cmd)
        recorded_configure[recorded_configure.index("-admin-token") + 1] = redact(control_token)
        state["commands"]["configure"] = quote(recorded_configure)
        log("configuring the isolated control plane")
        with (output / "configure.log").open("w") as handle:
            proc = run(configure_cmd, stdout=handle, stderr=subprocess.STDOUT)
            state["configure_exit"] = proc.returncode
        if proc.returncode != 0:
            tail = (output / "configure.log").read_text()[-2000:]
            raise RuntimeError(f"democtl configure failed (exit {proc.returncode}):\n{tail}")

        api_key = (output / "api-key").read_text().strip()
        if not api_key:
            raise RuntimeError("democtl configure wrote an empty API key")
        state["api_key_prefix"] = redact(api_key)
        os.chmod(output / "api-key", 0o600)
        log(f"configured tenant-iso, API key prefix {redact(api_key)}")

        # ----------------------------------------------------- 5. load cases
        binary = output / "loadtest"
        for side in ("direct", "gateway"):
            target = (urls["mock_primary"] if side == "direct" else urls["gateway"]) + "/v1/chat/completions"
            key = "" if side == "direct" else api_key
            for case_id, mode, concurrency, requests, rate in LOAD_CASES:
                for round_no in range(1, ROUNDS + 1):
                    record = run_case(state, output, binary, side, case_id, mode, concurrency,
                                      requests, rate, round_no, target, key, urls["metrics"])
                    state["runs"].append(record)
                    if not record["valid"]:
                        reason = (f"{record['case']} r{round_no}: success={record['success']} "
                                  f"total={record['total']} requests={requests} exit={record['exit_code']}")
                        state["invalid_reasons"].append(reason)
                        log(f"INVALID: {reason} -- stopping the load matrix early")
                        break
                if state["invalid_reasons"]:
                    break
            if state["invalid_reasons"]:
                break

        # -------------------------------------------------- 6. reconciliation
        log("waiting for metering reconciliation")
        state["reconciliation"] = wait_for_reconciliation(urls["metering"] + "/reconcile", metering_token, state)
        write_json(output / "reconciliation.json", state["reconciliation"]["response"])
        if state["reconciliation"]["ok"]:
            log("reconciliation ok, pending_messages=0")
        else:
            reason = f"reconciliation did not settle: {state['reconciliation']['detail']}"
            state["invalid_reasons"].append(reason)
            log(f"WARNING: {reason}")
    except Exception as exc:  # noqa: BLE001 - recorded, artefacts still written
        state["error"] = f"{type(exc).__name__}: {exc}"
        log(f"ERROR: {state['error']}")
    finally:
        finalize(state, args, output, git_start, compose, compose_env, project)

    write_json(output / "parameters.json", state["parameters"])
    write_json(output / "environment.json", build_environment(state, args, output, git_start))
    write_json(output / "index.json", build_index(state, output, valid=not state["invalid_reasons"] and not state["error"]))
    write_readme(output, state, args)
    write_manifest(output)

    if state["error"]:
        log(f"run FAILED: {state['error']}")
        return 1
    if state["invalid_reasons"]:
        for reason in state["invalid_reasons"]:
            log(f"invalid: {reason}")
        return 3
    log(f"run complete; artefacts in {output}")
    if not args.keep:
        log("teardown verified: no containers or volumes left under llm-gw-iso")
    return 0


def run_case(state, output, binary, side, case_id, mode, concurrency, requests, rate,
             round_no, target, key, metrics_url):
    stem = f"{side}-{case_id}-r{round_no}"
    result_path = output / "raw" / f"{stem}.json"
    log_path = output / "raw" / f"{stem}.log"
    argv = [
        str(binary), "-target", target,
        "-c", str(concurrency), "-n", str(requests), "-rate", str(rate),
        "-json", str(result_path), "-key", key,
    ]
    samples = []
    stop = threading.Event()
    sampler = None
    if side == "gateway":
        sampler = threading.Thread(target=sample_loop, args=(metrics_url, samples, stop), daemon=True)
        sampler.start()
    log(f"{stem}: c={concurrency} n={requests} rate={rate} -> {target}")
    started = time.time()
    with log_path.open("w") as handle:
        proc = subprocess.run(argv, cwd=ROOT, stdout=handle, stderr=subprocess.STDOUT)
    wall_seconds = time.time() - started
    if sampler is not None:
        stop.set()
        sampler.join(timeout=5)
        samples.append({"unix_time": time.time(), "elapsed_seconds": wall_seconds, "gateway": sample_metrics(metrics_url)})
        write_json(output / "resources" / f"{stem}.json", {
            "case": stem,
            "side": side,
            "round": round_no,
            "metrics_url": metrics_url,
            "interval_seconds": SAMPLE_INTERVAL_SECONDS,
            "wall_seconds": wall_seconds,
            "samples": samples,
        })

    record = {
        "case": f"{side}-{case_id}",
        "side": side,
        "case_id": case_id,
        "mode": mode,
        "round": round_no,
        "concurrency": concurrency,
        "requests": requests,
        "rate_qps": rate,
        "target": target,
        "exit_code": proc.returncode,
        "wall_seconds": wall_seconds,
        "raw_result": rel_or_abs(result_path),
        "raw_log": rel_or_abs(log_path),
        "resources": rel_or_abs(output / "resources" / f"{stem}.json") if side == "gateway" else None,
        "valid": False,
    }
    if not result_path.exists():
        record["failure"] = f"loadtest exited {proc.returncode} without writing a result"
        log(f"{stem}: FAILED - {record['failure']}")
        return record
    data = json.loads(result_path.read_text())
    report = data.get("report", {})
    record.update({
        "success": report.get("Success"),
        "total": report.get("Total"),
        "errors": report.get("Errors"),
        "status_codes": report.get("Status"),
        "elapsed_seconds": data.get("elapsed_seconds"),
        "throughput_rps": data.get("success_per_second"),
        "ttft_p50_ms": ns_to_ms(report.get("TTFT", {}).get("P50")),
        "ttft_p95_ms": ns_to_ms(report.get("TTFT", {}).get("P95")),
        "ttft_avg_ms": ns_to_ms(report.get("TTFT", {}).get("Avg")),
        "total_p50_ms": ns_to_ms(report.get("TotalDur", {}).get("P50")),
        "total_p95_ms": ns_to_ms(report.get("TotalDur", {}).get("P95")),
        "avg_chunks": report.get("AvgChunks"),
    })
    record["valid"] = proc.returncode == 0 and record["success"] == requests and record["total"] == requests
    if side == "gateway":
        record["resources_summary"] = summarise_samples(samples)
    log(f"{stem}: {record['success']}/{record['total']} ok, "
        f"{(record['throughput_rps'] or 0):.1f} req/s, "
        f"ttft p50={(record['ttft_p50_ms'] or 0):.1f}ms, wall={wall_seconds:.1f}s")
    return record


def sample_loop(url, samples, stop):
    while not stop.is_set():
        samples.append({"unix_time": time.time(), "gateway": sample_metrics(url)})
        stop.wait(SAMPLE_INTERVAL_SECONDS)


def ns_to_ms(value):
    return None if value is None else round(value / 1e6, 3)


def summarise_samples(samples):
    rss = [s["gateway"].get("process_resident_memory_bytes") for s in samples if "process_resident_memory_bytes" in s["gateway"]]
    heap = [s["gateway"].get("go_memstats_heap_alloc_bytes") for s in samples if "go_memstats_heap_alloc_bytes" in s["gateway"]]
    cpu = [s["gateway"].get("process_cpu_seconds_total") for s in samples if "process_cpu_seconds_total" in s["gateway"]]
    gc = [s["gateway"].get("go_gc_duration_seconds_count") for s in samples if "go_gc_duration_seconds_count" in s["gateway"]]
    return {
        "samples": len(samples),
        "sample_errors": sum(1 for s in samples if "sample_error" in s["gateway"]),
        "rss_peak_mib": round(max(rss) / 1048576, 2) if rss else None,
        "heap_peak_mib": round(max(heap) / 1048576, 2) if heap else None,
        "cpu_seconds_delta": round(cpu[-1] - cpu[0], 4) if len(cpu) > 1 else None,
        "gc_cycles_delta": round(gc[-1] - gc[0], 1) if len(gc) > 1 else None,
    }


def wait_for_reconciliation(url, token, state):
    deadline = time.time() + RECONCILE_TIMEOUT_SECONDS
    attempts = 0
    use_token = True
    status, body = None, {}
    while True:
        attempts += 1
        status, body = http_json(url, token if use_token else None, timeout=15)
        if status in (401, 403) and use_token:
            state["warnings"].append("metering rejected the admin token; retrying /reconcile without Authorization")
            use_token = False
            continue
        if status == 200 and isinstance(body, dict) and body.get("ok") and \
                (body.get("report") or {}).get("pending_messages") == 0:
            return {"ok": True, "attempts": attempts, "http_status": status,
                    "pending_messages": body["report"]["pending_messages"], "response": body}
        if time.time() >= deadline:
            detail = (f"http={status} ok={(body or {}).get('ok')} "
                      f"pending={(body or {}).get('report', {}).get('pending_messages')} "
                      f"after {attempts} polls / {RECONCILE_TIMEOUT_SECONDS}s")
            return {"ok": False, "attempts": attempts, "http_status": status, "detail": detail, "response": body}
        time.sleep(1)


def collect_inventory(state, compose, compose_env, project):
    code, out, _ = capture(["docker", "ps", "-a", "--filter", f"label=com.docker.compose.project={project}",
                            "--format", "{{.Names}}"])
    containers = [line.strip() for line in out.splitlines() if line.strip()]
    state["containers"] = sorted(containers)
    images = []
    for name in containers:
        code, out, _ = capture(["docker", "inspect", "--format",
                                "{{.Name}}|{{.Config.Image}}|{{.Image}}|{{.State.Status}}", name])
        if code == 0 and out.strip():
            container, image, image_id, status = (out.strip().split("|") + ["", "", "", ""])[:4]
            images.append({"container": container.lstrip("/"), "image": image, "image_id": image_id[:19], "status": status})
    state["images"] = images
    code, out, _ = capture(["docker", "volume", "ls",
                            "--filter", f"label=com.docker.compose.project={project}", "--format", "{{.Name}}"])
    state["volumes"] = sorted(line.strip() for line in out.splitlines() if line.strip())


def finalize(state, args, output, print_state, compose, compose_env, project):
    if state["up_started"]:
        log("capturing compose logs")
        with (output / "compose-logs.txt").open("w") as handle:
            run(compose + ["logs", "--no-color", "--tail", "5000"], env=compose_env,
                stdout=handle, stderr=subprocess.STDOUT)

    if args.keep:
        state["teardown"] = {"skipped": True, "reason": "--keep",
                             "manual_command": state["commands"]["compose_down"]}
        log(f"--keep: stack left running as {project}; clean up with: {state['commands']['compose_down']}")
    elif state["up_started"]:
        if not project.startswith(PROJECT_PREFIX):
            state["teardown"] = {"ok": False, "detail": f"refusing to tear down unexpected project {project!r}"}
            log(f"REFUSING to tear down project {project!r}")
        else:
            log(f"tearing down {project}")
            with (output / "teardown.log").open("w") as handle:
                proc = run(compose + ["down", "-v", "--remove-orphans"], env=compose_env,
                           stdout=handle, stderr=subprocess.STDOUT)
            state["teardown"] = {
                "ok": proc.returncode == 0,
                "exit": proc.returncode,
                "command": state["commands"]["compose_down"],
            }
            verify_teardown(state, project)
    else:
        state["teardown"] = {"skipped": True, "reason": "the stack was never started"}

    end_state = git_state()
    state["source_changed_during_run"] = (
        end_state["git_status_sha256"] != print_state["git_status_sha256"]
        or end_state["git_diff_head_sha256"] != print_state["git_diff_head_sha256"]
    )
    if state["source_changed_during_run"]:
        def status_map(lines):
            return {line[3:].strip(): line[:2].strip() for line in lines if len(line) > 3}
        before_map, after_map = status_map(print_state["dirty_files"]), status_map(end_state["dirty_files"])
        state["source_changes"] = sorted(
            f"{path} ({before_map.get(path, 'clean')} -> {after_map.get(path, 'clean')})"
            for path in set(before_map) | set(after_map)
            if before_map.get(path) != after_map.get(path)
        )[:50]
        state["source_changes_note"] = (
            "paths whose git-status code changed; paths whose code stayed the same can still have "
            "changed content, see git_diff_head_sha256 vs git_end.git_diff_head_sha256"
        )
        log("WARNING: the worktree changed while the baseline was running; the source at build time is not the source now")
    state["git_end"] = end_state


def verify_teardown(state, project):
    code, out, _ = capture(["docker", "ps", "-a", "--filter", f"label=com.docker.compose.project={project}",
                            "--format", "{{.Names}}"])
    remaining_containers = [line for line in out.splitlines() if line.strip()]
    code, out, _ = capture(["docker", "volume", "ls", "--filter", f"label=com.docker.compose.project={project}",
                            "--format", "{{.Name}}"])
    remaining_volumes = [line for line in out.splitlines() if line.strip()]
    code, out, _ = capture(["docker", "ps", "-a", "--filter", f"name={PROJECT_PREFIX}", "--format", "{{.Names}}"])
    other = [line for line in out.splitlines() if line.strip()]
    code, out, _ = capture(["docker", "volume", "ls", "--filter", f"name={PROJECT_PREFIX}", "--format", "{{.Name}}"])
    other_volumes = [line for line in out.splitlines() if line.strip()]
    state["teardown"].update({
        "containers_remaining": remaining_containers,
        "volumes_remaining": remaining_volumes,
        "containers_with_iso_prefix": other,
        "volumes_with_iso_prefix": other_volumes,
        "verified": not remaining_containers and not remaining_volumes and not other and not other_volumes,
    })
    if state["teardown"]["verified"]:
        log("teardown verified: no llm-gw-iso containers or volumes remain")
    else:
        log(f"WARNING: leftovers after teardown: containers={other or remaining_containers} "
            f"volumes={other_volumes or remaining_volumes}")
        state["invalid_reasons"].append("teardown left containers or volumes behind")


def environment_docker_config(output):
    return pathlib.Path(output) / ".docker"


def build_environment(state, args, output, print_state):
    code, docker_version, _ = capture(["docker", "version", "--format", "{{.Server.Version}}"])
    code, compose_version, _ = capture(["docker", "compose", "version", "--short"])
    code, docker_info, _ = capture(["docker", "info", "--format",
                                    "{{.NCPU}}|{{.MemTotal}}|{{.OperatingSystem}}|{{.Driver}}"])
    parts = (docker_info.strip().split("|") + ["", "", "", ""])[:4] if code == 0 else ["", "", "", ""]
    code, go_version, _ = capture(["go", "version"])
    environment = {
        "run_id": state["run_id"],
        "project": state["project"],
        "output_dir": rel_or_abs(output),
        "commit": print_state["commit"],
        "dirty": print_state["dirty"],
        "dirty_file_count": print_state["dirty_file_count"],
        "dirty_files": print_state["dirty_files"],
        "git_status_sha256": print_state["git_status_sha256"],
        "git_diff_sha256": print_state["git_diff_sha256"],
        "git_diff_head_sha256": print_state["git_diff_head_sha256"],
        "source_changed_during_run": state["source_changed_during_run"],
        "source_changes": state.get("source_changes", []),
        "source_changes_note": state.get("source_changes_note"),
        "git_end": state.get("git_end"),
        "host": {
            "platform": platform.platform(),
            "machine": platform.machine(),
            "processor": platform.processor(),
            "python": platform.python_version(),
            "cpu_count": os.cpu_count(),
            "memory_bytes": host_memory_bytes(),
            "kernel": platform.release(),
        },
        "docker": {
            "server_version": docker_version.strip(),
            "compose_version": compose_version.strip(),
            "vm_cpus": parts[0],
            "vm_memory_bytes": int(parts[1]) if parts[1].isdigit() else None,
            "operating_system": parts[2],
            "storage_driver": parts[3],
        },
        "go": go_version.strip(),
        "build_recipe": state.get("build_recipe"),
        "isolation": {
            "compose_project": state["project"],
            "project_prefix": PROJECT_PREFIX,
            "ports": state["ports"],
            "named_volumes": "project-scoped by `-p`; no override needed",
            "volumes_created": state.get("volumes", []),
            "containers": state.get("containers", []),
            "tokens": state["tokens"],
        },
        "commands": state["commands"],
        "compose_environment": state.get("compose_env", {}),
        "docker_config": rel_or_abs(environment_docker_config(output)),
        "docker_config_note": "run-local empty config.json: no credential helper, buildx state under <output>/.docker; compose is always invoked with `--pull never`",
        "images": state.get("images", []),
        "api_key_prefix": state["api_key_prefix"],
        "started_unix": state.get("started_unix"),
        "finished_unix": time.time(),
        "warnings": state["warnings"],
        "invalid_reasons": state["invalid_reasons"],
        "error": state["error"],
        "teardown": state["teardown"],
    }
    return environment


def build_index(state, output, valid):
    cases = {}
    for record in state["runs"]:
        entry = cases.setdefault(record["case"], {"side": record["side"], "case_id": record["case_id"],
                                                  "mode": record["mode"], "concurrency": record["concurrency"],
                                                  "requests": record["requests"], "rate_qps": record["rate_qps"],
                                                  "rounds": [], "valid": True, "files": []})
        entry["rounds"].append({
            "round": record["round"],
            "valid": record["valid"],
            "exit_code": record["exit_code"],
            "success": record.get("success"),
            "total": record.get("total"),
            "wall_seconds": record["wall_seconds"],
            "throughput_rps": record.get("throughput_rps"),
            "ttft_p50_ms": record.get("ttft_p50_ms"),
            "ttft_p95_ms": record.get("ttft_p95_ms"),
            "total_p50_ms": record.get("total_p50_ms"),
            "raw_result": record["raw_result"],
            "raw_log": record["raw_log"],
            "resources": record["resources"],
            "failure": record.get("failure"),
        })
        entry["valid"] = entry["valid"] and record["valid"]
        for key in ("raw_result", "raw_log", "resources"):
            if record.get(key):
                entry["files"].append(record[key])
    return {
        "run_id": state["run_id"],
        "project": state["project"],
        "commit": state["commit"],
        "dirty": state["dirty"],
        "dry_run": state["dry_run"],
        "valid": valid,
        "invalid_reasons": state["invalid_reasons"],
        "error": state["error"],
        "source_changed_during_run": state["source_changed_during_run"],
        "cases": cases,
        "reconciliation": None if state["reconciliation"] is None else {
            "ok": state["reconciliation"]["ok"],
            "http_status": state["reconciliation"]["http_status"],
            "attempts": state["reconciliation"]["attempts"],
            "file": rel_or_abs(output / "reconciliation.json"),
        },
        "teardown": state["teardown"],
        "artefacts": {
            "environment": rel_or_abs(output / "environment.json"),
            "parameters": rel_or_abs(output / "parameters.json"),
            "index": rel_or_abs(output / "index.json"),
            "readme": rel_or_abs(output / "README.md"),
            "raw": rel_or_abs(output / "raw"),
            "resources": rel_or_abs(output / "resources"),
            "reconciliation": rel_or_abs(output / "reconciliation.json"),
            "compose_override": rel_or_abs(output / "compose-override.yml"),
            "compose_logs": rel_or_abs(output / "compose-logs.txt"),
        },
    }


def median(values):
    values = sorted(values)
    if not values:
        return None
    middle = len(values) // 2
    if len(values) % 2:
        return values[middle]
    return (values[middle - 1] + values[middle]) / 2


def stats(values):
    values = [value for value in values if value is not None]
    if not values:
        return None
    return {"median": round(median(values), 3), "min": round(min(values), 3),
            "max": round(max(values), 3), "rounds": [round(value, 3) for value in values]}


def summarise(state):
    groups = {}
    for record in state["runs"]:
        group = groups.setdefault(record["case"], {"side": record["side"], "case_id": record["case_id"],
                                                   "mode": record["mode"], "concurrency": record["concurrency"],
                                                   "requests": record["requests"], "rate_qps": record["rate_qps"],
                                                   "records": []})
        group["records"].append(record)
    summary = {}
    for case, group in groups.items():
        records = group["records"]
        summary[case] = {
            "side": group["side"],
            "case_id": group["case_id"],
            "mode": group["mode"],
            "concurrency": group["concurrency"],
            "requests": group["requests"],
            "rate_qps": group["rate_qps"],
            "rounds": len(records),
            "all_valid": all(record["valid"] for record in records),
            "throughput_rps": stats([record.get("throughput_rps") for record in records]),
            "ttft_p50_ms": stats([record.get("ttft_p50_ms") for record in records]),
            "ttft_p95_ms": stats([record.get("ttft_p95_ms") for record in records]),
            "total_p50_ms": stats([record.get("total_p50_ms") for record in records]),
            "wall_seconds": stats([record.get("wall_seconds") for record in records]),
            "rss_peak_mib": stats([(record.get("resources_summary") or {}).get("rss_peak_mib") for record in records]),
            "cpu_seconds_delta": stats([(record.get("resources_summary") or {}).get("cpu_seconds_delta") for record in records]),
        }
    return summary


def fmt(stats_value, unit=""):
    if not stats_value:
        return "n/a"
    median_value = stats_value["median"]
    if abs(median_value) >= 100:
        return f"{median_value:.0f}{unit} [{stats_value['min']:.0f}..{stats_value['max']:.0f}]"
    return f"{median_value:.2f}{unit} [{stats_value['min']:.2f}..{stats_value['max']:.2f}]"


def write_readme(output, state, args):
    summary = summarise(state)
    environment = build_environment(state, args, output, state.get("git_end") or {
        "commit": state["commit"], "dirty": state["dirty"], "dirty_file_count": state["dirty_file_count"],
        "dirty_files": [], "git_status_sha256": "", "git_diff_sha256": "", "git_diff_head_sha256": "",
    })
    lines = []
    lines.append(f"# T01A isolated low-load baseline ({state['run_id']})")
    lines.append("")
    status = "FAILED" if state["error"] else ("INVALID" if state["invalid_reasons"] else "ok")
    lines.append(f"- status: **{status}**" + (" (dry run)" if state["dry_run"] else ""))
    lines.append(f"- commit: `{environment['commit']}` (dirty={environment['dirty']}, "
                 f"{environment['dirty_file_count']} modified/untracked paths, --allow-dirty={args.allow_dirty})")
    lines.append(f"- compose project: `{state['project']}` (isolated containers, networks and named volumes)")
    lines.append(f"- host: {environment['host']['platform']} / {environment['host']['machine']}, "
                 f"cpus={environment['host']['cpu_count']}, "
                 f"memory={environment['host']['memory_bytes']}")
    lines.append(f"- docker: server {environment['docker']['server_version']}, compose {environment['docker']['compose_version']}, "
                 f"vm cpus={environment['docker']['vm_cpus']}")
    lines.append(f"- {environment['go']}")
    if environment.get("build_recipe"):
        lines.append(f"- build recipe: {environment['build_recipe']}")
    lines.append("")
    lines.append("This is a **W1 low-load baseline**, not a capacity test. It compares a direct call to the")
    lines.append("upstream mock with the same call through the Gateway, 3 rounds per case, on one machine.")
    lines.append("")
    if state["error"]:
        lines.append(f"**Run error:** `{state['error']}`")
        lines.append("")
    if state["invalid_reasons"]:
        lines.append("**Invalid reasons:**")
        lines.extend(f"- {reason}" for reason in state["invalid_reasons"])
        lines.append("")
    if state["warnings"]:
        lines.append("**Warnings:**")
        lines.extend(f"- {warning}" for warning in state["warnings"])
        lines.append("")

    for side in ("direct", "gateway"):
        rows = [value for value in summary.values() if value["side"] == side]
        if not rows:
            continue
        rows.sort(key=lambda value: (value["mode"], value["concurrency"]))
        lines.append(f"## {side}")
        lines.append("")
        lines.append("| case | mode | c | n | rate | rounds | ok | throughput req/s (median [min..max]) | TTFT p50 ms | TTFT p95 ms | total p50 ms | wall s |"
                     + (" RSS peak MiB | CPU s |" if side == "gateway" else ""))
        lines.append("|---|---|---|---|---|---|---|---|---|---|---|---" + ("|---|" if side == "gateway" else ""))
        for row in rows:
            cells = [
                row["case_id"], row["mode"], str(row["concurrency"]), str(row["requests"]),
                str(row["rate_qps"]), str(row["rounds"]),
                "yes" if row["all_valid"] else "NO",
                fmt(row["throughput_rps"]), fmt(row["ttft_p50_ms"]), fmt(row["ttft_p95_ms"]),
                fmt(row["total_p50_ms"]), fmt(row["wall_seconds"]),
            ]
            if side == "gateway":
                cells.append(fmt(row["rss_peak_mib"]))
                cells.append(fmt(row["cpu_seconds_delta"]))
            lines.append("| " + " | ".join(cells) + " |")
        lines.append("")

    reconciliation = state["reconciliation"]
    lines.append("## Reconciliation")
    lines.append("")
    if reconciliation:
        lines.append(f"- `ok={reconciliation['ok']}` http={reconciliation['http_status']} "
                     f"polls={reconciliation['attempts']} "
                     f"pending_messages={(reconciliation.get('response') or {}).get('report', {}).get('pending_messages')}")
        lines.append(f"- raw response: `{rel_or_abs(output / 'reconciliation.json')}`")
    else:
        lines.append("- not attempted")
    lines.append("")
    lines.append("## Validity limits")
    lines.append("")
    lines.append("1. **Single-host Docker Desktop baseline.** The load generator, the Compose stack and the")
    lines.append("   Docker VM all share one machine, and the mock, the Gateway and the meter all run in")
    lines.append("   containers on that host. Absolute latencies and throughput are not comparable to")
    lines.append("   dedicated Linux hardware or to any production environment.")
    lines.append("2. **The direct path produces no billing.** `direct` calls `mock-primary` on the host port")
    lines.append("   directly, so it never reaches the Gateway or metering: ledger counts, reconciliation")
    lines.append("   totals and Gateway request counters cover **gateway traffic only** and must not be")
    lines.append("   compared with direct request counts as if both were billed.")
    lines.append("3. **Fixed-arrival mode blocks under saturation, and these rates are far below it.**")
    lines.append("   `cmd/loadtest -rate` waits on a ticker before starting each request and then blocks on a")
    lines.append("   shared concurrency semaphore once in-flight requests pile up; when the target cannot")
    lines.append("   keep up the *offered* rate is silently not achieved. The cases here (c=1/60, c=4/120,")
    lines.append("   20 req/s) are deliberately far below any plausible saturation point, so **nothing here")
    lines.append("   says anything about capacity, headroom, queueing or the knee of the latency curve**.")
    lines.append("4. **TTFT is first non-empty `data:` event, not first content.** `internal/loadtest` starts")
    lines.append("   the TTFT clock at the first non-empty SSE `data:` line. The mock's first event is a")
    lines.append("   role-only chunk, so the reported TTFT is *time to first event*, not time to first")
    lines.append("   content token. T01B owns the first_event/first_content split.")
    lines.append("")
    lines.append("Additional caveats for this run:")
    lines.append("")
    lines.append(f"- Dirty worktree: the tree was dirty by {environment['dirty_file_count']} paths when the run")
    lines.append("  started (parallel workstream); the images were built from whatever was in the tree then.")
    if state["source_changed_during_run"]:
        lines.append("- **The worktree changed while this run was executing**; the source hashes at start and")
        lines.append("  end differ, so the measured build is not the current tree.")
    if state.get("build_recipe") and "generated" in state["build_recipe"]:
        lines.append("- **Build fallback:** the stock `RUN go mod download` could not reach")
        lines.append(f"  `proxy.golang.org` from the build container, so the images were built from a")
        lines.append(f"  generated copy of `Dockerfile` that only adds `ENV GOPROXY={FALLBACK_GOPROXY}`.")
        lines.append("  The rest of the Dockerfile (same base images, same `go build -trimpath` commands)")
        lines.append("  is unchanged; the copy is recorded in environment.json.")
    lines.append(f"- Limiter: `GATEWAY_LIMITER_RATE={LIMITER_ENV['GATEWAY_LIMITER_RATE']}` / "
                 f"`GATEWAY_LIMITER_BURST={LIMITER_ENV['GATEWAY_LIMITER_BURST']}` override the Demo default 20 rps")
    lines.append("  so the token bucket cannot distort the comparison; this is *not* the production policy.")
    lines.append("")
    lines.append("## Reproduce")
    lines.append("")
    lines.append("```bash")
    lines.append("export GOCACHE=/Users/lihualin/Projects/llm-gateway/.gocache   # sandbox blocks the default build cache")
    lines.append(f"python3 scripts/isolated-baseline.py --output {rel_or_abs(output)}"
                 + (" --allow-dirty" if args.allow_dirty else "")
                 + (" --no-build" if args.no_build else "")
                 + (" --keep" if args.keep else ""))
    lines.append("```")
    lines.append("")
    lines.append("## T01A 隔离基线")
    lines.append("")
    lines.append("本次运行的隔离基线产物（`data/` 已被 gitignore，不入库）：")
    lines.append("")
    lines.append(f"- `environment.json`：commit/dirty 与 diff 哈希、宿主机/Docker/Go 版本、项目名、端口映射、"
                 f"实际使用的镜像 tag、执行的完整命令、teardown 结果")
    lines.append(f"- `parameters.json`：用例矩阵、轮数、mock 行为、限流设置、限流与 token 说明")
    lines.append(f"- `index.json`：用例 -> 文件与有效性")
    lines.append(f"- `raw/`：每个用例每轮的 loadtest JSON 结果与日志；`resources/`：gateway 用例的 0.5s 资源采样")
    lines.append(f"- `reconciliation.json`：metering `/reconcile` 原始响应")
    lines.append(f"- `compose-override.yml`（0600）：仅覆盖宿主机端口与本次运行的随机 token")
    lines.append("")
    (output / "README.md").write_text("\n".join(lines) + "\n")
    write_json(output / "summary.json", {"run_id": state["run_id"], "project": state["project"],
                                         "status": status, "cases": summary,
                                         "reconciliation": None if not reconciliation else {
                                             "ok": reconciliation["ok"],
                                             "http_status": reconciliation["http_status"],
                                             "pending_messages": (reconciliation.get("response") or {}).get("report", {}).get("pending_messages"),
                                         },
                                         "invalid_reasons": state["invalid_reasons"],
                                         "warnings": state["warnings"]})


MANIFEST_EXCLUDE = {"manifest.json", "api-key", "loadtest", "compose-override.yml"}


def write_manifest(output):
    """Hash every regular artefact file; skip secrets/binaries and symlinks."""
    entries = []
    for path in sorted(output.rglob("*")):
        if path.is_symlink() or not path.is_file():
            continue
        if path.name in MANIFEST_EXCLUDE or ".docker" in path.parts:
            continue
        data = path.read_bytes()
        entries.append({"path": rel_or_abs(path), "bytes": len(data), "sha256": sha256_hex(data)})
    write_json(output / "manifest.json", {"files": entries, "count": len(entries)})


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        raise
    except KeyboardInterrupt:
        print("[iso] interrupted", file=sys.stderr)
        sys.exit(130)
    except Exception as exc:  # noqa: BLE001 - top-level guard
        print(f"[iso] fatal: {type(exc).__name__}: {exc}", file=sys.stderr)
        sys.exit(1)
