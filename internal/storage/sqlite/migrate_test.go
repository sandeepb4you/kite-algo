package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateFromEmpty(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "new.db")
	db := openDB(t, path)

	if err := migrate(ctx, db, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	v, err := userVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	want := steps[len(steps)-1].version
	if v != want {
		t.Errorf("user_version = %d, want %d", v, want)
	}

	// Baseline and migration tables must both exist.
	for _, table := range []string{"orders", "fills", "positions", "candles",
		"kite_sessions", "web_sessions", "candle_coverage", "instrument_snapshots"} {
		var name string
		err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after migration: %v", table, err)
		}
	}
}

// TestMigrateIsIdempotent covers the normal case: every process start runs the
// migrator, and the overwhelming majority of those runs have nothing to do.
func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "repeat.db")
	db := openDB(t, path)

	for i := 0; i < 3; i++ {
		if err := migrate(ctx, db, nil); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
}

// TestMigrateUpgradesAPreVersioningDatabase is the compatibility case that
// matters most: databases created before the migrator existed carry
// user_version 0 but already have the baseline tables. They must upgrade in
// place rather than erroring or being rebuilt.
func TestMigrateUpgradesAPreVersioningDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openDB(t, path)

	// Simulate the old startup path: apply schema.sql directly, no version stamp.
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("apply legacy schema: %v", err)
	}
	if v, _ := userVersion(ctx, db); v != 0 {
		t.Fatalf("legacy database should start at version 0, got %d", v)
	}

	// Put a row in so we can prove the upgrade preserved existing data.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders (id, exchange, trading_symbol, product, order_type, side,
			quantity, status, mode, created_at, updated_at)
		VALUES ('legacy-1','NFO','NIFTY24AUG24500CE','MIS','MARKET','BUY',75,'COMPLETE','paper','x','y')`,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err := migrate(ctx, db, nil); err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}

	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM orders WHERE id='legacy-1'`).Scan(&id); err != nil {
		t.Errorf("existing data was lost during migration: %v", err)
	}

	// And the new columns/tables are present.
	var oi int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(open_interest,0) FROM candles LIMIT 1`).Scan(&oi); err != nil && err != sql.ErrNoRows {
		t.Errorf("open_interest column missing after upgrade: %v", err)
	}
}

// TestMigrationFilesAreWellFormed catches naming and numbering mistakes at test
// time rather than on a production start-up.
func TestMigrationFilesAreWellFormed(t *testing.T) {
	steps, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("no migrations found; the embed pattern is probably wrong")
	}
	for i, m := range steps {
		if m.version < 2 {
			t.Errorf("%s has version %d; 1 is reserved for the schema.sql baseline",
				m.name, m.version)
		}
		if m.sql == "" {
			t.Errorf("%s is empty", m.name)
		}
		if i > 0 && m.version <= steps[i-1].version {
			t.Errorf("%s is not ordered after %s", m.name, steps[i-1].name)
		}
	}
}
