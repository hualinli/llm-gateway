#!/usr/bin/env bash
# Three independent local HTTP providers; exit/Ctrl-C stops only these children.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
mock_dir="${MOCK_FLEET_DIR:-$root/data/mock-fleet}"
mock_bind="${MOCK_BIND:-127.0.0.1}"
mock_port="${MOCK_BASE_PORT:-18180}"
if ! [[ "$mock_port" =~ ^[0-9]+$ ]] || (( mock_port < 1024 || mock_port > 65533 )); then
  echo 'MOCK_BASE_PORT must be 1024..65533' >&2
  exit 2
fi
mkdir -p "$mock_dir"
go build -o "$mock_dir/upstream-mock" ./cmd/upstream-mock
pids=()
cleanup() {
  trap - EXIT INT TERM
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 0' INT TERM
"$mock_dir/upstream-mock" -listen "$mock_bind:$mock_port" -name mock-fixed -mode fixed \
  -text '夜色降临，小镇的灯一盏盏亮起来。旅人推开书店的门，看见桌上留着一封未署名的信。' \
  -repeat 100 -tokens 256 -chunk-tokens 4 -tps 200 -ttft 20ms >"$mock_dir/fixed.log" 2>&1 &
pids+=("$!")
"$mock_dir/upstream-mock" -listen "$mock_bind:$((mock_port+1))" -name mock-echo -mode echo \
  -repeat 32 -tokens 256 -chunk-tokens 2 -tps 40 -ttft 100ms >"$mock_dir/echo.log" 2>&1 &
pids+=("$!")
"$mock_dir/upstream-mock" -listen "$mock_bind:$((mock_port+2))" -name mock-flaky -mode info \
  -repeat 10 -tokens 256 -chunk-tokens 4 -tps 80 -ttft 50ms \
  -fault-every 5 -fault-status 503 >"$mock_dir/flaky.log" 2>&1 &
pids+=("$!")
echo "Mock fleet running; Ctrl-C stops all three. Logs: $mock_dir"
echo "fixed: http://$mock_bind:$mock_port      (200 TPS per stream)"
echo "echo:  http://$mock_bind:$((mock_port+1)) (40 TPS per stream)"
echo "flaky: http://$mock_bind:$((mock_port+2)) (80 TPS; every fifth request fails with 503)"
echo 'Each instance exposes /healthz, /stats and /metrics; chat endpoint: /v1/chat/completions.'
while true; do
  for pid in "${pids[@]}"; do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "A mock exited; stopping fleet. Inspect logs in $mock_dir" >&2
      exit 1
    fi
  done
  sleep 1
done
