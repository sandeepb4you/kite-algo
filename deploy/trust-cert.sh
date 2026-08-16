#!/usr/bin/env bash
#
# Extract Caddy's local CA root certificate.
#
# Without a domain there is no Let's Encrypt certificate, so Caddy signs with a
# CA of its own. Install this root once on each device you browse from and the
# trading UI becomes an ordinary padlocked HTTPS site.
#
# That is worth the five minutes. The alternative is clicking through a browser
# security warning every session — which is precisely the reflex you do not want
# to have trained the day a warning is telling you something true, and it also
# means you cannot tell a certificate swap from business as usual.
#
# Run on the server, then copy the file out.
#
set -euo pipefail

OUT="${1:-caddy-root.crt}"
ROOT_IN_CONTAINER=/data/caddy/pki/authorities/local/root.crt

if ! docker compose ps caddy --status running >/dev/null 2>&1; then
	echo "caddy is not running; start the stack first: docker compose up -d" >&2
	exit 1
fi

# Caddy generates the CA lazily, on first certificate issuance.
if ! docker compose exec -T caddy test -f "$ROOT_IN_CONTAINER"; then
	echo "no local CA yet — load the site once in a browser so Caddy issues," >&2
	echo "then run this again." >&2
	exit 1
fi

docker compose exec -T caddy cat "$ROOT_IN_CONTAINER" >"$OUT"
echo "wrote $OUT"

cat <<EOF

Copy it to each device and install it as a TRUSTED ROOT:

  from your laptop:
    scp root@<server-ip>:$PWD/$OUT .

  macOS:
    sudo security add-trusted-cert -d -r trustRoot \\
      -k /Library/Keychains/System.keychain $OUT

  Windows (PowerShell as Administrator):
    Import-Certificate -FilePath $OUT \\
      -CertStoreLocation Cert:\\LocalMachine\\Root

  Linux (Debian/Ubuntu):
    sudo cp $OUT /usr/local/share/ca-certificates/caddy-kite.crt
    sudo update-ca-certificates

  Android / iOS:
    mail or AirDrop the file to the device and open it; both will offer to
    install it as a certificate. On Android it lands under
    Settings > Security > Encryption & credentials > Install a certificate > CA.

Firefox keeps its own trust store and ignores the system one — import it under
Settings > Privacy & Security > Certificates > View Certificates > Authorities.

This root is specific to THIS server. Anyone holding its private key could
impersonate any site to a device that trusts it, so the key stays on the server
(in the caddy-data volume) and only the .crt above ever leaves.
EOF
