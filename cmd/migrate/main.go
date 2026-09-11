package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	migrate "github.com/golang-migrate/migrate/v4"
	"llm-gateway/migrations"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: migrate up|down-one|version (POSTGRES_DSN required)")
	}
	action := os.Args[1]
	if action != "up" && action != "down-one" && action != "version" {
		log.Fatal("unknown migration action")
	}
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		log.Fatal("POSTGRES_DSN is required")
	}
	if err := run(dsn, action); err != nil {
		log.Fatal(err)
	}
}

func run(dsn, action string) (err error) {
	m, err := migrations.Open(dsn)
	if err != nil {
		return err
	}
	defer func() { a, b := m.Close(); err = errors.Join(err, a, b) }()
	switch action {
	case "up":
		err = m.Up()
	case "down-one":
		err = m.Steps(-1)
	case "version":
		var version uint
		var dirty bool
		version, dirty, err = m.Version()
		if errors.Is(err, migrate.ErrNilVersion) {
			fmt.Println("version=none dirty=false")
			return nil
		}
		if err == nil {
			fmt.Printf("version=%d dirty=%t\n", version, dirty)
		}
	}
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	return err
}
