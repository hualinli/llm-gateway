package billing

import "context"

// Bounded contexts for T07A's two write paths. They live next to the settlement
// code (rather than with the admission timeouts) because both are background
// paths: a dispatcher bookkeeping write or a settlement transaction must fail
// closed on its own short bound instead of waiting for the read-oriented
// DefaultQueryTimeout.

// withDispatchTimeout bounds one dispatcher bookkeeping transaction (a claim, a
// mark-dispatched, a retry record or an exception). It never covers a publish:
// Redis is called outside any transaction.
func (s *PGAccountStore) withDispatchTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.DispatchTimeout
	if timeout <= 0 {
		timeout = DefaultDispatchMarkTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// withSettlementTimeout bounds one settlement transaction.
func (s *PGAccountStore) withSettlementTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.SettlementTimeout
	if timeout <= 0 {
		timeout = DefaultSettlementTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// withSettlementVerifyTimeout bounds the detached re-query that resolves a
// settlement COMMIT whose outcome is unknown. It is deliberately separate from
// the transaction bound: the caller's context may already be cancelled, and
// "possibly committed" must be resolved by reading the same execution id, not by
// assuming a rollback.
func (s *PGAccountStore) withSettlementVerifyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.SettlementVerifyTimeout
	if timeout <= 0 {
		timeout = DefaultSettlementVerifyTimeout
	}
	return context.WithTimeout(ctx, timeout)
}
