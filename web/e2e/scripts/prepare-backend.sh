#!/usr/bin/env bash
# Prepares an isolated Console backend for the T08 Playwright suite.
#
# What it does (all idempotent, nothing touches an existing demo stack):
#   1. starts two throwaway Docker containers — PostgreSQL on 127.0.0.1:15435 and
#      Redis on 127.0.0.1:16381 — unless they already exist;
#   2. applies this checkout's migrations with `cmd/migrate`;
#   3. writes the local E2E admin password (mode 0600) and bootstraps the
#      platform administrator plus the two demo enterprises
#      (`console bootstrap --demo-data`).
#
# Then run `web/e2e/scripts/start-console.sh` in another terminal and `npm run e2e`
# from `web/`. See ../README.md for the full walkthrough.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

PG_CONTAINER="${E2E_PG_CONTAINER:-t08-e2e-postgres}"
REDIS_CONTAINER="${E2E_REDIS_CONTAINER:-t08-e2e-redis}"
PG_PORT="${E2E_PG_PORT:-15435}"
REDIS_PORT="${E2E_REDIS_PORT:-16381}"
PG_DB="${E2E_PG_DB:-gateway_e2e}"
PG_USER="${E2E_PG_USER:-gateway_e2e}"
PG_PASSWORD="${E2E_PG_PASSWORD:-e2e_only_local}"

export GOCACHE="${GOCACHE:-$REPO_ROOT/.gocache}"
export POSTGRES_DSN="postgres://${PG_USER}:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/${PG_DB}?sslmode=disable"

if ! docker inspect "$PG_CONTAINER" >/dev/null 2>&1; then
  echo "==> starting $PG_CONTAINER (postgres:16.15 on 127.0.0.1:$PG_PORT)"
  docker run -d --name "$PG_CONTAINER" \
    -p "127.0.0.1:${PG_PORT}:5432" \
    -e "POSTGRES_DB=${PG_DB}" -e "POSTGRES_USER=${PG_USER}" -e "POSTGRES_PASSWORD=${PG_PASSWORD}" \
    postgres:16.15 >/dev/null
else
  echo "==> reusing $PG_CONTAINER"
  docker start "$PG_CONTAINER" >/dev/null 2>&1 || true
fi

if ! docker inspect "$REDIS_CONTAINER" >/dev/null 2>&1; then
  echo "==> starting $REDIS_CONTAINER (redis:7.4.11 on 127.0.0.1:$REDIS_PORT)"
  docker run -d --name "$REDIS_CONTAINER" \
    -p "127.0.0.1:${REDIS_PORT}:6379" \
    redis:7.4.11 redis-server --save '' --appendonly no >/dev/null
else
  echo "==> reusing $REDIS_CONTAINER"
  docker start "$REDIS_CONTAINER" >/dev/null 2>&1 || true
fi

echo "==> waiting for PostgreSQL"
for _ in $(seq 1 60); do
  if docker exec "$PG_CONTAINER" pg_isready -U "$PG_USER" -d "$PG_DB" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$PG_CONTAINER" pg_isready -U "$PG_USER" -d "$PG_DB" >/dev/null

echo "==> applying migrations ($POSTGRES_DSN)"
GATEWAY_PROFILE=demo go run ./cmd/migrate up

echo "==> bootstrapping platform admin + demo enterprises"
mkdir -p data
PASSWORD_FILE="${E2E_ADMIN_PASSWORD_FILE:-data/t08-e2e-admin-password}"
if [[ ! -f "$PASSWORD_FILE" ]]; then
  printf 't08-e2e-admin-pw' >"$PASSWORD_FILE"
  chmod 600 "$PASSWORD_FILE"
fi
GATEWAY_PROFILE=demo go run ./cmd/console bootstrap \
  --admin-email "${E2E_ADMIN_EMAIL:-t08-admin@example.com}" \
  --admin-password-file "$PASSWORD_FILE" \
  --demo-data \
  --demo-credentials-file "${E2E_DEMO_CREDENTIALS_FILE:-data/t08-e2e-demo-credentials.txt}"

cat <<EOF

Backend data is ready.

  Console : web/e2e/scripts/start-console.sh   (listens on 127.0.0.1:18092)
  Admin   : ${E2E_ADMIN_EMAIL:-t08-admin@example.com} / ${PASSWORD_FILE}
  Demo    : ${E2E_DEMO_CREDENTIALS_FILE:-data/t08-e2e-demo-credentials.txt} (0600)

Next: start the Console, then run \`npm run e2e\` from web/.
EOF
