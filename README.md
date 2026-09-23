# starhane-fm-server

A Go HTTP API for searching podcasts (via the iTunes Search API) and serving
episode lists from a Postgres catalogue that a background crawler keeps fresh.

## How it works

The API used to download and parse a podcast's entire RSS feed on every request
for its episodes. It no longer does. Instead:

- Any podcast returned by a search or a lookup is **upserted into Postgres**
  along with its feed URL, and that feed is scheduled for crawling. Browsing the
  API is what fills the catalogue — nothing is seeded up front.
- A separate **crawler** service claims due feeds from Postgres and refreshes
  them: conditional GETs, per-host rate limiting, adaptive intervals.
- `GET /api/v1/podcasts/{id}/episodes` **reads from the database**. Only if a
  feed has never been crawled does it crawl once, synchronously, then serve.

This matters because iTunes rate-limits to roughly 20 requests per minute per IP
and every user shares this server's IP.

Audio is never downloaded, proxied or re-hosted. Only enclosure URLs are stored.

## Requirements

- Go 1.26+
- PostgreSQL 18 (to match production; see below)

## Local setup

### 1. Postgres

Production runs PostgreSQL 18, so development should too.

```bash
brew install postgresql@18
brew link postgresql@18   # make it the default psql/pg_ctl on your PATH
make db-create            # creates starhane_fm and starhane_fm_test
```

Postgres normally starts at login. `brew services` is unreliable on macOS here,
so the service is a LaunchAgent at
`~/Library/LaunchAgents/homebrew.mxcl.postgresql@18.plist`. It must set
`LC_ALL`, because launchd starts processes with no locale and Postgres 18
aborts startup without one:

```xml
<key>EnvironmentVariables</key>
<dict><key>LC_ALL</key><string>en_US.UTF-8</string></dict>
```

`make db-start` / `db-stop` / `db-status` drive it by hand when needed. They use
`PG_BIN` (default `/opt/homebrew/opt/postgresql@18/bin`) and `PGDATA` — override
those if your layout differs.

If something else is already listening on 5432, stop it or point `DATABASE_URL`
at another port.

### 2. Configuration

```bash
cp .env.example .env
```

The makefile loads `.env` automatically (`-include .env` / `export`). There is
deliberately **no dotenv library in the Go code**, so production reads real
environment variables and nothing tries to read a file that is not there.
`.env` is git-ignored; `.env.example` is committed.

### 3. Migrations

```bash
make migrate-up
```

### 4. Run it

```bash
make run-api        # :8000
make run-crawler    # in another terminal
```

## Make targets

| Target | What it does |
|---|---|
| `make run-api` / `make dev` | Run the API |
| `make run-crawler` | Run the crawler |
| `make build` | Build all three binaries into `bin/` |
| `make test` | `go test ./...` |
| `make check` | fmt, vet and test |
| `make db-start` / `db-stop` / `db-status` | Control the local Postgres |
| `make db-create` / `db-drop` | Create or drop both databases |
| `make migrate-up` | Apply pending migrations |
| `make migrate-down` | Roll back **one** migration |
| `make migrate-down-all` | Roll back everything |
| `make migrate-status` | Show applied version and available migrations |
| `make migrate-new name=add_something` | Create an up/down migration pair |
| `make docker-build` | Build the container image |

## Environment variables

Every setting comes from the environment. Nothing is hardcoded and no hostname
or SSL mode is assumed — it all comes from the connection string.

### Required

| Variable | Description |
|---|---|
| `DATABASE_URL` | Postgres connection string. Required by all three binaries. |

### API

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8000` | Port the API listens on |
| `ITUNES_TIMEOUT` | `10s` | Timeout for iTunes requests |
| `ITUNES_SEARCH_LIMIT` | unset | Results per search (1–200; iTunes defaults to 50) |
| `ITUNES_COUNTRY` | unset | Two-letter store code, e.g. `GB` |

### Logging (both services)

| Variable | Values | Default |
|---|---|---|
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `LOG_FORMAT` | `json`, `text` | `json` |

### Connection pool (both services)

Sized explicitly, because the API and the crawler share one database's
connection limit. The pool logs the server's `max_connections` at startup next
to its own size, so sizing can be checked from the logs.

| Variable | Default |
|---|---|
| `DB_MAX_CONNS` | `10` |
| `DB_MIN_CONNS` | `1` |

### Crawler

| Variable | Default | Description |
|---|---|---|
| `CRAWLER_WORKERS` | `8` | Concurrent feed fetches |
| `CRAWLER_BATCH_SIZE` | `20` | Feeds claimed per pass |
| `CRAWLER_POLL_INTERVAL` | `30s` | Wait between passes when nothing is due |
| `CRAWLER_HTTP_TIMEOUT` | `30s` | Per-request timeout |
| `CRAWLER_MAX_BODY_BYTES` | `20971520` | Response size cap (20 MiB) |
| `CRAWLER_HOST_RPS` | `1` | Requests per second per host |
| `CRAWLER_HOST_BURST` | `2` | Burst allowance per host |
| `CRAWLER_USER_AGENT` | `starhane-fm/1.0 (+https://github.com/shanejwalsh/starhane-fm-server)` | Sent on every feed request |
| `CRAWLER_MIN_INTERVAL` | `1h` | Floor for the adaptive interval |
| `CRAWLER_MAX_INTERVAL` | `24h` | Ceiling for the adaptive interval |
| `CRAWLER_MAX_FAILURES` | `10` | Consecutive failures before a feed is marked dead |
| `CRAWLER_DEAD_RECHECK` | `720h` | How often dead feeds are retried (30 days) |
| `CRAWLER_LEASE_DURATION` | `15m` | How far a claim pushes a feed's next check |
| `CRAWLER_SYNC_BUDGET` | `20s` | Cap on the API's first-request crawl |

### Tests

| Variable | Description |
|---|---|
| `TEST_DATABASE_URL` | Integration tests skip with a message when unset. **Each run drops and recreates its schema** — point it at a throwaway database. |

## The crawler

The crawler is a separate binary sharing packages and the database with the API.

- **Postgres is the queue.** Feeds are claimed with `SELECT ... FOR UPDATE SKIP
  LOCKED`, so several crawler processes can share one queue without
  coordinating. A claim also leases the feed forward, so a worker that dies
  mid-crawl releases its feeds rather than stranding them.
- **Bounded concurrency** via `errgroup` with `SetLimit`.
- **Cheap fetches.** Conditional GET replays the stored `ETag` and
  `Last-Modified`. Because many hosts ignore that and send a full body anyway,
  the body is also hashed and compared — that is what actually saves the parse.
  Bodies are capped with `io.LimitReader`; an oversized feed is a failure, not a
  truncated parse.
- **Per-host rate limiting**, since one publisher often serves many feeds.
- **Moves are honoured.** A 301, a 308 or an `<itunes:new-feed-url>` updates the
  stored URL. If the destination is already in the catalogue, the old row
  becomes a redirect stub instead of colliding.
- **Failures back off.** 410 marks a feed dead at once; repeated failures do so
  after a budget; both are then retried monthly. A 429 honours `Retry-After` and
  does not count towards that budget — being throttled is the host working as
  intended, not the feed being broken.
- **Adaptive scheduling.** A feed that changed is re-checked at its episode
  cadence (median gap, so one ancient back-catalogue entry cannot skew it);
  a quiet one backs off gradually and an erroring one faster, both clamped.
  Every next check carries jitter so feeds claimed together do not stay in
  lockstep.
- **Graceful shutdown** via `signal.NotifyContext`; the context reaches every
  fetch and every query.

Crawl outcomes are logged with feed id, host, status, outcome, episode count and
duration.

## Base URL

All routes are mounted under:

```
/api/v1/podcasts
```

e.g. `http://localhost:8000/api/v1/podcasts`

CORS is wide open (`AllowedOrigins: *`) so the API can be called directly from a browser.

## Endpoints

Error responses are currently a bare JSON string (e.g. `"expected 1 podcast,
found 0"`), not an object.

### `GET /api/v1/podcasts`

Search for podcasts via the iTunes Search API. Results are also stored in the
catalogue and their feeds scheduled for crawling.

Note this path redirects (`301`) to `/api/v1/podcasts/`; any HTTP client that
follows redirects handles it transparently.

**Query params**

| Param        | Type   | Required | Description                                    |
|--------------|--------|----------|-------------------------------------------------|
| `searchTerm` | string | no       | Search term to look up podcasts by. If omitted, an empty search term is sent to iTunes. |

**Example**

```
GET /api/v1/podcasts?searchTerm=hardcore+history
```

**Response** `200 OK` — array of `Podcast`:

```json
[
  {
    "id": "1234567",
    "title": "Hardcore History",
    "artwork_url_30": "https://...",
    "artwork_url_100": "https://...",
    "artwork_url_600": "https://...",
    "artist_name": "Dan Carlin",
    "genres": ["History"],
    "explicit": false
  }
]
```

**Errors**

- `500 Internal Server Error` — the upstream iTunes search request failed.

A failure to write to the catalogue does not fail the request.

---

### `GET /api/v1/podcasts/{podcastId}`

Look up a single podcast by its iTunes collection ID. The result is stored and
its feed scheduled for crawling.

**Path params**

| Param       | Type   | Required | Description                     |
|-------------|--------|----------|----------------------------------|
| `podcastId` | int    | yes      | The iTunes collection ID of the podcast. |

**Example**

```
GET /api/v1/podcasts/1234567
```

**Response** `200 OK` — a single `Podcast` (see shape above).

**Errors**

- `400 Bad Request` — `podcastId` isn't a valid integer.
- `404 Not Found` — no podcast (or more than one) found for the given ID.

---

### `GET /api/v1/podcasts/{podcastId}/episodes`

Serve a podcast's episodes from the catalogue.

If the podcast is not yet known, it is looked up on iTunes and stored. If its
feed has never been crawled, it is crawled once, synchronously, and then served.
Every request after that is a database read.

Episodes are returned in **feed order** — the order they appear in the RSS
document — and `pubDate` is the feed's own date string, unparsed, both matching
what this endpoint has always returned. Episodes a publisher has removed from
the feed stop being served.

**Path params**

| Param       | Type   | Required | Description                     |
|-------------|--------|----------|----------------------------------|
| `podcastId` | int    | yes      | The iTunes collection ID of the podcast. |

**Example**

```
GET /api/v1/podcasts/1234567/episodes
```

**Response** `200 OK` — array of `EpisodeResponse`:

```json
[
  {
    "id": "guid-value",
    "title": "Episode Title",
    "description": "Episode description",
    "audioUrl": "https://.../episode.mp3",
    "audioLength": 123456,
    "author": "Author Name",
    "pubDate": "Wed, 01 Jan 2025 00:00:00 +0000",
    "link": "https://...",
    "isExplicit": false,
    "duration": "01:02:03"
  }
]
```

`id` is the feed's `<guid>`. When a feed omits it, or repeats it across items, a
hash of the enclosure URL is used instead, so every episode has a stable id.

**Errors**

- `400 Bad Request` — `podcastId` isn't a valid integer.
- `404 Not Found` — no podcast (or more than one) found for the given ID.
- `500 Internal Server Error` — the feed could not be fetched or parsed, the
  podcast has no feed URL, or the database is unreachable.

## Logging

Structured logging via [`log/slog`](https://pkg.go.dev/log/slog). Every request
is logged once on completion with its method, path, status, bytes written and
duration, and is tagged with a `request_id` (taken from an incoming
`X-Request-ID` header, or generated, and echoed back in the response). Panics in
handlers are recovered and logged with a stack trace.

Handlers take the request-scoped logger from the context with
`logging.FromContext`, never the package-level `slog`, so their lines carry the
`request_id` that ties them to the request summary.

```bash
LOG_LEVEL=debug LOG_FORMAT=text make run-api
```

## Testing

```bash
make test                      # or: go test ./...
go test ./crawl -run TestParse # a single test
```

Unit tests cover the scheduling maths, guid fallback, date parsing and
redirect handling. Fetcher tests run against `httptest` servers covering 304s,
429s with `Retry-After` in both formats, permanent and temporary redirects,
oversized bodies and malformed XML. Handler tests use fakes, so no network.

Integration tests need `TEST_DATABASE_URL` and skip with an explanation when it
is unset. Each test binary drops and migrates **its own schema**, because
`go test ./...` runs packages in parallel and they would otherwise drop tables
underneath one another.

## Deployment (Railway)

The image is multi-stage and contains all three binaries (`/app/api`,
`/app/crawler`, `/app/migrate`) on a distroless non-root base with CA
certificates. Each service picks its binary via its start command.

| | API service | Crawler service |
|---|---|---|
| Start command | `/app/api` | `/app/crawler` |
| Pre-deploy command | `/app/migrate up` | — |
| Public domain | yes | **no** |
| App sleeping | may be enabled | **must be disabled** |
| `DATABASE_URL` | `${{Postgres.DATABASE_URL}}` | `${{Postgres.DATABASE_URL}}` |
| `LOG_FORMAT` / `LOG_LEVEL` | `json` / `info` | `json` / `info` |

Both services reference the Postgres service's **private** URL, so database
traffic stays on the internal network.

Migrations run as the API's pre-deploy command, never at startup: two services
racing to migrate the same database would be a problem.

A worker has no inbound HTTP, so Railway's app sleeping would stop the crawler
whenever it is idle. It must be disabled on that service.

Pool sizes must be set so that the API and crawler together stay well under the
database's `max_connections`. Both services log that value at startup.

## Project layout

```
cmd/
  api/            API entrypoint
  crawler/        crawler entrypoint
  migrate/        migration command (up / down / status / force)
config/           all environment parsing, in one place
crawl/            fetching, parsing, scheduling and the crawl worker pool
db/               connection pool, embedded migrations and the migration runner
  migrations/     SQL, compiled into the binaries via embed.FS
internal/testdb/  clean, migrated schema for integration tests
logging/          slog setup, request-logging middleware, context helpers
server/           router, CORS, middleware, route registration
service/
  podcast/        podcast route handlers
store/            every SQL query the application runs
types/            response types (Podcast, EpisodeResponse)
utils/            JSON helpers and mappers from upstream and stored types
```

Podcast search and lookups, and RSS parsing, are delegated to
[`github.com/shanejwalsh/itunes-xml-parser`](https://github.com/shanejwalsh/itunes-xml-parser).
The crawler owns its own HTTP requests and uses that library's `feeds.Parse` on
bodies it has already fetched, which is what makes conditional GET and body
hashing possible.
