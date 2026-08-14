package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrate brings the database up to the latest schema version.
//
// Versioning is tracked in SQLite's own `PRAGMA user_version`, which needs no
// extra table and lives in the database header, so the version and the schema
// can never disagree.
//
// The original schema.sql is the version 1 baseline. It is written entirely with
// CREATE TABLE IF NOT EXISTS, so applying it to a database created before
// versioning existed is a no-op that simply stamps it as version 1 — existing
// databases upgrade in place with no export/import.
//
// Rules for adding a migration:
//   - Add a new migrations/NNNN_name.sql; never edit one that has shipped.
//   - Each file runs in its own transaction and bumps user_version on success.
//   - Migrations must be idempotent where SQLite allows it (IF NOT EXISTS), so a
//     half-applied upgrade after a crash can be retried.
func migrate(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}

	// Version 0 means either a brand-new file or a database created before
	// versioning. Applying the baseline covers both.
	if current == 0 {
		if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("apply baseline schema: %w", err)
		}
		if err := setUserVersion(ctx, db, 1); err != nil {
			return err
		}
		current = 1
		if logger != nil {
			logger.Info("database schema initialized", "version", 1)
		}
	}

	steps, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range steps {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
		if logger != nil {
			logger.Info("database migrated", "version", m.version, "name", m.name)
		}
		current = m.version
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads and orders the embedded migration files.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// Files are named NNNN_description.sql.
		parts := strings.SplitN(e.Name(), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("migration %q is not named NNNN_description.sql", e.Name())
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("migration %q has a non-numeric version prefix", e.Name())
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })

	// A duplicate version would make the applied set depend on file ordering.
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("migrations %s and %s share version %d",
				out[i-1].name, out[i].name, out[i].version)
		}
	}
	return out, nil
}

// applyMigration runs one migration and stamps the new version atomically.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	// PRAGMA does not accept a bound parameter, and the value here comes from a
	// filename we parsed as an integer, so interpolation is safe.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("stamp version %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

func setUserVersion(ctx context.Context, db *sql.DB, v int) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}
