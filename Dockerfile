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

# ca-certificates: every Kite API call is HTTPS and fails without them.
# tzdata: the Go binary embeds its own copy (time/tzdata), but having it here
# too makes the TZ env var work for log timestamps.
RUN apk add --no-cache ca-certificates tzdata

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
