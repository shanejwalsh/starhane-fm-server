# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make dev                              # go run cmd/main.go  (listens on :8000)
make build                            # go build -o starhane-fm-server cmd/main.go
go test ./...                         # all tests
go test ./logging -run TestParseLevel # a single test
go vet ./...
LOG_LEVEL=debug LOG_FORMAT=text make dev
```

Docker: `docker build -t starhane-fm-server . && docker run -p 8000:8000 starhane-fm-server`.

Note `make build` writes `./starhane-fm-server` at the repo root, and that path is
*not* covered by `.gitignore` (which ignores `bin`, `main`, `.env`) — don't commit it.

## Architecture

A stateless HTTP API — no database, no cache. Every request fans out to the iTunes
Search API and/or a podcast's RSS feed, maps the upstream shape to a local response
type, and returns it.

All upstream I/O lives in an **external** module,
[`github.com/shanejwalsh/itunes-xml-parser`](https://github.com/shanejwalsh/itunes-xml-parser)
(pinned in `go.mod`), split into `itunes` (Search API) and `feeds` (RSS parsing).
Nothing in this repo talks to iTunes directly, so changing how search or feed parsing
behaves usually means bumping that dependency's version, not editing code here.

Request flow:

- `cmd/main.go` builds the logger from env, then `cmd/api/api.go` wires everything:
  a `mux` router under `/api/v1`, wide-open CORS, and the podcast handler constructed
  with concrete `*itunes.ItunesApiServices` / `*feeds.RssFeedService` values (no
  interfaces, so handlers aren't unit-testable without a real network — the only
  tests today are in `logging/`).
- `logging.Middleware` wraps the **entire** stack including CORS and the router
  (deliberately, not `router.Use`) so unmatched routes and preflights are logged too.
- `service/podcast/routes.go` holds all three handlers. Route paths are declared as
  consts at the top of the file; add new routes there.
- `utils/mappers.go` is the boundary: upstream `itunes.Result` / `feeds.Episode`
  never escape into responses. Keep it that way — `types/` is the public API shape.

Two handler conventions to follow:

1. **Never use the package-level `slog` in a handler.** Get the request-scoped
   logger with `logging.FromContext(ctx)` — it carries the `request_id` that ties
   handler logs to the request-completed summary line.
2. Log level maps to response class: 4xx → `Warn`, 5xx → `Error`, success detail →
   `Debug`.

Handlers currently write errors as a bare JSON string (`utils.WriteJson(res, status,
err.Error())`), which leaks upstream error text to clients. `utils.WriteError` wraps
errors as `{"error": ...}` but is unused; prefer it for new handlers. `types.Episode`
is likewise dead code — `types.EpisodeResponse` is the live episode shape.

The port is hardcoded to `8000` in `cmd/main.go`; there is no `PORT` env var.

## Endpoints

`GET /api/v1/podcasts?searchTerm=…`, `GET /api/v1/podcasts/{podcastId}`, and
`GET /api/v1/podcasts/{podcastId}/episodes`. The episodes route does two upstream
calls: an iTunes lookup to resolve `FeedURL`, then a fetch/parse of that feed.
Lookups go through `Handler.lookupPodcast`, which treats `ResultCount != 1` as a
404. See `README.md` for full request/response payloads — keep it in sync when
endpoints or response types change.
