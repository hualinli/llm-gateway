#!/usr/bin/env bash
# Runs the Console (control plane) for the T08 Playwright suite on 127.0.0.1:18092.
#
# Without CONTROL_ADDR the Key endpoints answer `503 unavailable` — the specs
# assert that honest state instead of a fake empty list. To exercise key creation
# end to end, add the Control channel (mTLS or the local insecure admin service)
# exactly as the README describes for `make console-run-tls`.
#
# Gateway (`/v1`) is optional: the Playground/data-plane spec is skipped unless a
# Gateway is reachable at E2E_GATEWAY_URL (default 127.0.0.1:18090).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

export GOCACHE="${GOCACHE:-$REPO_ROOT/.gocache}"
export POSTGRES_DSN="${POSTGRES_DSN:-postgres://gateway_e2e:e2e_only_local@127.0.0.1:15435/gateway_e2e?sslmode=disable}"
export GATEWAY_PROFILE="${GATEWAY_PROFILE:-demo}"
export CONSOLE_ADDR="${CONSOLE_ADDR:-127.0.0.1:18092}"
export CONSOLE_DIAGNOSTICS_ADDR="${CONSOLE_DIAGNOSTICS_ADDR:-127.0.0.1:18093}"
export CONSOLE_SECURE_COOKIES="${CONSOLE_SECURE_COOKIES:-false}"
# The suite logs in a handful of seeded identities once per run. The default
# login bucket (burst 5, 5/min) is sized for humans; a scripted run would spend
# its first minute throttled. This only widens the local test backend.
export CONSOLE_LOGIN_BURST="${CONSOLE_LOGIN_BURST:-50}"
export CONSOLE_LOGIN_RATE="${CONSOLE_LOGIN_RATE:-60}"

echo "console listening on $CONSOLE_ADDR (profile=$GATEWAY_PROFILE)"
exec go run ./cmd/console
