#!/usr/bin/env bash
set -euo pipefail

# Legacy democtl/mock-provider acceptance only: this checks demo proxy failover
# and ledger reconciliation, not Console identity, enterprise memberships, or
# account billing. Use scripts/release-candidate.sh for Console acceptance.

cd "$(dirname "$0")/.."
sha="$(git rev-parse HEAD)"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
evidence_dir="${EVIDENCE_DIR:-data/verification/demo-${sha:0:12}-${stamp}}"
mkdir -p "$evidence_dir"
evidence_dir="$(cd "$evidence_dir" && pwd)"
printf '%s\n' "$sha" > "$evidence_dir/commit.txt"
git status --short > "$evidence_dir/worktree-status.txt"

restore_mocks() {
  docker compose --profile demo-mocks stop mock-primary mock-backup >/dev/null 2>&1 || true
}
trap restore_mocks EXIT

make ensure-master-key > "$evidence_dir/setup.log" 2>&1
HOST_UID="$(id -u)" HOST_GID="$(id -g)" docker compose --profile demo-mocks up -d --build --wait >> "$evidence_dir/setup.log" 2>&1
go run ./cmd/democtl configure -primary-url http://mock-primary:18080 -backup-url http://mock-backup:18081 > "$evidence_dir/configure.log" 2>&1

ledger_count() {
  curl --fail --silent --show-error http://127.0.0.1:8091/ledger/count | python3 -c 'import json,sys; print(json.load(sys.stdin)["count"])'
}

for run in 1 2 3; do
  run_dir="$evidence_dir/run-$run"
  mkdir -p "$run_dir"
  # `docker compose start --wait` is not supported by every Compose 2.x build
  # (unknown flag on v2.24); `up -d --wait` starts/keeps the profile services and
  # waits for health, which is what the round needs.
  docker compose --profile demo-mocks up -d --wait mock-primary mock-backup > "$run_dir/start.log" 2>&1
  before="$(ledger_count)"
  go run ./cmd/democtl request -expect-provider primary > "$run_dir/primary.log" 2>&1
  docker compose --profile demo-mocks stop mock-primary > "$run_dir/stop-primary.log" 2>&1
  go run ./cmd/democtl request -expect-provider backup > "$run_dir/backup.log" 2>&1
  docker compose --profile demo-mocks stop mock-backup > "$run_dir/stop-backup.log" 2>&1
  go run ./cmd/democtl request -expect-status 502 > "$run_dir/unavailable.log" 2>&1
  after="$(ledger_count)"
  curl --fail --silent --show-error http://127.0.0.1:8091/reconcile > "$run_dir/reconcile.json"
  python3 - "$run_dir" "$before" "$after" "$sha" <<'PY'
import json, pathlib, sys
directory, before, after, commit = pathlib.Path(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
payload = json.loads((directory / "reconcile.json").read_text())
report = payload["report"]
assert after == before + 2, f"ledger count {before}->{after}, expected +2"
assert payload["ok"], f"reconciliation failed: {report}"
assert report["pending_messages"] == 0, f"pending messages: {report}"
assert report["dlq_messages"] == 0, f"DLQ messages: {report}"
summary = {"commit": commit, "run": directory.name, "primary_sse": "pass", "backup_failover_sse": "pass", "both_down_502": "pass", "ledger_before": before, "ledger_after": after, "reconciliation": report}
(directory / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
print(f"PASS {directory.name}: primary SSE, backup failover, both down 502, ledger {before}->{after}, reconciliation")
PY
done

printf 'PASS all three runs on commit %s\nEvidence: %s\n' "$sha" "$evidence_dir"
