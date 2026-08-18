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

RATIO=$(awk "BEGIN{printf \"%.1f\", $RAW_BYTES/$GZ_BYTES}")
SUMMARY="$(summarize) (${RATIO}x compression)"
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
