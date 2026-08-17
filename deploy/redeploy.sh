#!/usr/bin/env bash
#
# Redeploy the trading platform: pull, rebuild, restart, verify.
#
#   ./redeploy.sh              pull and redeploy
#   ./redeploy.sh --no-pull    redeploy the working tree as it stands
#   ./redeploy.sh --rollback   go back to the image this replaced
#   ./redeploy.sh -y           skip the market-hours confirmation
#
# Exits non-zero if the app does not come back healthy, so it is safe to chain
# something after it.
#
# What this deliberately does NOT do: touch the database. `docker compose up`
# leaves the kite-data volume alone, and the captured option candles in it
# cannot be re-downloaded once contracts expire. Nothing here should ever grow
# a `docker compose down -v`.

set -euo pipefail

# Everything below runs inside a brace group, which bash must read and parse in
# full before executing any of it.
#
# This script git-pulls, and one of the files a pull can rewrite is this script.
# Bash reads a plain script incrementally and keeps a byte offset into the file,
# so replacing it mid-run resumes execution at a shifted position — halfway
# through a line, in the middle of a word. The failure is spectacular and reads
# like nothing to do with deployment.
{

	# Resolve before the cd: $0 may be a relative path, and it is read again
	# below for --help.
SELF="$(readlink -f "$0")"
cd "$(dirname "$SELF")"

PULL=1
ROLLBACK=0
ASSUME_YES=0
for arg in "$@"; do
	case "$arg" in
	--no-pull) PULL=0 ;;
	--rollback) ROLLBACK=1 ;;
	-y | --yes) ASSUME_YES=1 ;;
	-h | --help)
		sed -n '3,16p' "$SELF" | sed 's/^# \?//'
		exit 0
		;;
	*)
		echo "unknown option: $arg (try --help)" >&2
		exit 2
		;;
	esac
done

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33m!! %s\033[0m\n' "$*" >&2; }
die() {
	printf '\033[31m!! %s\033[0m\n' "$*" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Preflight. Everything here is cheap and runs BEFORE the running app is
# touched, so a misconfigured box fails while it is still serving.
# ---------------------------------------------------------------------------

# Migrate the pre-directory-mount layout in place.
#
# config.yaml and secrets.yaml used to be bind-mounted as individual files and
# now live one directory down. They are git-ignored, so a pull cannot move them
# and a box that skipped this step would start against an empty conf/ — the app
# would fall back to defaults, which is a far worse outcome than failing.
if [ -f config.yaml ] && [ ! -f conf/config.yaml ]; then
	warn "moving config.yaml -> conf/config.yaml (directory-mount layout)"
	mkdir -p conf && mv config.yaml conf/config.yaml
fi
if [ -f secrets.yaml ] && [ ! -f secrets/secrets.yaml ]; then
	warn "moving secrets.yaml -> secrets/secrets.yaml (directory-mount layout)"
	mkdir -p secrets && mv secrets.yaml secrets/secrets.yaml
fi

[ -f .env ] || die "missing .env — see README section 1"
[ -f conf/config.yaml ] || die "missing conf/config.yaml — see README section 1"
[ -f secrets/secrets.yaml ] || die "missing secrets/secrets.yaml — see README section 2"

# The recurring failure on this deployment: the container runs as UID 10001 and
# a bind mount carries host ownership through unchanged, so root-owned secrets
# crash-loop the app on "permission denied" before it reaches any of its own
# checks. Editors that write-then-rename silently reset the owner to root, so
# this is re-checked on every deploy rather than assumed.
#
# The directory needs it too: 10001 cannot read a file it cannot traverse to,
# and the error is the same "permission denied" with no hint of which of the
# two is at fault.
for p in secrets secrets/secrets.yaml; do
	owner="$(stat -c '%u:%g' "$p")"
	if [ "$owner" != "10001:10001" ]; then
		warn "$p is owned by $owner, not 10001:10001 — fixing"
		chown 10001:10001 "$p" || die "chown failed; re-run as root"
	fi
done
# 700 on the directory, 600 on the file: root and the app, nobody else.
[ "$(stat -c '%a' secrets)" = "700" ] || { chmod 700 secrets; }
[ "$(stat -c '%a' secrets/secrets.yaml)" = "600" ] || {
	warn "tightening secrets/secrets.yaml to 600"
	chmod 600 secrets/secrets.yaml
}

# conf/ is deliberately left world-readable. It holds no credentials by design,
# and a 600 config owned by 10001 would break the moment an editor replaced it
# as root — trading one silent failure for another.
chmod 755 conf 2>/dev/null || true

# ---------------------------------------------------------------------------
# Market-hours guard.
# ---------------------------------------------------------------------------
#
# A redeploy drops the Zerodha websocket and restarts the process. Positions
# and the login survive (both are in sqlite), but running strategy instances do
# not — they are in-memory and do not come back on their own. Doing that at
# 11:00 on a Wednesday with a straddle on is a bad afternoon.
#
# A weekday/clock test, not a holiday-aware one: the calendar lives in the app,
# and a wrong "market is closed" here would be worse than an unnecessary prompt.
now_ist="$(TZ=Asia/Kolkata date +'%u %H%M')"
dow="${now_ist% *}"
hhmm="${now_ist#* }"
if [ "$dow" -le 5 ] && [ "$hhmm" -ge 0915 ] && [ "$hhmm" -le 1530 ] && [ "$ASSUME_YES" -eq 0 ]; then
	warn "market hours ($(TZ=Asia/Kolkata date +'%a %H:%M') IST). Restarting drops the"
	warn "data feed and stops any running strategy — it will NOT restart itself."
	printf 'Continue? [y/N] '
	read -r reply </dev/tty || reply=n
	case "$reply" in y | Y | yes) ;; *) die "aborted" ;; esac
fi

# ---------------------------------------------------------------------------
# Rollback path: retag and restart, no build.
# ---------------------------------------------------------------------------

if [ "$ROLLBACK" -eq 1 ]; then
	docker image inspect kite-algo:previous >/dev/null 2>&1 ||
		die "no kite-algo:previous image — nothing to roll back to"
	say "Rolling back to the previous image"
	docker tag kite-algo:previous kite-algo:latest
	docker compose up -d --no-build --force-recreate app
else
	if [ "$PULL" -eq 1 ]; then
		say "Pulling"
		before="$(git rev-parse --short HEAD)"
		# --ff-only: a merge commit created unattended on a production box is
		# not something anyone wants to discover later.
		git pull --ff-only
		after="$(git rev-parse --short HEAD)"
		if [ "$before" = "$after" ]; then
			echo "already up to date at $after"
		else
			echo "$before -> $after"
			git --no-pager log --oneline "$before..$after" | sed 's/^/    /'
		fi
	fi

	# Keep the outgoing image so --rollback has somewhere to go. Best effort:
	# on the very first deploy there is nothing to tag yet.
	docker image inspect kite-algo:latest >/dev/null 2>&1 &&
		docker tag kite-algo:latest kite-algo:previous

	say "Building and restarting"
	# --force-recreate is not belt-and-braces, it is the point.
	#
	# config.yaml is bind-mounted and read once at process start. When the image
	# is unchanged — a config-only edit, or a redeploy with nothing new to pull —
	# plain `up -d` sees an up-to-date container, prints "Running", and leaves
	# the old process alive with the old config still loaded. The deploy then
	# reports success having applied nothing, which is the worst possible
	# outcome for a command whose job is to apply changes.
	docker compose up -d --build --force-recreate
fi

# ---------------------------------------------------------------------------
# Verify. A deploy that returns success while the app crash-loops is worse than
# one that fails loudly, and this app has several startup checks that exit(1) —
# bad config, unreadable secrets, no web password.
# ---------------------------------------------------------------------------

say "Waiting for health"

# Ask compose for the container rather than assuming deploy-app-1: the name is
# derived from the directory, so it changes if this checkout is moved.
cid="$(docker compose ps -q app)"
[ -n "$cid" ] || die "no app container — 'docker compose up' did not create one"

deadline=$((SECONDS + 120))
status=""
while [ "$SECONDS" -lt "$deadline" ]; do
	status="$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo missing)"
	state="$(docker inspect -f '{{.State.Status}}' "$cid" 2>/dev/null || echo missing)"
	case "$status:$state" in
	healthy:*) break ;;
	*:restarting)
		echo
		docker compose logs --tail 30 app
		die "app is restarting — see the log above"
		;;
	esac
	printf '.'
	sleep 3
done
echo

if [ "$status" != "healthy" ]; then
	docker compose logs --tail 30 app
	die "app did not become healthy within 120s (status: $status)"
fi

# The healthcheck only proves the process answers HTTP. These fields say whether
# it is actually able to trade: an app that is up but disconnected from Zerodha,
# or halted, looks perfectly healthy by every other measure.
say "Up"
# `|| true` because a grep that matches nothing exits 1, and under `set -e` that
# would fail the deploy after it has already succeeded.
docker compose exec -T app wget -qO- http://127.0.0.1:8080/healthz |
	tr ',' '\n' | tr -d '{}"' |
	grep -E 'mode|kite_state|streaming|halted|order_routing|strategies' |
	sed 's/^/    /' || true

cat <<'EOF'

    If kite_state is not connected, log in again at /connect — the Zerodha
    token expires daily and a redeploy does not refresh it.

EOF

} # end of the parse-it-all-first brace group
