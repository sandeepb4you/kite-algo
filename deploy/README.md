# Deploying kite-algo with Docker

Single box, single container, TLS and an IP allowlist in front. Everything here
assumes you already have a box with Docker and a domain you can point at it.

> **One instance. Always.** The database is SQLite: one file, one writer. A
> second container mounting the same volume corrupts it, and the corruption is
> not obvious — it surfaces as a read that quietly comes back wrong. Never
> `--scale app=2`, never run a second copy "just to test".

---

## 1. Prepare the files

```sh
cd deploy
cp .env.example        .env
cp config.example.yaml config.yaml
```

Edit `.env`:

```ini
SITE_ADDRESS=kite.yourdomain.com
ALLOWED_IPS=203.0.113.42            # space separated; CIDR ok
```

Edit `config.yaml` — the three lines that must agree with each other:

| Setting | Value |
|---|---|
| `web.public_url` | `https://kite.yourdomain.com` — same host as `SITE_ADDRESS` |
| `web.addr` | `0.0.0.0:8080` — inside the container only; not published to the host |
| `web.trust_proxy` | `true` — Caddy is in front |

`trust_proxy` is not optional here. The login lockout and the live-arming
lockout both key off the client address; without it every request appears to
come from Caddy and the throttle becomes global instead of per-attacker.

## 2. Create the secrets file

```sh
cp ../secrets.example.yaml secrets.yaml
$EDITOR secrets.yaml          # api_key + api_secret
chmod 600 secrets.yaml
```

Then set the operator password. It is an interactive prompt, so it needs a TTY:

```sh
docker compose run --rm -it \
  -v "$PWD/secrets.yaml:/secrets/secrets.yaml" \
  app -set-password
```

The application **refuses to start** on a non-loopback address with no password
set, so there is no way to skip this by accident.

## 3. Register the redirect URL with Zerodha

In <https://developers.kiteconnect.com/apps>, set your app's redirect URL to:

```
https://kite.yourdomain.com/kite/callback
```

Zerodha matches this **character for character** and allows no wildcards. A
trailing slash, `http` instead of `https`, or a different subdomain all fail —
and they fail at the end of the login round trip, which makes it look like the
login itself is broken.

## 4. DNS, then up

Point an `A` record at the box **before** starting: Caddy obtains the
certificate over HTTP-01, which needs port 80 reachable from the internet at
that moment. The allowlist does not apply to the ACME challenge path.

```sh
docker compose up -d --build
docker compose logs -f app
```

You are looking for:

```
web ui listening              addr=0.0.0.0:8080 public_url=https://kite.yourdomain.com
daily option capture scheduled run_at="15:40 IST"
```

## 5. Log in to Zerodha

Open `https://kite.yourdomain.com`, sign in with the operator password, then
**Connect** to Zerodha.

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
| Logs | `docker compose logs -f app` |
| Restart | `docker compose restart app` |
| Upgrade | `git pull && docker compose up -d --build app` |
| Capture a missed day | `docker compose exec app trading -config /etc/kite-algo/config.yaml -capture 2026-08-14` |
| Shell | `docker compose exec app sh` |

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
