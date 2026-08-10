package database

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// migrationFiles embeds every migration file at compile time, so the
// binary can self-migrate on boot with no external migration tool or
// volume mount required. This is the canonical copy used at runtime; the
// human-facing copy at the repository root (/migrations) is kept in sync
// and is what contributors edit — see the Makefile's `sync-migrations`
// target.
//
//go:embed migrations/*.sql
var migrationFilesRaw embed.FS

// migrationFiles is migrationFilesRaw rooted at the migrations/ subdirectory
// so callers can address files by their bare name (e.g. "0001_init.up.sql").
var migrationFiles = mustSub(migrationFilesRaw, "migrations")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Migrate applies every pending *.up.sql migration in lexical order,
// tracking applied versions in a schema_migrations table so re-runs are
// idempotent (safe to call on every boot).
func (d *DB) Migrate() error {
	if _, err := d.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     VARCHAR(255) PRIMARY KEY,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("migrate: create tracking table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return fmt.Errorf("migrate: read migrations dir: %w", err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		version := strings.TrimSuffix(name, ".up.sql")

		var exists bool
		if err := d.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("migrate: check version %s: %w", version, err)
		}
		if exists {
			continue
		}

		contents, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}

		tx, err := d.Begin()
		if err != nil {
			return fmt.Errorf("migrate: begin tx for %s: %w", name, err)
		}

		if _, err := tx.Exec(string(contents)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: apply %s: %w", name, err)
		}

		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: record %s: %w", name, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate: commit %s: %w", name, err)
		}
	}

	return nil
}
