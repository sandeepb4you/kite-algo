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
       cp .env.example        .env              # SITE_ADDRESS = this server's public IP
       cp config.example.yaml config.yaml       # web.public_url = https://<that IP>
       cp ../secrets.example.yaml secrets.yaml  # Kite api_key + api_secret
       chmod 600 secrets.yaml

  3. Set the operator password (interactive)
       docker compose run --rm -it \\
         -v "\$PWD/secrets.yaml:/secrets/secrets.yaml:Z" app -set-password

  4. Start
       docker compose up -d --build
       docker compose logs -f app

  5. Trust the certificate on your laptop and phone
       ./trust-cert.sh

  6. BEFORE relying on it, copy your existing database over — a fresh volume
     starts empty, and the captured option candles cannot be re-downloaded:
       # on your workstation, with the local app STOPPED:
       scp data/trading.db root@<this-server>:/tmp/trading.db
       # here:
       docker compose down            # note: NO -v, that would delete the volume
       docker compose cp /tmp/trading.db app:/data/trading.db
       docker compose up -d

EOF
