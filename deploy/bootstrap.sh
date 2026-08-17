#!/usr/bin/env bash
#
# Provision a fresh Ubuntu server (Utho or any other provider) to run kite-algo.
#
# Idempotent: safe to re-run. Installs Docker, sets up a host firewall as a
# second layer behind the provider's, and creates the directory the deployment
# lives in. It deliberately does NOT start anything — the config and secrets
# have to be filled in by hand first, and a half-configured trading process
# starting on boot is not a thing to automate.
#
# Usage, as root on the server:
#   ./bootstrap.sh <your-browsing-ip>
#
set -euo pipefail

ALLOW_IP="${1:-}"
if [[ -z "$ALLOW_IP" ]]; then
	echo "usage: $0 <your-browsing-ip>    # the address you will reach the UI from" >&2
	echo "  find it with:  curl -s https://api.ipify.org" >&2
	exit 1
fi

APP_DIR=/opt/kite-algo

echo "==> Packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl git ufw

echo "==> Docker"
if ! command -v docker >/dev/null 2>&1; then
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
		-o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
		>/etc/apt/sources.list.d/docker.list
	apt-get update -qq
	apt-get install -y -qq docker-ce docker-ce-cli containerd.io \
		docker-buildx-plugin docker-compose-plugin
fi
systemctl enable --now docker

echo "==> Host firewall (second layer, behind the provider's cloud firewall)"
# Two layers on purpose. The provider's firewall is configured in a console
# nobody looks at for months; this one lives next to the thing it protects and
# is visible in `ufw status` on any login. Either alone would do; both means a
# mistake in one is not an exposure.
#
# Port 80 is NOT opened. The certificate comes from Caddy's internal CA, so
# there is no ACME challenge to serve and nothing needs it.
ufw --force reset >/dev/null
ufw default deny incoming
ufw default allow outgoing
ufw allow from "$ALLOW_IP" to any port 22 proto tcp comment 'ssh from operator'
ufw allow from "$ALLOW_IP" to any port 443 proto tcp comment 'trading ui from operator'
ufw --force enable
ufw status verbose

echo "==> Application directory"
mkdir -p "$APP_DIR"
chmod 750 "$APP_DIR"

cat <<EOF

==> Done.

Docker is installed and the host firewall allows only $ALLOW_IP on 22 and 443.

NOTHING IS RUNNING YET. Next, on this server:

  1. Put the source in $APP_DIR
       git clone <your-repo> $APP_DIR
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

  5. Trust the certificate on your laptop and phone, so the UI is a normal
     padlocked site rather than one you click a warning past every session:
       ./trust-cert.sh

EOF
