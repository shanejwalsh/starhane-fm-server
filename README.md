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

- `404 Not Found` — no podcast (or more than one) found for the given ID, or `podcastId` isn't a valid integer.

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

- `500 Internal Server Error` — `podcastId` isn't a valid integer, or the RSS feed request/parse failed.
- `404 Not Found` — no podcast (or more than one) found for the given ID.

## Project layout

```
cmd/
  main.go        entrypoint, starts the API server on port 8000
  api/api.go      server setup: router, CORS, middleware, route registration
service/
  podcast/routes.go   podcast route handlers
types/            response/domain types (Podcast, Episode, EpisodeResponse)
utils/            JSON helpers and mappers from upstream itunes/feeds types to response types
```

Podcast search and lookups are delegated to
[`github.com/shanejwalsh/itunes-xml-parser`](https://github.com/shanejwalsh/itunes-xml-parser),
which wraps the iTunes Search API (`itunes` package) and RSS feed parsing
(`feeds` package).
