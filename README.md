# starhane-fm-server

A Go HTTP API for searching podcasts (via the iTunes Search API) and fetching
episode lists (by parsing the podcast's RSS feed).

## Requirements

- Go 1.23+

## Running

```bash
make dev      # go run cmd/main.go
make build    # go build -o starhane-fm-server cmd/main.go
```

The server listens on port `8000` by default (`cmd/main.go`).

Docker:

```bash
docker build -t starhane-fm-server .
docker run -p 8000:8000 starhane-fm-server
```

## Logging

The server uses structured logging via the standard library's
[`log/slog`](https://pkg.go.dev/log/slog). Every request is logged once on
completion with its method, path, status, bytes written and duration, and is
tagged with a `request_id` (taken from an incoming `X-Request-ID` header, or
generated, and echoed back in the response). Panics in handlers are recovered
and logged with a stack trace.

Configure it with environment variables:

| Variable     | Values                           | Default |
|--------------|----------------------------------|---------|
| `LOG_LEVEL`  | `debug`, `info`, `warn`, `error` | `info`  |
| `LOG_FORMAT` | `json`, `text`                   | `json`  |

```bash
LOG_LEVEL=debug LOG_FORMAT=text make dev
```

## Base URL

All routes are mounted under:

```
/api/v1/podcasts
```

e.g. `http://localhost:8000/api/v1/podcasts`

CORS is wide open (`AllowedOrigins: *`) so the API can be called directly from a browser.

## Endpoints

### `GET /api/v1/podcasts`

Search for podcasts via the iTunes Search API.

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

---

### `GET /api/v1/podcasts/{podcastId}`

Look up a single podcast by its iTunes collection ID.

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

Fetch the episode list for a podcast by resolving its RSS feed URL from
iTunes and parsing the feed.

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

**Errors**

- `400 Bad Request` — `podcastId` isn't a valid integer.
- `500 Internal Server Error` — the RSS feed request/parse failed.
- `404 Not Found` — no podcast (or more than one) found for the given ID.

## Project layout

```
cmd/
  main.go        entrypoint, starts the API server on port 8000
  api/api.go      server setup: router, CORS, middleware, route registration
logging/          slog setup, request-logging middleware, context helpers
service/
  podcast/routes.go   podcast route handlers
types/            response/domain types (Podcast, Episode, EpisodeResponse)
utils/            JSON helpers and mappers from upstream itunes/feeds types to response types
```

Podcast search and lookups are delegated to
[`github.com/shanejwalsh/itunes-xml-parser`](https://github.com/shanejwalsh/itunes-xml-parser),
which wraps the iTunes Search API (`itunes` package) and RSS feed parsing
(`feeds` package).
