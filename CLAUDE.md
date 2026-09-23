# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make run-api                          # go run ./cmd/api  (listens on :8000)
make run-crawler                      # go run ./cmd/crawler
make build                            # all three binaries into bin/
make check                            # gofmt, go vet, go test ./...
go test ./crawl -run TestParsePubDate # a single test
make db-start && make db-create       # local Postgres 18
make migrate-up                       # apply migrations
LOG_LEVEL=debug LOG_FORMAT=text make run-api
```

`make` loads `.env` (`-include .env` / `export`). There is no dotenv library in
the Go code, so production reads real env vars. `bin/`, `.env` and `go.work` are
git-ignored; `.dockerignore` keeps `go.work` out of the build context.

Integration tests need `TEST_DATABASE_URL` and skip with a message without it.
Each test binary drops and migrates **its own Postgres schema** (see
`internal/testdb`), because `go test ./...` runs packages in parallel.

## Architecture

Three binaries over one Postgres database:

- `cmd/api` — the HTTP API.
- `cmd/crawler` — refreshes feeds in the background.
- `cmd/migrate` — applies embedded migrations. **Never migrate at startup**;
  the API and crawler would race.

The API is no longer stateless. Every podcast it returns from an iTunes search
or lookup is upserted into Postgres with its feed URL, but the feed starts
**dormant** and is not crawled. Requesting a podcast's episodes activates its
feed — that is the only thing that does. `GET /api/v1/podcasts/{id}/episodes`
reads from the database; only a feed that has never been crawled is fetched
synchronously, once, via the same `crawl.Crawler` the crawler service uses.

Package map:

- `config` — all env parsing, one place. Add new settings here, not `os.Getenv`
  at the point of use.
- `db` — pgxpool construction plus the embedded migrations and their runner.
- `store` — every SQL query. Hand-written pgx with `db` struct tags and
  `RowToStructByNameLax`; no sqlc. A column with no matching struct field is an
  error, so column lists and structs move together.
- `crawl` — `fetch.go` (conditional GET, size cap, redirects, 429),
  `parse.go` (feed → episodes, guid fallback), `schedule.go` (interval maths,
  pure functions), `crawler.go` (`CrawlFeed`, the shared unit of work),
  `worker.go` (claim loop and pool).
- `server` — router, CORS, middleware, graceful shutdown.
- `service/podcast` — handlers, behind interfaces (`ItunesService`,
  `Catalogue`, `FeedCrawler`) so they are testable without a network.
- `utils/mappers.go` — the boundary. Upstream `itunes.Result` and stored
  `store.Episode` never escape into responses; `types/` is the public API shape.

Upstream I/O lives in
[`github.com/shanejwalsh/itunes-xml-parser`](https://github.com/shanejwalsh/itunes-xml-parser).
The crawler owns its own HTTP requests and calls that library's `feeds.Parse` on
bodies it already has — that separation is what makes conditional GET and body
hashing possible, so do not reintroduce `feeds.GetFeed` in the crawl path.

## Conventions

1. **Never use the package-level `slog` in a handler.** Get the request-scoped
   logger with `logging.FromContext(ctx)` — it carries the `request_id` that
   ties handler logs to the request-completed summary line.
2. Log level maps to response class: 4xx → `Warn`, 5xx → `Error`, success detail
   → `Debug`.
3. To report something on the request summary line, call `logging.Annotate` /
   `logging.AnnotateCache` rather than emitting a second line. The middleware
   collects them and writes one line per request.
4. `logging.Middleware` wraps the **entire** stack including CORS and the router
   (deliberately, not `router.Use`) so unmatched routes and preflights are
   logged too.
5. Route paths are consts at the top of `service/podcast/routes.go`.
6. Pass `context.Context` to every query and every outbound request.
7. Existing error paths write a bare JSON string
   (`utils.WriteJson(res, status, err.Error())`). That leaks upstream error text
   and `utils.WriteError` wraps errors as `{"error": ...}` instead — prefer it
   for new endpoints, but do not retrofit the three existing ones without
   deciding to change the public contract.
8. Never download, proxy or re-host audio. Only enclosure URLs are stored.

## Things that exist for a reason

- `episodes.pub_date_raw` — the API has always returned the feed's own
  unparsed date string. `pub_date` is the parsed value, used only for cadence.
- `episodes.position` — the episode list is in feed order, not date order.
- `episodes.last_crawl_seq` vs `feeds.crawl_seq` — `EpisodesByFeed` serves only
  the episodes the latest crawl saw, so a removed episode disappears while its
  row survives.
- Claiming feeds leases `next_check_at` forward, so a crashed worker's feeds
  return to the queue.
- `feeds.status` is the lifecycle: `dormant` (seeded, never requested) →
  `active` (requested) → `dead` (retried on `CRAWLER_DEAD_RECHECK`), plus
  `redirected` stubs. Only `active` and `dead` are ever claimed. Do not make
  `UpsertFeed` create active feeds — that is what bounds catalogue growth.
- `crawl.Crawler.apply` promotes a dormant feed to active before writing,
  because the API activates a feed and then hands the crawler the struct it read
  beforehand; writing that back would undo the activation.
- Episodes absent for `CRAWLER_EPISODE_GRACE_CRAWLS` crawls are deleted, but
  never when a parse produced zero episodes.
- Permanent redirects are deliberately **not** followed by the HTTP client; the
  new URL is recorded instead.

## Endpoints

`GET /api/v1/podcasts?searchTerm=…` (301s to the trailing-slash form),
`GET /api/v1/podcasts/{podcastId}`, and
`GET /api/v1/podcasts/{podcastId}/episodes`. Lookups go through
`Handler.lookupPodcast`, which treats `ResultCount != 1` as a 404. See
`README.md` for full request/response payloads — keep it in sync when endpoints
or response types change.

## Deployment

Railway, from the multi-stage `Dockerfile` that builds all three binaries into
one distroless image. Each service picks its binary via its start command;
`migrate up` runs as the API's pre-deploy command. The crawler service must have
app sleeping **disabled** — it has no inbound HTTP and would otherwise be
stopped whenever idle. Pool sizes are set so both services stay under the
database's `max_connections`, which is logged at startup.
