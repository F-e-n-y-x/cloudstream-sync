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

This pulls `ghcr.io/f-e-n-y-x/cloudstream-sync:latest`, published automatically on every
push to master by `.github/workflows/docker-publish.yml`. **The first time that workflow
runs, go to the package's settings on GitHub and set its visibility to Public** - a package
pushed with the workflow's default token is created private, and a plain `docker pull` (or
Portainer without registry credentials configured) gets `denied: requested access to the
resource is denied` until that is changed. This only has to be done once.

Then create your first account. Registration is **closed by default** so that a stranger who
finds the URL cannot use your server as free storage. Open it just long enough to register:

```bash
# in docker-compose.yml set SYNC_OPEN_REGISTRATION: "true", then
docker compose up -d
curl -X POST http://localhost:9909/api/v1/account \
     -H 'Content-Type: application/json' \
     -d '{"deviceName":"Phone"}'
```

Save the `token` from the response — it is shown once and only its hash is stored. Then set
`SYNC_OPEN_REGISTRATION` back to `"false"` and `docker compose up -d` again.

Every other device joins by pairing, not by registering.

### With Portainer

`portainer-stack.yml` is the same service as `docker-compose.yml`, with two differences.
A named volume stands in for the `./data` bind mount, since Portainer's web editor has no
repository checkout to resolve a relative path against. And the port is a plain `9909`
rather than a `${SYNC_PORT:-9909}`-style variable: Portainer's variable substitution has
been seen to silently corrupt a value that starts with a bare colon (`SYNC_ADDR: ":9909"`
came back as `SYNC_ADDR: "9909"` - the server then fails to start, since that is not a
valid listen address) and to drop one of two otherwise identical-looking `ports:` lines.
To use a different port here, edit the literal `9909` directly in the three places it
appears in the file, rather than reintroducing a variable.

In Portainer: **Stacks → Add stack**, paste the contents of `portainer-stack.yml` (or point
it at this repository and that file, if deploying from a Git repo), then deploy. Toggle
`SYNC_OPEN_REGISTRATION` to `"true"` in the stack's environment variables just long enough
to create the first account (see above), then back to `"false"` and redeploy.

### Without Docker

```bash
go build -o cloudstream-sync ./cmd/server
./cloudstream-sync -db ./sync.db -addr :9909
```

### Configuration

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-addr` | `SYNC_ADDR` | `:9909` | Listen address (TCP for the API, UDP for discovery) |
| `-db` | `SYNC_DB` | `/data/cloudstream-sync.db` | SQLite database path |
| `-open-registration` | `SYNC_OPEN_REGISTRATION` | `false` | Allow account creation |
| `-healthcheck` | — | — | Probe a running server, exit 0 if healthy |

With Docker, change the port with a single `SYNC_PORT` variable rather than editing `-addr`/
`SYNC_ADDR` and the `ports:` mapping separately - `docker-compose.yml` derives both from it, so
they cannot drift apart and leave the container listening on a port Docker no longer forwards:

```bash
SYNC_PORT=7000 docker compose up -d
```

or in a `.env` file next to `docker-compose.yml`:

```
SYNC_PORT=7000
```

Changing it moves discovery (below) to the new port too, since both listen on the same one.

## Auto-detect on the local network

The app can find a server on the same LAN instead of the user typing an IP address: it
broadcasts a UDP packet containing the exact bytes `CLOUDSTREAM_SYNC_DISCOVER_V1` to the
subnet's broadcast address on port 9909 (or whatever `SYNC_PORT` was changed to), and this
server replies directly to the sender with `CLOUDSTREAM_SYNC_V1 <port>`.

This runs unauthenticated, deliberately - the same trust boundary as `/healthz`. A reply
only reveals that a cloudstream-sync server exists at that address; it carries no account
or device information, and redeeming a pairing code or key is still required to actually
join one. Docker needs the UDP port forwarded for this to work across a container boundary,
which both `docker-compose.yml` and `portainer-stack.yml` already do.

### Put it behind TLS

The server speaks plain HTTP and does not terminate TLS. Tokens are bearer credentials, so on
anything but a trusted LAN put it behind a reverse proxy with HTTPS (Caddy, nginx, Traefik) or
reach it over a private network such as Tailscale or WireGuard.

## Pairing a second device

Two ways in, pick whichever suits the moment:

**A one-time code** - the account's first device needs to be online to issue one:
1. Ask for a code: `POST /api/v1/pair`
2. Enter it on the new device: `POST /api/v1/pair/redeem`

Codes are four characters from an alphabet with no `I`, `O`, `0` or `1`, so they survive
being read off a TV screen and typed on a phone without much room for a transcription error.
They last ten minutes and are single use — short and typeable means guessable, and those two
limits are what make that acceptable.

**A persistent pairing key** - set once, works whenever, no device needs to be online:
1. Set it from an already-signed-in device: `POST /api/v1/pair/setup-key`
2. Any device can join with it, any time: `POST /api/v1/pair/setup-key/redeem`

Unlike a code, redeeming a key does not consume it - it keeps working until you change or
clear it (`DELETE /api/v1/pair/setup-key`). That persistence is also the tradeoff: choose
something you would not mind functioning as a second password to your account (at least 4
characters is enforced, but longer is safer given it never expires), and treat changing it
as how you revoke access once every device that should have it does.

## Live presence

Separate from record syncing, devices can report a short-lived "what am I doing right now"
status so the rest of your account can see it - for example, "Playing Some Show S01E02" while
a video is open on another device. This is a live indicator only: no remote control, no
handoff of playback state, just visibility.

- `POST /api/v1/presence` (authenticated, body `{"status": "..."}`) - set or replace this
  device's current status.
- `DELETE /api/v1/presence` (authenticated) - clear it, e.g. when playback stops.
- `GET /api/v1/presence` (authenticated) - list what every other device on the account is
  doing, excluding the caller's own device.

Status entries expire on their own: anything older than 90 seconds is left out of `GET`
results, so a device that stops reporting (closed app, lost network, crash) disappears from
other devices' view shortly after rather than sticking around stale. There is no need to call
`DELETE` for correctness, only to clear the status promptly instead of waiting out the
freshness window.

## API

All authenticated endpoints take `Authorization: Bearer <token>`.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | no | Liveness |
| `POST` | `/api/v1/account` | no | Create an account and its first device |
| `POST` | `/api/v1/pair` | yes | Issue a one-time pairing code |
| `POST` | `/api/v1/pair/redeem` | no | Redeem a pairing code, returns a token |
| `POST` | `/api/v1/pair/setup-key` | yes | Set/replace the persistent pairing key |
| `DELETE` | `/api/v1/pair/setup-key` | yes | Clear the persistent pairing key |
| `POST` | `/api/v1/pair/setup-key/redeem` | no | Join using the persistent pairing key |
| `GET` | `/api/v1/status` | yes | Account, device and current cursor |
| `GET` | `/api/v1/devices` | yes | List devices on the account |
| `DELETE` | `/api/v1/devices/{id}` | yes | Revoke a device |
| `GET` | `/api/v1/records` | yes | Pull changes |
| `POST` | `/api/v1/records` | yes | Push changes |
| `POST` | `/api/v1/presence` | yes | Set this device's live status |
| `DELETE` | `/api/v1/presence` | yes | Clear this device's live status |
| `GET` | `/api/v1/presence` | yes | See other devices' live status |

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
cmd/server        entry point, flags, graceful shutdown, healthcheck probe, LAN discovery
internal/api      HTTP handlers, auth middleware, routing
internal/store    SQLite schema, merge logic, pairing
```

## Licence

Same licence as CloudStream (GPL-3.0).
