package billing

import "database/sql"

// PGStore bundles the price-version store and the account store behind one
// value, so cmd/console wires billing with a single call. Both halves share the
// Console's pool; neither opens its own (ADR-0002 §2.6).
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
