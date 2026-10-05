#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${PROMETHEUS_IMAGE:-prom/prometheus:v3.14.0}"
run() { docker run --rm --entrypoint /bin/promtool -v "$root/observability/rules:/rules:ro" -v "$root/observability/rules:/etc/prometheus/rules:ro" -v "$root/prometheus.yml:/etc/prometheus/prometheus.yml:ro" -w /rules "$image" "$@"; }
run check config /etc/prometheus/prometheus.yml
run check rules /rules/billing.yml
run test rules /rules/billing.test.yml
