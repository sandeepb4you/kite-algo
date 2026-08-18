package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// Backing up the one file that cannot be rebuilt.
//
// Everything else here is reconstructible: orders and fills are a record of what
// happened, positions are re-fetched from the broker, sessions are re-issued on
// the next login. The captured option candles and the daily instrument snapshots
// are not. Kite's /instruments feed lists only LIVE contracts and historical
// candles are keyed by instrument_token, so once a weekly option expires its token
// is gone from the API and its price history is unpurchasable at any price.
//
// Done in-process rather than by shelling out to the sqlite3 CLI, for two reasons.
// The runtime image deliberately installs no packages — it is alpine plus one
// static binary — so a CLI backup needs a second container and an apk fetch on a
// box that trades real money. And this way the copy is made by the same driver and
// the same WAL settings the writer uses, rather than by a second implementation
// that has to agree with them.

// BackupResult describes a completed backup, for the operator's log and for the
// alert that reports it.
type BackupResult struct {
	Path  string
	Bytes int64
	// SnapshotDays is how many distinct trading days of instrument snapshots the
	// copy contains. This is the number that actually matters: it is the count of
	// days that remain backtestable if the live database is lost.
	SnapshotDays int
	Candles      int64
	Took         time.Duration
}

// BackupInto writes a consistent, compacted copy of the database at dbPath to
// dest, then verifies the copy before returning.
//
// VACUUM INTO rather than a file copy. The database runs in WAL mode, where the
// -wal file holds committed pages that have not been checkpointed into the main
// file yet — so copying trading.db alone yields a database silently missing recent
// transactions, and copying the set of files while a writer is active can tear
// them. VACUUM INTO goes through SQLite, takes a read snapshot, and writes a
// defragmented copy with no free pages, which is usually smaller than the source.
// It is safe to run against a live database.
//
// The copy is opened and checked before this returns. An unverified backup is not
// a backup: the failure mode worth engineering against here is not "the backup
// broke", it is "the backup broke and nobody found out until they needed it".
func BackupInto(ctx context.Context, dbPath, dest string) (BackupResult, error) {
	var out BackupResult
	started := time.Now()

	if _, err := os.Stat(dest); err == nil {
		// Never overwrite. A backup job that clobbers is one bad argument away
		// from destroying the copy it was meant to protect.
		return out, fmt.Errorf("backup destination %s already exists", dest)
	} else if !os.IsNotExist(err) {
		return out, fmt.Errorf("check destination %s: %w", dest, err)
	}

	if _, err := os.Stat(dbPath); err != nil {
		return out, fmt.Errorf("source database %s: %w", dbPath, err)
	}

	// Opened WITHOUT the schema or migrations. This runs as a separate process
	// against a live database, and a backup job is the last thing that should be
	// altering the thing it is copying.
	src, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return out, fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = src.Close() }()

	// A generous busy timeout above, because this may overlap a tick write. The
	// alternative — failing on contention — turns a nightly job into a coin flip.
	if _, err := src.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return out, fmt.Errorf("vacuum into %s: %w", dest, err)
	}

	fi, err := os.Stat(dest)
	if err != nil {
		return out, fmt.Errorf("stat backup: %w", err)
	}
	out.Path, out.Bytes = dest, fi.Size()

	if err := verifyBackup(ctx, dest, &out); err != nil {
		return out, err
	}
	out.Took = time.Since(started)
	return out, nil
}

// verifyBackup opens the copy and confirms it is a readable database holding the
// data worth keeping.
func verifyBackup(ctx context.Context, dest string, out *BackupResult) error {
	db, err := sql.Open("sqlite", "file:"+dest+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open backup for verification: %w", err)
	}
	defer func() { _ = db.Close() }()

	// quick_check rather than integrity_check: it catches the corruption that
	// actually happens to a copied file and does not walk every index, which on a
	// multi-gigabyte database is the difference between seconds and many minutes.
	// A nightly job that takes an hour gets switched off.
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return fmt.Errorf("quick_check on backup: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("backup failed quick_check: %s", check)
	}

	// Structural soundness is not the same as usefulness — an empty database
	// passes quick_check. Count what the backup exists to preserve.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT as_of) FROM instrument_snapshots`).Scan(&out.SnapshotDays); err != nil {
		return fmt.Errorf("count snapshot days in backup: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM candles`).Scan(&out.Candles); err != nil {
		return fmt.Errorf("count candles in backup: %w", err)
	}
	return nil
}

// Summary renders the result as one line for a log or an alert.
func (r BackupResult) Summary() string {
	return fmt.Sprintf("%s — %.1f MB, %d snapshot day(s), %d candles, in %s",
		r.Path, float64(r.Bytes)/(1024*1024), r.SnapshotDays, r.Candles,
		r.Took.Round(time.Millisecond))
}
