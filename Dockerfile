# Build with the pure-Go SQLite driver (modernc.org/sqlite) so CGO stays off and the result
# is a single static binary. That is what lets the runtime stage be scratch, and it means the
# same image runs on a Raspberry Pi, a NAS or a VPS without a libc to match.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/cloudstream-sync \
    ./cmd/server

FROM scratch

# The server talks HTTPS to nothing, but certificates are copied anyway so a future
# outbound call (or a health probe through a proxy) does not fail confusingly.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/cloudstream-sync /cloudstream-sync

# Non-root. scratch has no /etc/passwd, so the uid is given numerically.
USER 65532:65532

# The database lives here; mount a volume or the data disappears with the container.
VOLUME ["/data"]

# TCP for the HTTP API, UDP for LAN discovery (see cmd/server/discovery.go) - the app
# broadcasts a probe on this same port number to find the server without the user typing
# an IP address.
EXPOSE 9909
EXPOSE 9909/udp

ENV SYNC_ADDR=":9909" \
    SYNC_DB="/data/cloudstream-sync.db"

ENTRYPOINT ["/cloudstream-sync"]
