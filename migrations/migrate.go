// Package migrations owns versioned schema changes. Services only call Check;
// schema changes run explicitly through cmd/migrate or integration setup.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed *.sql
var files embed.FS

//go:embed check.sql
var checkSQL string

const Version = 12

var ErrSchema = errors.New("database schema requires migration")

func Open(dsn string) (*migrate.Migrate, error) {
	source, err := iofs.New(files, ".")
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	return m, nil
}

func Up(dsn string) error {
	m, err := Open(dsn)
	if err != nil {
		return err
	}
	err = m.Up()
	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	sourceErr, dbErr := m.Close()
	return errors.Join(err, sourceErr, dbErr)
}

// Check is read-only. Never silently run a migration from a service constructor.
func Check(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, checkSQL).Scan(&version, &dirty); err != nil {
		return fmt.Errorf("%w; run make migrate-up before starting services: %v", ErrSchema, err)
	}
	if dirty || version != Version {
		return fmt.Errorf("%w: version=%d dirty=%t, expected %d", ErrSchema, version, dirty, Version)
	}
	return nil
}
