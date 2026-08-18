#!/usr/bin/env bash
#
# Nightly backup of the one file here that cannot be rebuilt.
#
# Orders and fills are a record, positions come back from the broker, sessions are
# re-issued on the next login. The captured option candles and the daily instrument
# snapshots are not recoverable: Kite lists only LIVE contracts, and historical
# candles are keyed by instrument_token, so an expired weekly's price history is
# unpurchasable at any price. Losing trading.db loses every day of it.
#
# Run from a systemd timer — see deploy/systemd/. Safe to run by hand at any time,
# including during market hours: the copy is taken through SQLite (VACUUM INTO)
# rather than by copying files, so a concurrent writer cannot tear it.
#
# Usage:  ./backup.sh [--report]
#           --report  send a summary on SUCCESS too. Sundays do this anyway, so a
#                     channel silent for more than a week is broken, not quiet.
set -euo pipefail

cd "$(dirname "$(readlink -f "$0")")"

DEST_DIR="${KITE_BACKUP_DIR:-/var/backups/kite-algo}"
KEEP_DAYS="${KITE_BACKUP_KEEP_DAYS:-14}"
KEEP_MONTHLY_DAYS="${KITE_BACKUP_KEEP_MONTHLY_DAYS:-365}"

# The UID the app image runs as. The backup is written by the container, so the
# destination has to be writable by that user and not by root — see the chown
# below, which is the difference between this working and a permission error every
# night at two in the morning.
APP_UID="${KITE_APP_UID:-10001}"

# Offsite. An rclone remote NAME plus path, e.g. "utho:kite-algo-backups".
# Configured on the host with `sudo rclone config`, so no endpoint and no
# credential lives in this repo or in the image. Empty disables offsite entirely.
RCLONE_REMOTE="${KITE_BACKUP_RCLONE_REMOTE:-}"
# Extra rclone flags, unquoted on purpose so multiple flags work — e.g.
# "--bwlimit 10M" to stay out of the way of market data if this ever runs by day.
RCLONE_FLAGS="${KITE_BACKUP_RCLONE_FLAGS:-}"

REPORT_SUCCESS="no"
if [ "${1:-}" = "--report" ]; then REPORT_SUCCESS="yes"; fi
# Report success once a week, decided here rather than by a second timer: two
# units firing at the same minute on the same directory is a race, and the one
# that lost would exit early having sent nothing.
if [ "$(date +%u)" = "7" ]; then REPORT_SUCCESS="yes"; fi

STAMP="$(date +%F)"
IN_CONTAINER="/backups/trading-$STAMP.db"
ON_HOST="$DEST_DIR/trading-$STAMP.db"
MARKER="$DEST_DIR/.last-success"

# notify sends one line through the app's own alert channel.
#
# Through the binary rather than curl, because the bot token lives in secrets.yaml
# and parsing YAML in shell to find it would be fragile and a second place a
# credential is read. Never fatal: a backup that succeeded but could not narrate
# itself is still a backup.
notify() {
  docker compose run --rm -T app \
    -config /etc/kite-algo/config.yaml -notify-send "$1" >/dev/null 2>&1 || true
}

# summarize describes the state of the backup directory. It reads the directory
# rather than this run's variables, so it is correct whether or not this run did
# any work.
summarize() {
  local newest kept total mb
  newest=$(ls -1t "$DEST_DIR"/trading-*.db.gz 2>/dev/null | head -1 || true)
  kept=$(find "$DEST_DIR" -maxdepth 1 -name 'trading-*.db.gz' | wc -l)
  total=$(du -sh "$DEST_DIR" 2>/dev/null | cut -f1 || echo "?")
  mb=$(( $(stat -c %s "${newest:-/dev/null}" 2>/dev/null || echo 0) / 1048576 ))
  printf 'kite-algo backup OK — %s, %s MB, %s copies kept, %s total in %s' \
    "$(basename "${newest:-none}")" "$mb" "$kept" "${total:-?}" "$DEST_DIR"
}

# Any unhandled failure reports itself. A backup job that fails silently is
# indistinguishable from one that was never installed — which is exactly the state
# this replaces, where the only backup was a command documented in a README.
on_error() {
  local line=$1
  notify "[CRITICAL] kite-algo backup FAILED (line $line) on $(hostname).

The irreplaceable data is the captured option candles and instrument snapshots;
they cannot be re-fetched once contracts expire. Check:
  journalctl -u kite-backup --since today
  df -h $DEST_DIR"
  echo "backup failed at line $line" >&2
}
trap 'on_error $LINENO' ERR

mkdir -p "$DEST_DIR"
# The container writes the copy, and it runs as an unprivileged user. Without this
# the very first run fails with a permission error inside the container, which
# surfaces as an opaque non-zero exit from compose.
chown "$APP_UID:$APP_UID" "$DEST_DIR"
chmod 700 "$DEST_DIR"

# Refuse rather than overwrite today's copy. A second run in one day is a manual
# test or a misfiring timer, and neither is a reason to discard a good backup.
if [ -e "$ON_HOST.gz" ]; then
  echo "today's backup already exists: $ON_HOST.gz (not redone)"
  # Still report if this run was meant to. A weekly heartbeat that goes quiet
  # because an earlier run got there first is the failure being avoided here.
  if [ "$REPORT_SUCCESS" = "yes" ]; then
    notify "$(summarize) (already taken today)"
  fi
  exit 0
fi

echo "==> writing $ON_HOST"
# -v adds a NEW mount path rather than overriding one the service already has.
# compose run cannot override the service's own :ro mounts — that is why
# -set-password needs plain docker run — but a path it does not use is fine.
docker compose run --rm -T -v "$DEST_DIR:/backups" app \
  -config /etc/kite-algo/config.yaml -backup "$IN_CONTAINER"

if [ ! -s "$ON_HOST" ]; then
  echo "backup file missing or empty: $ON_HOST" >&2
  exit 1
fi
RAW_BYTES=$(stat -c %s "$ON_HOST")

echo "==> compressing"
# Instrument masters and candles compress hard: endless repeated symbol prefixes
# and monotonic timestamps. -9 costs seconds on a job that has all night.
gzip -9 "$ON_HOST"
GZ_BYTES=$(stat -c %s "$ON_HOST.gz")
chmod 600 "$ON_HOST.gz"

echo "==> rotating"
# Two retentions, deliberately. Dailies cover the ordinary case — something broke
# recently and yesterday is fine. The month-start copies cover what dailies cannot:
# corruption or a bad delete that went unnoticed for weeks, where every surviving
# daily already contains the damage.
find "$DEST_DIR" -maxdepth 1 -name 'trading-*-[0-9][0-9].db.gz' \
  ! -name 'trading-*-01.db.gz' -mtime "+$KEEP_DAYS" -print -delete
find "$DEST_DIR" -maxdepth 1 -name 'trading-*-01.db.gz' \
  -mtime "+$KEEP_MONTHLY_DAYS" -print -delete

date -Iseconds > "$MARKER"

# --- offsite ----------------------------------------------------------------
#
# Everything above this line protects against corruption, a bad delete, and
# "yesterday was fine". None of it survives losing the box: the copy sits on the
# same disk as the database. What is at stake is every day of captured option
# data, which no amount of money buys back from Kite once the contracts expire.
#
# The remote is an rclone remote NAME, configured on the host, so no provider
# endpoint or credential appears in this repo or in the image. At ~15 MB a night
# compressed this is a rounding error in bandwidth either way.
OFFSITE="no offsite configured"
if [ -n "$RCLONE_REMOTE" ]; then
  if ! command -v rclone >/dev/null 2>&1; then
    notify "[CRITICAL] kite-algo backup: KITE_BACKUP_RCLONE_REMOTE is set to
'$RCLONE_REMOTE' but rclone is not installed on $(hostname).

The LOCAL copy is fine — $ON_HOST.gz — but nothing is leaving this box, so a disk
or VM loss takes every day of captured option data with it."
    echo "rclone not installed; offsite skipped" >&2
    exit 1
  fi

  echo "==> uploading to $RCLONE_REMOTE"
  UPLOAD_LOG=$(mktemp)
  # copyto rather than copy: an explicit destination name cannot silently become
  # a directory if the remote path is mistyped. rclone verifies the checksum
  # after upload for S3-compatible backends, so a zero exit already means the
  # bytes arrived intact.
  if ! rclone copyto $RCLONE_FLAGS \
      "$ON_HOST.gz" "$RCLONE_REMOTE/$(basename "$ON_HOST.gz")" \
      >"$UPLOAD_LOG" 2>&1; then
    notify "[CRITICAL] kite-algo offsite upload FAILED on $(hostname).

The LOCAL copy is safe — $(basename "$ON_HOST.gz") — so nothing is lost today, but
the offsite copy is missing and a disk or VM loss would be unrecoverable.

rclone said:
$(tail -5 "$UPLOAD_LOG")"
    echo "offsite upload failed:" >&2
    cat "$UPLOAD_LOG" >&2
    rm -f "$UPLOAD_LOG"
    exit 1
  fi
  rm -f "$UPLOAD_LOG"

  # Confirm the object is actually there at the right size. rclone's own
  # checksum check is the real guarantee; this catches the case where it wrote
  # somewhere other than where we think it did, which a checksum cannot.
  REMOTE_BYTES=$(rclone lsl "$RCLONE_REMOTE/$(basename "$ON_HOST.gz")" 2>/dev/null \
    | awk '{print $1}' | head -1)
  if [ "${REMOTE_BYTES:-0}" != "$GZ_BYTES" ]; then
    notify "[CRITICAL] kite-algo offsite copy is the wrong size on $(hostname).

Local $(basename "$ON_HOST.gz") is $GZ_BYTES bytes, the remote object reports
${REMOTE_BYTES:-nothing}. Treat the offsite copy for today as absent."
    echo "offsite size mismatch: local=$GZ_BYTES remote=${REMOTE_BYTES:-none}" >&2
    exit 1
  fi

  echo "==> pruning $RCLONE_REMOTE"
  # Deliberately NOT `rclone delete` with --filter rules.
  #
  # This is the one destructive step in the job, and rclone's include/exclude
  # rules are order-dependent in a way that is easy to get subtly wrong. A filter
  # that silently fails to match does not error — it widens the delete, and the
  # thing being widened is the set of backups. So the decision is made here, one
  # object at a time, and rclone is only ever told to remove a specific name.
  #
  # Age comes from the FILENAME, not from the object's timestamp: a remote mtime
  # is upload time, so a re-upload would make an old backup look new and exempt
  # itself from pruning forever.
  #
  # Verified behaviour at the defaults (14 daily / 365 monthly), as of today
  # being 2026-08-18:
  #
  #   trading-2026-08-18.db.gz     0d    keep
  #   trading-2026-08-04.db.gz    14d    keep    (boundary is inclusive)
  #   trading-2026-08-03.db.gz    15d    PRUNE
  #   trading-2026-07-01.db.gz    48d    keep    (month-start, 365d limit)
  #   trading-2025-08-01.db.gz   382d    PRUNE   (month-start, past its limit)
  #   trading-2025-08-17.db.gz   366d    PRUNE
  #   notes.txt                     -    untouched, not ours
  #   trading-bogus.db.gz           -    untouched, unparseable date
  TODAY_EPOCH=$(date +%s)
  while read -r obj; do
    case "$obj" in
      trading-????-??-??.db.gz) ;;
      *) continue ;;   # not ours; never touch it
    esac
    day=${obj#trading-}
    day=${day%.db.gz}
    obj_epoch=$(date -d "$day" +%s 2>/dev/null) || continue
    age_days=$(( (TODAY_EPOCH - obj_epoch) / 86400 ))

    limit=$KEEP_DAYS
    case "$day" in
      *-01) limit=$KEEP_MONTHLY_DAYS ;;   # month-start copies live much longer
    esac

    if [ "$age_days" -gt "$limit" ]; then
      echo "    pruning $obj (${age_days}d old, limit ${limit}d)"
      rclone deletefile "$RCLONE_REMOTE/$obj" || true
    fi
  done < <(rclone lsf "$RCLONE_REMOTE" 2>/dev/null || true)

  REMOTE_COUNT=$(rclone lsf "$RCLONE_REMOTE" 2>/dev/null | grep -c '^trading-.*\.db\.gz$' || true)
  OFFSITE="offsite OK ($RCLONE_REMOTE, $REMOTE_COUNT copies)"
fi

RATIO=$(awk "BEGIN{printf \"%.1f\", $RAW_BYTES/$GZ_BYTES}")
SUMMARY="$(summarize) (${RATIO}x compression)
$OFFSITE"
echo "$SUMMARY"
if [ "$REPORT_SUCCESS" = "yes" ]; then
  notify "$SUMMARY"
fi

# Running out of disk is the failure this job meets eventually. Better to say so
# while there is still room than to fail on the night there is not.
AVAIL_MB=$(df -Pm "$DEST_DIR" | awk 'NR==2{print $4}')
if [ "$AVAIL_MB" -lt 2048 ]; then
  notify "kite-algo backup: only ${AVAIL_MB} MB free on $DEST_DIR.
Backups will start failing. Prune older copies or lower KITE_BACKUP_KEEP_DAYS."
fi

exit 0
