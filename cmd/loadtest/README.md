# cmd/loadtest — SSE load generator and its report contract

`cmd/loadtest` is the load generator the isolated benchmark runner drives
(`scripts/isolated-baseline.py`, T01A) and the tool the capacity experiments
(B01/B06/B09, T11) use. It is deliberately small: arrival scheduling, SSE
measurement, client-side pacing and one JSON report. It is not a generic load
platform.

Companion tool: `cmd/upstream-mock` serves the synthetic SSE streams. The load
shapes below are requested from it per request through mock-only request fields
(`x_mock_*`), so one mock process can serve the whole S/M/L matrix — including
through the Gateway, because the OpenAI adapter keeps unknown request fields
verbatim. A real provider would reject those fields: **never use `-shape`,
`-large-event` or `-role-only-first` against a paid endpoint.**

## 1. Modes

| `-mode` | Arrival model | Use |
|---|---|---|
| `auto` (default) | `open` when `-rate > 0`, else `closed` | keeps the historical command line working |
| `open` | every arrival keeps its planned monotonic timestamp; admission is bounded by `-c` slots plus the `-admission-queue` depth; overflow is an explicit counted drop | overload, latency knee, B00/B09 |
| `closed` | exactly `-c` streams in flight; a freed slot is the next arrival | fixed active streams, resource upper bound |
| `fixed` | **historical behaviour**: one ticker-driven arrival per interval that blocks on the concurrency semaphore when slots are full | B00 A side only; the schedule silently stretches, so the round is not a capacity measurement |

In `open` mode the arrival loop never blocks on concurrency: the scheduled
timestamp is not delayed by a full slot set, and the excess becomes
`loadgen_dropped` (`no_free_slot` when the queue depth is 0, `queue_full`
otherwise). `-admission-queue 0` means "admit only when a slot is free right
now"; the default is `4*c`, which absorbs short bursts while keeping the drain
bounded (`≈ queued/c × stream duration`). A queued arrival is dispatched the
moment a slot frees; the wait appears as `dispatch_lag`, never inside
`first_event`/`first_content`. The per-request `-timeout` starts at dispatch,
not at the scheduled timestamp.

## 2. Flags

| Flag | Default | Meaning |
|---|---|---|
| `-target` | `http://localhost:8090/v1/chat/completions` | target URL (Gateway or mock) |
| `-c` | `100` | concurrency slots (max active streams) |
| `-n` | `1000` | total scheduled arrivals |
| `-rate` | `0` | planned arrival rate, requests/second (float) |
| `-mode` | `auto` | `auto\|open\|closed\|fixed` |
| `-admission-queue` | `-1` | open-loop admission queue depth; `-1` = `4*c`, `0` = drop as soon as every slot is busy |
| `-timeout` | `0` (auto) | per-request timeout from dispatch; auto = `max(60s, shape duration + 30s, slow phase + 60s, transfer time + 60s)` |
| `-cancel-after` | `0` | cancel each request this long after dispatch (client hang-up, counted as `cancelled`/`explicit`) |
| `-shape` | `off` | request the S/M/L stream shape from the mock: `off\|s\|m\|l` |
| `-large-event` | `off` | request large SSE events: `off\|1k\|64k\|1m\|over` |
| `-role-only-first` | `false` | ask the mock for a role-only first frame (separates `first_event` from `first_content`) |
| `-slow-client` | `normal` | client read pacing: `normal\|10k\|1k\|stop` (10 KiB/s, 1 KiB/s, full stop) |
| `-slow-client-duration` | `0` | length of the throttled/stopped phase; `0` = whole stream (for `stop`, only cancellation/timeout ends it). Reading resumes at full speed afterwards, which is what makes post-stall recovery measurable |
| `-max-event-bytes` | `0` (4 MiB) | client SSE event budget passed to `internal/provider/sse` |
| `-key` | `$GATEWAY_API_KEY` | bearer token; prefer the environment variable so it never shows up in a process list |
| `-json` | empty | write the structured report to this path |
| `-tolerate-drops` | `false` | exit 0 when every scheduled arrival is accounted for (started or dropped), for deliberate overload rounds |

Exit code: `0` when every scheduled arrival completed; `1` otherwise (with
`-tolerate-drops`: `0` when `started + loadgen_dropped + not_scheduled ==
scheduled`); `2` for invalid flags. The generator never hides its own drops
behind a zero exit code unless `-tolerate-drops` is explicit.

The per-request timeout default **exceeds the longest stream shape** (L ≈
10.3 s), so a normal long stream is not misclassified as a timeout. The
historical hardcoded 10 s is gone; `-timeout` is always available.

### Stream shapes (§3.1 of `docs/plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md`)

| `-shape` | first event | content events | interval | event line |
|---|---|---|---|---|
| `s` | 20 ms | 8 | 15 ms | 256 B |
| `m` | 100 ms | 100 | 20 ms | 512 B |
| `l` | 300 ms | 500 | 20 ms | 512 B (≈10.3 s stream) |

`-shape` sends `x_mock_ttft_ms`, `x_mock_token_interval_ms`, `x_mock_tokens`
and `x_mock_event_line_bytes` in the request body. Without `-shape` the mock's
process flags decide the shape, exactly as before.

### Large events

`-large-event` sends `x_mock_event_line_bytes`; sizes are measured the way the
SSE readers bound a line, i.e. `data: ` + payload + newline:

| `-large-event` | line | expected outcome |
|---|---|---|
| `1k` | 1024 B | complete |
| `64k` | 64 KiB | complete |
| `1m` | 1 MiB | complete, exactly on the shared reader's 1 MiB line cap |
| `over` | 1 MiB + 1 KiB | bounded rejection: `parse_error` against the mock directly, `incomplete` through the Gateway (the Gateway refuses the oversized event before it reaches the client) |

Pair MiB tiers with a small `-tokens` (the mock's default 8 would stream 8 MiB
per request). The client never buffers a body: it holds one bounded event at a
time (`-max-event-bytes`, default 4 MiB) and the shared reader caps a line at
1 MiB. A single data line above 1 MiB is rejected by design; that is the
`over` tier, not a bug to work around.

## 3. Report contract (`-json`)

Top level: `target`, `concurrency`, `requests`, `rate_limit_qps`, `mode`,
`elapsed_seconds`, `success_per_second`, `timeout_seconds`,
`loadgen_dropped`, `loadgen_dropped_percent`, `capacity_round_valid`,
`load_profile` (mode, planned rate, queue, shape, large event, slow client,
timeout, expected stream seconds) and `report`.

`report` keeps every historical key — `Total`, `Success`, `Errors`, `Status`,
`TTFT`, `TotalDur`, `AvgChunks` — so existing consumers keep working, and adds:

- accounting: `Scheduled`, `Started`, `Completed`, `Rejected`,
  `TransportError`, `Incomplete`, `Cancelled`, `ParseError`, `NotScheduled`,
  `Abandoned`, `LoadgenDropped`, `DroppedNoFreeSlot`, `DroppedQueueFull`,
  `CancelExplicit`, `CancelTimeout`, `PeakActive`, `ActiveAtEnd`,
  `GoroutinesPeak`;
- rates: `PlannedStartRatePerSecond` (the target), `ArrivalRatePerSecond`
  (what the generator actually delivered: `(scheduled-1)/arrival window`),
  `AchievedStartRatePerSecond` (`started/arrival window`, i.e. streams started
  per second of offered load, directly comparable with the planned rate),
  `SuccessPerSecond` (`completed/run seconds`, the sustained completion rate),
  plus the windows they were computed over (`ArrivalWindowSeconds`,
  `StartWindowSeconds` — the span between the first and last start, which
  collapses when the system is saturated — and `RunSeconds`);
- latency blocks (each with `Count/P50/P95/P99/Min/Max/Avg`): `FirstEvent`
  (the legacy `TTFT` carries the same numbers), `FirstContent`,
  `StreamDuration`, `TotalDur`, `DispatchLag` (scheduled→started, includes
  admission queueing), `ScheduledToTerminal` (scheduled→terminal),
  `ArrivalLag` (scheduled→arrival);
- volume: `AvgEvents`, `AvgContentEvents`, `AvgChunks`, `AvgBytesPerStream`,
  `BytesRead`. The event/byte averages cover every request that reached an
  HTTP 200 stream (completed, incomplete, cancelled mid-stream, parse error);
  use `Completed`/`Success` for success-only analysis. Rejections and transport
  failures are excluded, and `TotalDur`/`StreamDuration` follow the same rule;
- validity: `CapacityRule`, `CapacityRoundValid`, `CapacityInvalidReasons`.

Invariants: `Started == Completed + Rejected + TransportError + Incomplete +
Cancelled + ParseError`; `Total == Started`; `Success == Completed`;
`Errors == TransportError + Incomplete + Cancelled + ParseError` (rejections are
counted separately, so a fast 429 never pollutes success latency);
`Scheduled == Started + LoadgenDropped + Abandoned + NotScheduled`.

Metric definitions:

- **first_event**: first SSE event whose joined data is non-empty and not
  `[DONE]`. Comments, keepalives, empty frames and framing-only events do not
  count. This is what the historical `TTFT` measured.
- **first_content**: first non-empty text delta decoded from
  `choices[].delta.content` (string, or a text-part array); the legacy
  `choices[].text` is accepted too. Role-only, `finish_reason` and usage-only
  frames never produce a `first_content`.
- Both are measured from dispatch. A stream with no content has **no**
  `first_content` observation (`Count` stays low) — the total duration is never
  used as a fake TTFT.
- **dispatch_lag** and **scheduled_to_terminal** are measured from the planned
  timestamp, so queueing time is visible instead of being folded into network
  latency.
- **completed**: HTTP 200 and `[DONE]` received. **incomplete**: HTTP 200
  without `[DONE]`. **rejected**: non-200 (429/503/…). **transport_error**:
  dial/TLS/header failure or a mid-stream read error that is neither a
  cancellation nor a parse-budget violation. **cancelled**: the generator
  cancelled the request (`explicit` for `-cancel-after`, `timeout` for
  `-timeout`). **parse_error**: the shared reader rejected the framing
  (line/event over budget).

## 4. Generator validity rule (0.1 %)

> Generator-side drops above **0.1 %** of scheduled arrivals — and arrivals
> delivered below **99 %** of the planned rate — invalidate that round for
> capacity conclusions. Re-run with more generator resources or a separate load
> host.

The rule is published three times so it cannot get lost: in the JSON report
(`capacity_rule`, `capacity_round_valid`, `capacity_invalid_reasons`), in the
log line (`INVALID for capacity conclusions: …`), and here. A round with
`capacity_round_valid: false` may still be reported as *generator* evidence
(B00), but its numbers must not be quoted as the system's capacity.

Invalid reasons the tool emits: `loadgen_dropped` share above 0.1 %, arrival
rate below 99 % of the plan (the historical `fixed` mode trips this with zero
drops — that is the point), `Abandoned > 0`, `NotScheduled > 0`, a mismatch
between scheduled starts and recorded observations, and zero arrivals.

## 5. B00 proof: drops are visible, the old mode hid them

```bash
# Long stream (L shape), c=2, target 100/s, no admission queue.
go run ./cmd/upstream-mock -listen 127.0.0.1:18080 -ttft 300ms -token-interval 20ms -tokens 500 &
go run ./cmd/loadtest -target http://127.0.0.1:18080/v1/chat/completions \
  -mode open -c 2 -n 100 -rate 100 -admission-queue 0 -timeout 60s -json /tmp/open.json
# -> scheduled=100 started=2 loadgen_dropped=98 (98%) peak_active=2
#    arrival_rate≈100/s (schedule kept), capacity_round_valid=false
```

The same load with the historical scheduler (`-mode fixed`) reports
`loadgen_dropped=0` while `arrival_rate` collapses to `c / stream duration`
(e.g. 0.2/s for two 10 s streams) and `dispatch_lag`/`arrival_lag` grow without
bound: the offered load was silently reduced, which is exactly the failure T01B
removes. Both modes stay available so the comparison is reproducible.

## 6. What this tool does not do

- No TLS/HTTP2 tuning, no connection statistics beyond what the report shows,
  no distributed load generation (use several hosts and compare `arrival_rate`).
- No safety switches for production profiles; `x_mock_*` request fields only
  affect `cmd/upstream-mock`.
- It measures one process's view. Capacity conclusions require the isolated
  runner (T01A) with a fixed commit, a warm-up, several rounds, and generator
  validity per round.
