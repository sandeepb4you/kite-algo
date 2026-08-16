#!/usr/bin/env bash
#
# Provision a fresh Rocky Linux 9 server (Utho or any RHEL-family host).
#
# Idempotent: safe to re-run. Installs Docker, configures firewalld, and leaves
# SELinux enforcing. It deliberately does NOT start anything — config and
# secrets have to be filled in by hand first, and a half-configured trading
# process starting on boot is not something to automate.
#
# Usage, as root on the server:
#   ./bootstrap-rocky.sh <your-browsing-ip>
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
dnf -y -q install dnf-plugins-core curl git firewalld policycoreutils-python-utils

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

echo "==> firewalld"
systemctl enable --now firewalld

# SSH: an ordinary host service, so firewalld's zone rules do apply.
firewall-cmd --permanent --zone=public --remove-service=ssh >/dev/null 2>&1 || true
firewall-cmd --permanent --zone=public --remove-service=cockpit >/dev/null 2>&1 || true
firewall-cmd --permanent --zone=public --add-rich-rule="rule family=ipv4 source address=$ALLOW_IP service name=ssh accept" >/dev/null

# HTTPS is a DOCKER-PUBLISHED port, and that is a different problem.
#
# Docker's published ports are DNAT'd and traverse the FORWARD chain, while
# firewalld's zone rules act on INPUT. So a `firewall-cmd --add-port` rule does
# NOT filter a container's published port — the port stays open to the world and
# the firewall looks like it is configured. The chain Docker leaves for exactly
# this is DOCKER-USER, consulted before its own rules; a firewalld direct rule
# there is persistent across reboots and Docker restarts.
firewall-cmd --permanent --direct --add-rule ipv4 filter DOCKER-USER 0 \
	-p tcp --dport 443 '!' -s "$ALLOW_IP" -j DROP >/dev/null
firewall-cmd --reload

echo "==> Application directory"
mkdir -p "$APP_DIR"
chmod 750 "$APP_DIR"

echo
echo "==> firewalld state"
firewall-cmd --list-all --zone=public | sed 's/^/    /'
echo "    DOCKER-USER direct rules:"
firewall-cmd --direct --get-all-rules | sed 's/^/      /'

cat <<EOF

==> Done. Docker installed, SELinux enforcing, firewalld allowing only $ALLOW_IP.

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
