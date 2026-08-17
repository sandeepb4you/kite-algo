# Build.
#
# CGO stays off: the SQLite driver (modernc.org/sqlite) is pure Go, which is why
# this image needs no C toolchain and no libsqlite at runtime.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Trimmed and stripped: this binary is copied into the runtime image and the
# debug tables are dead weight there.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/trading ./cmd/trading


# Runtime.
#
# Alpine rather than scratch/distroless on purpose. This is a self-hosted box
# and the operator has to exec in at least once, to run `trading -set-password`
# — an interactive prompt. A runtime with no shell turns that into an
# awkward one-off container invocation for no security gain worth the trouble,
# given the process is not internet-facing.
FROM alpine:3.20

# No `apk add` here, deliberately.
#
# ca-certificates and tzdata used to be installed from the Alpine mirror, which
# made every rebuild depend on dl-cdn.alpinelinux.org being reachable. That is a
# CDN which fails intermittently — "temporary error (try again later)" is its
# own wording — and it publishes AAAA records, so a host with a default IPv6
# route but no working IPv6 path stalls on it too. Either way the build died on
# a step that needs no network at all.
#
# ca-certificates: still required — every Kite API call is HTTPS and fails
# without a trust store. Copied from the build stage, which demonstrably has one:
# `go mod download` above talks to proxy.golang.org over HTTPS and could not
# have succeeded otherwise.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# tzdata: not installed, and not needed. internal/app/session.go imports
# _ "time/tzdata", so the binary carries the whole database and the TZ setting
# below resolves against that embedded copy rather than /usr/share/zoneinfo.
#
# What this costs: `date` inside `docker compose exec app sh` reports UTC,
# because busybox has no embedded database to fall back on. Application log
# timestamps are unaffected — those come from the Go program, in IST.

# Runs as a non-root user that owns only its data directory.
RUN adduser -D -u 10001 -h /home/trading trading \
 && mkdir -p /data \
 && chown -R trading:trading /data

COPY --from=build /out/trading /usr/local/bin/trading

USER trading
WORKDIR /home/trading

# The database lives here and must be a mounted volume. Losing it loses every
# expired contract's captured history, which cannot be downloaded again.
VOLUME ["/data"]

EXPOSE 8080

# Logs in IST, matching every timestamp the application itself prints.
ENV TZ=Asia/Kolkata

# Config is mounted read-only at /etc/kite-algo/config.yaml.
ENTRYPOINT ["/usr/local/bin/trading"]
CMD ["-config", "/etc/kite-algo/config.yaml"]
