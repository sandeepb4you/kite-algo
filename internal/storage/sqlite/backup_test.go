package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/storage"
)

// A backup must be a readable database containing what the original held.
func TestBackupIntoProducesAUsableCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "trading.db")

	store, err := New(ctx, src, nil)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	seedSnapshot(t, store)
	// Deliberately NOT closed before backing up: the whole point is that this
	// works against a live database in WAL mode, where the -wal file holds
	// committed pages the main file does not yet have. A file copy here would
	// silently lose the row just written.
	t.Cleanup(func() { _ = store.Close() })

	dest := filepath.Join(dir, "backup.db")
	res, err := BackupInto(ctx, src, dest)
	if err != nil {
		t.Fatalf("BackupInto: %v", err)
	}

	if res.Bytes <= 0 {
		t.Error("backup reports zero bytes")
	}
	if res.SnapshotDays != 1 {
		t.Errorf("snapshot days = %d, want 1 — the copy is missing the data it "+
			"exists to preserve", res.SnapshotDays)
	}
	if !strings.Contains(res.Summary(), "snapshot day") {
		t.Errorf("summary is not operator-readable: %s", res.Summary())
	}

	// And the copy must open on its own, as a database rather than a blob.
	copyStore, err := New(ctx, dest, nil)
	if err != nil {
		t.Fatalf("the backup does not open as a database: %v", err)
	}
	defer func() { _ = copyStore.Close() }()

	var days int
	if err := copyStore.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT as_of) FROM instrument_snapshots`).Scan(&days); err != nil {
		t.Fatalf("read the backup: %v", err)
	}
	if days != 1 {
		t.Errorf("the reopened backup holds %d snapshot days, want 1", days)
	}
}

// Refusing to overwrite is not politeness. A backup job one bad argument away
// from clobbering its own output can destroy the copy it exists to protect.
func TestBackupRefusesToOverwrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "trading.db")

	store, err := New(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	dest := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(dest, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := BackupInto(ctx, src, dest); err == nil {
		t.Fatal("an existing destination was overwritten")
	}
	// And it must be untouched.
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "precious" {
		t.Errorf("the existing file was modified: %q, %v", got, err)
	}
}

// A missing source must fail loudly rather than producing an empty database that
// passes every structural check.
func TestBackupFailsOnAMissingSource(t *testing.T) {
	dir := t.TempDir()
	_, err := BackupInto(context.Background(),
		filepath.Join(dir, "nope.db"), filepath.Join(dir, "out.db"))
	if err == nil {
		t.Fatal("backing up a nonexistent database succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "out.db")); statErr == nil {
		t.Error("a backup file was created for a database that does not exist")
	}
}

// seedSnapshot writes one day of instrument master, which is the data the backup
// exists to preserve.
func seedSnapshot(t *testing.T, store *Store) {
	t.Helper()
	day := time.Date(2026, 8, 19, 15, 40, 0, 0, time.UTC)
	rows := []storage.InstrumentRow{{
		InstrumentToken: 12345, TradingSymbol: "NIFTY2681824350CE",
		Name: "NIFTY", Expiry: day.AddDate(0, 0, 1), Strike: 24350,
		LotSize: 75, InstrumentType: "CE", Segment: "NFO-OPT",
		Exchange: "NFO", TickSize: 0.05,
	}}
	if err := store.SaveInstrumentSnapshot(context.Background(), day, rows); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}
