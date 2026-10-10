#!/usr/bin/env python3
"""Repeatable local, closed-loop SSE benchmark against one Compose checkout."""

import argparse
import json
import os
import pathlib
import platform
import subprocess
import time
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
COMPOSE = ["docker", "compose", "-f", "docker-compose.yml", "-f", "docker-compose.mock.yml", "-f", "docker-compose.bench.yml", "--profile", "demo-mocks"]


def command(args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def metrics():
    try:
        with urllib.request.urlopen("http://127.0.0.1:8090/metrics", timeout=2) as response:
            lines = response.read().decode().splitlines()
        names = ("process_cpu_seconds_total", "process_resident_memory_bytes", "go_memstats_heap_alloc_bytes", "go_gc_duration_seconds_count")
        return {key: float(value) for line in lines if line and not line.startswith("#") and "{" not in line for key, value in [line.split()[:2]] if key in names}
    except Exception as exc:
        return {"sample_error": str(exc)}


def run_case(binary, output_dir, name, target, key, concurrency, requests, repetition, rate=0):
    path = output_dir / f"{name}-c{concurrency}-r{repetition}"
    path.mkdir(parents=True)
    result = path / "result.json"
    args = [str(binary), "-target", target, "-c", str(concurrency), "-n", str(requests), "-rate", str(rate), "-json", str(result)]
    if key:
        args.extend(["-key", key])
    samples = []
    with (path / "loadtest.log").open("w") as log:
        started = time.time()
        process = subprocess.Popen(args, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
        while process.poll() is None:
            samples.append({"unix_time": time.time(), "gateway": metrics()})
            time.sleep(0.5)
        samples.append({"unix_time": time.time(), "gateway": metrics()})
    (path / "resource-samples.json").write_text(json.dumps(samples, indent=2) + "\n")
    if not result.exists():
        raise RuntimeError(f"{name} c={concurrency} repeat={repetition} exited {process.returncode} without result")
    data = json.loads(result.read_text())
    data.update({"scenario": name, "repeat": repetition, "exit_code": process.returncode, "wall_started_unix": started})
    result.write_text(json.dumps(data, indent=2) + "\n")
    print(f"{name} c={concurrency} r={repetition}: {data['report']['Success']}/{data['report']['Total']} success, {data['success_per_second']:.1f}/s", flush=True)
    return data


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True)
    parser.add_argument("--arrival-only", action="store_true", help="run fixed arrival rate cases only")
    parser.add_argument("--no-build", action="store_true", help="reuse already built local Compose images")
    args = parser.parse_args()
    output_dir = pathlib.Path(args.output).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    commit = command(["git", "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
    status = command(["git", "status", "--short"], capture_output=True, text=True).stdout
    if status:
        raise RuntimeError("commit benchmark code before measuring; worktree is dirty")
    environment = {"commit": commit, "platform": platform.platform(), "machine": platform.machine(), "python": platform.python_version(), "go": command(["go", "version"], capture_output=True, text=True).stdout.strip(), "docker_compose": command(["docker", "compose", "version"], capture_output=True, text=True).stdout.strip(), "started_unix": time.time(), "scenarios": "fixed arrival 100/300 requests/s at c64" if args.arrival_only else "closed-loop; primary/direct c1/16/64/128; backup c16; three repeats each"}
    for key, value in (("cpu", "hw.ncpu"), ("memory_bytes", "hw.memsize")):
        result = subprocess.run(["sysctl", "-n", value], capture_output=True, text=True)
        environment[key] = result.stdout.strip() if result.returncode == 0 else "unavailable"
    (output_dir / "environment.json").write_text(json.dumps(environment, indent=2) + "\n")
    binary = output_dir / "loadtest"
    command(["go", "build", "-o", str(binary), "./cmd/loadtest"])
    with (output_dir / "compose-up.log").open("w") as log:
        command(["make", "ensure-master-key"], stdout=log, stderr=subprocess.STDOUT)
        command(COMPOSE + ["up", "-d", "--no-build" if args.no_build else "--build", "--wait"], stdout=log, stderr=subprocess.STDOUT, env={**os.environ, "HOST_UID": str(os.getuid()), "HOST_GID": str(os.getgid())})
    try:
        with (output_dir / "configure.log").open("w") as log:
            command(["go", "run", "./cmd/democtl", "configure", "-primary-url", "http://mock-primary:18080", "-backup-url", "http://mock-backup:18081"], stdout=log, stderr=subprocess.STDOUT)
        key = (ROOT / "data/demo-api-key").read_text().strip()
        command(["go", "run", "./cmd/democtl", "request", "-expect-provider", "primary"], stdout=subprocess.DEVNULL)
        cases = []
        for name, target, api_key in (("direct", "http://127.0.0.1:18080/v1/chat/completions", ""), ("gateway", "http://127.0.0.1:8090/v1/chat/completions", key)):
            if args.arrival_only:
                for rate, requests in ((100, 600), (300, 900)):
                    for repetition in range(1, 4):
                        cases.append(run_case(binary, output_dir, f"{name}-rate{rate}", target, api_key, 64, requests, repetition, rate))
            else:
                for concurrency, requests in ((1, 60), (16, 240), (64, 480), (128, 640)):
                    for repetition in range(1, 4):
                        cases.append(run_case(binary, output_dir, name, target, api_key, concurrency, requests, repetition))
        if not args.arrival_only:
            command(COMPOSE + ["stop", "mock-primary"], stdout=subprocess.DEVNULL)
            command(["go", "run", "./cmd/democtl", "request", "-expect-provider", "backup"], stdout=subprocess.DEVNULL)
            for repetition in range(1, 4):
                cases.append(run_case(binary, output_dir, "backup", "http://127.0.0.1:8090/v1/chat/completions", key, 16, 240, repetition))
        deadline = time.time() + 60
        while True:
            try:
                with urllib.request.urlopen("http://127.0.0.1:8091/reconcile", timeout=30) as response:
                    reconcile = json.load(response)
                if reconcile["ok"] and reconcile["report"]["pending_messages"] == 0:
                    break
            except Exception:
                pass
            if time.time() >= deadline:
                raise RuntimeError("metering reconciliation did not settle within 60 seconds")
            time.sleep(1)
        (output_dir / "reconcile.json").write_text(json.dumps(reconcile, indent=2) + "\n")
        (output_dir / "index.json").write_text(json.dumps({"commit": commit, "cases": [{"scenario": item["scenario"], "concurrency": item["concurrency"], "repeat": item["repeat"], "success": item["report"]["Success"], "total": item["report"]["Total"], "status": item["report"]["Status"]} for item in cases], "reconciliation_ok": reconcile["ok"]}, indent=2) + "\n")
    finally:
        command(COMPOSE + ["stop", "mock-primary", "mock-backup"], stdout=subprocess.DEVNULL)
        # Restore the standard local Demo limiter after this experiment.
        command(["docker", "compose", "up", "-d", "--no-deps", "--wait", "gateway"], stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
