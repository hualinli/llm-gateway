// Package failpoint is the test-only fault-injection surface of T10.
//
// It exists so the failure matrix can ask a running process to stop at one
// exact window — "the reservation is committed, the upstream has not been
// called", "the receipt row is inserted, the transaction has not committed",
// "the settlement is committed, the Redis ACK has not happened" — instead of
// racing `kill -9` against a wall clock and hoping the window was hit.
//
// # This cannot be enabled in production
//
// Three independent gates, in order of strength:
//
//  1. **Build tag.** The whole implementation lives in
//     `failpoint_enabled.go` behind `//go:build failpoint`. Every other build
//     (including the release build, `go build ./...`, and `go test ./...`)
//     compiles `failpoint.go`, where Attach is a no-op, Enabled and Armed are
//     always false, and Hit always returns nil. A production binary therefore
//     contains no code that can stop, kill or fail the process at a window,
//     whatever the environment says.
//  2. **Attach refuses `prod`.** In a `-tags failpoint` build, Attach returns an
//     error when the runtime profile is `prod` (GATEWAY_PROFILE/APP_ENV) *and*
//     GATEWAY_FAILPOINTS is non-empty. Every service calls Attach during
//     startup and turns an error into a fatal exit, so the tagged build refuses
//     to run as production with failpoints armed.
//  3. **Production preflight.** `gatewayChecks` (cmd/gateway) and
//     `meteringChecks` (cmd/metering) both require GATEWAY_FAILPOINTS to be
//     empty. That check is compiled into *every* build, so a plain
//     `GATEWAY_PROFILE=prod` start with a failpoint environment is fatal even
//     without the build tag.
//
// # Arming
//
//	GATEWAY_FAILPOINTS="name=action,name2,..."
//
// Entries are separated by commas or whitespace; an omitted action means
// `hold`. Actions:
//
//	hold   (default) log the hit marker, sleep GATEWAY_FAILPOINT_HOLD
//	       (default 120s), log the release marker, continue
//	kill   log the hit marker, then SIGKILL this process (unconditional,
//	       unrecoverable — this is the "kill -9 at this exact window" action)
//	exit   log the hit marker, then os.Exit(197)
//	panic  log the hit marker, then panic
//	error  log the hit marker, then return ErrInjected from Hit
//
// Each failpoint fires **at most once per process**: the second Hit of the same
// name is a no-op. Scenarios that need a second window start a second process,
// which is what the failure runner does anyway.
//
// # Hit markers
//
// Every hit and release writes exactly one line to stderr:
//
//	FAILPOINT {"event":"hit","name":"receipt.before_insert","action":"hold","pid":4711,"at":"..."}
//
// `scripts/failure-matrix.py` tails the process log for that line and only then
// issues its own SIGKILL, which is what makes the window deterministic: the
// process is provably stopped *at* the window before anything else happens.
//
// # Catalog
//
// The names below are the complete set. They are grouped by the window that
// matters for the F-matrix in docs/DEVELOPMENT.md
// §5. "kill" is the action the failure runner uses; the other actions exist so
// a scenario can choose a different injected fault without a rebuild.
//
//	Operator data plane (cmd/gateway/billing.go):
//	  admission.before_reserve             entered admit(), before the price
//	                                       lookup/reserve transaction exists
//	  admission.after_reserve              Reserve returned: the execution row
//	                                       and the reservation are committed,
//	                                       MarkExecuting has not run
//	  admission.after_mark_executing       the execution is `executing` and the
//	                                       upstream call is about to start
//	  admission.commit_response_lost       the reserve transaction committed but
//	                                       the caller is told it failed
//	                                       (F19; needs action=error)
//	  receipt.after_commit_before_done     FinalizeReceipt returned a committed
//	                                       receipt; the gate has not released
//	                                       "[DONE]" yet
//
//	Durable receipt (internal/billing/receipt_pg.go, inside the receipt tx):
//	  receipt.before_insert                the execution row lock is held, no
//	                                       receipt row written yet
//	  receipt.after_insert_before_commit   the receipt, the execution
//	                                       transition and the audit row are
//	                                       written, COMMIT has not run
//	  receipt.before_commit                alias window owned by the commit
//	                                       helper (same instant as above)
//	  receipt.after_commit                 COMMIT returned successfully
//	  receipt.commit_response_lost         COMMIT succeeded, the caller sees an
//	                                       unknown outcome and must re-query by
//	                                       execution id (F19; action=error)
//
//	Settlement (internal/billing/settlement_settle_pg.go):
//	  settlement.before_commit             the debit/release/account/execution/
//	                                       receipt/audit writes are done, COMMIT
//	                                       has not run
//	  settlement.after_commit              the settlement is durable
//	  settlement.commit_response_lost      COMMIT succeeded, the caller sees an
//	                                       unknown outcome (F19; action=error)
//	  settlement.ack_before                committed settlement, XACK not issued
//	                                       (F05: kill here, restart, redeliver)
//	  settlement.ack_after                 XACK returned
//
//	Recovery release (internal/billing/recovery_pg.go):
//	  recovery.before_commit               the release entry, the account
//	                                       projection, the execution state and
//	                                       the audit row are written, COMMIT has
//	                                       not run
//	  recovery.after_commit                the release is durable
package failpoint

// EnvVar arms the process's failpoints. It is declared here, outside any build
// constraint, so the production preflight of every build can refuse it: a
// service started with GATEWAY_PROFILE=prod and a non-empty GATEWAY_FAILPOINTS
// must fail to start even when it was built without the `failpoint` tag.
//
// An empty value means "none armed".
const EnvVar = "GATEWAY_FAILPOINTS"
