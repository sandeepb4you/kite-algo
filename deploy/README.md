# Deploying kite-algo with Docker

Single box, single container, TLS and an IP allowlist in front. Written against
an Utho cloud server, but nothing here is provider-specific beyond "an Ubuntu VM
with a public IP".

This build runs **without a domain**, on the server's IP, with a certificate
from Caddy's own CA.

> **One instance. Always.** The database is SQLite: one file, one writer. A
> second container mounting the same volume corrupts it, and the corruption is
> not obvious — it surfaces as a read that quietly comes back wrong. Never
> `--scale app=2`, never run a second copy "just to test".

---

## 0. The server (Utho or any other provider)

Create an **Ubuntu 22.04/24.04 cloud server**. Anything with 2 GB RAM and 40 GB
of disk is ample — this is one Go process and a SQLite file.

Pick a **region in India**. Every order and every historical request goes to
Zerodha's servers in India, and the WebSocket tick feed is latency-sensitive in
a way a backtest is not. A server in Mumbai and one in Frankfurt are not the
same product.

Then, in the provider's **cloud firewall**, allow only:

| Port | From | Why |
|---|---|---|
| 22/tcp | your IP | SSH |
| 443/tcp | your IP | the trading UI |

**Port 80 is not needed and should stay closed.** There is no ACME challenge to
serve — the certificate comes from Caddy's own CA, see step 5.

Then bootstrap it, as root.

A minimal cloud image ships neither git nor Docker, so fetch the script
directly rather than cloning first — the script is what installs git:

```sh
# Rocky / Alma / RHEL 9 — no host firewall, takes no argument
curl -fsSL https://raw.githubusercontent.com/sandeepb4you/kite-algo/master/deploy/bootstrap-rocky.sh -o bootstrap.sh
chmod +x bootstrap.sh
./bootstrap.sh

# Ubuntu / Debian — sets up ufw, so it needs the IP you browse from
curl -fsSL https://raw.githubusercontent.com/sandeepb4you/kite-algo/master/deploy/bootstrap.sh -o bootstrap.sh
chmod +x bootstrap.sh
./bootstrap.sh "$(curl -s https://api.ipify.org)"     # or pass your IP explicitly
```

The two paths differ on purpose. The RHEL script configures **no host firewall**:
firewalld does not filter Docker-published ports through its zones anyway, and on
some cloud images it fails to load its own config outright. The cloud firewall
and Caddy's `ALLOWED_IPS` allowlist carry access control there instead.

(If you already have git, cloning first and running the script from
`deploy/` works exactly the same.)

Both install Docker. The Ubuntu script also sets up ufw as a second layer behind
the cloud firewall; the RHEL one does not, for the reasons below.

> **Two RHEL-family details the Ubuntu path does not have.**
>
> **SELinux stays enforcing.** The compose file marks its bind mounts `:Z` so
> Docker relabels them. Without that the container cannot read its own config
> and fails with a permission error that never mentions SELinux. Turning
> SELinux off would trade a real protection for five minutes of convenience on
> a box holding broker credentials.
>
> **No host firewall, so the cloud firewall is load-bearing.** firewalld was
> not worth keeping here: its zones act on INPUT while Docker's published ports
> are DNAT'd through FORWARD, so a `firewall-cmd --add-port` rule leaves a
> container port open to the world while the firewall looks configured. Working
> around that needs a `DOCKER-USER` direct rule, and on some cloud images
> firewalld refuses to apply any permanent config at all (`RUNNING_BUT_FAILED`).
>
> What replaces it: the provider's cloud firewall, and Caddy's `ALLOWED_IPS`
> allowlist, which answers 404 to every address not on it. **Check the cloud
> firewall by hand** — nothing on the box will catch a mistake there, and port
> 22 in particular now has no second layer in front of it.

## 1. Prepare the files

```sh
cd /opt/kite-algo/deploy
mkdir -p conf secrets
cp .env.example        .env
cp config.example.yaml conf/config.yaml
```

`conf/` and `secrets/` are mounted into the container as **directories**, not as
individual files. A single-file bind mount is pinned to an inode when the
container is created, and most editors save by writing a temporary file and
renaming it over the original — so the host file changes while the container
keeps reading the old contents, through restarts, until it is recreated. This
layout resolves the path on each access instead.

Edit `.env`:

```ini
SITE_ADDRESS=203.0.113.10           # the server's public IP, no scheme
ALLOWED_IPS=27.7.11.10              # where you browse from; CIDR ok
```

Edit `conf/config.yaml` — the settings that must agree:

| Setting | Value |
|---|---|
| `web.public_url` | `https://203.0.113.10` — the same IP, with the scheme |
| `web.addr` | `0.0.0.0:8080` — inside the container only, never published |
| `web.trust_proxy` | `true` — Caddy is in front |

`trust_proxy` is not optional. The login lockout and the live-arming lockout key
off the client address; without it every request looks like it came from Caddy
and the throttle becomes global instead of per-attacker.

## 2. Secrets

```sh
cp ../secrets.example.yaml secrets/secrets.yaml
$EDITOR secrets/secrets.yaml         # api_key + api_secret
chown -R 10001:10001 secrets         # NOT optional — see below
chmod 700 secrets
chmod 600 secrets/secrets.yaml
ls -lnd secrets; ls -ln secrets/secrets.yaml

docker compose build app             # the next step needs the image to exist
docker run --rm -it \
  -e TRADING_SECRETS_PATH=/secrets/secrets.yaml \
  -v "$PWD/secrets:/secrets:Z" \
  kite-algo:latest -set-password
```

**Plain `docker run`, not `docker compose run`.** The compose service mounts
`secrets/` read-only, and a `-v` on the same container path does not override
that — the compose form prompts you for a password, hashes it, and *then* dies
on `open /secrets/secrets.yaml: read-only file system`. Running the image
directly is what gets you a writable mount.

The other two arguments are not decoration either:

- **`-e TRADING_SECRETS_PATH`** — `docker run` does not inherit the compose
  service's `environment:`, and without it the path falls back to
  `~/.trading/secrets.yaml` *inside* the container. That write succeeds, reports
  success, and vanishes with the container.
- **No `-config`** — `config.yaml` sets `web.addr` to `0.0.0.0:8080`, and the
  startup check rejects a non-loopback address when no password is set yet,
  which is the very thing you are running this to fix. Omitting the flag leaves
  `web.addr` at its `127.0.0.1:8080` default, which passes.

Minimum 12 characters. A short password or a mismatched confirmation exits with
an error and writes nothing — rerun the command.

**The `chown` is required, and `chmod 600` alone actively breaks things.** The
container runs as UID 10001, not root, and a bind mount carries the host's
ownership straight through — so a root-owned `600` file leaves the app crash-
looping on `read secrets /secrets/secrets.yaml: permission denied` before it
reaches any of its own checks.

The directory needs the same treatment for the same reason: a file you cannot
traverse to is a file you cannot read, and it fails with the identical message.
That is why the `chown` is `-R` and the directory is `700`.

Set the owner rather than loosening the mode to `644`. Root still reads and
writes the file either way, `600` still keeps every other account on the box
out, and your `api_secret` does not become world-readable to get there. The same
applies if you ever replace this file: re-`chown` it, or the app stops on the
next restart. `./redeploy.sh` re-checks and repairs both on every run, which is
the reliable way to not think about this again.

`conf/` is deliberately left world-readable. It holds no credentials by design,
and a `600` config owned by 10001 would break the moment an editor rewrote it as
root — trading one silent failure for another.

The app **refuses to start** on a non-loopback address with no password, so this
cannot be skipped by accident.

## 3. Register the redirect URL with Zerodha

At <https://developers.kiteconnect.com/apps>, set the redirect URL to:

```
https://203.0.113.10/kite/callback
```

Character for character, no wildcards.

> ⚠️ **Confirm Zerodha accepts an IP-based redirect URL before you rely on this.**
> Their console may require a hostname. If it refuses, the entire login flow is
> blocked — no session, no trading, no capture — and the fix is a domain. A `.in`
> domain costs a few hundred rupees a year, and a free DuckDNS subdomain works
> too; either also gets you a real Let's Encrypt certificate and makes step 5
> unnecessary. Worth five minutes to check first.

## 4. Start

```sh
docker compose up -d --build
docker compose logs -f app
```

Look for `web ui listening` with the right `kite_redirect_url`.

## 5. Trust the certificate (no domain, so no Let's Encrypt)

Let's Encrypt will not issue for a bare IP, so Caddy is its own CA. Install its
root once per device and the UI becomes an ordinary padlocked site:

```sh
./trust-cert.sh
```

Do this rather than clicking through the warning each session. A UI that trains
you to dismiss browser security prompts has taught you to ignore the one that
turns out to be real — and around a money-moving interface that is the wrong
habit to build.

## 6. Log in to Zerodha

Open `https://203.0.113.10`, sign in with the operator password, then **Connect**.

---

## Running it day to day

**You must log in to Zerodha every trading day.** Kite access tokens expire
around 06:00 IST and can only be renewed through an interactive browser login —
there is no way to automate it. Deploying to a server does not change this.

That matters more than it sounds: the instrument snapshot and the daily option
capture both run off that session. A day nobody logs in is a day of option data
that **cannot be recovered afterwards**, because Kite drops expired contracts
from its feed entirely.

| Task | Command |
|---|---|
| Logs | `docker compose logs -f app` (add `--since 1m` — it replays the whole backlog by default) |
| Restart | `docker compose restart app` |
| Redeploy | `./redeploy.sh` |
| Undo a bad deploy | `./redeploy.sh --rollback` |
| Capture a missed day | see below |
| Shell | `docker compose exec app sh` |

**Redeploying.** `./redeploy.sh` pulls, rebuilds, restarts and then waits until
the app reports healthy, failing loudly with the log tail if it does not — a
deploy that returns success while the app crash-loops is the one failure mode
worth engineering against here.

It always recreates the container, even when the image is unchanged. `config.yaml`
is bind-mounted and read once at startup, so a config-only edit followed by a
plain `docker compose up -d` leaves the old process running with the old settings
and reports success — a redeploy that applied nothing. It re-checks `secrets.yaml` ownership every run,
because an editor that writes-then-renames resets it to root and the app will
not start. It also asks for confirmation during market hours, since a restart
drops the data feed and stops any running strategy without restarting it.

The image it replaces is kept as `kite-algo:previous`, so `--rollback` retags
and restarts without a build.

**Capturing a specific past day** needs the CLI — the capture panel's button
always targets the most recent trading day. Stop the app first: two processes
writing the same SQLite file is the thing this deployment is built to avoid.

```sh
docker compose stop app
docker compose run --rm app -config /etc/kite-algo/config.yaml -capture 2026-08-14
docker compose up -d app
```


## Alerts

The missing-login and capture-result alerts go to Telegram. The bot token is a
credential, so it lives in `secrets/secrets.yaml` alongside the Kite keys — not in
`conf/config.yaml`, which is deliberately world-readable:

```yaml
notify:
  telegram:
    bot_token: "8123456:AA..."
```

`enabled`, `chat_id` and `repeat_every` go in `conf/config.yaml`. Verify delivery
without waiting for a real alert:

```sh
docker compose run --rm app -config /etc/kite-algo/config.yaml -notify-test
```

That reads the same mounted config and secrets the service does, so a success here
means the running container can send too. Press Start in the bot first — Telegram
refuses messages to a user who has never started it.

`-config` is required here — `compose run` replaces the CMD, and without it the
capture scope silently falls back to defaults instead of your configured
underlyings, expiries and strike range.

## Backups

The database holds every captured option candle and every daily instrument
snapshot. Kite lists only LIVE contracts and keys historical candles by
instrument token, so an expired weekly's price history cannot be re-fetched at any
price. Everything else in the file is reconstructible; this is not.

### Install

```sh
cd /opt/kite-algo/deploy
sudo cp systemd/kite-backup.service systemd/kite-backup.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kite-backup.timer
```

Check it took, and when it next runs:

```sh
systemctl list-timers kite-backup.timer
```

Then force one immediately rather than waiting for the night:

```sh
sudo systemctl start kite-backup.service
journalctl -u kite-backup -n 30 --no-pager
ls -lh /var/backups/kite-algo/
```

### What it does

`backup.sh` runs `tradebot -backup`, which issues `VACUUM INTO` through the app's
own SQLite driver. Not a file copy: the database runs in WAL mode, so the `-wal`
file holds committed pages the main file does not have yet, and copying
`trading.db` alone yields a database silently missing recent transactions. VACUUM
INTO takes a read snapshot and writes a defragmented copy, safe against a live
writer — so this is safe to run during market hours, though the timer runs at
02:00 IST when the app is idle anyway.

The copy is then **verified before it counts**: `PRAGMA quick_check`, plus a count
of snapshot days and candles. An empty database passes every structural check, so
the counts are the only version of "the backup worked" worth trusting. They appear
in the log line and in the weekly report.

Then gzip -9, then rotation: 14 dailies, plus the 1st-of-month copies kept for a
year. Dailies cover the ordinary case where something broke recently. The monthlies
cover what dailies cannot — corruption or a bad delete noticed weeks later, when
every surviving daily already contains the damage.

### Reporting

Failures go to Telegram, always. Successes go once a week (Sundays), because a
channel that only ever speaks on failure cannot be told apart from a broken one,
while a message every morning is noise that gets the channel muted — taking the
real alerts with it. `--report` forces a success message for a manual run.

There is also a `.last-success` marker in the backup directory, and
`systemctl list-timers` for the timer itself.

### Sizing

Measured on 134 MB of real data: the compacted copy was 126 MB and gzipped to
**17 MB, a 7.4x ratio**, in about 9 seconds. Growth is roughly 40 MB of database
per trading day, dominated by `instrument_snapshots` — the whole NFO+BFO master,
written once a day with two indexes on it — not by the candles.

That means each nightly copy grows with the database, and 26 retained full copies
of a multi-gigabyte database is tens of GB by year end. Three ways to handle it:

- lower `KITE_BACKUP_KEEP_DAYS` in the unit file;
- point `KITE_BACKUP_DIR` at a bigger or separate disk;
- or store the copies with `restic` or `borg`, which dedupe at block level, so N
  nightly copies of a mostly append-only database cost roughly one copy plus the
  daily deltas.

Check headroom with `df -h /var/backups/kite-algo`. The job warns through Telegram
below 2 GB free rather than waiting to fail.

### Restoring

```sh
gunzip -c /var/backups/kite-algo/trading-2026-08-19.db.gz > /tmp/restored.db
docker compose stop app
docker run --rm -v deploy_kite-data:/data -v /tmp:/in alpine:3.20   cp /in/restored.db /data/trading.db
docker compose start app
```

Stop the app first. Replacing the file under a running process leaves it holding a
deleted inode and writing to nothing. Confirm the volume name with
`docker volume ls | grep kite-data` — it is prefixed with the compose project name.

---

## Going live

Real orders need all of this, in order:

1. `mode: live` and `live_confirm: true` in `config.yaml`
2. `docker compose up -d app`
3. Open **Live**, type `I UNDERSTAND` and your password, arm

Only manual orders placed on the live desk reach the exchange. Strategies stay
simulated by construction — there is no config value that changes that.

---

## Two things this setup does not do

**Backups run nightly.** See the Backups section below. The database is the one
thing here that cannot be rebuilt: once a weekly contract expires, its history is
not purchasable from Kite at any price.

**An IP allowlist breaks when your address changes.** Residential ISPs rotate
addresses. When yours does you are locked out of your own trading UI, possibly
while holding a position. Keep SSH access to the box so you can edit
`deploy/.env` and run `docker compose up -d caddy`.
