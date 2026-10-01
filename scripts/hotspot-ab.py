#!/usr/bin/env python3
"""T11 hotspot A/B and recovery-report harness (B03 / B08 / B07 evidence).

What this is
------------
One experiment harness for the T11 half that is *not* the capacity ladder:

* ``hotspot``  — B03: the shipped admission/settlement transaction shape
  (``SELECT ... FOR UPDATE`` on ``billing_accounts``, then a read-modify-write
  UPDATE computed in Go) against an equivalent **conditional-update** shape
  (the availability/release predicate moved into the ``UPDATE ... WHERE`` so the
  row is locked by the write itself and the projection is computed in SQL).
  Measured for 1 / 10 / 1000 accounts over a concurrency ladder, for both
  ``reserve`` (admission) and ``settle`` (settlement).
* ``query``    — B08: the shipped console usage page/totals and statement
  page/totals queries on 100k and 1M rows, against the same queries with
  improved indexes and (optionally) a daily rollup.  P95/P99, EXPLAIN
  (ANALYZE, BUFFERS) plans, ``pg_stat_statements`` buffers and DB time.
* ``settle``   — receipts settled by the *real* dispatcher/consumer path under a
  steady stream of admissions: receipt->settled lag distribution, oldest
  outstanding outbox age, Redis latency, PostgreSQL pool/lock waiters.
* ``recovery`` — RTO and backlog drain after Redis outages (10/30/60 s),
  PostgreSQL outages at admission/finalize/settle, and ``kill -9`` of the
  gateway and of metering, all under load from ``cmd/loadtest``.

Two implementations, one production path
----------------------------------------
The harness never changes Go code and never adds a second production path.
Variant A executes *the shipped statement texts* (they are embedded below
verbatim and ``sqlcheck`` refuses to run if they no longer appear in the Go
source).  Variant B is an experiment-only restatement of the same transaction
with the account predicate in the UPDATE.  ``sqlcheck`` is a drift guard: if the
shipped SQL changes, the harness fails loudly instead of quietly measuring a
shape the product no longer uses.

Isolation
---------
``hotspot``/``query`` start a private PostgreSQL container
(``llm-gw-t11-ab-<runid>-pg``, tmpfs, probed free port, project-scoped name) and
apply the real migrations with ``cmd/migrate``.  ``settle``/``recovery`` reuse
the T10 runner's isolated Compose project by importing ``scripts/failure-matrix.py``
(no daily demo stack, no shared ``make test-up`` project).  Nothing outside this
run's own prefix is ever stopped, deleted or truncated.

Usage
-----
    python3 -m venv /tmp/t11venv && /tmp/t11venv/bin/pip install 'psycopg[binary]' redis
    /tmp/t11venv/bin/python scripts/hotspot-ab.py sqlcheck
    /tmp/t11venv/bin/python scripts/hotspot-ab.py hotspot --output docs/verification/2026-10-01/t11-hotspots
    /tmp/t11venv/bin/python scripts/hotspot-ab.py query   --output docs/verification/2026-10-01/t11-hotspots
    /tmp/t11venv/bin/python scripts/hotspot-ab.py settle  --output docs/verification/2026-10-01/t11-recovery-report
    /tmp/t11venv/bin/python scripts/hotspot-ab.py recovery --output docs/verification/2026-10-01/t11-recovery-report

Requires: docker, go, python3 with ``psycopg`` (and ``redis`` for ``settle``).
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import platform
import queue
import random
import re
import statistics
import string
import subprocess
import sys
import tempfile
import threading
import traceback
import time
import urllib.request
from datetime import datetime, timedelta, timezone

ROOT = pathlib.Path(__file__).resolve().parent.parent
PG_IMAGE = "postgres:16.15"
CONTAINER_PREFIX = "llm-gw-t11-ab-"

# Server settings for the experiment container. max_connections is raised so the
# concurrency ladder (up to 256 clients + samplers) measures the row lock and
# not the connection limit; pg_stat_statements gives per-statement buffers and
# DB time; track_io_timing makes blk_read_time real instead of zero.
PG_SERVER_FLAGS = [
    "-c", "max_connections=400",
    "-c", "shared_preload_libraries=pg_stat_statements",
    "-c", "pg_stat_statements.max=5000",
    "-c", "pg_stat_statements.track=all",
    "-c", "track_io_timing=on",
    "-c", "log_min_duration_statement=1000",
]


def log(message):
    print(f"[t11] {datetime.now(timezone.utc).strftime('%H:%M:%S')} {message}", flush=True)


def now_utc():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def must_run(args, what, cwd=None, timeout=1800, env=None):
    proc = subprocess.run(args, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
    if proc.returncode != 0:
        raise RuntimeError(f"{what} failed (exit {proc.returncode}): {' '.join(map(str, args))}\n"
                           f"stdout: {proc.stdout[-2000:]}\nstderr: {proc.stderr[-2000:]}")
    return proc


def try_run(args, cwd=None, timeout=300, env=None):
    try:
        return subprocess.run(args, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired as err:
        return subprocess.CompletedProcess(args, 124, str(err.stdout or ""), str(err.stderr or ""))


def free_port():
    import socket
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def wait_tcp(port, timeout, what):
    import socket
    deadline = time.time() + timeout
    while time.time() < deadline:
        s = socket.socket()
        s.settimeout(0.5)
        try:
            s.connect(("127.0.0.1", port))
            return True
        except OSError:
            time.sleep(0.2)
        finally:
            s.close()
    raise RuntimeError(f"timeout waiting for {what} on 127.0.0.1:{port}")


def percentile(values, p):
    """Linear-interpolation percentile over a non-empty list."""
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = (len(ordered) - 1) * (p / 100.0)
    low = int(rank)
    high = min(low + 1, len(ordered) - 1)
    frac = rank - low
    return ordered[low] * (1 - frac) + ordered[high] * frac


def latency_block(values):
    if not values:
        return {"count": 0}
    return {
        "count": len(values),
        "p50": round(percentile(values, 50), 3),
        "p95": round(percentile(values, 95), 3),
        "p99": round(percentile(values, 99), 3),
        "min": round(min(values), 3),
        "max": round(max(values), 3),
        "mean": round(statistics.fmean(values), 3),
    }


def reservoir(values, size=512, seed=11):
    if len(values) <= size:
        return [round(v, 3) for v in values]
    rng = random.Random(seed)
    return [round(v, 3) for v in rng.sample(values, size)]


# ---------------------------------------------------------------------------
# 1. The shipped SQL, verbatim, plus the drift guard
# ---------------------------------------------------------------------------
#
# Every fragment below is the *effective* text of a constant in
# internal/billing.  ``ANCHORS`` maps each fragment to a literal that must still
# appear in the Go source; ``sqlcheck`` verifies all of them (and the "hotspot"
# / "query" commands call it before they measure anything).

ACCOUNT_COLUMNS = """org_id, currency, posted_balance_micro, reserved_micro,
       status, version, created_at, updated_at"""

ENTRY_COLUMNS = """id, org_id, kind, amount_micro, direction, business_id, payload_digest,
       reason, operator_user_id, request_id, posted_at, created_at"""

EXECUTION_COLUMNS = """execution_id, org_id, user_id, key_id, gateway_id, price_version_id,
       model, status, reserved_micro, input_tokens_upper, output_tokens_upper, request_id,
       created_at, updated_at, receipt_recorded_at, terminal_reason"""

RECEIPT_COLUMNS = """execution_id, event_id, org_id, user_id, key_id, model,
       prompt_tokens, output_tokens, total_tokens, price_version_id, charge_micro,
       occurred_at, digest, status, dispatch_after, claim_until, attempts, last_error,
       created_at, settled_at"""

PRICE_VERSION_COLUMNS = """id, model, currency,
       input_price_micro_per_million, output_price_micro_per_million,
       provider_input_cost_micro_per_million, provider_output_cost_micro_per_million,
       effective_at, created_by, created_at"""

SQL = {
    # -- internal/billing/account_pg.go -------------------------------------
    "ensure_account": """INSERT INTO billing_accounts
  (org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
VALUES ($1, 'CNY', 0, 0, 'active', 1, $2, $2)
ON CONFLICT (org_id) DO NOTHING""",
    "get_account": f"""SELECT {ACCOUNT_COLUMNS}
FROM billing_accounts
WHERE org_id = $1""",
    "lock_account": f"""SELECT {ACCOUNT_COLUMNS}
FROM billing_accounts
WHERE org_id = $1 FOR UPDATE""",
    "update_account": f"""UPDATE billing_accounts
SET posted_balance_micro = $2, reserved_micro = $3, version = version + 1, updated_at = $4
WHERE org_id = $1
RETURNING {ACCOUNT_COLUMNS}""",
    "get_entry_by_business": f"""SELECT {ENTRY_COLUMNS}
FROM billing_entries
WHERE business_id = $1 AND kind = $2""",
    "insert_entry": f"""INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
   operator_user_id, request_id, posted_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING {ENTRY_COLUMNS}""",
    "insert_adjustment_audit": """INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES ($1, $2, $3, $4, $5, 'success', $6, $7, NULL, $8)""",
    # -- internal/billing/execution_pg.go ------------------------------------
    "get_execution": f"""SELECT {EXECUTION_COLUMNS}
FROM billing_executions
WHERE execution_id = $1""",
    "lock_execution": f"""SELECT {EXECUTION_COLUMNS}
FROM billing_executions
WHERE execution_id = $1 FOR UPDATE""",
    "insert_execution": f"""INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, request_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
RETURNING {EXECUTION_COLUMNS}""",
    "update_execution": f"""UPDATE billing_executions
SET status = $2,
    updated_at = $3,
    receipt_recorded_at = COALESCE($4, receipt_recorded_at),
    terminal_reason = COALESCE($5, terminal_reason)
WHERE execution_id = $1
RETURNING {EXECUTION_COLUMNS}""",
    "insert_execution_audit": """INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES ($1, $2, $3, $4, $5, 'success', $6, $7, NULL, $8)""",
    "get_price_version": f"""SELECT {PRICE_VERSION_COLUMNS}
FROM price_versions
WHERE id = $1""",
    # -- internal/billing/settlement_pg.go -----------------------------------
    "lock_receipt": f"""SELECT {RECEIPT_COLUMNS}
FROM usage_receipts
WHERE execution_id = $1
FOR UPDATE""",
    "mark_receipt_settled": f"""
UPDATE usage_receipts
SET status = 'settled',
    settled_at = $2,
    claim_until = NULL,
    last_error = NULL
WHERE execution_id = $1
  AND status IN ('pending_dispatch', 'dispatched')
RETURNING {RECEIPT_COLUMNS}""",
    "insert_receipt_audit": """INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES ($1, $2, $3, $4, $5, 'success', $6, $7, $8, $9)""",
}

# Fragments whose text is composed from other constants in Go: the anchor is the
# literal that must appear in the source, not the composed text.
ANCHORS = {
    "internal/billing/account_pg.go": [
        ACCOUNT_COLUMNS,
        """INSERT INTO billing_accounts
  (org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
VALUES ($1, 'CNY', 0, 0, 'active', 1, $2, $2)
ON CONFLICT (org_id) DO NOTHING""",
        """FROM billing_accounts
WHERE org_id = $1""",
        "` FOR UPDATE`",
        """UPDATE billing_accounts
SET posted_balance_micro = $2, reserved_micro = $3, version = version + 1, updated_at = $4
WHERE org_id = $1
RETURNING """,
        ENTRY_COLUMNS,
        """INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
   operator_user_id, request_id, posted_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING """,
        """WHERE business_id = $1 AND kind = $2""",
    ],
    "internal/billing/execution_pg.go": [
        EXECUTION_COLUMNS,
        """FROM billing_executions
WHERE execution_id = $1""",
        "queryGetExecution + ` FOR UPDATE`",
        """INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, request_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
RETURNING """,
        """UPDATE billing_executions
SET status = $2,
    updated_at = $3,
    receipt_recorded_at = COALESCE($4, receipt_recorded_at),
    terminal_reason = COALESCE($5, terminal_reason)
WHERE execution_id = $1
RETURNING """,
    ],
    "internal/billing/price_versions_pg.go": [
        PRICE_VERSION_COLUMNS,
        """FROM price_versions
WHERE id = $1""",
    ],
    "internal/billing/settlement_pg.go": [
        """FROM usage_receipts
WHERE execution_id = $1
FOR UPDATE""",
        """UPDATE usage_receipts
SET status = 'settled',
    settled_at = $2,
    claim_until = NULL,
    last_error = NULL
WHERE execution_id = $1
  AND status IN ('pending_dispatch', 'dispatched')
RETURNING """,
    ],
    "internal/billing/receipt_pg.go": [
        """INSERT INTO usage_receipts
  (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
   total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
   dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
ON CONFLICT (execution_id) DO NOTHING
RETURNING """,
    ],
}

# The console usage store is a different file with its own constants.
USAGE_WHERE = """FROM metering_events
WHERE org_id = $1
  AND created_at >= $2
  AND created_at <= $3
  AND ($4 = '' OR model = $4)
  AND ($5 = 0 OR user_id = $5)
  AND ($6 = '' OR key_id = $6)"""

USAGE_SELECT_COLUMNS = """SELECT event_id, created_at, model, provider,
       prompt_tokens, completion_tokens, total_tokens, cost_cents,
       tenant_id, org_id, user_id, key_id, execution_id """

QUERY_SQL = {
    "usage_page": USAGE_SELECT_COLUMNS + USAGE_WHERE + """
ORDER BY created_at DESC, event_id DESC
LIMIT $7""",
    "usage_page_after_cursor": USAGE_SELECT_COLUMNS + USAGE_WHERE + """
  AND (created_at, event_id) < ($7, $8)
ORDER BY created_at DESC, event_id DESC
LIMIT $9""",
    "usage_totals": """SELECT count(*),
       COALESCE(SUM(prompt_tokens), 0),
       COALESCE(SUM(completion_tokens), 0),
       COALESCE(SUM(total_tokens), 0),
       COALESCE(SUM(cost_cents), 0) """ + USAGE_WHERE,
}

QUERY_ANCHORS = {
    "internal/console/usage_pg.go": [
        USAGE_WHERE,
        USAGE_SELECT_COLUMNS,
        """ORDER BY created_at DESC, event_id DESC
LIMIT $7""",
        """  AND (created_at, event_id) < ($7, $8)
ORDER BY created_at DESC, event_id DESC
LIMIT $9""",
        """SELECT count(*),
       COALESCE(SUM(prompt_tokens), 0),
       COALESCE(SUM(completion_tokens), 0),
       COALESCE(SUM(total_tokens), 0),
       COALESCE(SUM(cost_cents), 0) """,
    ],
}

STATEMENT_DEBIT_PREFIX = "billing_receipt:debit:"

STATEMENT_SQL = {
    "statement_page": f"""SELECT b.id, b.org_id, b.kind, b.direction, b.amount_micro, b.business_id,
       b.reason, b.operator_user_id, b.request_id, b.posted_at, b.created_at,
       CASE
         WHEN b.kind = 'debit' AND b.business_id LIKE '{STATEMENT_DEBIT_PREFIX}%'
           THEN (SELECT r.occurred_at FROM usage_receipts r
                  WHERE r.execution_id = substring(b.business_id FROM length('{STATEMENT_DEBIT_PREFIX}') + 1))
       END AS occurred_at
FROM billing_entries b
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3
ORDER BY b.posted_at DESC, b.id DESC
LIMIT $4""",
    "statement_page_after_cursor": f"""SELECT b.id, b.org_id, b.kind, b.direction, b.amount_micro, b.business_id,
       b.reason, b.operator_user_id, b.request_id, b.posted_at, b.created_at,
       CASE
         WHEN b.kind = 'debit' AND b.business_id LIKE '{STATEMENT_DEBIT_PREFIX}%'
           THEN (SELECT r.occurred_at FROM usage_receipts r
                  WHERE r.execution_id = substring(b.business_id FROM length('{STATEMENT_DEBIT_PREFIX}') + 1))
       END AS occurred_at
FROM billing_entries b
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3
  AND (b.posted_at, b.id) < ($4, $5)
ORDER BY b.posted_at DESC, b.id DESC
LIMIT $6""",
    "statement_totals": """SELECT count(*),
       COALESCE(SUM(CASE WHEN b.direction = 'credit' AND b.kind IN ('credit', 'debit', 'adjustment')
                         THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.direction = 'debit' AND b.kind IN ('credit', 'debit', 'adjustment')
                         THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.kind = 'reservation' THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.kind = 'release' THEN b.amount_micro ELSE 0 END), 0)
FROM billing_entries b
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3""",
    "statement_late_totals": f"""SELECT count(*), COALESCE(SUM(b.amount_micro), 0)
FROM billing_entries b
JOIN usage_receipts r
  ON b.kind = 'debit'
 AND b.business_id LIKE '{STATEMENT_DEBIT_PREFIX}%'
 AND r.execution_id = substring(b.business_id FROM length('{STATEMENT_DEBIT_PREFIX}') + 1)
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3
  AND r.occurred_at < $2""",
}

STATEMENT_ANCHORS = {
    "internal/billing/statement_pg.go": [
        STATEMENT_DEBIT_PREFIX,
        """CASE
         WHEN b.kind = 'debit' AND b.business_id LIKE '""",
        """END AS occurred_at
FROM billing_entries b
WHERE b.org_id = $1 AND b.posted_at >= $2 AND b.posted_at < $3""",
        """ORDER BY b.posted_at DESC, b.id DESC
LIMIT $4""",
        """  AND (b.posted_at, b.id) < ($4, $5)
ORDER BY b.posted_at DESC, b.id DESC
LIMIT $6""",
        """SELECT count(*),
       COALESCE(SUM(CASE WHEN b.direction = 'credit' AND b.kind IN ('credit', 'debit', 'adjustment')
                         THEN b.amount_micro ELSE 0 END), 0),""",
        """SELECT count(*), COALESCE(SUM(b.amount_micro), 0)
FROM billing_entries b
JOIN usage_receipts r
  ON b.kind = 'debit'
 AND b.business_id LIKE '""",
        """  AND r.occurred_at < $2""",
    ],
}

# Variant B: the experiment-only conditional-update statements.  No Go source,
# by construction: production keeps exactly one path.
SQL_B = {
    "account_reserve_cond": f"""UPDATE billing_accounts
SET reserved_micro = reserved_micro + $2, version = version + 1, updated_at = $3
WHERE org_id = $1
  AND status = 'active'
  AND posted_balance_micro - reserved_micro >= $2
RETURNING {ACCOUNT_COLUMNS}""",
    "account_settle_debit_cond": f"""UPDATE billing_accounts
SET posted_balance_micro = posted_balance_micro - $2, version = version + 1, updated_at = $3
WHERE org_id = $1
RETURNING {ACCOUNT_COLUMNS}""",
    "account_release_cond": f"""UPDATE billing_accounts
SET reserved_micro = reserved_micro - $2, version = version + 1, updated_at = $3
WHERE org_id = $1
  AND reserved_micro >= $2
RETURNING {ACCOUNT_COLUMNS}""",
}


def sqlcheck(verbose=False):
    """Fail loudly when the shipped SQL is no longer what this harness measures."""
    wanted = dict(ANCHORS)
    for path, anchors in QUERY_ANCHORS.items():
        wanted.setdefault(path, []).extend(anchors)
    for path, anchors in STATEMENT_ANCHORS.items():
        wanted.setdefault(path, []).extend(anchors)
    problems = []
    for path, anchors in sorted(wanted.items()):
        text = (ROOT / path).read_text(encoding="utf-8")
        for anchor in anchors:
            if anchor not in text:
                problems.append(f"{path}: anchor not found: {anchor[:80]!r}")
    if problems:
        raise SystemExit("sqlcheck FAILED — the shipped SQL changed; update scripts/hotspot-ab.py:\n  "
                         + "\n  ".join(problems))
    if verbose:
        for path, anchors in sorted(wanted.items()):
            print(f"ok {path}: {len(anchors)} anchors")
    return {"files": sorted(wanted), "anchors": sum(len(a) for a in wanted.values())}


# ---------------------------------------------------------------------------
# 2. psycopg helpers
# ---------------------------------------------------------------------------

def import_psycopg():
    try:
        import psycopg  # noqa: F401
    except ImportError:
        raise SystemExit(
            "psycopg is required:\n"
            "  python3 -m venv /tmp/t11venv\n"
            "  /tmp/t11venv/bin/pip install 'psycopg[binary]' redis\n"
            "  /tmp/t11venv/bin/python scripts/hotspot-ab.py ...")
    import psycopg
    return psycopg


_PLACEHOLDER = re.compile(r"\$(\d+)")


def sqlify(text):
    """Translate the shipped ``$n`` placeholders into psycopg ``%s`` ones.

    The SQL text itself is *not* rewritten: ``%`` is escaped and the ``$n``
    sequence is replaced in place, so the statement the server parses is the
    shipped statement.
    """
    out = text.replace("%", "%%")
    order = []

    def repl(match):
        order.append(int(match.group(1)))
        return "%s"

    out = _PLACEHOLDER.sub(repl, out)
    return out, order


class Conn:
    """One psycopg connection that executes the shipped statement texts by name."""

    def __init__(self, conn, app_name="t11-hotspot"):
        self.conn = conn
        self.app_name = app_name
        self.statements = 0

    def cursor(self):
        return self.conn.cursor()

    def run(self, sql_name, args, text=None):
        text = text if text is not None else SQL[sql_name] if sql_name in SQL else SQL_B[sql_name]
        translated, order = sqlify(text)
        params = [args[i - 1] for i in order] if order else None
        cur = self.conn.cursor()
        cur.execute(translated, params)
        self.statements += 1
        return cur

    def execute(self, text, params=None):
        cur = self.conn.cursor()
        cur.execute(text, params)
        self.statements += 1
        return cur

    def commit(self):
        self.conn.commit()

    def rollback(self):
        self.conn.rollback()


class Pool:
    """A tiny fixed-size connection pool (psycopg's pool is a separate package)."""

    def __init__(self, dsn, size, app_name="t11-hotspot", setup=None):
        self.psycopg = import_psycopg()
        self.dsn = dsn
        self.size = size
        self.app_name = app_name
        self.items = queue.LifoQueue()
        self.all = []
        for _ in range(size):
            conn = self.psycopg.connect(dsn, autocommit=False)
            with conn.cursor() as cur:
                cur.execute(f"SET application_name = '{app_name}'")
            conn.commit()
            if setup:
                setup(conn)
            wrapped = Conn(conn, app_name)
            self.all.append(wrapped)
            self.items.put(wrapped)

    def acquire(self, timeout=60):
        return self.items.get(timeout=timeout)

    def release(self, conn):
        try:
            conn.rollback()
        except Exception:  # noqa: BLE001
            pass
        self.items.put(conn)

    def close(self):
        for conn in self.all:
            try:
                conn.conn.close()
            except Exception:  # noqa: BLE001
                pass


class PgSampler(threading.Thread):
    """Samples PostgreSQL lock waiters and the container's CPU while a round runs."""

    def __init__(self, dsn, container, interval=0.1):
        super().__init__(daemon=True)
        self.psycopg = import_psycopg()
        self.dsn = dsn
        self.container = container
        self.interval = interval
        self.stop_event = threading.Event()
        self.samples = []

    def run(self):
        conn = self.psycopg.connect(self.dsn, autocommit=True)
        with conn.cursor() as cur:
            cur.execute("SET application_name = 't11-sampler'")
        while not self.stop_event.is_set():
            try:
                with conn.cursor() as cur:
                    cur.execute("""
                        SELECT count(*) FILTER (WHERE wait_event_type = 'Lock'),
                               count(*) FILTER (WHERE state = 'active'),
                               count(*) FILTER (WHERE wait_event_type = 'LWLock'),
                               count(*) FILTER (WHERE state = 'idle in transaction')
                        FROM pg_stat_activity
                        WHERE datname = 'gateway_test' AND pid <> pg_backend_pid()""")
                    lock_waiters, active, lwlock_waiters, idle_tx = cur.fetchone()
                cpu_usec = self.cpu_usec()
                self.samples.append({
                    "at": time.time(),
                    "lock_waiters": lock_waiters,
                    "active": active,
                    "lwlock_waiters": lwlock_waiters,
                    "idle_in_tx": idle_tx,
                    "cpu_usec": cpu_usec,
                })
            except Exception as err:  # noqa: BLE001
                self.samples.append({"at": time.time(), "error": str(err)[:200]})
            self.stop_event.wait(self.interval)
        conn.close()

    def cpu_usec(self):
        proc = try_run(["docker", "exec", self.container, "cat", "/sys/fs/cgroup/cpu.stat"], timeout=10)
        if proc.returncode != 0:
            return None
        for line in proc.stdout.splitlines():
            if line.startswith("usage_usec"):
                return int(line.split()[1])
        return None

    def stop(self):
        self.stop_event.set()
        self.join(timeout=10)
        usable = [s for s in self.samples if "error" not in s]
        cpu = [s["cpu_usec"] for s in usable if s.get("cpu_usec") is not None]
        return {
            "samples": len(usable),
            "errors": len(self.samples) - len(usable),
            "lock_waiters_max": max((s["lock_waiters"] for s in usable), default=None),
            "lock_waiters_mean": round(statistics.fmean([s["lock_waiters"] for s in usable]), 3) if usable else None,
            "active_max": max((s["active"] for s in usable), default=None),
            "active_mean": round(statistics.fmean([s["active"] for s in usable]), 2) if usable else None,
            "lwlock_waiters_max": max((s["lwlock_waiters"] for s in usable), default=None),
            "idle_in_tx_max": max((s["idle_in_tx"] for s in usable), default=None),
            "cpu_usec_delta": (cpu[-1] - cpu[0]) if len(cpu) >= 2 else None,
        }


# ---------------------------------------------------------------------------
# 3. The isolated PostgreSQL lab for hotspot/query
# ---------------------------------------------------------------------------

class Lab:
    """A private PostgreSQL container with the real migrations applied."""

    def __init__(self, run_id, keep=False, dsn=None, build_dir=None):
        self.run_id = run_id
        self.container = f"{CONTAINER_PREFIX}{run_id}-pg"
        self.keep = keep
        self.port = free_port()
        self.build_dir = build_dir
        self._own_build_dir = False
        self._external = dsn is not None
        self.dsn = dsn

    def start(self):
        if self._external:
            self.apply_migrations()
            return
        log(f"starting {self.container} on 127.0.0.1:{self.port}")
        args = ["docker", "run", "-d", "--name", self.container,
                "-p", f"127.0.0.1:{self.port}:5432",
                "-e", "POSTGRES_DB=gateway_test", "-e", "POSTGRES_USER=gateway_test",
                "-e", "POSTGRES_PASSWORD=test_only_local",
                "--tmpfs", "/var/lib/postgresql/data"]
        args += [PG_IMAGE] + PG_SERVER_FLAGS
        must_run(args, "docker run postgres")
        wait_tcp(self.port, 60, "postgres")
        self.wait_ready()
        self.dsn = f"postgres://gateway_test:test_only_local@127.0.0.1:{self.port}/gateway_test?sslmode=disable"
        self.apply_migrations()

    def wait_ready(self, timeout=90):
        deadline = time.time() + timeout
        while time.time() < deadline:
            proc = try_run(["docker", "exec", self.container, "psql", "-U", "gateway_test",
                            "-d", "gateway_test", "-At", "-c", "SELECT 1"], timeout=15)
            if proc.returncode == 0 and proc.stdout.strip() == "1":
                return
            time.sleep(0.3)
        raise RuntimeError("postgres did not accept statements")

    def apply_migrations(self):
        if self.build_dir is None:
            self.build_dir = pathlib.Path(tempfile.mkdtemp(prefix=f"{CONTAINER_PREFIX}{self.run_id}-build-"))
            self._own_build_dir = True
        build_dir = self.build_dir
        binary = build_dir / "migrate"
        if not binary.exists():
            must_run(["go", "build", "-o", str(binary), "./cmd/migrate"], "build migrate", cwd=ROOT, timeout=900)
        env = dict(os.environ)
        env["POSTGRES_DSN"] = self.dsn
        must_run([str(binary), "up"], "migrate up", cwd=ROOT, env=env, timeout=300)

    def psql(self, sql, database="gateway_test"):
        return must_run(["docker", "exec", "-i", self.container, "psql", "-v", "ON_ERROR_STOP=1",
                         "-U", "gateway_test", "-d", database, "-At", "-c", sql], "psql").stdout.strip()

    def pg_settings(self):
        psycopg = import_psycopg()
        keys = ["server_version", "max_connections", "shared_buffers", "work_mem",
                "synchronous_commit", "wal_level", "track_io_timing", "fsync", "full_page_writes"]
        with psycopg.connect(self.dsn, autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT name, setting FROM pg_settings WHERE name IN {tuple(keys)}")
            return {name: setting for name, setting in cur.fetchall()}

    def teardown(self):
        if self.keep:
            return {"kept": True, "container": self.container}
        result = {"kept": False, "container": self.container}
        if not self._external:
            log(f"removing {self.container}")
            must_run(["docker", "rm", "-f", self.container], "docker rm")
        if self._own_build_dir and self.build_dir:
            import shutil
            shutil.rmtree(self.build_dir, ignore_errors=True)
            result["removed_build_dir"] = str(self.build_dir)
        return result


# ---------------------------------------------------------------------------
# 4. B03: hotspot A/B on the admission and settlement transactions
# ---------------------------------------------------------------------------

ACCOUNT_IDLE_BALANCE = 10 ** 12  # 1,000,000 CNY of headroom; the wallet is never the refusal reason
RESERVE_AMOUNT = 89              # micro, the same upper bound T10 measures for the counted shape
RESERVE_CHARGE = 89
SETTLE_CHARGE = 89

# The id of the price version this lab publishes; the shipped reserve path pins
# a price version by id, so the harness reads the real id back instead of
# assuming 1.
PRICE_VERSION = {"id": None}

BUSINESS_RESERVATION = "billing_execution:reservation:"
BUSINESS_RELEASE = "billing_execution:release:"
BUSINESS_DEBIT = "billing_receipt:debit:"


class Refused(Exception):
    """A money-path refusal (insufficient funds / suspended / missing row)."""

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


class Replayed(Exception):
    """An idempotent replay: the transaction moved nothing."""


def account_tuple(row):
    return {
        "org_id": row[0], "currency": row[1], "posted": row[2], "reserved": row[3],
        "status": row[4], "version": row[5],
    }


def reserve_for_update(cur, org, execution_id, amount, now, replay_probe=None):
    """Variant A: the shipped admission transaction shape.

    Statement order and predicates follow internal/billing/execution_pg.go
    ``reserveTx`` exactly: ensure -> lock -> price -> replay -> status/available
    check -> insert execution -> insert entry -> absolute-value UPDATE -> audit.
    Returns (lock_wait_seconds, account).
    """
    conn = cur  # Conn
    conn.run("ensure_account", [org, now])
    lock_start = time.perf_counter()
    row = conn.run("lock_account", [org]).fetchone()
    lock_wait = time.perf_counter() - lock_start
    if row is None:
        raise Refused("account_missing")
    account = account_tuple(row)
    conn.run("get_price_version", [PRICE_VERSION["id"]]).fetchone()
    existing = conn.run("get_execution", [execution_id]).fetchone()
    if existing is not None:
        raise Replayed("exists")
    if account["status"] != "active":
        raise Refused("suspended")
    if account["posted"] - account["reserved"] < amount:
        raise Refused("insufficient_funds")
    conn.run("insert_execution", [
        execution_id, org, None, None, "gw-t11", PRICE_VERSION["id"], "t11-model", "reserved",
        amount, 16, 32, None, now])
    conn.run("insert_entry", [
        org, "reservation", amount, "credit", BUSINESS_RESERVATION + execution_id,
        "0" * 64, "reserve t11-model", None, None, now, now])
    posted, reserved = account["posted"], account["reserved"] + amount
    conn.run("update_account", [org, posted, reserved, now])
    conn.run("insert_execution_audit", [
        None, org, "billing.execution.reserved", "billing_execution", execution_id,
        json.dumps({"execution_id": execution_id, "reserved_micro": str(amount)}), None, now])
    return lock_wait, account


def reserve_cond_update(cur, org, execution_id, amount, now):
    """Variant B: the same transaction with the predicate inside the UPDATE."""
    conn = cur
    conn.run("ensure_account", [org, now])
    conn.run("get_price_version", [PRICE_VERSION["id"]]).fetchone()
    existing = conn.run("get_execution", [execution_id]).fetchone()
    if existing is not None:
        raise Replayed("exists")
    lock_start = time.perf_counter()
    row = conn.run("account_reserve_cond", [org, amount, now]).fetchone()
    lock_wait = time.perf_counter() - lock_start
    if row is None:
        # Classify the refusal outside the lock: exactly one extra read only on
        # the refusal path, which is the path that stays rare in a funded wallet.
        probe = conn.run("get_account", [org]).fetchone()
        if probe is None:
            raise Refused("account_missing")
        state = account_tuple(probe)
        raise Refused("suspended" if state["status"] != "active" else "insufficient_funds")
    account = account_tuple(row)
    conn.run("insert_execution", [
        execution_id, org, None, None, "gw-t11", PRICE_VERSION["id"], "t11-model", "reserved",
        amount, 16, 32, None, now])
    conn.run("insert_entry", [
        org, "reservation", amount, "credit", BUSINESS_RESERVATION + execution_id,
        "0" * 64, "reserve t11-model", None, None, now, now])
    conn.run("insert_execution_audit", [
        None, org, "billing.execution.reserved", "billing_execution", execution_id,
        json.dumps({"execution_id": execution_id, "reserved_micro": str(amount)}), None, now])
    return lock_wait, account


EXPECTED_DIGEST = "a" * 64


def transition_execution(conn, execution_id, now):
    """The shipped TransitionExecutionTx shape: lock, decide, then UPDATE.

    Both settle variants use this identical statement pair: the state-machine
    guard is not the A/B variable, the account write strategy is.
    """
    row = conn.run("lock_execution", [execution_id]).fetchone()
    if row is None:
        raise Refused("execution_missing")
    status = row[7]
    if status == "settled":
        raise Replayed("execution_settled")
    if status != "receipt_recorded":
        raise Refused("execution_" + status)
    conn.run("update_execution", [execution_id, "settled", now, None, None]).fetchone()


def settle_for_update(cur, org, execution_id, charge, release, now):
    """Variant A: the shipped settlement transaction shape (settleReceiptTx).

    receipt lock -> digest/status guard -> execution lock + guarded transition ->
    (ensure, lock, replay, insert entry, absolute UPDATE, adjustment audit) x2 ->
    guarded receipt settle -> receipt audit.  Returns (lock_wait, account).
    """
    conn = cur
    lock_start = time.perf_counter()
    receipt = conn.run("lock_receipt", [execution_id]).fetchone()
    first_lock_wait = time.perf_counter() - lock_start
    if receipt is None:
        raise Refused("receipt_missing")
    status = receipt[13]
    if status == "settled":
        raise Replayed("settled")
    if status == "exception":
        raise Refused("receipt_exception")
    if receipt[12] != EXPECTED_DIGEST:
        raise Refused("receipt_payload_conflict")
    transition_execution(conn, execution_id, now)
    account = None
    for kind, amount, business in (
            ("debit", charge, BUSINESS_DEBIT + execution_id),
            ("release", release, BUSINESS_RELEASE + execution_id)):
        if amount <= 0:
            continue
        conn.run("ensure_account", [org, now])
        row = conn.run("lock_account", [org]).fetchone()
        state = account_tuple(row)
        existing = conn.run("get_entry_by_business", [business, kind]).fetchone()
        if existing is not None:
            account = state
            continue
        direction = "debit"
        conn.run("insert_entry", [
            org, kind, amount, direction, business, "0" * 64,
            f"settle usage receipt {execution_id}", None, None, now, now])
        posted = state["posted"] - amount if kind == "debit" else state["posted"]
        reserved = state["reserved"] - amount if kind == "release" else state["reserved"]
        row = conn.run("update_account", [org, posted, reserved, now]).fetchone()
        account = account_tuple(row)
        conn.run("insert_adjustment_audit", [
            None, org, "billing.adjustment", "billing_account", str(org),
            json.dumps({"execution_id": execution_id, "kind": kind}), None, now])
    settled = conn.run("mark_receipt_settled", [execution_id, now]).fetchone()
    if settled is None:
        raise Refused("receipt_left_dispatchable")
    conn.run("insert_receipt_audit", [
        None, org, "billing.settlement.settled", "billing_execution", execution_id,
        json.dumps({"execution_id": execution_id, "charge_micro": str(charge)}),
        None, execution_id, now])
    return first_lock_wait, account


def settle_cond_update(cur, org, execution_id, charge, release, now):
    """Variant B: the same settlement with conditional (relative) account writes."""
    conn = cur
    lock_start = time.perf_counter()
    receipt = conn.run("lock_receipt", [execution_id]).fetchone()
    first_lock_wait = time.perf_counter() - lock_start
    if receipt is None:
        raise Refused("receipt_missing")
    status = receipt[13]
    if status == "settled":
        raise Replayed("settled")
    if status == "exception":
        raise Refused("receipt_exception")
    if receipt[12] != EXPECTED_DIGEST:
        raise Refused("receipt_payload_conflict")
    transition_execution(conn, execution_id, now)
    account = None
    for kind, amount, business in (
            ("debit", charge, BUSINESS_DEBIT + execution_id),
            ("release", release, BUSINESS_RELEASE + execution_id)):
        if amount <= 0:
            continue
        sql_name = "account_settle_debit_cond" if kind == "debit" else "account_release_cond"
        row = conn.run(sql_name, [org, amount, now]).fetchone()
        if row is None:
            raise Refused("release_exceeds_reserved" if kind == "release" else "account_missing")
        account = account_tuple(row)
        existing = conn.run("get_entry_by_business", [business, kind]).fetchone()
        if existing is not None:
            continue
        conn.run("insert_entry", [
            org, kind, amount, "debit", business, "0" * 64,
            f"settle usage receipt {execution_id}", None, None, now, now])
        conn.run("insert_adjustment_audit", [
            None, org, "billing.adjustment", "billing_account", str(org),
            json.dumps({"execution_id": execution_id, "kind": kind}), None, now])
    settled = conn.run("mark_receipt_settled", [execution_id, now]).fetchone()
    if settled is None:
        raise Refused("receipt_left_dispatchable")
    conn.run("insert_receipt_audit", [
        None, org, "billing.settlement.settled", "billing_execution", execution_id,
        json.dumps({"execution_id": execution_id, "charge_micro": str(charge)}),
        None, execution_id, now])
    return first_lock_wait, account


VARIANTS = {
    "for_update": {"reserve": reserve_for_update, "settle": settle_for_update},
    "cond_update": {"reserve": reserve_cond_update, "settle": settle_cond_update},
}


class Pgbench:
    """Runs pgbench inside the lab container with two interleaved scripts.

    pgbench is used for the performance cells because it is a C client: the
    Python driver's per-statement overhead (measured as the ``driver`` control)
    is larger than the effect under test at high concurrency.  Both variants run
    in the *same* pgbench process, interleaved per transaction with equal
    weights, so client count, sockets, caches and the wall-clock window are
    identical for A and B (stronger than an ABBA rotation across rounds).
    """

    def __init__(self, lab, run_id):
        self.lab = lab
        self.run_id = run_id
        self.dir = "/tmp/t11-hotspot"
        self.counter = 0
        must_run(["docker", "exec", lab.container, "mkdir", "-p", self.dir], "mkdir scripts dir")

    def copy_script(self, name, text):
        host_dir = pathlib.Path(tempfile.mkdtemp(prefix="t11-pgbench-"))
        host = host_dir / f"{name}.sql"
        host.write_text(text, encoding="utf-8")
        target = f"{self.dir}/{name}.sql"
        must_run(["docker", "cp", str(host), f"{self.lab.container}:{target}"], "docker cp script")
        return target

    def run(self, scripts, concurrency, seconds, defs=None):
        """scripts: list of (container_path, weight)."""
        self.counter += 1
        prefix = f"{self.dir}/log-{self.counter}"
        cmd = ["docker", "exec", self.lab.container, "pgbench", "-n",
               "-U", "gateway_test", "-d", "gateway_test", "-M", "prepared",
               "-c", str(concurrency), "-j", str(min(concurrency, 8)),
               "-T", str(max(1, int(round(seconds)))), "-r",
               "-l", "--sampling-rate", "1", f"--log-prefix={prefix}"]
        for path, weight in scripts:
            cmd += ["-f", f"{path}@{weight}"]
        merged_defs = dict(pgbench_defines())
        merged_defs.update(defs or {})
        for key, value in merged_defs.items():
            cmd += ["-D", f"{key}={value}"]
        proc = must_run(cmd, "pgbench", timeout=int(seconds) + 600)
        out = proc.stdout
        result = {
            "concurrency": concurrency,
            "seconds": seconds,
            "tps": parse_pgbench_number(out, r"tps = ([0-9.]+)"),
            "transactions": parse_pgbench_number(out, r"actually processed: (\d+)"),
            "failed": parse_pgbench_number(out, r"number of failed transactions: (\d+)"),
            "latency_average_ms": parse_pgbench_number(out, r"latency average = ([0-9.]+)"),
            "statement_latencies_ms": parse_pgbench_statements(out)[0],
            "statement_latencies_per_script": {
                str(no): block for no, block in parse_pgbench_statements(out)[1].items()},
            "stdout_tail": out[-1200:],
        }
        logs = try_run(["docker", "exec", self.lab.container, "sh", "-c",
                        f"cat {prefix}.* 2>/dev/null"], timeout=300).stdout
        per_script = {}
        for line in logs.splitlines():
            parts = line.split()
            if len(parts) != 6:
                continue
            try:
                script_no = int(parts[3])
                latency_us = int(parts[2])
            except ValueError:
                continue
            per_script.setdefault(script_no, []).append(latency_us / 1000.0)
        result["per_script_latency_ms"] = {
            str(no): latency_block(values) for no, values in sorted(per_script.items())}
        result["per_script_count"] = {str(no): len(values) for no, values in sorted(per_script.items())}
        result["log_lines"] = sum(len(v) for v in per_script.values())
        must_run(["docker", "exec", self.lab.container, "sh", "-c",
                  f"rm -f {prefix}.*"], "rm pgbench logs")
        return result


def parse_pgbench_number(text, pattern):
    match = re.search(pattern, text)
    return float(match.group(1)) if match else None


STATEMENT_ROW = re.compile(r"^\s*([0-9.]+)\s+(\d+)\s+(\S.*)$")


def parse_pgbench_statements(text):
    """The ``-r`` blocks: average ms and failures per statement, per script.

    pgbench 16 prints one ``SQL script N:`` section per script (with the script
    path on the header line) and one statement-latency block inside it.  The
    statement text may wrap over several lines, so a row without a leading
    "<ms> <failures>" pair is a continuation of the previous statement.
    """
    per_script = {}
    merged = {}
    script_no = 0
    in_block = False
    current = None
    for line in text.splitlines():
        if line.startswith("SQL script "):
            match = re.match(r"SQL script (\d+):", line)
            if match:
                script_no = int(match.group(1)) - 1
            in_block = False
            continue
        if line.lstrip().startswith("- statement latencies in milliseconds"):
            in_block = True
            current = None
            per_script.setdefault(script_no, {})
            continue
        if not in_block:
            continue
        if not line.strip() or line.lstrip().startswith("- "):
            in_block = bool(line.strip()) and in_block
            if not line.strip():
                continue
        match = STATEMENT_ROW.match(line)
        if match:
            # The key is the statement's first line (pgbench prints the rest as
            # continuation lines).  Within one script two statements can share a
            # first line (the two account UPDATEs of settlement); they then
            # aggregate, which the report states where it matters.
            current = match.group(3).strip()
            entry = {"avg_ms": float(match.group(1)), "failures": int(match.group(2))}
            per_script[script_no][current] = entry
            merged[current] = entry
    return merged, per_script


def pgss_snapshot(conn):
    with conn.cursor() as cur:
        cur.execute("""SELECT query, calls, total_exec_time, shared_blks_hit, shared_blks_read
                       FROM pg_stat_statements""")
        return {row[0]: {"calls": row[1], "total_exec_time": row[2],
                         "shared_blks_hit": row[3], "shared_blks_read": row[4]} for row in cur.fetchall()}


def pgss_delta(before, after):
    delta = []
    for query, values in after.items():
        old = before.get(query, {"calls": 0, "total_exec_time": 0.0,
                                 "shared_blks_hit": 0, "shared_blks_read": 0})
        calls = values["calls"] - old["calls"]
        if calls <= 0:
            continue
        delta.append({
            "query": " ".join(query.split())[:160],
            "calls": calls,
            "total_exec_time_ms": round(values["total_exec_time"] - old["total_exec_time"], 2),
            "mean_exec_time_ms": round((values["total_exec_time"] - old["total_exec_time"]) / calls, 4),
            "shared_blks_hit": values["shared_blks_hit"] - old["shared_blks_hit"],
            "shared_blks_read": values["shared_blks_read"] - old["shared_blks_read"],
        })
    delta.sort(key=lambda item: item["total_exec_time_ms"], reverse=True)
    return delta[:40]


def reset_hotspot_tables(conn):
    with conn.cursor() as cur:
        cur.execute("TRUNCATE billing_entries, usage_receipts, billing_executions, console_audit_events")
    conn.commit()


def seed_accounts_for(conn, org_ids, balance=ACCOUNT_IDLE_BALANCE, reserved=0):
    """Seeds the wallets *and* their opening ledger credit.

    The projection check is only meaningful when the ledger explains the whole
    balance, so the seeded balance is written as a real `credit` entry (the same
    thing an administrator top-up does) rather than as a bare column update.
    """
    with conn.cursor() as cur:
        cur.execute("""
            INSERT INTO billing_accounts (org_id, posted_balance_micro, reserved_micro, status, version,
                                          created_at, updated_at)
            SELECT g, %s, %s, 'active', 1, now(), now()
            FROM unnest(%s::bigint[]) AS g
            ON CONFLICT (org_id) DO UPDATE
              SET posted_balance_micro = EXCLUDED.posted_balance_micro,
                  reserved_micro = EXCLUDED.reserved_micro, status = 'active',
                  version = 1, updated_at = now()""",
                    (balance, reserved, org_ids))
        cur.execute("""
            INSERT INTO billing_entries
              (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
               operator_user_id, request_id, posted_at, created_at)
            SELECT g, 'credit', %s, 'credit', 'bench:seed-credit:' || g, repeat('0', 64),
                   'bench seed credit', NULL, NULL, now(), now()
            FROM unnest(%s::bigint[]) AS g
            ON CONFLICT (business_id, kind) DO NOTHING""", (balance, org_ids))
    conn.commit()


def seed_settlement_work(conn, org_ids, count, amount=RESERVE_AMOUNT, charge=SETTLE_CHARGE):
    """Bulk-creates executions + reservations + receipts so a settle round has input.

    The rows are exactly the state settleReceiptTx expects: execution in
    receipt_recorded, receipt in dispatched, reservation posted on the account.
    """
    per_org = max(1, count // max(1, len(org_ids)))
    with conn.cursor() as cur:
        cur.execute("""
            INSERT INTO billing_executions
              (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
               reserved_micro, input_tokens_upper, output_tokens_upper, request_id,
               created_at, updated_at, receipt_recorded_at)
            SELECT 'exec_seed_' || org || '_' || g, org, NULL, NULL, 'gw-t11', %s, 't11-model',
                   'receipt_recorded', %s, 16, 32, NULL, now(), now(), now()
            FROM unnest(%s::bigint[]) AS org, generate_series(1, %s) AS g""",
                    (PRICE_VERSION["id"], amount, org_ids, per_org))
        cur.execute("""
            INSERT INTO usage_receipts
              (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
               total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
               dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
            SELECT e.execution_id, 'evt_seed_' || e.execution_id, e.org_id, NULL, NULL, 't11-model',
                   16, 32, 48, %s, %s, now(), repeat('a', 64), 'dispatched', now(), NULL, 0, NULL, now(), NULL
            FROM billing_executions e
            WHERE e.execution_id LIKE 'exec_seed_%%'""", (PRICE_VERSION["id"], charge))
        cur.execute("""
            INSERT INTO billing_entries
              (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
               operator_user_id, request_id, posted_at, created_at)
            SELECT e.org_id, 'reservation', %s, 'credit',
                   'billing_execution:reservation:' || e.execution_id, repeat('0', 64),
                   'seed reservation', NULL, NULL, now(), now()
            FROM billing_executions e WHERE e.execution_id LIKE 'exec_seed_%%'""", (amount,))
        cur.execute("""
            UPDATE billing_accounts SET reserved_micro = %s, updated_at = now()
            WHERE org_id = ANY(%s::bigint[])""", (per_org * amount, org_ids))
    conn.commit()
    with conn.cursor() as cur:
        cur.execute("""SELECT execution_id, org_id FROM billing_executions
                       WHERE execution_id LIKE 'exec_seed_%%' ORDER BY execution_id""")
        return cur.fetchall()


def correctness_refusal_test(conn, pool, variant, org, user_id=None, amount=RESERVE_AMOUNT, funded=25):
    """F11-style: a wallet funded for exactly `funded` reservations, 4x demand.

    Both variants must admit exactly `funded` and refuse the rest, with the
    projection equal to the ledger afterwards.
    """
    start_org = org
    with conn.cursor() as cur:
        cur.execute("""SELECT count(*) FROM billing_entries WHERE org_id = %s""", (start_org,))
        if cur.fetchone()[0]:
            raise RuntimeError(f"refusal org {start_org} is not empty")
        cur.execute("""
            INSERT INTO billing_accounts (org_id, posted_balance_micro, reserved_micro, status, version,
                                          created_at, updated_at)
            VALUES (%s, %s, 0, 'active', 1, now(), now())
            ON CONFLICT (org_id) DO UPDATE SET posted_balance_micro = EXCLUDED.posted_balance_micro,
                reserved_micro = 0, version = 1, updated_at = now()""",
                    (start_org, amount * funded))
    conn.commit()

    outcomes = {"ok": 0, "insufficient_funds": 0, "other": 0}
    admit_latency, refuse_latency = [], []
    lock = threading.Lock()
    barrier = threading.Barrier(funded * 4, timeout=120)

    def worker(index):
        conn_ = pool.acquire()
        try:
            barrier.wait()
            now = datetime.now(timezone.utc)
            started = time.perf_counter()
            try:
                VARIANTS[variant]["reserve"](conn_, start_org, f"exec_refuse_{variant}_{index}", amount, now)
                conn_.commit()
                elapsed = (time.perf_counter() - started) * 1000.0
                with lock:
                    outcomes["ok"] += 1
                    admit_latency.append(elapsed)
            except Refused as err:
                conn_.rollback()
                elapsed = (time.perf_counter() - started) * 1000.0
                with lock:
                    outcomes[err.reason if err.reason in outcomes else "other"] += 1
                    refuse_latency.append(elapsed)
            except Exception:  # noqa: BLE001
                conn_.rollback()
                with lock:
                    outcomes["other"] += 1
        finally:
            pool.release(conn_)

    threads = [threading.Thread(target=worker, args=(i,), daemon=True) for i in range(funded * 4)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    with conn.cursor() as cur:
        cur.execute("""SELECT posted_balance_micro, reserved_micro, version FROM billing_accounts
                       WHERE org_id = %s""", (start_org,))
        posted, reserved, version = cur.fetchone()
        cur.execute("""SELECT count(*), COALESCE(SUM(CASE WHEN kind = 'reservation' THEN amount_micro ELSE 0 END), 0)
                       FROM billing_entries WHERE org_id = %s""", (start_org,))
        entries, ledger_reserved = cur.fetchone()
        cur.execute("SELECT count(*) FROM billing_executions WHERE org_id = %s", (start_org,))
        executions = cur.fetchone()[0]
        cur.execute("SELECT count(*) FROM console_audit_events WHERE actor_org_id = %s", (start_org,))
        audits = cur.fetchone()[0]
    return {
        "variant": variant,
        "funded_reservations": funded,
        "demand": funded * 4,
        "outcomes": outcomes,
        "account": {"posted": posted, "reserved": reserved, "version": version},
        "ledger_entries": entries,
        "ledger_reserved": ledger_reserved,
        "executions": executions,
        "audits": audits,
        "admit_latency_ms": latency_block(admit_latency),
        "refuse_latency_ms": latency_block(refuse_latency),
        "no_overdraft": reserved == amount * funded and outcomes["ok"] == funded,
        "projection_matches_ledger": reserved == ledger_reserved and posted == amount * funded,
        "ledger_matches_executions": entries == executions == funded,
        "version_matches": version == 1 + funded,
    }


def project_check(conn, org_ids):
    """Projection == ledger for every org, plus per-execution postings <= 1."""
    with conn.cursor() as cur:
        cur.execute("""
            SELECT a.org_id, a.posted_balance_micro, a.reserved_micro,
                   COALESCE(SUM(CASE WHEN e.kind IN ('credit','debit','adjustment')
                                     THEN CASE WHEN e.direction = 'credit' THEN e.amount_micro
                                               ELSE -e.amount_micro END ELSE 0 END), 0)::bigint,
                   COALESCE(SUM(CASE WHEN e.kind IN ('reservation','release')
                                     THEN CASE WHEN e.direction = 'credit' THEN e.amount_micro
                                               ELSE -e.amount_micro END ELSE 0 END), 0)::bigint
            FROM billing_accounts a LEFT JOIN billing_entries e ON e.org_id = a.org_id
            WHERE a.org_id = ANY(%s::bigint[]) GROUP BY a.org_id, a.posted_balance_micro, a.reserved_micro""",
                    (org_ids,))
        rows = cur.fetchall()
    mismatches = [{"org_id": r[0], "posted": r[1], "ledger_posted": r[3],
                   "reserved": r[2], "ledger_reserved": r[4]}
                  for r in rows if r[1] != r[3] or r[2] != r[4]]
    return {"accounts": len(rows), "mismatches": mismatches}


def org_expression(accounts, base=800000):
    if accounts == 1:
        return f"\\set org {base + 1}"
    return f"\\set org {base} + (:client_id % {accounts}) + 1"


def pgbench_defines():
    """Colon-bearing literals travel as bound variables.

    pgbench's lexer reads a ``:name`` inside a quoted SQL literal as a variable
    reference, and in prepared mode it then turns it into a parameter whose type
    cannot be inferred.  The shipped business-id prefixes therefore travel with
    ``-D`` and appear in the script as ``:receipt_debit_prefix``.
    """
    return {
        "reservation_prefix": "billing_execution:reservation:",
        "release_prefix": "billing_execution:release:",
        "receipt_debit_prefix": "billing_receipt:debit:",
        "reserve_reason": "reserve t11-model",
        "settle_reason": "settle usage receipt",
        "bench_entry_prefix": "bench:reservation:",
    }


def pgbench_reserve_script(variant, accounts, price_version_id, with_ensure=True):
    """The admission transaction, as pgbench sees it (one statement per round trip).

    Variant A keeps the shipped statement order: the account row is locked by
    ``SELECT ... FOR UPDATE`` *before* the price and replay reads.  Variant B
    takes the same lock inside a conditional ``UPDATE`` after those reads.  Both
    write the reservation with the same relative arithmetic the Go path performs
    (the Go-side ``posted + amount`` is not a measurable cost; the round-trip
    count and the lock acquisition point are the explanatory variables).
    """
    eid = "'exec_' || :client_id || '_' || :n"
    head = [r"\set n :n + 1", org_expression(accounts)]
    if with_ensure:
        head.append("""INSERT INTO billing_accounts
  (org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
VALUES (:org, 'CNY', 0, 0, 'active', 1, now(), now())
ON CONFLICT (org_id) DO NOTHING;""")
    price = f"""SELECT {PRICE_VERSION_COLUMNS}
FROM price_versions
WHERE id = {price_version_id};"""
    replay = f"""SELECT {EXECUTION_COLUMNS}
FROM billing_executions
WHERE execution_id = {eid};"""
    insert_execution = f"""INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, request_id, created_at, updated_at)
VALUES ({eid}, :org, NULL, NULL, 'gw-t11', {price_version_id}, 't11-model', 'reserved',
        {RESERVE_AMOUNT}, 16, 32, NULL, now(), now());"""
    insert_entry = f"""INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
   operator_user_id, request_id, posted_at, created_at)
VALUES (:org, 'reservation', {RESERVE_AMOUNT}, 'credit',
        :reservation_prefix || {eid}, repeat('0', 64), :reserve_reason,
        NULL, NULL, now(), now());"""
    audit = f"""INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES (NULL, :org, 'billing.execution.reserved', 'billing_execution', {eid}, 'success',
        '{{}}'::jsonb, NULL, NULL, now());"""
    lock = f"""SELECT {ACCOUNT_COLUMNS}
FROM billing_accounts
WHERE org_id = :org FOR UPDATE;"""
    update_absolute = f"""UPDATE billing_accounts
SET posted_balance_micro = posted_balance_micro, reserved_micro = reserved_micro + {RESERVE_AMOUNT},
    version = version + 1, updated_at = now()
WHERE org_id = :org;"""
    update_conditional = f"""UPDATE billing_accounts
SET reserved_micro = reserved_micro + {RESERVE_AMOUNT}, version = version + 1, updated_at = now()
WHERE org_id = :org
  AND status = 'active'
  AND posted_balance_micro - reserved_micro >= {RESERVE_AMOUNT};"""
    if variant == "for_update":
        body = [lock, price, replay, insert_execution, insert_entry, update_absolute, audit]
    else:
        body = [price, replay, update_conditional, insert_execution, insert_entry, audit]
    return "\n".join(head + body) + "\n"


def pgbench_settle_script(variant, accounts, price_version_id, with_ensure=True):
    """The settlement transaction, as pgbench sees it.

    Both variants keep the shipped lock order (receipt -> execution -> account)
    and the same replay pre-check, entry insert and audit rows.  The A/B
    variable is only the account write: an explicit ``SELECT ... FOR UPDATE``
    plus absolute-column UPDATEs (A) against conditional/relative UPDATEs that
    take the lock themselves (B).
    """
    eid = "'exec_' || :client_id || '_' || :n"
    head = [
        r"\set n :n + 1",
        org_expression(accounts),
        # Harness guard: an under-seeded (client, n) pair must fail the
        # transaction instead of posting money for a receipt that does not
        # exist.  Identical in both variants, one index probe.
        f"""SELECT 1 / (SELECT count(*) FROM usage_receipts WHERE execution_id = {eid});""",
    ]
    lock_receipt = f"""SELECT {RECEIPT_COLUMNS}
FROM usage_receipts
WHERE execution_id = {eid}
FOR UPDATE;"""
    lock_execution = f"""SELECT {EXECUTION_COLUMNS}
FROM billing_executions
WHERE execution_id = {eid} FOR UPDATE;"""
    update_execution = f"""UPDATE billing_executions
SET status = 'settled',
    updated_at = now(),
    receipt_recorded_at = COALESCE(NULL, receipt_recorded_at),
    terminal_reason = COALESCE(NULL, terminal_reason)
WHERE execution_id = {eid};"""
    settle_receipt = f"""
UPDATE usage_receipts
SET status = 'settled',
    settled_at = now(),
    claim_until = NULL,
    last_error = NULL
WHERE execution_id = {eid}
  AND status IN ('pending_dispatch', 'dispatched');"""
    settlement_audit = f"""INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES (NULL, :org, 'billing.settlement.settled', 'billing_execution', {eid}, 'success',
        '{{}}'::jsonb, NULL, {eid}, now());"""
    ensure = """INSERT INTO billing_accounts
  (org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
VALUES (:org, 'CNY', 0, 0, 'active', 1, now(), now())
ON CONFLICT (org_id) DO NOTHING;"""

    postings = []
    for kind, amount, business in (
            ("debit", SETTLE_CHARGE, f":receipt_debit_prefix || {eid}"),
            ("release", RESERVE_AMOUNT, f":release_prefix || {eid}")):
        replay = f"""SELECT {ENTRY_COLUMNS}
FROM billing_entries
WHERE business_id = {business} AND kind = '{kind}';"""
        insert = f"""INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
   operator_user_id, request_id, posted_at, created_at)
VALUES (:org, '{kind}', {amount}, 'debit', {business}, repeat('0', 64),
        :settle_reason, NULL, NULL, now(), now());"""
        audit = f"""INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES (NULL, :org, 'billing.adjustment', 'billing_account', CAST(:org AS text), 'success',
        '{{}}'::jsonb, NULL, NULL, now());"""
        postings.append((kind, replay, insert, audit, amount))

    body = [lock_receipt, lock_execution, update_execution]
    for kind, replay, insert, audit, amount in postings:
        if variant == "for_update":
            if kind == "debit":
                update = f"""UPDATE billing_accounts
SET posted_balance_micro = posted_balance_micro - {amount}, reserved_micro = reserved_micro,
    version = version + 1, updated_at = now()
WHERE org_id = :org;"""
            else:
                update = f"""UPDATE billing_accounts
SET posted_balance_micro = posted_balance_micro, reserved_micro = reserved_micro - {amount},
    version = version + 1, updated_at = now()
WHERE org_id = :org;"""
            items = (ensure if with_ensure else None, lock_account(), replay, insert, update, audit)
        else:
            if kind == "debit":
                update = f"""UPDATE billing_accounts
SET posted_balance_micro = posted_balance_micro - {amount}, version = version + 1, updated_at = now()
WHERE org_id = :org;"""
            else:
                update = f"""UPDATE billing_accounts
SET reserved_micro = reserved_micro - {amount}, version = version + 1, updated_at = now()
WHERE org_id = :org
  AND reserved_micro >= {amount};"""
            items = (ensure if with_ensure else None, update, replay, insert, audit)
        body += [item for item in items if item]
    body += [settle_receipt, settlement_audit]
    return "\n".join(head + body) + "\n"


def lock_account():
    return f"""SELECT {ACCOUNT_COLUMNS}
FROM billing_accounts
WHERE org_id = :org FOR UPDATE;"""


def seed_settle_work_for_clients(conn, clients, accounts, per_client, price_version_id,
                                 amount=RESERVE_AMOUNT, charge=SETTLE_CHARGE, org_base=800000):
    """Seeds exactly the (client, n) execution ids the pgbench settle script asks for."""
    org_ids = [org_base + 1 + index for index in range(accounts)]
    with conn.cursor() as cur:
        cur.execute("""
            WITH spec AS (
              SELECT i AS client, k, %s + (i %% %s) + 1 AS org
              FROM generate_series(0, %s) AS i, generate_series(1, %s) AS k
            )
            INSERT INTO billing_executions
              (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
               reserved_micro, input_tokens_upper, output_tokens_upper, request_id,
               created_at, updated_at, receipt_recorded_at)
            SELECT 'exec_' || client || '_' || k, org, NULL, NULL, 'gw-t11', %s, 't11-model',
                   'receipt_recorded', %s, 16, 32, NULL, now(), now(), now()
            FROM spec""",
                    (org_base, accounts, clients - 1, per_client, price_version_id, amount))
        cur.execute("""
            INSERT INTO usage_receipts
              (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
               total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
               dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
            SELECT e.execution_id, 'evt_' || e.execution_id, e.org_id, NULL, NULL, 't11-model',
                   16, 32, 48, %s, %s, now(), repeat('a', 64), 'dispatched', now(), NULL, 0, NULL,
                   now(), NULL
            FROM billing_executions e WHERE e.execution_id LIKE 'exec_%%'""",
                    (price_version_id, charge))
        cur.execute("""
            INSERT INTO billing_entries
              (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
               operator_user_id, request_id, posted_at, created_at)
            SELECT e.org_id, 'reservation', %s, 'credit',
                   'billing_execution:reservation:' || e.execution_id, repeat('0', 64),
                   'seed reservation', NULL, NULL, now(), now()
            FROM billing_executions e WHERE e.execution_id LIKE 'exec_%%'""", (amount,))
        cur.execute("""
            UPDATE billing_accounts a
            SET reserved_micro = COALESCE((SELECT SUM(e.amount_micro) FROM billing_entries e
                                           WHERE e.org_id = a.org_id AND e.kind = 'reservation'), 0),
                updated_at = now()
            WHERE a.org_id = ANY(%s::bigint[])""", (org_ids,))
    conn.commit()
    with conn.cursor() as cur:
        cur.execute("SELECT count(*) FROM billing_executions WHERE execution_id LIKE 'exec_%%'")
        return cur.fetchone()[0]


def cmd_hotspot(args):
    sqlcheck(verbose=True)
    psycopg = import_psycopg()
    output = pathlib.Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    (output / "raw").mkdir(exist_ok=True)
    run_id = "".join(random.choices(string.ascii_lowercase + string.digits, k=8))
    lab = Lab(run_id, keep=args.keep, dsn=args.pg_dsn)
    report = {
        "task": "T11 B03 hotspot A/B",
        "run_id": run_id,
        "started_at": now_utc(),
        "commit": try_run(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip(),
        "dirty": len(try_run(["git", "status", "--porcelain"], cwd=ROOT).stdout.splitlines()),
        "host": {"platform": platform.platform(), "python": sys.version.split()[0],
                 "cpus": os.cpu_count()},
        "parameters": {
            "round_seconds": args.round_seconds,
            "accounts": args.accounts,
            "concurrency": args.concurrency,
            "ops": args.ops,
            "reserve_amount_micro": RESERVE_AMOUNT,
            "settle_charge_micro": SETTLE_CHARGE,
            "client": "pgbench -M simple (one statement per round trip), interleaved scripts",
            "variant_a": "SELECT ... FOR UPDATE + read-modify-write UPDATE (shipped statement order)",
            "variant_b": "conditional UPDATE ... WHERE available >= amount (experiment only)",
        },
        "cells": {},
        "sqlcheck": "passed",
    }
    try:
        lab.start()
        report["environment"] = {
            "pg_image": PG_IMAGE,
            "pgbench": try_run(["docker", "exec", lab.container, "pgbench", "--version"]).stdout.strip(),
            "server_flags": PG_SERVER_FLAGS,
            "settings": lab.pg_settings(),
        }
        conn = Conn(psycopg.connect(lab.dsn, autocommit=True))
        conn.execute("CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
        org_base = 800000
        org_count = 1000
        ladder_orgs = list(range(org_base + 1, org_base + 1 + org_count))
        with conn.conn.cursor() as cur:
            cur.execute("""INSERT INTO users (email, password_hash, status, platform_admin, auth_version,
                                              created_at, updated_at)
                           VALUES ('t11-hot@t11.test', 'hash', 'active', false, 1, now(), now())
                           RETURNING id""")
            user_id = cur.fetchone()[0]
            cur.execute("""INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
                           SELECT g, 'enterprise', 't11-hot-' || g, 'active', %s, now(), now()
                           FROM unnest(%s::bigint[]) AS g ON CONFLICT (id) DO NOTHING""",
                        (user_id, ladder_orgs))
            cur.execute("""INSERT INTO price_versions (model, currency, input_price_micro_per_million,
                                                       output_price_micro_per_million, effective_at, created_at)
                           VALUES ('t11-model', 'CNY', 1000000, 1000000, now() - interval '1 hour', now())
                           RETURNING id""")
            PRICE_VERSION["id"] = cur.fetchone()[0]
        conn.commit()

        # Correctness gate first: both variants must be money-equivalent.
        gate_pool = Pool(lab.dsn, 100, app_name="t11-hotspot-gate")
        report["correctness"] = []
        ladder_orgs = ladder_orgs[2:]
        for index, variant in enumerate(("for_update", "cond_update")):
            result = correctness_refusal_test(conn, gate_pool, variant,
                                              org=org_base + 1 + index, user_id=user_id)
            report["correctness"].append(result)
            log(f"correctness {variant}: {result['outcomes']} no_overdraft={result['no_overdraft']} "
                f"admit_p50={result['admit_latency_ms'].get('p50')}ms "
                f"refuse_p50={result['refuse_latency_ms'].get('p50')}ms")
        gate_pool.close()

        pgbench = Pgbench(lab, run_id)
        cells = []
        for op in args.ops.split(","):
            op_accounts = args.accounts if op != "driver" else [1]
            op_ladder = args.concurrency if op != "driver" else [1, 8, 32, 128]
            for accounts in op_accounts:
                org_ids = [org_base + 1 + index for index in range(accounts)]
                for concurrency in op_ladder:
                    cell_key = f"{op}_accounts{accounts}_c{concurrency}"
                    if op == "driver":
                        path = pgbench.copy_script("driver", "SELECT 1;\n")
                        sampler = PgSampler(lab.dsn, lab.container, interval=0.1)
                        sampler.start()
                        run = pgbench.run([(path, 1)], concurrency, args.round_seconds)
                        run["pg"] = sampler.stop()
                        report["cells"][cell_key] = {"op": op, "accounts": accounts,
                                                     "concurrency": concurrency, "run": run}
                        log(f"{cell_key}: {run['tps']}/s (harness ceiling control)")
                        continue
                    reset_hotspot_tables(conn.conn)
                    seed_accounts_for(conn.conn, org_ids)
                    if op == "settle":
                        scripts = [("settle_for_update", pgbench_settle_script("for_update", accounts, PRICE_VERSION["id"])),
                                   ("settle_cond_update", pgbench_settle_script("cond_update", accounts, PRICE_VERSION["id"]))]
                    elif op == "ensure":
                        scripts = [("reserve_with_ensure", pgbench_reserve_script("for_update", accounts, PRICE_VERSION["id"], True)),
                                   ("reserve_without_ensure", pgbench_reserve_script("for_update", accounts, PRICE_VERSION["id"], False))]
                    elif op == "ensure_settle":
                        scripts = [("settle_with_ensure", pgbench_settle_script("for_update", accounts, PRICE_VERSION["id"], True)),
                                   ("settle_without_ensure", pgbench_settle_script("for_update", accounts, PRICE_VERSION["id"], False))]
                    else:
                        scripts = [("reserve_for_update", pgbench_reserve_script("for_update", accounts, PRICE_VERSION["id"])),
                                   ("reserve_cond_update", pgbench_reserve_script("cond_update", accounts, PRICE_VERSION["id"]))]
                    paths = [(pgbench.copy_script(name, text), 1) for name, text in scripts]
                    attempts = 0
                    per_client = None
                    while True:
                        attempts += 1
                        if op in ("settle", "ensure_settle"):
                            reset_hotspot_tables(conn.conn)
                            seed_accounts_for(conn.conn, org_ids)
                            per_client = settle_seed_size(args.round_seconds, concurrency) * (2 ** (attempts - 1))
                            seeded = seed_settle_work_for_clients(
                                conn.conn, concurrency, accounts, per_client, PRICE_VERSION["id"])
                            report.setdefault("seeds", {})[cell_key] = {
                                "per_client": per_client, "rows": seeded, "attempts": attempts}
                        before = pgss_snapshot(conn.conn)
                        sampler = PgSampler(lab.dsn, lab.container, interval=0.1)
                        sampler.start()
                        try:
                            run = pgbench.run(paths, concurrency, args.round_seconds, defs={"n": 0})
                            run["pg"] = sampler.stop()
                            break
                        except RuntimeError as err:
                            sampler.stop()
                            if op not in ("settle", "ensure_settle") or attempts >= 4:
                                raise
                            log(f"{cell_key}: settle seed of {per_client}/client was too small "
                                f"({str(err)[:120]}); re-seeding with double")
                    run["seed_attempts"] = attempts
                    run["per_script_latency_ms"] = {
                        scripts[int(no)][0]: block for no, block in run["per_script_latency_ms"].items()}
                    run["per_script_count"] = {
                        scripts[int(no)][0]: count for no, count in run["per_script_count"].items()}
                    run["pg_stat_statements_delta"] = pgss_delta(before, pgss_snapshot(conn.conn))
                    cell = {"op": op, "accounts": accounts, "concurrency": concurrency, "run": run}
                    for name, _ in scripts:
                        block = run["per_script_latency_ms"].get(name, {})
                        cell.setdefault("per_variant", {})[name] = {
                            "transactions": run["per_script_count"].get(name),
                            "throughput_per_second": round(
                                (run["per_script_count"].get(name) or 0) / max(run["seconds"], 1e-9), 2),
                            "latency_ms": block,
                        }
                    report["cells"][cell_key] = cell
                    with (output / "raw" / f"hotspot-{cell_key}.json").open("w") as handle:
                        json.dump(cell, handle, indent=2, sort_keys=True)
                    labels = [name for name, _ in scripts]
                    summary = " | ".join(
                        f"{name} {cell['per_variant'][name]['throughput_per_second']}/s "
                        f"p50={cell['per_variant'][name]['latency_ms'].get('p50')} "
                        f"p99={cell['per_variant'][name]['latency_ms'].get('p99')}"
                        for name in labels)
                    log(f"{cell_key}: {summary}")
        report["projection"] = project_check(conn.conn, ladder_orgs[:max(args.accounts)])
    finally:
        report["finished_at"] = now_utc()
        report["teardown"] = lab.teardown()
    write_json(output / "raw" / f"hotspot-summary-{args.ops}.json", report)
    print(json.dumps({"run_id": run_id, "cells": len(report["cells"]), "output": str(output)}, indent=2))
    return 0


def settle_seed_size(seconds, concurrency):
    """Per-client settle work items: sized for the lock-bound rate, retried if short."""
    return max(200, int(-(-seconds * 1200 // max(concurrency, 1))))


def write_json(path, payload):
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, sort_keys=True, default=str), encoding="utf-8")
    return path


# ---------------------------------------------------------------------------
# 6. B08: query A/B (placeholder until the dataset builder lands)
# ---------------------------------------------------------------------------

# The read mix.  Script order is the contract with the log parser: the number in
# the pgbench log is the position in this tuple.
QUERY_SCRIPTS = (
    "usage_page",
    "usage_page_deep",
    "usage_page_model",
    "usage_totals",
    "statement_page",
    "statement_totals",
    "write_metering",
    "write_entry",
)
QUERY_WEIGHTS = (3, 2, 2, 2, 2, 1, 1, 1)

ROLLUP_DDL = """
CREATE TABLE IF NOT EXISTS usage_daily_rollup (
  org_id BIGINT NOT NULL,
  day TIMESTAMPTZ NOT NULL,
  model TEXT NOT NULL,
  user_id BIGINT,
  key_id TEXT,
  events BIGINT NOT NULL,
  prompt_tokens BIGINT NOT NULL,
  completion_tokens BIGINT NOT NULL,
  total_tokens BIGINT NOT NULL,
  cost_cents BIGINT NOT NULL,
  PRIMARY KEY (org_id, day, model, user_id, key_id)
)"""

ROLLUP_BUILD = """
INSERT INTO usage_daily_rollup (org_id, day, model, user_id, key_id, events,
                                prompt_tokens, completion_tokens, total_tokens, cost_cents)
SELECT org_id, date_trunc('day', created_at), model, user_id, key_id,
       count(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
       COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_cents), 0)
FROM metering_events
GROUP BY org_id, date_trunc('day', created_at), model, user_id, key_id"""

ROLLUP_INDEX = ("CREATE INDEX IF NOT EXISTS idx_usage_daily_rollup_org_day "
                "ON usage_daily_rollup (org_id, day)")

IMPROVED_INDEXES = {
    "idx_metering_events_org_window_cover": """
CREATE INDEX IF NOT EXISTS idx_metering_events_org_window_cover ON metering_events
  (org_id, created_at DESC, event_id DESC)
  INCLUDE (model, provider, prompt_tokens, completion_tokens, total_tokens,
           cost_cents, tenant_id, user_id, key_id, execution_id)""",
    "idx_metering_events_org_model_window": """
CREATE INDEX IF NOT EXISTS idx_metering_events_org_model_window ON metering_events
  (org_id, model, created_at DESC, event_id DESC)""",
    "idx_billing_entries_org_kind_posted": """
CREATE INDEX IF NOT EXISTS idx_billing_entries_org_kind_posted ON billing_entries
  (org_id, kind, posted_at DESC, id DESC)""",
}


def strip_sql(text):
    """The shipped ``$n`` statement text with literals inlined (pgbench side)."""
    return text


def plain_read_scripts(tier, variant):
    """The read queries of one tier/variant as plain SQL (EXPLAIN runs on these).

    The prefixes are literal here; the pgbench form below swaps them for bound
    variables because pgbench's lexer reads a colon inside a quoted literal as a
    variable reference.

    ``variant`` is ``baseline``, ``indexes`` or ``rollup``.  Only the usage
    totals script changes between variants (the rollup version); every other
    script is identical, so a difference in its numbers is a difference in the
    plan, not in the query text.
    """
    org = tier["hot_org"]
    window_from, window_to = tier["window_from"], tier["window_to"]
    month_from, month_to = tier["month_from"], tier["month_to"]

    def usage(sql, params):
        out = sql
        for index, value in enumerate(params, start=1):
            out = out.replace(f"${index}", value)
        return out

    scripts = []
    scripts.append(usage(f"""{USAGE_SELECT_COLUMNS}
{USAGE_WHERE}
ORDER BY created_at DESC, event_id DESC
LIMIT $7;""", [str(org), f"'{window_from}'", f"'{window_to}'", "''", "0", "''", "51"]))
    scripts.append(usage(f"""{USAGE_SELECT_COLUMNS}
{USAGE_WHERE}
  AND (created_at, event_id) < ($7, $8)
ORDER BY created_at DESC, event_id DESC
LIMIT $9;""", [str(org), f"'{window_from}'", f"'{window_to}'", "''", "0", "''",
               f"'{tier['cursor_ts']}'", f"'{tier['cursor_eid']}'", "51"]))
    scripts.append(usage(f"""{USAGE_SELECT_COLUMNS}
{USAGE_WHERE}
ORDER BY created_at DESC, event_id DESC
LIMIT $7;""", [str(org), f"'{window_from}'", f"'{window_to}'", f"'{tier['model']}'",
               "0", "''", "51"]))
    if variant == "rollup":
        scripts.append(f"""SELECT COALESCE(SUM(events), 0),
       COALESCE(SUM(prompt_tokens), 0),
       COALESCE(SUM(completion_tokens), 0),
       COALESCE(SUM(total_tokens), 0),
       COALESCE(SUM(cost_cents), 0)
FROM usage_daily_rollup
WHERE org_id = {org} AND day >= '{tier["rollup_from"]}' AND day <= '{tier["rollup_to"]}';""")
    else:
        scripts.append(usage(f"""SELECT count(*),
       COALESCE(SUM(prompt_tokens), 0),
       COALESCE(SUM(completion_tokens), 0),
       COALESCE(SUM(total_tokens), 0),
       COALESCE(SUM(cost_cents), 0)
{USAGE_WHERE};""", [str(org), f"'{window_from}'", f"'{window_to}'", "''", "0", "''"]))
    scripts.append(f"""SELECT b.id, b.org_id, b.kind, b.direction, b.amount_micro, b.business_id,
       b.reason, b.operator_user_id, b.request_id, b.posted_at, b.created_at,
       CASE
         WHEN b.kind = 'debit' AND b.business_id LIKE '{STATEMENT_DEBIT_PREFIX}%'
           THEN (SELECT r.occurred_at FROM usage_receipts r
                  WHERE r.execution_id = substring(b.business_id FROM length('{STATEMENT_DEBIT_PREFIX}') + 1))
       END AS occurred_at
FROM billing_entries b
WHERE b.org_id = {org} AND b.posted_at >= '{month_from}' AND b.posted_at < '{month_to}'
ORDER BY b.posted_at DESC, b.id DESC
LIMIT 51;""")
    scripts.append(f"""SELECT count(*),
       COALESCE(SUM(CASE WHEN b.direction = 'credit' AND b.kind IN ('credit', 'debit', 'adjustment')
                         THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.direction = 'debit' AND b.kind IN ('credit', 'debit', 'adjustment')
                         THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.kind = 'reservation' THEN b.amount_micro ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN b.kind = 'release' THEN b.amount_micro ELSE 0 END), 0)
FROM billing_entries b
WHERE b.org_id = {org} AND b.posted_at >= '{month_from}' AND b.posted_at < '{month_to}';
SELECT count(*), COALESCE(SUM(b.amount_micro), 0)
FROM billing_entries b
JOIN usage_receipts r
  ON b.kind = 'debit'
 AND b.business_id LIKE '{STATEMENT_DEBIT_PREFIX}%'
 AND r.execution_id = substring(b.business_id FROM length('{STATEMENT_DEBIT_PREFIX}') + 1)
WHERE b.org_id = {org} AND b.posted_at >= '{month_from}' AND b.posted_at < '{month_to}'
  AND r.occurred_at < '{month_from}';""")
    return scripts


WRITE_METERING = """INSERT INTO metering_events
  (event_id, tenant_id, model, provider, prompt_tokens, completion_tokens, total_tokens,
   cost_cents, created_at, org_id, user_id, key_id, execution_id)
VALUES ('bench_evt_' || :client_id || '_' || :tag || '_' || :n, 'org:1', 'model-1', 'mock',
        100, 50, 150, 1, now(), 1, 1, 'key-1',
        'bench_exec_' || :client_id || '_' || :tag || '_' || :n);"""

WRITE_ENTRY = """INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
   operator_user_id, request_id, posted_at, created_at)
VALUES (1, 'reservation', 89, 'credit',
        :bench_entry_prefix || :client_id || '_' || :tag || '_' || :n,
        repeat('0', 64), 'bench write', NULL, NULL, now(), now());"""


def query_scripts(tier, variant):
    """The pgbench scripts: the read queries plus two writes (mixed workload)."""
    def to_variables(script):
        # The pgbench form binds the colon-bearing prefix as a -D variable; the
        # plain (EXPLAIN) form keeps the literal.
        return (script
                .replace("'" + STATEMENT_DEBIT_PREFIX + "%'", ":receipt_debit_prefix || '%'")
                .replace("length('" + STATEMENT_DEBIT_PREFIX + "')", "length(:receipt_debit_prefix)"))

    scripts = ["\\set n :n + 1\n" + to_variables(script)
               for script in plain_read_scripts(tier, variant)]
    scripts.append("\\set n :n + 1\n" + WRITE_METERING)
    scripts.append("\\set n :n + 1\n" + WRITE_ENTRY)
    return scripts


def split_statements(script):
    return [part.strip() for part in script.split(";\n") if part.strip()]


def build_dataset(conn, spec, price_model="model-1"):
    with conn.cursor() as cur:
        cur.execute("SET synchronous_commit = off")
        cur.execute("""
            INSERT INTO users (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
            VALUES ('t11-query@t11.test', 'hash', 'active', false, 1, now(), now())
            ON CONFLICT (email) DO UPDATE SET updated_at = now()
            RETURNING id""")
        user_id = cur.fetchone()[0]
        cur.execute("""
            INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
            SELECT g, 'enterprise', 't11-query-' || g, 'active', %s, now(), now()
            FROM generate_series(1, 50) AS g ON CONFLICT (id) DO NOTHING""", (user_id,))
        cur.execute("""
            INSERT INTO price_versions (model, currency, input_price_micro_per_million,
                                        output_price_micro_per_million, effective_at, created_at)
            VALUES (%s, 'CNY', 1000000, 1000000, now() - interval '1 hour', now())
            RETURNING id""", (price_model,))
        price_version = cur.fetchone()[0]
        cur.execute("""
            INSERT INTO metering_events
              (event_id, tenant_id, model, provider, prompt_tokens, completion_tokens, total_tokens,
               cost_cents, created_at, org_id, user_id, key_id, execution_id)
            SELECT 'evt_' || g,
                   'org:' || CASE WHEN g %% 5 = 0 THEN 1 ELSE 1 + (g %% 50) END,
                   'model-' || (g %% 5), 'mock',
                   (g %% 900) + 100, (g %% 400) + 50, (g %% 1300) + 150, (g %% 100) + 1,
                   now() - ((g %% 44640) * interval '1 minute'),
                   CASE WHEN g %% 5 = 0 THEN 1 ELSE 1 + (g %% 50) END,
                   1 + (g %% 20), 'key-' || (1 + (g %% 5)), 'exec_' || g
            FROM generate_series(1, %s) AS g""", (spec["events"],))
        cur.execute("""
            INSERT INTO billing_executions
              (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
               reserved_micro, input_tokens_upper, output_tokens_upper, request_id,
               created_at, updated_at, receipt_recorded_at)
            SELECT 'bench_exec_' || g, CASE WHEN g %% 5 = 0 THEN 1 ELSE 1 + (g %% 50) END,
                   NULL, NULL, 'gw-bench', %s, 'model-' || (g %% 5), 'settled', 89, 16, 32, NULL,
                   now() - ((g %% 86400) * interval '1 minute'), now(), now()
            FROM generate_series(1, %s) AS g""", (price_version, spec["receipts"]))
        cur.execute("""
            INSERT INTO usage_receipts
              (execution_id, event_id, org_id, user_id, key_id, model, prompt_tokens, output_tokens,
               total_tokens, price_version_id, charge_micro, occurred_at, digest, status,
               dispatch_after, claim_until, attempts, last_error, created_at, settled_at)
            SELECT e.execution_id, 'recv_evt_' || e.execution_id, e.org_id, NULL, NULL, e.model,
                   16, 32, 48, %s, 89, e.created_at, repeat('a', 64), 'settled',
                   now(), NULL, 0, NULL, now(), now()
            FROM billing_executions e WHERE e.execution_id LIKE 'bench_exec_%%'""", (price_version,))
        cur.execute("""
            INSERT INTO billing_entries
              (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
               operator_user_id, request_id, posted_at, created_at)
            SELECT CASE WHEN g %% 5 = 0 THEN 1 ELSE 1 + (g %% 50) END,
                   CASE g %% 4 WHEN 0 THEN 'debit' WHEN 1 THEN 'reservation' WHEN 2 THEN 'release'
                               ELSE 'credit' END,
                   (g %% 1000) + 1,
                   CASE g %% 4 WHEN 3 THEN 'credit' ELSE 'debit' END,
                   CASE g %% 4 WHEN 0 THEN 'billing_receipt:debit:bench_exec_' || g
                               ELSE 'bench_entry:' || g END,
                   repeat('0', 64), 'bench statement', NULL, NULL,
                   now() - ((g %% 86400) * interval '1 minute'), now()
            FROM generate_series(1, %s) AS g""", (spec["entries"],))
        cur.execute("SET synchronous_commit = on")
        for table in ("metering_events", "billing_entries", "billing_executions", "usage_receipts"):
            cur.execute(f"ANALYZE {table}")
    conn.commit()


def tier_window(conn, hot_org, model="model-1"):
    with conn.cursor() as cur:
        cur.execute("SELECT max(created_at), min(created_at) FROM metering_events WHERE org_id = %s",
                    (hot_org,))
        newest, oldest = cur.fetchone()
        cur.execute("""SELECT created_at, event_id FROM metering_events
                       WHERE org_id = %s ORDER BY created_at DESC, event_id DESC
                       OFFSET 4000 LIMIT 1""", (hot_org,))
        cursor_ts, cursor_eid = cur.fetchone()
        cur.execute("""SELECT date_trunc('day', max(posted_at)) - interval '1 month',
                              date_trunc('day', max(posted_at)) + interval '1 day'
                       FROM billing_entries WHERE org_id = %s""", (hot_org,))
        month_from, month_to = cur.fetchone()
    return {
        "hot_org": hot_org, "model": model,
        "window_from": oldest.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "window_to": newest.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "cursor_ts": cursor_ts.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "cursor_eid": cursor_eid,
        "month_from": month_from.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "month_to": month_to.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "rollup_from": oldest.strftime("%Y-%m-%dT00:00:00Z"),
        "rollup_to": newest.strftime("%Y-%m-%dT00:00:00Z"),
    }


def set_schema(conn, variant):
    """Put the database in exactly one of the three schema variants."""
    with conn.cursor() as cur:
        cur.execute("DROP TABLE IF EXISTS usage_daily_rollup")
    conn.commit()
    for name, ddl in IMPROVED_INDEXES.items():
        with conn.cursor() as cur:
            if variant == "baseline":
                cur.execute(f"DROP INDEX IF EXISTS {name}")
            else:
                cur.execute(ddl)
    conn.commit()
    with conn.cursor() as cur:
        cur.execute("ANALYZE metering_events")
        cur.execute("ANALYZE billing_entries")
    conn.commit()
    built = None
    if variant == "rollup":
        started = time.perf_counter()
        with conn.cursor() as cur:
            cur.execute(ROLLUP_DDL)
            cur.execute(ROLLUP_BUILD)
            cur.execute(ROLLUP_INDEX)
            cur.execute("ANALYZE usage_daily_rollup")
        conn.commit()
        built = round(time.perf_counter() - started, 3)
    return {"variant": variant, "rollup_build_seconds": built}


def explain_plans(conn, tier, variant, out_dir, tag=None):
    plans = {}
    scripts = plain_read_scripts(tier, variant)
    for name, script in zip(QUERY_SCRIPTS, scripts):
        statements = split_statements(script)
        for index, statement in enumerate(statements):
            key = name if len(statements) == 1 else f"{name}#{index + 1}"
            with conn.cursor() as cur:
                try:
                    cur.execute("EXPLAIN (ANALYZE, BUFFERS) " + statement)
                    plans[key] = "\n".join(row[0] for row in cur.fetchall())
                except Exception as err:  # noqa: BLE001
                    conn.rollback()
                    plans[key] = f"EXPLAIN failed: {err}"
    suffix = f"-{tag}" if tag else ""
    for name, plan in plans.items():
        (out_dir / "plans" / f"{tier['name']}-{variant}{suffix}-{name}.txt").write_text(
            plan + "\n", encoding="utf-8")
    return {name: summarise_plan(plan) for name, plan in plans.items()}


def summarise_plan(plan):
    """The first access path of an EXPLAIN text plus its cost, for the tables."""
    lines = [line for line in plan.splitlines() if line.strip()]
    return {"nodes": [line.strip()[:120] for line in lines[:6]],
            "execution_ms": next((line.split("Execution Time: ")[1] for line in lines
                                  if line.startswith("Execution Time:")), None),
            "planning_ms": next((line.split("Planning Time: ")[1] for line in lines
                                 if line.startswith("Planning Time:")), None),
            "full": plan}


def rollup_consistency(conn, tier):
    out = {}
    with conn.cursor() as cur:
        cur.execute("""SELECT count(*)::bigint, COALESCE(SUM(prompt_tokens), 0)::bigint, COALESCE(SUM(total_tokens), 0)::bigint,
                              COALESCE(SUM(cost_cents), 0)::bigint
                       FROM metering_events WHERE org_id = %s""", (tier["hot_org"],))
        raw_all = cur.fetchone()
        cur.execute("""SELECT COALESCE(SUM(events), 0)::bigint, COALESCE(SUM(prompt_tokens), 0)::bigint,
                              COALESCE(SUM(total_tokens), 0)::bigint, COALESCE(SUM(cost_cents), 0)::bigint
                       FROM usage_daily_rollup WHERE org_id = %s""", (tier["hot_org"],))
        rollup_all = cur.fetchone()
        out["whole_history"] = {"raw": list(raw_all), "rollup": list(rollup_all),
                                "equal": tuple(raw_all) == tuple(rollup_all)}
        # A whole-day window on both sides: the raw side gets [day_start, day_end)
        # and the rollup side the same day range, so a difference here is a real
        # aggregation bug rather than a boundary artefact.
        cur.execute("""SELECT date_trunc('day', min(created_at)), date_trunc('day', max(created_at)) + interval '1 day'
                       FROM metering_events WHERE org_id = %s""", (tier["hot_org"],))
        whole_from, whole_to = cur.fetchone()
        cur.execute("""SELECT count(*)::bigint, COALESCE(SUM(prompt_tokens), 0)::bigint FROM metering_events
                       WHERE org_id = %s AND created_at >= %s AND created_at < %s""",
                    (tier["hot_org"], whole_from, whole_to))
        raw_window = cur.fetchone()
        cur.execute("""SELECT COALESCE(SUM(events), 0)::bigint, COALESCE(SUM(prompt_tokens), 0)::bigint
                       FROM usage_daily_rollup WHERE org_id = %s AND day >= %s AND day < %s""",
                    (tier["hot_org"], whole_from, whole_to))
        rollup_window = cur.fetchone()
        out["whole_day_window"] = {"from": whole_from.isoformat(), "to": whole_to.isoformat(),
                                   "raw": list(raw_window), "rollup": list(rollup_window),
                                   "equal": tuple(raw_window) == tuple(rollup_window)}
        cur.execute("""SELECT count(*) FROM metering_events
                       WHERE org_id = %s AND created_at >= %s AND created_at <= %s""",
                    (tier["hot_org"], tier["window_from"], tier["window_to"]))
        raw_partial = cur.fetchone()[0]
        # The API's window is an arbitrary [from, to]; a daily rollup can only
        # answer whole days, so this is the exactness the rollup would cost.
        cur.execute("""SELECT date_trunc('day', %s::timestamptz), date_trunc('day', %s::timestamptz) + interval '1 day'""",
                    (tier["window_from"], tier["window_to"]))
        partial_from, partial_to = cur.fetchone()
        cur.execute("""SELECT COALESCE(SUM(events), 0)::bigint FROM usage_daily_rollup
                       WHERE org_id = %s AND day >= %s AND day < %s""",
                    (tier["hot_org"], partial_from, partial_to))
        rollup_partial = cur.fetchone()[0]
        out["partial_day_window"] = {
            "requested_from": tier["window_from"], "requested_to": tier["window_to"],
            "raw_count": raw_partial,
            "rollup_enclosing_days": [partial_from.isoformat(), partial_to.isoformat()],
            "rollup_count_enclosing_days": rollup_partial,
            "overcount_rows": rollup_partial - raw_partial,
            "overcount_percent": round((rollup_partial - raw_partial) / raw_partial * 100.0, 2)
            if raw_partial else None,
            "exact": raw_partial == rollup_partial,
        }
    return out


def relation_sizes(conn):
    with conn.cursor() as cur:
        cur.execute("""
            SELECT relname, pg_relation_size(c.oid), pg_indexes_size(c.oid),
                   (SELECT count(*) FROM pg_stat_user_tables t WHERE t.relid = c.oid) AS stats
            FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'public' AND c.relkind = 'r'
              AND relname IN ('metering_events', 'billing_entries', 'billing_executions',
                              'usage_receipts', 'usage_daily_rollup')""")
        return {row[0]: {"table_bytes": row[1], "index_bytes": row[2]} for row in cur.fetchall()}


def index_sizes(conn):
    with conn.cursor() as cur:
        cur.execute("""SELECT indexrelname, pg_relation_size(indexrelid) FROM pg_stat_user_indexes
                       ORDER BY pg_relation_size(indexrelid) DESC""")
        return {row[0]: row[1] for row in cur.fetchall()}


def cmd_query(args):
    sqlcheck(verbose=True)
    psycopg = import_psycopg()
    output = pathlib.Path(args.output).resolve()
    (output / "plans").mkdir(parents=True, exist_ok=True)
    run_id = "".join(random.choices(string.ascii_lowercase + string.digits, k=8))
    lab = Lab(run_id, keep=args.keep, dsn=args.pg_dsn)
    report = {
        "task": "T11 B08 paged-query A/B",
        "run_id": run_id,
        "started_at": now_utc(),
        "commit": try_run(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip(),
        "host": {"platform": platform.platform(), "python": sys.version.split()[0],
                 "cpus": os.cpu_count()},
        "parameters": {
            "tiers": args.tiers,
            "round_seconds": args.query_seconds,
            "concurrency": args.concurrency,
            "variants": ["baseline", "indexes", "rollup"],
            "rotation": "baseline,indexes,rollup,rollup,indexes,baseline",
            "mix": dict(zip(QUERY_SCRIPTS, QUERY_WEIGHTS)),
            "improved_indexes": {name: " ".join(ddl.split()) for name, ddl in IMPROVED_INDEXES.items()},
            "rollup": " ".join(ROLLUP_BUILD.split())[:400],
        },
        "tiers": {},
    }
    tiers = {}
    try:
        lab.start()
        report["environment"] = {
            "pg_image": PG_IMAGE, "server_flags": PG_SERVER_FLAGS, "settings": lab.pg_settings()}
        conn = Conn(psycopg.connect(lab.dsn, autocommit=True))
        conn.execute("CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
        pgbench = Pgbench(lab, run_id)
        specs = {"100k": {"events": 100000, "entries": 100000, "receipts": 40000},
                 "1m": {"events": 1000000, "entries": 600000, "receipts": 200000}}
        for tier_name in args.tiers:
            spec = specs[tier_name]
            log(f"=== tier {tier_name}: fixture {spec}")
            set_schema(conn.conn, "baseline")
            with conn.conn.cursor() as cur:
                cur.execute("TRUNCATE metering_events, billing_entries, billing_executions, usage_receipts")
            conn.conn.commit()
            started = time.perf_counter()
            build_dataset(conn.conn, spec)
            build_seconds = round(time.perf_counter() - started, 2)
            tier = tier_window(conn.conn, hot_org=1)
            tier["name"] = tier_name
            tier["fixture"] = spec
            tier["build_seconds"] = build_seconds
            sizes = relation_sizes(conn.conn)
            tier["relation_sizes"] = sizes
            log(f"tier {tier_name} built in {build_seconds}s: {sizes}")
            rounds = []
            rotation = ["baseline", "indexes", "rollup", "rollup", "indexes", "baseline"]
            script_cache = {}
            for round_index, variant in enumerate(rotation, start=1):
                schema = set_schema(conn.conn, variant)
                if variant not in script_cache:
                    script_cache[variant] = [
                        (pgbench.copy_script(f"{tier_name}-{variant}-{name}", script), weight)
                        for name, script, weight in zip(QUERY_SCRIPTS, query_scripts(tier, variant),
                                                        QUERY_WEIGHTS)]
                before = pgss_snapshot(conn.conn)
                sampler = PgSampler(lab.dsn, lab.container, interval=0.2)
                sampler.start()
                run = pgbench.run(script_cache[variant], args.concurrency, args.query_seconds,
                                  defs={"n": 0, "tag": round_index})
                run["pg"] = sampler.stop()
                run["pg_stat_statements_delta"] = pgss_delta(before, pgss_snapshot(conn.conn))
                per_query = {}
                for number, values in run["per_script_latency_ms"].items():
                    name = QUERY_SCRIPTS[int(number)]
                    per_query[name] = {
                        "transactions": run["per_script_count"].get(number),
                        "latency_ms": values,
                        "throughput_per_second": round(
                            (run["per_script_count"].get(number) or 0) / run["seconds"], 2),
                    }
                plans = explain_plans(conn.conn, tier, variant, output, tag=f"r{round_index}")
                round_result = {"round": round_index, "variant": variant, "schema": schema,
                                "tps": run["tps"], "per_query": per_query,
                                "pg": run["pg"], "plans": plans,
                                "pg_stat_statements_delta": run["pg_stat_statements_delta"]}
                rounds.append(round_result)
                log(f"{tier_name} round{round_index} {variant}: tps={run['tps']:.0f} " + " ".join(
                    f"{name}={per_query[name]['latency_ms'].get('p95')}ms" for name in QUERY_SCRIPTS
                    if name in per_query))
            tier["rounds"] = rounds
            per_variant = {}
            for variant in ("baseline", "indexes", "rollup"):
                picked = [item for item in rounds if item["variant"] == variant]
                merged = {}
                for name in QUERY_SCRIPTS:
                    values = [item["per_query"][name]["latency_ms"] for item in picked
                              if name in item["per_query"]]
                    if not values:
                        continue
                    merged[name] = {
                        "p50_ms": round(statistics.median([v.get("p50", 0) for v in values]), 3),
                        "p95_ms": round(statistics.median([v.get("p95", 0) for v in values]), 3),
                        "p99_ms": round(statistics.median([v.get("p99", 0) for v in values]), 3),
                        "rounds": len(values),
                    }
                per_variant[variant] = merged
            tier["per_variant"] = per_variant
            improvement = {}
            for name in QUERY_SCRIPTS:
                base = per_variant.get("baseline", {}).get(name)
                for variant in ("indexes", "rollup"):
                    other = per_variant.get(variant, {}).get(name)
                    if not base or not other:
                        continue
                    improvement.setdefault(name, {})[f"{variant}_vs_baseline_percent"] = {
                        "p50": round((other["p50_ms"] - base["p50_ms"]) / base["p50_ms"] * 100.0, 2)
                        if base["p50_ms"] else None,
                        "p95": round((other["p95_ms"] - base["p95_ms"]) / base["p95_ms"] * 100.0, 2)
                        if base["p95_ms"] else None,
                    }
            tier["improvement"] = improvement
            tier["plans"] = {variant: explain_plans(conn.conn, tier, variant, output)
                             for variant in ("baseline", "indexes", "rollup")}
            set_schema(conn.conn, "rollup")
            tier["rollup_consistency"] = rollup_consistency(conn.conn, tier)
            tier["index_sizes"] = index_sizes(conn.conn)
            report["tiers"][tier_name] = tier
            write_json(output / "raw" / f"query-{tier_name}.json", tier)
            log(f"tier {tier_name} done: {json.dumps(tier['improvement'])}")
    finally:
        report["finished_at"] = now_utc()
        report["teardown"] = lab.teardown()
        write_json(output / "raw" / "query-summary.json", report)
    print(json.dumps({"run_id": run_id, "tiers": list(report["tiers"]), "output": str(output)}, indent=2))
    return 0


RECEIPT_STREAM = "usage:events:receipts"
SETTLEMENT_GROUP = "settlement-group"


def load_failure_matrix():
    """Import the T10 runner for its isolated stack (no process is started here)."""
    import importlib.util
    spec = importlib.util.spec_from_file_location("t11_failure_matrix",
                                                  ROOT / "scripts" / "failure-matrix.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class StreamLab:
    """The T10 isolated stack plus cmd/loadtest, used by settle and recovery."""

    def __init__(self, output, keep=False, instance="T11LI"):
        self.fm = load_failure_matrix()
        self.output = pathlib.Path(output).resolve()
        self.keep = keep
        self.instance = instance
        self.args = argparse.Namespace(keep=keep)
        self.stack = self.fm.Stack(self.args, self.output)
        self.org = None
        self.psycopg = import_psycopg()
        self.conn = None
        self.baseline = (0, 0)
        self.redis = None
        self.loadtest = None

    def start(self):
        log(f"starting the T10 isolated stack (project {self.stack.project})")
        self.stack.build()
        self.stack.start_dependencies()
        self.stack.apply_migrations()
        self.org = self.stack.seed_org(self.instance, balance_micro=5_000_000_000)
        self.stack.seed_prices()
        # cmd/loadtest sends the literal model "default"; the T10 routes do not
        # carry it, so this lab publishes the same providers under that name too.
        self.stack.psql("""
            INSERT INTO price_versions (model, currency, input_price_micro_per_million,
                                        output_price_micro_per_million, effective_at, created_at)
            VALUES ('default', 'CNY', 1000000, 1000000, now() - interval '1 hour', now())""")
        self.stack.start_control()
        self.stack.start_mocks()
        routes = {
            self.fm.MODEL_OK: ["p_ok"],
            self.fm.MODEL_FAIL503: ["p_503", "p_ok"],
            self.fm.MODEL_FAIL429: ["p_429", "p_ok"],
            self.fm.MODEL_HANG: ["p_hang", "p_ok"],
            self.fm.MODEL_CUT: ["p_cut"],
            self.fm.MODEL_COUNT: ["p_count"],
            "default": ["p_ok"],
        }
        providers = {
            "p_ok": {"id": "p_ok", "protocol": "openai-compatible",
                     "url": f"http://127.0.0.1:{self.stack.ports['mock_ok']}",
                     "model": self.fm.MODEL_OK, "timeouts": {"attempt_timeout_ms": 30000}},
            "p_503": {"id": "p_503", "protocol": "openai-compatible",
                      "url": f"http://127.0.0.1:{self.stack.ports['mock_503']}",
                      "model": self.fm.MODEL_FAIL503, "timeouts": {"attempt_timeout_ms": 5000}},
            "p_429": {"id": "p_429", "protocol": "openai-compatible",
                      "url": f"http://127.0.0.1:{self.stack.ports['mock_429']}",
                      "model": self.fm.MODEL_FAIL429, "timeouts": {"attempt_timeout_ms": 5000}},
            "p_hang": {"id": "p_hang", "protocol": "openai-compatible",
                       "url": f"http://127.0.0.1:{self.stack.ports['mock_hang']}",
                       "model": self.fm.MODEL_HANG, "timeouts": {"attempt_timeout_ms": 1500}},
            "p_cut": {"id": "p_cut", "protocol": "openai-compatible",
                      "url": f"http://127.0.0.1:{self.stack.ports['mock_cut']}",
                      "model": self.fm.MODEL_CUT, "timeouts": {"attempt_timeout_ms": 30000}},
            "p_count": {"id": "p_count", "protocol": "openai-compatible",
                        "url": f"http://127.0.0.1:{self.stack.counting.port}",
                        "model": self.fm.MODEL_COUNT, "timeouts": {"attempt_timeout_ms": 30000}},
        }
        body = self.stack.post_config(self.stack.key_records(), routes, providers)
        self.stack.desired_version = body.get("desired_version")
        self.stack.start_metering()
        self.stack.start_gateway("a")
        self.loadtest = self.stack.build_dir / "loadtest"
        must_run(["go", "build", "-o", str(self.loadtest), "./cmd/loadtest"], "build loadtest",
                 cwd=ROOT, timeout=600)
        self.conn = self.psycopg.connect(self.stack.dsn, autocommit=True)
        with self.conn.cursor() as cur:
            cur.execute("SET application_name = 't11-lab'")
        try:
            import redis as redis_module
            self.redis = redis_module.Redis(host="127.0.0.1", port=self.stack.ports["redis"],
                                            socket_timeout=2)
        except Exception:  # noqa: BLE001
            self.redis = None
        self.stack.build_dir  # keep
        return self

    # -- load ---------------------------------------------------------------

    def run_load(self, rate, seconds, tag, shape="s", concurrency=64):
        total = max(1, int(rate * seconds))
        json_path = self.output / "raw" / f"load-{tag}.json"
        json_path.parent.mkdir(parents=True, exist_ok=True)
        env = self.stack.base_env()
        env["GATEWAY_API_KEY"] = self.org.raw_key
        cmd = [str(self.loadtest),
               "-target", self.stack.gateway("a") + "/v1/chat/completions",
               "-mode", "open", "-c", str(concurrency), "-n", str(total),
               "-rate", str(rate), "-shape", shape, "-admission-queue", str(concurrency * 4),
               "-json", str(json_path), "-tolerate-drops", "-timeout", "60s"]
        started = time.time()
        proc = must_run(cmd, f"loadtest {tag}", cwd=ROOT, env=env, timeout=int(seconds) + 300)
        elapsed = time.time() - started
        report = json.loads(json_path.read_text(encoding="utf-8")) if json_path.exists() else {}
        inner = report.get("report", {})
        return {
            "tag": tag, "planned_rate": rate, "planned_arrivals": total, "seconds": round(elapsed, 2),
            "exit_code": proc.returncode,
            "tps": inner.get("SuccessPerSecond"),
            "scheduled": inner.get("Scheduled"), "started": inner.get("Started"),
            "completed": inner.get("Completed"), "rejected": inner.get("Rejected"),
            "transport_error": inner.get("TransportError"), "incomplete": inner.get("Incomplete"),
            "cancelled": inner.get("Cancelled"), "parse_error": inner.get("ParseError"),
            "loadgen_dropped": inner.get("LoadgenDropped"),
            "loadgen_dropped_percent": report.get("loadgen_dropped_percent"),
            "capacity_round_valid": report.get("capacity_round_valid"),
            "capacity_invalid_reasons": inner.get("CapacityInvalidReasons"),
            "status_counts": inner.get("Status"),
            "arrival_rate": inner.get("ArrivalRatePerSecond"),
            "achieved_start_rate": inner.get("AchievedStartRatePerSecond"),
            "first_content": inner.get("FirstContent"),
            "total_dur": inner.get("TotalDur"),
            "dispatch_lag": inner.get("DispatchLag"),
            "scheduled_to_terminal": inner.get("ScheduledToTerminal"),
            "peak_active": inner.get("PeakActive"),
            "raw": str(json_path.relative_to(self.output)),
        }

    def evidence_rate(self, rate, seconds=8.0, shape="s", concurrency=64, cap=40.0):
        """The in-harness capacity smoke: the highest valid rate, then 60% of it.

        The official ladder belongs to the parallel capacity task; this harness
        only needs a defensible constant, so it walks its own short ladder and
        then caps the load rate, reporting both numbers.
        """
        ladder = [rate]
        for step in (2, 4, 8, 16, 32):
            ladder.append(rate * step)
        best = None
        rounds = []
        for candidate in ladder:
            result = self.run_load(candidate, seconds, f"smoke-{candidate:g}", shape, concurrency)
            rounds.append(result)
            valid = bool(result["capacity_round_valid"]) and (result["loadgen_dropped"] or 0) == 0
            completed = result["completed"] or 0
            started = result["started"] or 0
            success = (completed / started) if started else 0.0
            log(f"smoke rate={candidate:g}: valid={valid} started={started} completed={completed} "
                f"rejected={result['rejected']} success={success:.3f}")
            if valid and success >= 0.995:
                best = candidate
            else:
                break
        chosen = (best or rate)
        return {"rounds": rounds, "highest_valid_rate": best,
                "capacity_60_percent": round(chosen * 0.6, 2),
                "load_rate": round(min(chosen * 0.6, cap), 2),
                "cap": cap,
                "rule": "min(60% of the highest smoke rate that stayed generator-valid with "
                        ">=99.5% success, cap): the official ladder is owned by the parallel "
                        "capacity task, so the recovery matrix uses this conservative constant"}

    # -- observations -------------------------------------------------------

    def ensure_conn(self):
        """The lab's own connection: reconnect after a PostgreSQL outage."""
        try:
            if self.conn is None or self.conn.closed:
                raise RuntimeError("closed")
            with self.conn.cursor() as cur:
                cur.execute("SELECT 1")
            return self.conn
        except Exception:  # noqa: BLE001
            try:
                if self.conn is not None:
                    self.conn.close()
            except Exception:  # noqa: BLE001
                pass
            self.conn = self.psycopg.connect(self.stack.dsn, autocommit=True)
            with self.conn.cursor() as cur:
                cur.execute("SET application_name = 't11-lab'")
            return self.conn

    def money_baseline(self):
        conn = self.ensure_conn()
        with conn.cursor() as cur:
            cur.execute("SELECT posted_balance_micro, reserved_micro FROM billing_accounts WHERE org_id = %s",
                        (self.org.org_id,))
            row = cur.fetchone()
        self.baseline = (row[0], row[1]) if row else (0, 0)
        return self.baseline

    def probe_success(self, timeout=60.0, interval=0.2, shape="s"):
        """One request per interval until a complete 200/[DONE]; returns seconds.

        This is the RTO observation: "the system serves a normal request again",
        not "the load generator finished its round".
        """
        started = time.time()
        body = json.dumps({
            "model": "default", "messages": [{"role": "user", "content": "hi"}], "stream": True,
            "x_mock_ttft_ms": 20, "x_mock_token_interval_ms": 15, "x_mock_tokens": 8,
            "x_mock_event_line_bytes": 256,
        }).encode()
        request = urllib.request.Request(
            self.stack.gateway("a") + "/v1/chat/completions", data=body,
            headers={"Authorization": "Bearer " + self.org.raw_key,
                     "Content-Type": "application/json"})
        attempts = 0
        last = None
        while time.time() - started < timeout:
            attempts += 1
            try:
                with urllib.request.urlopen(request, timeout=10) as response:
                    payload = response.read(1 << 20).decode("utf-8", "replace")
                    if response.status == 200 and "[DONE]" in payload:
                        return {"seconds": round(time.time() - started, 3), "attempts": attempts,
                                "status": 200}
                    last = f"status={response.status} no DONE"
            except Exception as err:  # noqa: BLE001
                last = f"{type(err).__name__}: {str(err)[:80]}"
            time.sleep(interval)
        return {"seconds": None, "attempts": attempts, "status": None, "last_error": last}

    def db_now(self):
        conn = self.ensure_conn()
        with conn.cursor() as cur:
            cur.execute("SELECT now()")
            return cur.fetchone()[0]

    def pg_state(self):
        out = {}
        conn = self.ensure_conn()
        with conn.cursor() as cur:
            cur.execute("""SELECT status, count(*) FROM usage_receipts GROUP BY status""")
            out["receipts"] = {row[0]: row[1] for row in cur.fetchall()}
            cur.execute("""SELECT count(*), COALESCE(max(EXTRACT(EPOCH FROM (now() - created_at))), 0)
                           FROM usage_receipts WHERE status IN ('pending_dispatch', 'dispatched')""")
            unsettled, oldest = cur.fetchone()
            out["unsettled_receipts"] = unsettled
            out["oldest_unsettled_age_s"] = round(float(oldest), 3)
            cur.execute("""SELECT count(*), COALESCE(max(EXTRACT(EPOCH FROM (now() - created_at))), 0)
                           FROM usage_receipts
                           WHERE status IN ('pending_dispatch', 'dispatched')
                             AND (dispatch_after IS NULL OR dispatch_after <= now())
                             AND (claim_until IS NULL OR claim_until <= now())""")
            due, due_oldest = cur.fetchone()
            out["due_receipts"] = due
            out["oldest_due_age_s"] = round(float(due_oldest), 3)
            cur.execute("SELECT status, count(*) FROM billing_executions GROUP BY status")
            out["executions"] = {row[0]: row[1] for row in cur.fetchall()}
            cur.execute("""SELECT count(*) FROM billing_executions
                           WHERE status IN ('reserved', 'executing') AND reserved_micro > 0""")
            out["held_reservations"] = cur.fetchone()[0]
            cur.execute("""SELECT COALESCE(SUM(reserved_micro), 0) FROM billing_accounts
                           WHERE org_id = %s""", (self.org.org_id,))
            out["reserved_micro"] = cur.fetchone()[0]
            cur.execute("""SELECT count(*) FILTER (WHERE wait_event_type = 'Lock'),
                                  count(*) FILTER (WHERE state = 'active')
                           FROM pg_stat_activity WHERE datname = current_database()
                             AND pid <> pg_backend_pid()""")
            out["lock_waiters"], out["active_backends"] = cur.fetchone()
        return out

    def redis_state(self):
        out = {}
        if self.redis is None:
            return {"error": "redis client unavailable"}
        try:
            started = time.perf_counter()
            self.redis.ping()
            out["ping_ms"] = round((time.perf_counter() - started) * 1000.0, 3)
        except Exception as err:  # noqa: BLE001
            out["ping_ms"] = None
            out["ping_error"] = str(err)[:120]
        try:
            out["stream_len"] = self.redis.xlen(RECEIPT_STREAM)
        except Exception:  # noqa: BLE001
            out["stream_len"] = None
        try:
            pending = self.redis.xpending(RECEIPT_STREAM, SETTLEMENT_GROUP)
            out["pending"] = pending.get("pending") if isinstance(pending, dict) else pending
        except Exception:  # noqa: BLE001
            out["pending"] = None
        return out

    def metering_metrics(self):
        text = None
        try:
            text = urllib.request.urlopen(
                f"http://127.0.0.1:{self.stack.ports['metering_diag']}/metrics", timeout=5).read().decode()
        except Exception as err:  # noqa: BLE001
            return {"error": str(err)[:120]}
        keep = ("metering_receipt_dispatch_total", "metering_receipt_settled_total",
                "metering_receipt_exception_total", "metering_receipt_oldest_due_age_seconds",
                "metering_receipt_backlog", "metering_receipt_dlq_total",
                "metering_billing_recovery_candidates_total")
        out = {}
        for line in text.splitlines():
            if line.startswith("#") or not line.strip():
                continue
            name = line.split("{")[0].split(" ")[0]
            if name in keep:
                out.setdefault(name, []).append(line.strip())
        return out

    def sample(self):
        return {"at": now_utc(), "pg": self.pg_state(), "redis": self.redis_state(),
                "metrics": self.metering_metrics()}


class Timeline(threading.Thread):
    """Samples the stack until stopped; ``phase`` labels the samples."""

    def __init__(self, lab, interval=0.5):
        super().__init__(daemon=True)
        self.lab = lab
        self.interval = interval
        self.stop_event = threading.Event()
        self.phase = "init"
        self.samples = []

    def run(self):
        while not self.stop_event.is_set():
            try:
                sample = self.lab.sample()
                sample["phase"] = self.phase
                self.samples.append(sample)
            except Exception as err:  # noqa: BLE001
                self.samples.append({"at": now_utc(), "phase": self.phase,
                                     "error": str(err)[:200]})
            self.stop_event.wait(self.interval)

    def stop(self):
        self.stop_event.set()
        self.join(timeout=10)
        return self.samples


def receipt_lag_rows(conn, org_id, since=None):
    with conn.cursor() as cur:
        if since is None:
            cur.execute("""
                SELECT EXTRACT(EPOCH FROM (settled_at - occurred_at)) * 1000.0,
                       EXTRACT(EPOCH FROM (settled_at - created_at)) * 1000.0,
                       status, attempts
                FROM usage_receipts WHERE org_id = %s""", (org_id,))
        else:
            cur.execute("""
                SELECT EXTRACT(EPOCH FROM (settled_at - occurred_at)) * 1000.0,
                       EXTRACT(EPOCH FROM (settled_at - created_at)) * 1000.0,
                       status, attempts
                FROM usage_receipts WHERE org_id = %s AND created_at >= %s""", (org_id, since))
        return cur.fetchall()


def settlement_rate(samples, phase):
    """Receipts settled per second inside one phase of the timeline."""
    picked = [s for s in samples if s.get("phase") == phase and isinstance(s.get("pg"), dict)]
    if len(picked) < 2:
        return None
    first, last = picked[0], picked[-1]
    settled = (last["pg"]["receipts"].get("settled", 0) - first["pg"]["receipts"].get("settled", 0))
    seconds = (datetime.fromisoformat(last["at"].replace("Z", "+00:00"))
               - datetime.fromisoformat(first["at"].replace("Z", "+00:00"))).total_seconds()
    if seconds <= 0:
        return None
    return {"settled": settled, "seconds": round(seconds, 2),
            "receipts_per_second": round(settled / seconds, 2)}


def backlog_peak(samples, phase):
    picked = [s for s in samples if s.get("phase") == phase and isinstance(s.get("pg"), dict)]
    if not picked:
        return None
    return {"max_unsettled": max(s["pg"]["unsettled_receipts"] for s in picked),
            "max_oldest_age_s": max(s["pg"]["oldest_unsettled_age_s"] for s in picked),
            "max_lock_waiters": max(s["pg"]["lock_waiters"] for s in picked),
            "max_ping_ms": max((s["redis"].get("ping_ms") or 0) for s in picked
                               if isinstance(s.get("redis"), dict))}


def money_report(conn, org_id, baseline=None):
    """One consistent snapshot of the money invariants.

    ``baseline`` is the account projection right after seeding: the T10 lab
    creates the wallet without a matching ledger credit, so the ledger *sum* is a
    movement, not a balance, and the comparison needs that starting point.
    """
    out = {}
    with conn.transaction(), conn.cursor() as cur:
        cur.execute("""
            SELECT a.posted_balance_micro, a.reserved_micro,
                   COALESCE(SUM(CASE WHEN e.kind IN ('credit','debit','adjustment')
                                     THEN CASE WHEN e.direction = 'credit' THEN e.amount_micro
                                               ELSE -e.amount_micro END ELSE 0 END), 0),
                   COALESCE(SUM(CASE WHEN e.kind IN ('reservation','release')
                                     THEN CASE WHEN e.direction = 'credit' THEN e.amount_micro
                                               ELSE -e.amount_micro END ELSE 0 END), 0)
            FROM billing_accounts a LEFT JOIN billing_entries e ON e.org_id = a.org_id
            WHERE a.org_id = %s GROUP BY a.posted_balance_micro, a.reserved_micro""", (org_id,))
        row = cur.fetchone()
        base_posted, base_reserved = (baseline or (0, 0))
        out["account"] = {"posted": row[0], "reserved": row[1],
                          "ledger_posted": row[2], "ledger_reserved": row[3],
                          "baseline_posted": base_posted, "baseline_reserved": base_reserved,
                          "posted_movement": row[0] - base_posted,
                          "projection_matches_ledger": (row[0] - base_posted) == row[2]
                                                       and (row[1] - base_reserved) == row[3]}
        cur.execute("SELECT status, count(*) FROM usage_receipts WHERE org_id = %s GROUP BY status",
                    (org_id,))
        out["receipts"] = {r[0]: r[1] for r in cur.fetchall()}
        cur.execute("SELECT status, count(*) FROM billing_executions WHERE org_id = %s GROUP BY status",
                    (org_id,))
        out["executions"] = {r[0]: r[1] for r in cur.fetchall()}
        # UNIQUE (business_id, kind) is the ledger's own guard; these two count
        # any row that slipped past it, by business key rather than by a column
        # billing_entries does not have.
        cur.execute("""SELECT count(*) FROM (
                         SELECT business_id FROM billing_entries
                         WHERE org_id = %s AND kind = 'debit'
                           AND business_id LIKE 'billing_receipt:debit:%%'
                         GROUP BY business_id HAVING count(*) > 1) AS duplicates""", (org_id,))
        out["duplicate_debits"] = cur.fetchone()[0]
        cur.execute("""SELECT count(*) FROM (
                         SELECT business_id FROM billing_entries
                         WHERE org_id = %s AND kind = 'reservation'
                         GROUP BY business_id HAVING count(*) > 1) AS duplicates""", (org_id,))
        out["duplicate_reservations"] = cur.fetchone()[0]
        cur.execute("""SELECT count(*) FROM usage_receipts r
                       WHERE r.org_id = %s AND r.status = 'settled'
                         AND NOT EXISTS (SELECT 1 FROM billing_entries e
                                         WHERE e.business_id = 'billing_receipt:debit:' || r.execution_id
                                           AND e.kind = 'debit')""", (org_id,))
        out["settled_receipts_without_debit"] = cur.fetchone()[0]
        cur.execute("""SELECT count(*) FROM billing_executions e
                       WHERE e.org_id = %s AND e.status = 'settled'
                         AND NOT EXISTS (SELECT 1 FROM usage_receipts r
                                         WHERE r.execution_id = e.execution_id
                                           AND r.status = 'settled')""", (org_id,))
        out["settled_executions_without_receipt"] = cur.fetchone()[0]
        cur.execute("""SELECT count(*) FROM billing_executions
                       WHERE org_id = %s AND status IN ('reserved', 'executing') AND reserved_micro > 0""",
                    (org_id,))
        out["held_reservations_at_end"] = cur.fetchone()[0]
        cur.execute("""SELECT COALESCE(SUM(amount_micro), 0)::bigint FROM billing_entries
                       WHERE org_id = %s AND kind = 'debit'
                         AND business_id LIKE 'billing_receipt:debit:%%'""", (org_id,))
        debits = cur.fetchone()[0]
        cur.execute("""SELECT COALESCE(SUM(charge_micro), 0)::bigint FROM usage_receipts
                       WHERE org_id = %s AND status = 'settled'""", (org_id,))
        charges = cur.fetchone()[0]
        out["debit_micro"] = debits
        out["settled_charge_micro"] = charges
        out["debit_equals_charge"] = debits == charges
    return out


def cmd_settle(args):
    sqlcheck(verbose=False)
    output = pathlib.Path(args.output).resolve()
    (output / "raw").mkdir(parents=True, exist_ok=True)
    report = {"task": "T11 settlement and observability under load", "started_at": now_utc(),
              "commit": try_run(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip(),
              "host": {"platform": platform.platform(), "cpus": os.cpu_count()},
              "parameters": {"seconds": args.seconds, "shape": args.shape,
                             "concurrency": args.concurrency, "rate": args.rate}}
    lab = StreamLab(output, keep=args.keep)
    timeline = None
    try:
        lab.start()
        report["environment"] = {
            "project": lab.stack.project, "ports": lab.stack.ports,
            "gateway": {key: value for key, value in lab.stack.gateway_env("a").items()
                        if key.startswith("GATEWAY_") or key in ("LIMITER_ON_REDIS_ERROR",)},
            "org_id": lab.org.org_id, "schema_version": lab.stack.evidence.get("schema_version"),
        }
        started_run = time.time()
        timeline = Timeline(lab, interval=args.interval)
        timeline.phase = "smoke"
        timeline.start()
        lab.money_baseline()
        capacity = lab.evidence_rate(args.rate or 10.0, shape=args.shape, cap=args.rate_cap)
        report["capacity_smoke"] = capacity
        rate = capacity["load_rate"]
        # The smoke leaves its own backlog behind; drain it before the steady
        # window so the lag distribution below belongs to the measured load.
        timeline.phase = "drain-smoke"
        smoke_drain, smoke_state = drain_until_idle(lab, args.drain_timeout)
        report["smoke_drain_seconds"] = smoke_drain
        report["smoke_state_after_drain"] = smoke_state
        report["smoke_settlement_rate"] = settlement_rate(timeline.samples, "smoke")
        report["smoke_backlog_peak"] = backlog_peak(timeline.samples, "smoke")
        steady_started = lab.db_now()
        report["steady_started_at"] = steady_started.isoformat()
        before = money_report(lab.ensure_conn(), lab.org.org_id, lab.baseline)
        timeline.phase = "steady"
        load = lab.run_load(rate, args.seconds, "steady", shape=args.shape,
                            concurrency=args.concurrency)
        timeline.phase = "drain"
        drain_seconds, state = drain_until_idle(lab, args.drain_timeout)
        drained_at = time.time() if drain_seconds is not None else None
        report["steady_drain_seconds"] = drain_seconds
        report["steady_state_after_drain"] = state
        report["steady_settlement_rate"] = settlement_rate(timeline.samples, "steady")
        report["steady_backlog_peak"] = backlog_peak(timeline.samples, "steady")
        timeline.phase = "done"
        samples = timeline.stop()
        timeline = None
        report["load"] = load
        report["rto"] = {
            "steady_started_at": datetime.fromtimestamp(started_run, timezone.utc).isoformat(),
            "load_seconds": load["seconds"],
            "drain_seconds": round((drained_at - (started_run + load["seconds"])), 3) if drained_at else None,
            "drained_within_timeout": drained_at is not None,
        }
        after = money_report(lab.ensure_conn(), lab.org.org_id, lab.baseline)
        report["money_before"] = before
        report["money_after"] = after
        report["money_delta"] = {
            key: (after[key] - before[key]) for key in ("duplicate_debits", "duplicate_reservations",
                                                        "settled_receipts_without_debit",
                                                        "settled_executions_without_receipt")
            if isinstance(before.get(key), int) and isinstance(after.get(key), int)}
        rows = receipt_lag_rows(lab.ensure_conn(), lab.org.org_id, since=steady_started)
        settled = [row for row in rows if row[2] == "settled"]
        report["steady_receipts"] = len(rows)
        report["settlement_lag_ms"] = latency_block([float(row[0]) for row in settled])
        report["outbox_dwell_ms"] = latency_block([float(row[1]) for row in settled])
        report["receipt_statuses"] = after["receipts"]
        report["attempts_histogram"] = {}
        for row in rows:
            key = str(row[3])
            report["attempts_histogram"][key] = report["attempts_histogram"].get(key, 0) + 1
        report["timeline"] = samples
        report["timeline_summary"] = timeline_summary(samples)
        write_json(output / "raw" / "settle-summary.json", report)
        write_json(output / "raw" / "settle-timeline.json", samples)
        print(json.dumps({"run_id": lab.stack.run_id, "rate": rate,
                          "settlement_lag_ms": report["settlement_lag_ms"],
                          "outbox_dwell_ms": report["outbox_dwell_ms"],
                          "load": {k: load[k] for k in ("scheduled", "started", "completed",
                                                        "rejected", "transport_error", "incomplete",
                                                        "loadgen_dropped", "capacity_round_valid")},
                          "money_delta": report["money_delta"]}, indent=2, default=str))
    finally:
        if timeline is not None:
            timeline.stop()
        report["finished_at"] = now_utc()
        report["teardown"] = lab.stack.teardown()
        write_json(output / "raw" / "settle-summary.json", report)
    return 0


def timeline_summary(samples):
    usable = [s for s in samples if "pg" in s]
    if not usable:
        return {}
    out = {"samples": len(usable), "phases": sorted({s.get("phase") for s in usable})}
    for field, path in (("oldest_unsettled_age_s", ("pg", "oldest_unsettled_age_s")),
                        ("unsettled_receipts", ("pg", "unsettled_receipts")),
                        ("lock_waiters", ("pg", "lock_waiters")),
                        ("ping_ms", ("redis", "ping_ms"))):
        values = [s[path[0]][path[1]] for s in usable
                  if isinstance(s.get(path[0]), dict) and isinstance(s[path[0]].get(path[1]), (int, float))]
        if values:
            out[field] = {"max": max(values), "mean": round(statistics.fmean(values), 3)}
    return out


def wait_for_http(url, timeout, what, expect=200):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            last = urllib.request.urlopen(url, timeout=3).status
            if last == expect:
                return True
        except Exception as err:  # noqa: BLE001
            last = str(err)[:80]
        time.sleep(0.2)
    raise RuntimeError(f"timeout waiting for {what} (last={last})")


def wait_held_reservations(lab, timeout=120.0, interval=1.0):
    """Waits for T07B to release the reservations a failed finalize left held."""
    started = time.time()
    first = None
    peak = 0
    while time.time() - started < timeout:
        state = lab.pg_state()
        held = state["held_reservations"]
        peak = max(peak, held)
        if first is None:
            first = held
        if held == 0:
            return {"seconds": round(time.time() - started, 3), "first_held": first,
                    "peak_held": peak, "final_held": 0, "converged": True}
        time.sleep(interval)
    return {"seconds": None, "first_held": first, "peak_held": peak,
            "final_held": lab.pg_state()["held_reservations"], "converged": False}


def drain_until_idle(lab, timeout, phase="drain"):
    """Waits until no receipt is unsettled; returns seconds and the last state."""
    started = time.time()
    state = lab.pg_state()
    while time.time() - started < timeout:
        state = lab.pg_state()
        if state["unsettled_receipts"] == 0:
            return round(time.time() - started, 3), state
        time.sleep(0.5)
    return None, state


def cmd_recovery(args):
    sqlcheck(verbose=False)
    output = pathlib.Path(args.output).resolve()
    (output / "raw").mkdir(parents=True, exist_ok=True)
    report = {"task": "T11 failure recovery (B07)", "started_at": now_utc(),
              "commit": try_run(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip(),
              "host": {"platform": platform.platform(), "cpus": os.cpu_count()},
              "parameters": {"shape": args.shape, "concurrency": args.concurrency,
                             "redis_outages_s": args.outages, "rate": args.rate},
              "scenarios": {}}
    lab = StreamLab(output, keep=args.keep)
    timeline = None
    try:
        lab.start()
        report["environment"] = {
            "project": lab.stack.project, "ports": lab.stack.ports, "org_id": lab.org.org_id,
            "schema_version": lab.stack.evidence.get("schema_version"),
            "gateway_env": {key: value for key, value in lab.stack.gateway_env("a").items()
                            if key.startswith("GATEWAY_") or key == "LIMITER_ON_REDIS_ERROR"},
        }
        lab.money_baseline()
        smoke_timeline = Timeline(lab, interval=0.5)
        smoke_timeline.phase = "smoke"
        smoke_timeline.start()
        capacity = lab.evidence_rate(args.rate or 10.0, shape=args.shape, cap=args.rate_cap)
        smoke_drain, smoke_state = drain_until_idle(lab, args.drain_timeout)
        smoke_samples = smoke_timeline.stop()
        report["capacity_smoke"] = capacity
        report["smoke_drain_seconds"] = smoke_drain
        report["smoke_state_after_drain"] = smoke_state
        report["smoke_settlement_rate"] = settlement_rate(smoke_samples, "smoke")
        report["rate"] = capacity["load_rate"]
        run_redis = args.redis_outage or not (args.pg_outage or args.kill)
        run_pg = args.pg_outage or not (args.redis_outage or args.kill)
        run_kill = args.kill or not (args.redis_outage or args.pg_outage)
        if run_redis:
            for seconds in args.outages:
                scenario_redis_outage(lab, report, seconds)
        if run_pg:
            scenario_pg_outage(lab, report, "admission")
            scenario_pg_outage(lab, report, "settle")
            scenario_pg_outage(lab, report, "finalize")
        if run_kill:
            scenario_kill(lab, report, "gateway")
            scenario_kill(lab, report, "metering")
        report["timeline_summary"] = {}
        write_json(output / "raw" / "recovery-summary.json", report)
        print(json.dumps({name: {"status": value.get("status"),
                                 "rto_seconds": value.get("rto_seconds"),
                                 "drain_seconds": value.get("drain_seconds"),
                                 "refusals": value.get("refusal_classes")}
                          for name, value in report["scenarios"].items()}, indent=2, default=str))
    finally:
        if timeline is not None:
            timeline.stop()
        report["finished_at"] = now_utc()
        report["teardown"] = lab.stack.teardown()
        write_json(output / "raw" / "recovery-summary.json", report)
    return 0


def scenario_common(lab, report, name, body):
    log(f"=== scenario {name}")
    timeline = Timeline(lab, interval=0.5)
    timeline.phase = "baseline"
    timeline.start()
    started = time.time()
    result = {"name": name, "started_at": now_utc()}
    try:
        body(result, timeline)
        result["status"] = "PASS"
    except Exception as err:  # noqa: BLE001
        result["status"] = "ERROR"
        result["error"] = f"{type(err).__name__}: {err}"
        result["traceback"] = traceback.format_exc()
    finally:
        samples = timeline.stop()
        result["seconds"] = round(time.time() - started, 2)
        result["timeline"] = samples
        report["scenarios"][name] = result
        lab.stack.write_json(f"raw/recovery-{name}.json", result)
        log(f"=== scenario {name}: {result['status']} ({result['seconds']}s)")
    return result


def scenario_redis_outage(lab, report, seconds):
    rate = report["rate"]
    name = f"redis-outage-{seconds}s"

    def body(result, timeline):
        baseline = lab.run_load(rate, 8, f"{name}-baseline", shape=report["parameters"]["shape"],
                                concurrency=report["parameters"]["concurrency"])
        result["baseline"] = baseline
        timeline.phase = "fault"
        fault_started = time.time()
        lab.stack.stop_dependency("redis")
        result["fault_started_at"] = now_utc()
        fault = lab.run_load(rate, seconds, f"{name}-fault", shape=report["parameters"]["shape"],
                             concurrency=report["parameters"]["concurrency"])
        result["fault"] = fault
        result["refusal_classes"] = fault.get("status_counts")
        timeline.phase = "recovery"
        recovery_started = time.time()
        lab.stack.start_dependency("redis")
        result["dependency_back_seconds"] = round(time.time() - fault_started - seconds, 3)
        # RTO is "a normal request is served again", probed directly rather than
        # inferred from the next load round.
        result["rto"] = lab.probe_success(timeout=60)
        result["rto_seconds"] = result["rto"]["seconds"]
        timeline.phase = "drain"
        drain_seconds, state = drain_until_idle(lab, 180)
        result["drain_seconds"] = drain_seconds
        result["state_after_drain"] = state
        drain_load = lab.run_load(rate, 10, f"{name}-after", shape=report["parameters"]["shape"],
                                  concurrency=report["parameters"]["concurrency"])
        result["after"] = drain_load
        result["final_drain_seconds"], result["final_state"] = drain_until_idle(lab, 180)
        result["money"] = money_report(lab.ensure_conn(), lab.org.org_id, lab.baseline)
        result["receipt_attempts"] = attempts_histogram(lab.ensure_conn(), lab.org.org_id)
    scenario_common(lab, report, name, body)


def attempts_histogram(conn, org_id):
    with conn.cursor() as cur:
        cur.execute("""SELECT attempts, status, count(*) FROM usage_receipts
                       WHERE org_id = %s GROUP BY attempts, status ORDER BY attempts""", (org_id,))
        return [{"attempts": row[0], "status": row[1], "count": row[2]} for row in cur.fetchall()]


def scenario_pg_outage(lab, report, point):
    rate = report["rate"]
    name = f"pg-outage-{point}"

    def body(result, timeline):
        shape = report["parameters"]["shape"]
        concurrency = report["parameters"]["concurrency"]
        baseline = lab.run_load(rate, 8, f"{name}-baseline", shape=shape, concurrency=concurrency)
        result["baseline"] = baseline
        timeline.phase = "fault"
        if point == "finalize":
            # A stream is in flight while the database goes away: the receipt
            # write at the end of the stream cannot commit.
            worker = threading.Thread(
                target=lambda: result.update(
                    {"inflight": lab.run_load(rate, 12, f"{name}-inflight", shape="m",
                                              concurrency=concurrency)}), daemon=True)
            worker.start()
            time.sleep(2.0)
            lab.stack.stop_dependency("postgres")
            result["fault_started_at"] = now_utc()
            worker.join(timeout=120)
        else:
            lab.stack.stop_dependency("postgres")
            result["fault_started_at"] = now_utc()
            fault = lab.run_load(rate, 10, f"{name}-fault", shape=shape, concurrency=concurrency)
            result["fault"] = fault
            result["refusal_classes"] = fault.get("status_counts")
        timeline.phase = "recovery"
        recovery_started = time.time()
        lab.stack.start_dependency("postgres")
        result["dependency_back_seconds"] = round(time.time() - recovery_started, 3)
        result["rto"] = lab.probe_success(timeout=90)
        result["rto_seconds"] = result["rto"]["seconds"]
        timeline.phase = "drain"
        drain_seconds, state = drain_until_idle(lab, 240)
        result["drain_seconds"] = drain_seconds
        result["state_after_drain"] = state
        after = lab.run_load(rate, 10, f"{name}-after", shape=shape, concurrency=concurrency)
        result["after"] = after
        result["final_drain_seconds"], result["final_state"] = drain_until_idle(lab, 240)
        # A failed finalize leaves the reservation held on purpose; T07B releases
        # it only after the liveness window.  Observe that convergence instead of
        # reporting a snapshot mid-window.
        result["janitor"] = wait_held_reservations(lab, timeout=120)
        result["money"] = money_report(lab.ensure_conn(), lab.org.org_id, lab.baseline)
        result["receipt_attempts"] = attempts_histogram(lab.ensure_conn(), lab.org.org_id)
    scenario_common(lab, report, name, body)


def scenario_kill(lab, report, which):
    rate = report["rate"]
    name = f"kill9-{which}"

    def body(result, timeline):
        shape = report["parameters"]["shape"]
        concurrency = report["parameters"]["concurrency"]
        baseline = lab.run_load(rate, 8, f"{name}-baseline", shape=shape, concurrency=concurrency)
        result["baseline"] = baseline
        timeline.phase = "fault"
        process = lab.stack.process(which if which == "metering" else "gateway_a")
        killed = process.kill9(reason="T11 kill -9")
        result["killed"] = killed
        result["fault_started_at"] = now_utc()
        time.sleep(0.5)
        fault = lab.run_load(rate, 8, f"{name}-fault", shape=shape, concurrency=concurrency)
        result["fault"] = fault
        result["refusal_classes"] = fault.get("status_counts")
        timeline.phase = "recovery"
        recovery_started = time.time()
        if which == "metering":
            lab.stack.start_metering()
        else:
            lab.stack.start_gateway("a")
        result["restart_seconds"] = round(time.time() - recovery_started, 3)
        result["rto"] = lab.probe_success(timeout=90)
        result["rto_seconds"] = result["rto"]["seconds"]
        timeline.phase = "drain"
        after = lab.run_load(rate, 12, f"{name}-after", shape=shape, concurrency=concurrency)
        result["after"] = after
        drain_seconds, state = drain_until_idle(lab, 240)
        result["drain_seconds"] = drain_seconds
        result["state_after_drain"] = state
        result["final_drain_seconds"], result["final_state"] = drain_until_idle(lab, 240)
        result["janitor"] = wait_held_reservations(lab, timeout=120)
        result["money"] = money_report(lab.ensure_conn(), lab.org.org_id, lab.baseline)
        result["receipt_attempts"] = attempts_histogram(lab.ensure_conn(), lab.org.org_id)
    scenario_common(lab, report, name, body)


# ---------------------------------------------------------------------------
# 5. CLI
# ---------------------------------------------------------------------------

def fmt(value, digits=2, dash="—"):
    if value is None:
        return dash
    if isinstance(value, float):
        return f"{value:.{digits}f}"
    return str(value)


def load_cells(output, prefix):
    cells = []
    for path in sorted((output / "raw").glob(f"{prefix}-*.json")):
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except Exception:  # noqa: BLE001
            continue
        if "per_variant" in payload or "run" in payload:
            cells.append(payload)
    return cells


def lock_statement_avg(run, variant):
    """Average latency of the statement that takes the account lock (pgbench -r).

    Variant A locks with ``SELECT … FOR UPDATE`` (script 0); variant B locks with
    its conditional ``UPDATE billing_accounts`` (script 1).  For settlement the
    same statement text appears twice per script, so the number is the average
    over both account writes, not over the first one alone.
    """
    blocks = run.get("statement_latencies_per_script") or {}
    script_no = "0" if variant == "a" else "1"
    block = blocks.get(script_no) or {}
    prefix = "SELECT org_id, currency" if variant == "a" else "UPDATE billing_accounts"
    for statement, values in block.items():
        if statement.startswith(prefix):
            return values["avg_ms"]
    return None


def cmd_summarize(args):
    output = pathlib.Path(args.output).resolve()
    out_lines = []

    def emit(line=""):
        out_lines.append(line)

    cells = load_cells(output, "hotspot")
    hotspot = [c for c in cells if c.get("op") in ("reserve", "settle")
               and len(c.get("per_variant", {})) == 2]
    ensure = [c for c in cells if c.get("op") in ("ensure",)]
    driver = [c for c in cells if c.get("op") == "driver"]
    for op in ("reserve", "settle"):
        picked = [c for c in hotspot if c["op"] == op]
        if not picked:
            continue
        emit(f"### {op}: shipped FOR UPDATE (A) vs conditional update (B)")
        emit()
        emit("| accounts | conc | A tx/s | A p50 | A p95 | A p99 | A lock avg | B tx/s | B p50 | B p95 | B p99 | B lock avg | B−A tx% | B−A p99% | lock waiters max | A cpu s | B cpu s |")
        emit("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for cell in sorted(picked, key=lambda c: (c["accounts"], c["concurrency"])):
            run = cell["run"]
            labels = list(cell["per_variant"])
            a, b = cell["per_variant"][labels[0]], cell["per_variant"][labels[1]]
            cpu = run.get("pg", {}).get("cpu_usec_delta")
            emit("| {a} | {c} | {at} | {ap50} | {ap95} | {ap99} | {alock} | {bt} | {bp50} | {bp95} | {bp99} | {block} | {dt} | {dp99} | {lw} | {cpu} |".format(
                a=cell["accounts"], c=cell["concurrency"],
                at=fmt(a["throughput_per_second"], 0), ap50=fmt(a["latency_ms"].get("p50")),
                ap95=fmt(a["latency_ms"].get("p95")), ap99=fmt(a["latency_ms"].get("p99")),
                alock=fmt(lock_statement_avg(run, "a")),
                bt=fmt(b["throughput_per_second"], 0), bp50=fmt(b["latency_ms"].get("p50")),
                bp95=fmt(b["latency_ms"].get("p95")), bp99=fmt(b["latency_ms"].get("p99")),
                block=fmt(lock_statement_avg(run, "b")),
                dt=fmt((b["throughput_per_second"] / a["throughput_per_second"] - 1) * 100 if a["throughput_per_second"] else None, 1),
                dp99=fmt((b["latency_ms"].get("p99", 0) / a["latency_ms"].get("p99", 1) - 1) * 100, 1),
                lw=run.get("pg", {}).get("lock_waiters_max"),
                cpu=fmt((cpu or 0) / 1e6, 1)))
        emit()

    if ensure:
        emit("### profile A/B: the `INSERT … ON CONFLICT DO NOTHING` ensure statement")
        emit()
        emit("| accounts | conc | with ensure tx/s | with p50 | with p99 | without tx/s | without p50 | without p99 | p50 delta % | tx delta % |")
        emit("|---|---|---|---|---|---|---|---|---|---|")
        for cell in sorted(ensure, key=lambda c: (c["accounts"], c["concurrency"])):
            labels = list(cell["per_variant"])
            a, b = cell["per_variant"][labels[0]], cell["per_variant"][labels[1]]
            emit("| {a} | {c} | {at} | {ap50} | {ap99} | {bt} | {bp50} | {bp99} | {dp50} | {dt} |".format(
                a=cell["accounts"], c=cell["concurrency"],
                at=fmt(a["throughput_per_second"], 0), ap50=fmt(a["latency_ms"].get("p50")),
                ap99=fmt(a["latency_ms"].get("p99")),
                bt=fmt(b["throughput_per_second"], 0), bp50=fmt(b["latency_ms"].get("p50")),
                bp99=fmt(b["latency_ms"].get("p99")),
                dp50=fmt((b["latency_ms"].get("p50", 0) / a["latency_ms"].get("p50", 1) - 1) * 100, 1),
                dt=fmt((b["throughput_per_second"] / a["throughput_per_second"] - 1) * 100 if a["throughput_per_second"] else None, 1)))
        emit()

    if driver:
        emit("### harness ceiling control (`SELECT 1` per transaction, pgbench)")
        emit()
        emit("| concurrency | tx/s | p50 ms | p99 ms |")
        emit("|---|---|---|---|")
        for cell in sorted(driver, key=lambda c: c["concurrency"]):
            run = cell["run"]
            emit(f"| {cell['concurrency']} | {fmt(run.get('tps'), 0)} | — | — |")
        emit()

    query_files = sorted((output / "raw").glob("query-*.json"))
    for path in query_files:
        payload = json.loads(path.read_text(encoding="utf-8"))
        if "rounds" not in payload:
            continue
        tier = payload["name"]
        emit(f"### query A/B — tier {tier} ({payload['fixture']}, built in {payload['build_seconds']}s)")
        emit()
        emit("rotation: " + ",".join(f"r{r['round']}={r['variant']}" for r in payload["rounds"]))
        emit()
        emit("| query | baseline p50 | baseline p95 | indexes p50 | indexes p95 | rollup p50 | rollup p95 | adjacent indexes/baseline p95 % | adjacent rollup/baseline p95 % |")
        emit("|---|---|---|---|---|---|---|---|---|")
        by_query = {}
        for round_ in payload["rounds"]:
            for name, values in round_["per_query"].items():
                by_query.setdefault(name, {})[round_["round"]] = values["latency_ms"]
        for name in QUERY_SCRIPTS:
            if name not in by_query:
                continue
            rounds = by_query[name]
            def median_of(variant):
                want = [r["round"] for r in payload["rounds"] if r["variant"] == variant]
                values = [rounds[r].get("p95") for r in want if r in rounds and rounds[r].get("p95")]
                return statistics.median(values) if values else None
            def median_p50(variant):
                want = [r["round"] for r in payload["rounds"] if r["variant"] == variant]
                values = [rounds[r].get("p50") for r in want if r in rounds and rounds[r].get("p50")]
                return statistics.median(values) if values else None
            # adjacent pairs: (1,2) and (6,5) compare neighbouring rounds only.
            pairs = [(1, 2, "indexes"), (6, 5, "indexes"), (1, 3, "rollup"), (6, 4, "rollup")]
            def adjacent(variant):
                ratios = []
                for first, second, want in pairs:
                    if want != variant or first not in rounds or second not in rounds:
                        continue
                    a = rounds[first].get("p95")
                    b = rounds[second].get("p95")
                    if a and b:
                        ratios.append((b / a - 1) * 100)
                return statistics.median(ratios) if ratios else None
            emit(f"| {name} | {fmt(median_p50('baseline'))} | {fmt(median_of('baseline'))} | "
                 f"{fmt(median_p50('indexes'))} | {fmt(median_of('indexes'))} | "
                 f"{fmt(median_p50('rollup'))} | {fmt(median_of('rollup'))} | "
                 f"{fmt(adjacent('indexes'), 1)} | {fmt(adjacent('rollup'), 1)} |")
        emit()
        consistency = payload.get("rollup_consistency", {})
        emit("rollup consistency: " + json.dumps(consistency, default=str))
        emit()

    settle_path = output / "raw" / "settle-summary.json"
    if settle_path.exists():
        payload = json.loads(settle_path.read_text(encoding="utf-8"))
        emit("### settlement and outbox observability under load")
        emit()
        emit("| metric | value |")
        emit("|---|---|")
        emit(f"| load rate | {payload.get('capacity_smoke', {}).get('load_rate')} req/s "
             f"(60% of {payload.get('capacity_smoke', {}).get('highest_valid_rate')}) |")
        emit(f"| steady receipts | {payload.get('steady_receipts')} |")
        emit(f"| settlement lag (receipt→settled) | {json.dumps(payload.get('settlement_lag_ms'))} |")
        emit(f"| outbox dwell (created→settled) | {json.dumps(payload.get('outbox_dwell_ms'))} |")
        emit(f"| smoke settlement rate | {json.dumps(payload.get('smoke_settlement_rate'))} |")
        emit(f"| steady settlement rate | {json.dumps(payload.get('steady_settlement_rate'))} |")
        emit(f"| smoke backlog peak | {json.dumps(payload.get('smoke_backlog_peak'))} |")
        emit(f"| steady backlog peak | {json.dumps(payload.get('steady_backlog_peak'))} |")
        emit(f"| steady drain seconds | {payload.get('steady_drain_seconds')} |")
        emit(f"| money delta | {json.dumps(payload.get('money_delta'))} |")
        emit(f"| money after | {json.dumps(payload.get('money_after'))} |")
        emit()

    for path in sorted((output / "raw").glob("recovery-*.json")):
        payload = json.loads(path.read_text(encoding="utf-8"))
        if "rto_seconds" not in payload and "fault" not in payload:
            continue
        emit(f"### {payload.get('name')} ({payload.get('status')})")
        emit()
        emit(f"- RTO seconds: {payload.get('rto_seconds')} (dependency back: {payload.get('dependency_back_seconds')}, "
             f"restart: {payload.get('restart_seconds')})")
        emit(f"- drain seconds: {payload.get('drain_seconds')}")
        emit(f"- refusal classes: {json.dumps(payload.get('refusal_classes'))}")
        emit(f"- baseline: {json.dumps({k: payload.get('baseline', {}).get(k) for k in ('started', 'completed', 'rejected', 'transport_error', 'incomplete')})}")
        emit(f"- fault: {json.dumps({k: payload.get('fault', {}).get(k) for k in ('started', 'completed', 'rejected', 'transport_error', 'incomplete', 'status_counts')})}")
        emit(f"- after: {json.dumps({k: payload.get('after', {}).get(k) for k in ('started', 'completed', 'rejected', 'transport_error', 'incomplete')})}")
        emit(f"- money: {json.dumps(payload.get('money'))}")
        emit(f"- attempts: {json.dumps(payload.get('receipt_attempts'))}")
        emit()

    text_out = "\n".join(out_lines)
    if args.markdown:
        pathlib.Path(args.markdown).write_text(text_out + "\n", encoding="utf-8")
        print(f"wrote {args.markdown}")
    else:
        print(text_out)
    return 0


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--output", required=True, help="evidence directory")
    common.add_argument("--keep", action="store_true", help="do not tear the lab down")
    common.add_argument("--pg-dsn", default=None,
                        help="use an existing PostgreSQL instead of starting one (hotspot/query)")

    check = sub.add_parser("sqlcheck", help="verify the embedded SQL still matches the Go source")
    check.add_argument("-v", "--verbose", action="store_true")

    summarize = sub.add_parser("summarize", parents=[common],
                               help="turn raw/*.json into the report tables")
    summarize.add_argument("--markdown", default=None, help="write markdown to this path")

    hotspot = sub.add_parser("hotspot", parents=[common], help="B03 hotspot A/B")
    hotspot.add_argument("--rounds", type=int, default=4, help="ABBA rotation length (default 4)")
    hotspot.add_argument("--round-seconds", type=float, default=3.0)
    hotspot.add_argument("--accounts", type=lambda s: [int(x) for x in s.split(",")],
                         default=[1, 10, 1000])
    hotspot.add_argument("--concurrency", type=lambda s: [int(x) for x in s.split(",")],
                         default=[1, 2, 4, 8, 16, 32, 64, 128])
    hotspot.add_argument("--ops", default="reserve,settle")
    hotspot.add_argument("--settle-seed", type=int, default=4000,
                         help="executions seeded per settle round")

    query = sub.add_parser("query", parents=[common], help="B08 query A/B on 100k/1M rows")
    query.add_argument("--tiers", type=lambda s: s.split(","), default=["100k", "1m"])
    query.add_argument("--query-seconds", type=float, default=8.0)
    query.add_argument("--concurrency", type=int, default=8)

    settle = sub.add_parser("settle", parents=[common], help="settlement/observability under load")
    settle.add_argument("--rate", type=float, default=None, help="admissions/second (default 60%% of the smoke ceiling)")
    settle.add_argument("--seconds", type=float, default=90.0)
    settle.add_argument("--shape", default="s", choices=["s", "m", "l"])
    settle.add_argument("--concurrency", type=int, default=64)
    settle.add_argument("--interval", type=float, default=0.5, help="sample interval seconds")
    settle.add_argument("--drain-timeout", type=float, default=240.0)
    settle.add_argument("--rate-cap", type=float, default=40.0)

    recovery = sub.add_parser("recovery", parents=[common], help="RTO/backlog failure recovery")
    recovery.add_argument("--rate", type=float, default=None)
    recovery.add_argument("--shape", default="s", choices=["s", "m", "l"])
    recovery.add_argument("--concurrency", type=int, default=64)
    recovery.add_argument("--outages", type=lambda s: [int(x) for x in s.split(",")], default=[10, 30, 60])
    recovery.add_argument("--redis-outage", action="store_true", help="run only the Redis outages")
    recovery.add_argument("--pg-outage", action="store_true", help="run only the PostgreSQL outages")
    recovery.add_argument("--kill", action="store_true", help="run only the kill -9 scenarios")
    recovery.add_argument("--interval", type=float, default=0.5)
    recovery.add_argument("--rate-cap", type=float, default=40.0)
    recovery.add_argument("--drain-timeout", type=float, default=240.0)

    args = parser.parse_args(argv)
    if args.command == "hotspot" and args.rounds > 4:
        parser.error("--rounds is at most 4 (ABBA)")
    return args


def main(argv=None):
    args = parse_args(argv)
    if args.command == "sqlcheck":
        result = sqlcheck(verbose=args.verbose)
        print(json.dumps(result, indent=2))
        return 0
    if args.command == "hotspot":
        return cmd_hotspot(args)
    if args.command == "query":
        return cmd_query(args)
    if args.command == "settle":
        return cmd_settle(args)
    if args.command == "recovery":
        return cmd_recovery(args)
    if args.command == "summarize":
        return cmd_summarize(args)
    raise SystemExit(f"unknown command {args.command}")


if __name__ == "__main__":
    sys.exit(main())