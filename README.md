# cloudstream-sync

CloudStream's self-hosted alternative to syncing through a third-party tracker account
(AniList, MAL, Simkl, Trakt). Those sync your watch *status* to that service. This syncs your
device's actual local app data directly between your own devices, through a server you run
yourself:

- Watch history and resume positions
- Library, bookmarks and subscriptions
- Search history
- Repositories (extensions then auto-install from them, matching per-device)
- A curated set of playback and display settings (picture/audio profile, preferred quality,
  dub/sub preference, and similar - deliberately not paths, hardware tuning or anything else
  that is right for one device and wrong for another)

No tracker account, no third party, no telemetry. Single static Go binary, SQLite storage,
one container - point it at your own server address from CloudStream's Settings and pair
your other devices to it.

Each category can be turned on or off per device, so a TV that should not carry your phone's
search history, or a tablet that only wants watch history, is one toggle away.

## Design

**The server is a dumb relay.** It never interprets what it stores. A record is an opaque
value under a `(category, key)` pair, and the server only decides which of two competing
writes wins and what a device has not yet seen. Every decision about *what* to sync, *when*,
and *how to apply it* lives in the app.

That is deliberate. It keeps the server small enough to audit, means app-side changes rarely
require a server upgrade, and makes the storage format the app's business rather than a
schema you have to migrate here.

**Conflicts resolve per key, last write wins.** Each record carries a client timestamp.
A newer write replaces an older one; an older write arriving late is ignored, which is the
case that otherwise loses data when a device syncs after being offline. Equal timestamps are
broken by device id, so two devices writing in the same millisecond converge instead of
flapping.

Because merging is per key, watching on the TV and then on the phone merges correctly rather
than one device's whole library overwriting the other's.

**Deletions are tombstones.** A delete travels like any other change, otherwise a device that
was offline would resurrect what another device removed.

## Running it

```bash
docker compose up -d
```

Then create your first account. Registration is **closed by default** so that a stranger who
finds the URL cannot use your server as free storage. Open it just long enough to register:

```bash
# in docker-compose.yml set SYNC_OPEN_REGISTRATION: "true", then
docker compose up -d
curl -X POST http://localhost:8080/api/v1/account \
     -H 'Content-Type: application/json' \
     -d '{"deviceName":"Phone"}'
```

Save the `token` from the response — it is shown once and only its hash is stored. Then set
`SYNC_OPEN_REGISTRATION` back to `"false"` and `docker compose up -d` again.

Every other device joins by pairing, not by registering.

### Without Docker

```bash
go build -o cloudstream-sync ./cmd/server
./cloudstream-sync -db ./sync.db -addr :8080
```

### Configuration

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-addr` | `SYNC_ADDR` | `:8080` | Listen address |
| `-db` | `SYNC_DB` | `/data/cloudstream-sync.db` | SQLite database path |
| `-open-registration` | `SYNC_OPEN_REGISTRATION` | `false` | Allow account creation |
| `-healthcheck` | — | — | Probe a running server, exit 0 if healthy |

With Docker, change the port with a single `SYNC_PORT` variable rather than editing `-addr`/
`SYNC_ADDR` and the `ports:` mapping separately - `docker-compose.yml` derives both from it, so
they cannot drift apart and leave the container listening on a port Docker no longer forwards:

```bash
SYNC_PORT=9090 docker compose up -d
```

or in a `.env` file next to `docker-compose.yml`:

```
SYNC_PORT=9090
```

### Put it behind TLS

The server speaks plain HTTP and does not terminate TLS. Tokens are bearer credentials, so on
anything but a trusted LAN put it behind a reverse proxy with HTTPS (Caddy, nginx, Traefik) or
reach it over a private network such as Tailscale or WireGuard.

## Pairing a second device

1. On a device that is already signed in, ask for a code: `POST /api/v1/pair`
2. Enter that code on the new device: `POST /api/v1/pair/redeem`

Codes are eight characters from an alphabet with no `I`, `O`, `0` or `1`, so they survive
being read off a TV screen. They last ten minutes and are single use — short and typeable
means guessable, and those two limits are what make that acceptable.

## API

All authenticated endpoints take `Authorization: Bearer <token>`.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | no | Liveness |
| `POST` | `/api/v1/account` | no | Create an account and its first device |
| `POST` | `/api/v1/pair` | yes | Issue a pairing code |
| `POST` | `/api/v1/pair/redeem` | no | Redeem a code, returns a token |
| `GET` | `/api/v1/status` | yes | Account, device and current cursor |
| `GET` | `/api/v1/devices` | yes | List devices on the account |
| `DELETE` | `/api/v1/devices/{id}` | yes | Revoke a device |
| `GET` | `/api/v1/records` | yes | Pull changes |
| `POST` | `/api/v1/records` | yes | Push changes |

### Pulling changes

```
GET /api/v1/records?since=<cursor>&category=resume&category=bookmarks&limit=500
```

`since` is the cursor from the previous pull; `0` means everything. `category` may repeat and
filters to the kinds of data this device syncs — that is how a per-device "sync watch history
but not extensions" choice is honoured. `hasMore` means page again immediately rather than
waiting for the next sync.

```json
{
  "records": [
    {
      "category": "resume",
      "key": "show/1",
      "value": "{\"pos\":120}",
      "updatedAt": 1731000000000,
      "deleted": false,
      "seq": 42
    }
  ],
  "cursor": 42,
  "hasMore": false
}
```

### Pushing changes

```json
POST /api/v1/records
{
  "records": [
    { "category": "resume", "key": "show/1", "value": "{\"pos\":120}", "updatedAt": 1731000000000 },
    { "category": "bookmarks", "key": "b/9", "value": "", "updatedAt": 1731000000001, "deleted": true }
  ]
}
```

`updatedAt` is Unix milliseconds and is what decides conflicts, so it must be when the change
actually happened, not when it was uploaded. `value` is whatever the app wants; the server
never parses it.

## Security

- Tokens are 32 random bytes; only a SHA-256 hash is stored, so a database leak does not hand
  over working credentials.
- Revoking a device invalidates its token immediately.
- Every query is scoped by account, so one account cannot read or modify another's data. There
  are tests for exactly this.
- An unknown token and a revoked token return the same error, so a caller cannot probe for
  which tokens once existed.
- Registration is closed by default.
- Request bodies are capped at 8 MiB.

## Development

```bash
go test ./...      # store and API tests, including the conflict-resolution cases
go vet ./...
gofmt -l .
```

The tests worth reading first are `TestLastWriteWins` and
`TestEqualTimestampsResolveDeterministically` in `internal/store`: they pin down the merge
behaviour everything else depends on.

## Layout

```
cmd/server        entry point, flags, graceful shutdown, healthcheck probe
internal/api      HTTP handlers, auth middleware, routing
internal/store    SQLite schema, merge logic, pairing
```

## Licence

Same licence as CloudStream (GPL-3.0).
