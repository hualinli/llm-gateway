package billing

import "database/sql"

// PGStore bundles the price-version store and the account store behind one
// value, so cmd/console wires billing with a single call. Both halves share the
// Console's pool; neither opens its own (ADR-0002 §2.6).
//
// T06B's execution/reservation methods are on *PGAccountStore, so PGStore
// carries them too: a service that wires NewPGStore gets prices, accounts and
// the execution state machine from one value.
type PGStore struct {
	*PGPriceVersionStore
	*PGAccountStore
}

// NewPGStore returns the PostgreSQL billing store.
func NewPGStore(db *sql.DB) *PGStore {
	return &PGStore{
		PGPriceVersionStore: NewPGPriceVersionStore(db),
		PGAccountStore:      NewPGAccountStore(db),
	}
}

// The bundled store satisfies every contract this package exports, including
// the T06B execution store.
var (
	_ PriceVersionStore = (*PGStore)(nil)
	_ AccountStore      = (*PGStore)(nil)
	_ ExecutionStore    = (*PGStore)(nil)
)
