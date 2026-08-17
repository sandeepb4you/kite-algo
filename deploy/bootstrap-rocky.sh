#!/usr/bin/env bash
#
# Provision a fresh Rocky Linux 9 server (Utho or any RHEL-family host).
#
# Idempotent: safe to re-run. Installs Docker and leaves SELinux enforcing. It
# deliberately does NOT start anything — config and secrets have to be filled in
# by hand first, and a half-configured trading process starting on boot is not
# something to automate.
#
# NO HOST FIREWALL is configured here. Access control lives in two other places,
# and both have to be right:
#
#   - the provider's cloud firewall — 22/tcp and 443/tcp from your IP, nothing
#     else. This is now the only thing standing in front of SSH.
#   - ALLOWED_IPS in deploy/.env — Caddy answers 404 to any other address, so
#     the trading UI stays closed even if 443 is reachable from the internet.
#
# Usage, as root on the server:
#   ./bootstrap-rocky.sh
#
set -euo pipefail

APP_DIR=/opt/kite-algo

echo "==> Packages"
dnf -y -q install dnf-plugins-core curl git policycoreutils-python-utils

echo "==> Docker"
if ! command -v docker >/dev/null 2>&1; then
	# Docker publishes no Rocky repo; the CentOS one is what Rocky is built to
	# be compatible with and is what Docker's own docs point RHEL clones at.
	dnf -y -q config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
	dnf -y -q install docker-ce docker-ce-cli containerd.io \
		docker-buildx-plugin docker-compose-plugin
fi
systemctl enable --now docker

echo "==> SELinux"
# Left ENFORCING. The compose file marks its bind mounts :Z so Docker relabels
# them, which is the supported way to do this — turning SELinux off would be
# trading a real protection for five minutes of convenience on a box that holds
# broker credentials.
getenforce || true
setsebool -P container_manage_cgroup on 2>/dev/null || true

echo "==> Application directory"
mkdir -p "$APP_DIR"
chmod 750 "$APP_DIR"

cat <<EOF

==> Done. Docker installed, SELinux enforcing, no host firewall by design.

    Access control is entirely the cloud firewall's job now. Confirm in the
    provider console that 22/tcp and 443/tcp are open to your address ONLY —
    with no host firewall there is no second layer to catch a mistake there.

NOTHING IS RUNNING YET. Next, on this server:

  1. Get the source into $APP_DIR
       git clone https://github.com/sandeepb4you/kite-algo.git $APP_DIR
       cd $APP_DIR/deploy

  2. Fill in the three files
       mkdir -p conf secrets
       cp .env.example        .env                      # SITE_ADDRESS = this server's public IP
       cp config.example.yaml conf/config.yaml          # web.public_url = https://<that IP>
       cp ../secrets.example.yaml secrets/secrets.yaml  # Kite api_key + api_secret
       chown -R 10001:10001 secrets                     # REQUIRED — see below
       chmod 700 secrets && chmod 600 secrets/secrets.yaml

     conf/ and secrets/ are mounted as DIRECTORIES. A single-file bind mount is
     pinned to an inode at container creation, and editors save by writing a
     temp file and renaming over the original — so the host file changes while
     the container goes on reading the old one, through every kind of restart.

     The chown is not tidiness. The container runs as UID 10001, not root, and
     a bind mount carries host ownership through unchanged — so root-owned
     secrets crash-loop the app on
       error: read secrets /secrets/secrets.yaml: permission denied
     The directory needs it too: a file you cannot traverse to fails the same
     way. Set the owner rather than relaxing the mode to 644: root still reads
     it either way, and your api_secret does not become world-readable.

  3. Set the operator password (interactive)
       docker compose build app
       docker run --rm -it \\
         -e TRADING_SECRETS_PATH=/secrets/secrets.yaml \\
         -v "\$PWD/secrets:/secrets:Z" \\
         kite-algo:latest -set-password

     Plain 'docker run', not 'docker compose run': the compose service mounts
     secrets/ read-only and a -v on the same path does not override it, so
     the compose form prompts for the password and then dies on
       error: open /secrets/secrets.yaml: read-only file system
     The -e is needed because docker run does not inherit compose's
     environment:, and there is no -config on purpose — config.yaml binds
     0.0.0.0, which the app refuses while no password is set.

  4. Start
       docker compose up -d --build
       docker compose logs -f app

  5. Trust the certificate on your laptop and phone
       ./trust-cert.sh

  6. BEFORE relying on it, copy your existing database over — a fresh volume
     starts empty, and the captured option candles cannot be re-downloaded.

     On your workstation, with the local app STOPPED (a live SQLite file whose
     WAL you did not also copy is a torn database that reads fine at first):
       gzip -c data/trading.db | ssh root@<this-server> 'gunzip -c > /tmp/trading.db'

     Here — write straight into the named volume. Do NOT use
     'docker compose down' + 'docker compose cp': down removes the container,
     leaving nothing to copy into. This works whether or not it exists:
       cd $APP_DIR/deploy
       docker volume ls | grep kite-data      # confirm the name below

       docker compose stop app 2>/dev/null || true
       docker run --rm -v deploy_kite-data:/data -v /tmp:/src:ro alpine \\
         sh -c 'cp /src/trading.db /data/trading.db \\
                && chown 10001:10001 /data/trading.db && ls -l /data'
       docker compose up -d

     The chown is required, not tidiness: the app runs as UID 10001 and a
     root-owned file makes every write fail with "attempt to write a readonly
     database" — which sounds like a mount problem and is not one.

EOF
