#!/usr/bin/env bash
# Full Compose preview: backend, static Web and independent mock providers.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command=preview
if [[ $# -gt 0 ]]; then
  case "$1" in
    up|start) shift ;;
    stop|status|down|help) command="$1"; shift ;;
    -h|--help) command=help; shift ;;
    -*) ;;
    *) echo 'Usage: scripts/preview.sh [up|stop|status|down] [release-candidate options]' >&2; exit 2 ;;
  esac
fi
exec bash "$root/scripts/release-candidate.sh" "$command" \
  --project-name gateway-preview --run-id local-preview \
  --work-dir "$root/data/preview/local" "$@"
