package main

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/Contictus/plimsoll/backend/migrations"
	"github.com/pressly/goose/v3"

	// Registers the "pgx" driver with database/sql. Blank because nothing here calls into
	// it: goose is written against the standard interface, and this is how pgx is reached
	// through it.
	_ "github.com/jackc/pgx/v5/stdlib"
)

const migrateUsage = "plimsollctl migrate"

// runMigrate applies every pending migration as the owner role.
//
// It exists so the compose topology can gate the api and the worker on the schema being
// current. Before it, a clean machine ran `make up` and watched the worker exit on a
// function that did not exist yet -- the services started before anything had created the
// tables they read, and the failure named a missing function rather than a missing step.
//
// It connects with PLIMSOLL_OWNER_DSN and never the app role, for the same reason goose
// does: migrations are DDL, and the role that serves requests must not hold DDL rights
// (K15, L12).
func runMigrate() error {
	dsn := os.Getenv(ownerDSNEnv)
	if dsn == "" {
		return fmt.Errorf("%s is not set", ownerDSNEnv)
	}

	// database/sql rather than pgx's own pool: goose is written against the standard
	// interface, and pgx registers a stdlib driver for exactly this. The connection is
	// short-lived and does no work of ours, so nothing here needs the decimal codec that
	// store.NewPool installs (L1) -- and saying so is cheaper than someone wondering.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		// The DSN carries a password, so it is never echoed (L13).
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
