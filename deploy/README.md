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
cp .env.example        .env
cp config.example.yaml config.yaml
```

Edit `.env`:

```ini
SITE_ADDRESS=203.0.113.10           # the server's public IP, no scheme
ALLOWED_IPS=27.7.11.10              # where you browse from; CIDR ok
```

Edit `config.yaml` — the settings that must agree:

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
cp ../secrets.example.yaml secrets.yaml
$EDITOR secrets.yaml            # api_key + api_secret
chown 10001:10001 secrets.yaml  # NOT optional — see below
chmod 600 secrets.yaml
ls -ln secrets.yaml             # want: -rw------- 1 10001 10001

docker compose build app        # the next step needs the image to exist
docker run --rm -it \
  -e TRADING_SECRETS_PATH=/secrets/secrets.yaml \
  -v "$PWD/secrets.yaml:/secrets/secrets.yaml:Z" \
  kite-algo:latest -set-password
```

**Plain `docker run`, not `docker compose run`.** The compose service mounts
`secrets.yaml` read-only, and a `-v` on the same container path does not
override that — the compose form prompts you for a password, hashes it, and
*then* dies on `open /secrets/secrets.yaml: read-only file system`. Running the
image directly is what gets you a writable mount.

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

Set the owner rather than loosening the mode to `644`. Root still reads and
writes the file either way, `600` still keeps every other account on the box
out, and your `api_secret` does not become world-readable to get there. The same
applies if you ever replace this file: re-`chown` it, or the app stops on the
next restart.

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

`-config` is required here — `compose run` replaces the CMD, and without it the
capture scope silently falls back to defaults instead of your configured
underlyings, expiries and strike range.

## Going live

Real orders need all of this, in order:

1. `mode: live` and `live_confirm: true` in `config.yaml`
2. `docker compose up -d app`
3. Open **Live**, type `I UNDERSTAND` and your password, arm

Only manual orders placed on the live desk reach the exchange. Strategies stay
simulated by construction — there is no config value that changes that.

---

## Two things this setup does not do

**No backups.** `deploy/config.yaml` puts the database on the `kite-data`
volume, and nothing copies it anywhere else. The captured option candles are
the one thing here that cannot be rebuilt: once a weekly contract expires, its
history is not purchasable from Kite at any price. A nightly job would be:

```sh
docker compose exec -T app sh -c \
  'sqlite3 /data/trading.db ".backup /tmp/b.db"' && \
docker compose cp app:/tmp/b.db "./backup-$(date +%F).db"
```

(that needs `sqlite3` in the image, which is not currently installed — say the
word and I will add it and a timer.)

**An IP allowlist breaks when your address changes.** Residential ISPs rotate
addresses. When yours does you are locked out of your own trading UI, possibly
while holding a position. Keep SSH access to the box so you can edit
`deploy/.env` and run `docker compose up -d caddy`.
