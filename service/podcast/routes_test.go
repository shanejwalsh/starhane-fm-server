package podcast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"

	"github.com/shanejwalsh/starhane-fm-server/crawl"
	"github.com/shanejwalsh/starhane-fm-server/logging"
	"github.com/shanejwalsh/starhane-fm-server/store"
	"github.com/shanejwalsh/starhane-fm-server/types"
)

// --- fakes -------------------------------------------------------------------

type fakeItunes struct {
	searchCalls int
	lookupCalls int

	response itunes.SearchResponse
	err      error
}

func (f *fakeItunes) SearchWithContext(context.Context, itunes.SearchParams) (itunes.SearchResponse, error) {
	f.searchCalls++
	return f.response, f.err
}

func (f *fakeItunes) FindByIdWithContext(context.Context, int) (itunes.SearchResponse, error) {
	f.lookupCalls++
	return f.response, f.err
}

type fakeCatalogue struct {
	upserted   []store.PodcastUpsert
	upsertErr  error
	podcast    store.Podcast
	feed       *store.Feed
	lookupErr  error
	episodes   []store.Episode
	episodeErr error

	requested    []int64
	requestedErr error
	activated    bool
}

func (f *fakeCatalogue) UpsertPodcasts(_ context.Context, podcasts []store.PodcastUpsert) error {
	f.upserted = append(f.upserted, podcasts...)
	return f.upsertErr
}

func (f *fakeCatalogue) UpsertPodcastWithFeed(_ context.Context, p store.PodcastUpsert) (store.Podcast, *store.Feed, error) {
	f.upserted = append(f.upserted, p)
	if f.upsertErr != nil {
		return store.Podcast{}, nil, f.upsertErr
	}
	if p.FeedURL == "" {
		return f.podcast, nil, nil
	}
	return f.podcast, f.feed, nil
}

func (f *fakeCatalogue) PodcastWithFeedByItunesID(context.Context, int64) (store.Podcast, *store.Feed, error) {
	return f.podcast, f.feed, f.lookupErr
}

func (f *fakeCatalogue) EpisodesByFeed(context.Context, int64) ([]store.Episode, error) {
	return f.episodes, f.episodeErr
}

func (f *fakeCatalogue) MarkFeedRequested(_ context.Context, feedID int64, _ time.Duration) (bool, error) {
	f.requested = append(f.requested, feedID)
	return f.activated, f.requestedErr
}

type fakeCrawler struct {
	calls  int
	report crawl.Report
	err    error
}

func (f *fakeCrawler) CrawlFeed(context.Context, store.Feed) (crawl.Report, error) {
	f.calls++
	return f.report, f.err
}

// --- helpers -----------------------------------------------------------------

func newTestHandler(ias ItunesService, catalogue Catalogue, crawler FeedCrawler) http.Handler {
	handler := NewHandler(ias, catalogue, crawler, Options{SyncCrawlBudget: 5 * time.Second})

	router := mux.NewRouter()
	sub := router.PathPrefix("/api/v1").Subrouter()
	sub.StrictSlash(true)
	handler.RegisterRoutes(sub)
	return router
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	// Handlers read the request-scoped logger from the context; a discarding
	// one keeps test output clean.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req = req.WithContext(logging.WithContext(req.Context(), logger))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func sampleResult() itunes.Result {
	return itunes.Result{
		CollectionID:           1234567,
		CollectionName:         "Test Podcast",
		ArtistName:             "Test Artist",
		FeedURL:                "https://example.com/feed.xml",
		ArtworkURL600:          "https://example.com/600.jpg",
		CollectionExplicitness: "notExplicit",
		Genres:                 []string{"Technology"},
	}
}

func sampleFeed(crawled bool) *store.Feed {
	feed := &store.Feed{ID: 7, URL: "https://example.com/feed.xml", Status: store.StatusActive}
	if crawled {
		now := time.Now()
		feed.LastSuccessAt = &now
	}
	return feed
}

// --- tests -------------------------------------------------------------------

func TestGetPodcastsStoresWhatItReturns(t *testing.T) {
	ias := &fakeItunes{response: itunes.SearchResponse{ResultCount: 1, Results: []itunes.Result{sampleResult()}}}
	catalogue := &fakeCatalogue{}

	rec := get(t, newTestHandler(ias, catalogue, &fakeCrawler{}), "/api/v1/podcasts/?searchTerm=test")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got []types.Podcast
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v (body: %s)", err, rec.Body)
	}
	if len(got) != 1 || got[0].Id != "1234567" || got[0].Title != "Test Podcast" {
		t.Errorf("response = %+v, want the mapped podcast", got)
	}

	// The lazy catalogue: browsing is what fills it.
	if len(catalogue.upserted) != 1 {
		t.Fatalf("upserted %d podcasts, want 1", len(catalogue.upserted))
	}
	if catalogue.upserted[0].FeedURL != "https://example.com/feed.xml" {
		t.Errorf("stored feed url = %q, want the one from iTunes", catalogue.upserted[0].FeedURL)
	}
}

func TestGetPodcastsSurvivesACatalogueFailure(t *testing.T) {
	ias := &fakeItunes{response: itunes.SearchResponse{ResultCount: 1, Results: []itunes.Result{sampleResult()}}}
	catalogue := &fakeCatalogue{upsertErr: errors.New("database is down")}

	rec := get(t, newTestHandler(ias, catalogue, &fakeCrawler{}), "/api/v1/podcasts/?searchTerm=test")

	// The caller got their answer from iTunes, so a catalogue write failing
	// must not turn a good response into an error.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 despite the catalogue failing", rec.Code)
	}
}

func TestGetPodcastsReportsUpstreamFailure(t *testing.T) {
	ias := &fakeItunes{err: errors.New("itunes unreachable")}

	rec := get(t, newTestHandler(ias, &fakeCatalogue{}, &fakeCrawler{}), "/api/v1/podcasts/?searchTerm=test")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestGetPodcastStatusCodes(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		response itunes.SearchResponse
		err      error
		want     int
	}{
		{
			name: "valid",
			path: "/api/v1/podcasts/1234567",
			response: itunes.SearchResponse{
				ResultCount: 1, Results: []itunes.Result{sampleResult()},
			},
			want: http.StatusOK,
		},
		{
			name: "not an integer",
			path: "/api/v1/podcasts/not-a-number",
			want: http.StatusBadRequest,
		},
		{
			name:     "no results",
			path:     "/api/v1/podcasts/999",
			response: itunes.SearchResponse{ResultCount: 0},
			want:     http.StatusNotFound,
		},
		{
			name: "upstream error",
			path: "/api/v1/podcasts/999",
			err:  errors.New("itunes unreachable"),
			want: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ias := &fakeItunes{response: tc.response, err: tc.err}
			rec := get(t, newTestHandler(ias, &fakeCatalogue{}, &fakeCrawler{}), tc.path)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestGetEpisodesServesFromCatalogueWithoutCallingItunes(t *testing.T) {
	ias := &fakeItunes{}
	catalogue := &fakeCatalogue{
		podcast: store.Podcast{ID: 1},
		feed:    sampleFeed(true),
		episodes: []store.Episode{
			{
				Guid: "ep1", Title: "One", Description: "First",
				AudioURL: "https://example.com/1.mp3", AudioLength: 123,
				PubDateRaw: "Wed, 01 Jan 2025 00:00:00 +0000",
				Duration:   "01:02:03", Explicit: true, Link: "https://example.com/1",
				Author: "Test Artist",
			},
			{Guid: "ep2", Title: "Two"},
		},
	}
	crawler := &fakeCrawler{}

	rec := get(t, newTestHandler(ias, catalogue, crawler), "/api/v1/podcasts/1234567/episodes")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}

	// This is the point of phase 1: no iTunes call and no feed download.
	if ias.lookupCalls != 0 {
		t.Errorf("iTunes was called %d times, want 0 for a catalogued podcast", ias.lookupCalls)
	}
	if crawler.calls != 0 {
		t.Errorf("crawled %d times, want 0 for an already-crawled feed", crawler.calls)
	}

	var got []types.EpisodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d episodes, want 2", len(got))
	}

	want := types.EpisodeResponse{
		ID: "ep1", Title: "One", Description: "First",
		AudioURL: "https://example.com/1.mp3", AudioLength: 123,
		Author: "Test Artist", PubDate: "Wed, 01 Jan 2025 00:00:00 +0000",
		Link: "https://example.com/1", IsExplicit: true, Duration: "01:02:03",
	}
	if got[0] != want {
		t.Errorf("episode = %+v,\nwant        %+v", got[0], want)
	}
}

func TestGetEpisodesCrawlsAColdFeedOnce(t *testing.T) {
	catalogue := &fakeCatalogue{podcast: store.Podcast{ID: 1}, feed: sampleFeed(false)}
	crawler := &fakeCrawler{}

	rec := get(t, newTestHandler(&fakeItunes{}, catalogue, crawler), "/api/v1/podcasts/1234567/episodes")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	if crawler.calls != 1 {
		t.Errorf("crawled %d times, want exactly 1 for a feed never crawled before", crawler.calls)
	}
}

func TestGetEpisodesFallsBackToItunesForAnUnknownPodcast(t *testing.T) {
	ias := &fakeItunes{response: itunes.SearchResponse{ResultCount: 1, Results: []itunes.Result{sampleResult()}}}
	catalogue := &fakeCatalogue{lookupErr: pgx.ErrNoRows, feed: sampleFeed(true)}

	rec := get(t, newTestHandler(ias, catalogue, &fakeCrawler{}), "/api/v1/podcasts/1234567/episodes")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	if ias.lookupCalls != 1 {
		t.Errorf("iTunes was called %d times, want 1 for a podcast not in the catalogue", ias.lookupCalls)
	}
	if len(catalogue.upserted) != 1 {
		t.Errorf("upserted %d podcasts, want the newly discovered one stored", len(catalogue.upserted))
	}
}

func TestGetEpisodesStatusCodes(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		ias       *fakeItunes
		catalogue *fakeCatalogue
		crawler   *fakeCrawler
		want      int
	}{
		{
			name:      "not an integer",
			path:      "/api/v1/podcasts/abc/episodes",
			ias:       &fakeItunes{},
			catalogue: &fakeCatalogue{},
			crawler:   &fakeCrawler{},
			want:      http.StatusBadRequest,
		},
		{
			name:      "unknown podcast",
			path:      "/api/v1/podcasts/999/episodes",
			ias:       &fakeItunes{response: itunes.SearchResponse{ResultCount: 0}},
			catalogue: &fakeCatalogue{lookupErr: pgx.ErrNoRows},
			crawler:   &fakeCrawler{},
			want:      http.StatusNotFound,
		},
		{
			name: "podcast with no feed url",
			path: "/api/v1/podcasts/999/episodes",
			ias: &fakeItunes{response: itunes.SearchResponse{
				ResultCount: 1,
				Results:     []itunes.Result{{CollectionID: 999, CollectionName: "No feed"}},
			}},
			catalogue: &fakeCatalogue{lookupErr: pgx.ErrNoRows},
			crawler:   &fakeCrawler{},
			want:      http.StatusInternalServerError,
		},
		{
			name:      "first crawl fails",
			path:      "/api/v1/podcasts/1234567/episodes",
			ias:       &fakeItunes{},
			catalogue: &fakeCatalogue{feed: sampleFeed(false)},
			crawler:   &fakeCrawler{err: errors.New("feed unreachable")},
			want:      http.StatusInternalServerError,
		},
		{
			name:      "reading episodes fails",
			path:      "/api/v1/podcasts/1234567/episodes",
			ias:       &fakeItunes{},
			catalogue: &fakeCatalogue{feed: sampleFeed(true), episodeErr: errors.New("database is down")},
			crawler:   &fakeCrawler{},
			want:      http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, newTestHandler(tc.ias, tc.catalogue, tc.crawler), tc.path)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestGetEpisodesReturnsAnEmptyArrayNotNull(t *testing.T) {
	catalogue := &fakeCatalogue{feed: sampleFeed(true), episodes: nil}

	rec := get(t, newTestHandler(&fakeItunes{}, catalogue, &fakeCrawler{}), "/api/v1/podcasts/1234567/episodes")

	// A feed with no episodes must serialise as [], not null: clients iterate
	// the response.
	if body := rec.Body.String(); body != "[]\n" {
		t.Errorf("body = %q, want %q", body, "[]\n")
	}
}

// The search route is registered under a path prefix, so mux's StrictSlash
// redirects the bare path to the trailing-slash form. This has always been the
// behaviour; the test exists so a routing change cannot alter it silently.
func TestSearchPathRedirectsToTrailingSlash(t *testing.T) {
	handler := newTestHandler(&fakeItunes{}, &fakeCatalogue{}, &fakeCrawler{})

	rec := get(t, handler, "/api/v1/podcasts?searchTerm=test")

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if location := rec.Header().Get("Location"); location != "/api/v1/podcasts/?searchTerm=test" {
		t.Errorf("Location = %q, want the trailing-slash form", location)
	}
}

func TestIsPodcast(t *testing.T) {
	cases := map[string]struct {
		result itunes.Result
		want   bool
	}{
		"podcast from search": {
			itunes.Result{CollectionID: 1, Kind: "podcast", FeedURL: "https://example.com/f.xml"}, true,
		},
		"podcast from lookup": {
			itunes.Result{CollectionID: 1, WrapperType: "track", Kind: "podcast"}, true,
		},
		"collection with a feed but no kind": {
			itunes.Result{CollectionID: 1, FeedURL: "https://example.com/f.xml"}, true,
		},
		// An artist ID resolves fine but carries no collectionId and no feed.
		// Storing it would key a row on 0, which every other non-podcast would
		// then collide with.
		"artist": {
			itunes.Result{WrapperType: "artist", ArtistName: "Someone"}, false,
		},
		"album": {
			itunes.Result{CollectionID: 5, WrapperType: "collection", Kind: "album"}, false,
		},
	}

	for name, tc := range cases {
		if got := isPodcast(&tc.result); got != tc.want {
			t.Errorf("%s: isPodcast = %v, want %v", name, got, tc.want)
		}
	}
}

func TestNonPodcastIdIsNotStored(t *testing.T) {
	// iTunes returns one result, but it is an artist rather than a podcast.
	ias := &fakeItunes{response: itunes.SearchResponse{
		ResultCount: 1,
		Results:     []itunes.Result{{WrapperType: "artist", ArtistName: "Someone"}},
	}}
	catalogue := &fakeCatalogue{lookupErr: pgx.ErrNoRows}

	rec := get(t, newTestHandler(ias, catalogue, &fakeCrawler{}), "/api/v1/podcasts/1305204570/episodes")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if len(catalogue.upserted) != 0 {
		t.Errorf("stored %+v, want nothing — an artist is not a podcast", catalogue.upserted)
	}
}

func TestSearchResultsThatAreNotPodcastsAreNotStored(t *testing.T) {
	ias := &fakeItunes{response: itunes.SearchResponse{
		ResultCount: 2,
		Results:     []itunes.Result{sampleResult(), {WrapperType: "artist", ArtistName: "Someone"}},
	}}
	catalogue := &fakeCatalogue{}

	rec := get(t, newTestHandler(ias, catalogue, &fakeCrawler{}), "/api/v1/podcasts/?searchTerm=test")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(catalogue.upserted) != 1 {
		t.Fatalf("stored %d rows, want only the real podcast", len(catalogue.upserted))
	}
	if catalogue.upserted[0].ItunesID != 1234567 {
		t.Errorf("stored itunes id %d, want 1234567", catalogue.upserted[0].ItunesID)
	}
}

// annotatedGet drives a request and returns the attributes the handler reported
// for the summary line.
func annotatedGet(t *testing.T, handler http.Handler, path string) map[string]any {
	t.Helper()

	var buf bytes.Buffer
	logger := logging.New(&buf, logging.Config{Level: "debug", Format: "json"})
	wrapped := logging.Middleware(logger)(handler)

	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var summary map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatalf("decoding summary line: %v\n%s", err, buf.String())
	}
	return summary
}

func TestEpisodesReportsCacheOutcome(t *testing.T) {
	cases := []struct {
		name       string
		ias        *fakeItunes
		catalogue  *fakeCatalogue
		crawler    *fakeCrawler
		wantCache  string
		wantReason string
	}{
		{
			name:      "everything already catalogued",
			ias:       &fakeItunes{},
			catalogue: &fakeCatalogue{feed: sampleFeed(true), episodes: []store.Episode{{Guid: "a"}}},
			crawler:   &fakeCrawler{},
			wantCache: "hit",
		},
		{
			name: "podcast not known yet",
			ias: &fakeItunes{response: itunes.SearchResponse{
				ResultCount: 1, Results: []itunes.Result{sampleResult()},
			}},
			catalogue:  &fakeCatalogue{lookupErr: pgx.ErrNoRows, feed: sampleFeed(true)},
			crawler:    &fakeCrawler{},
			wantCache:  "miss",
			wantReason: "unknown_podcast",
		},
		{
			name:       "known podcast, feed never crawled",
			ias:        &fakeItunes{},
			catalogue:  &fakeCatalogue{feed: sampleFeed(false)},
			crawler:    &fakeCrawler{},
			wantCache:  "miss",
			wantReason: "uncrawled_feed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler(tc.ias, tc.catalogue, tc.crawler)
			summary := annotatedGet(t, handler, "/api/v1/podcasts/1234567/episodes")

			if summary["cache"] != tc.wantCache {
				t.Errorf("cache = %v, want %v", summary["cache"], tc.wantCache)
			}
			if tc.wantReason == "" {
				if _, ok := summary["miss_reason"]; ok {
					t.Errorf("a hit should carry no miss_reason, got %v", summary["miss_reason"])
				}
			} else if summary["miss_reason"] != tc.wantReason {
				t.Errorf("miss_reason = %v, want %v", summary["miss_reason"], tc.wantReason)
			}
		})
	}
}

func TestSearchReportsCacheBypass(t *testing.T) {
	ias := &fakeItunes{response: itunes.SearchResponse{ResultCount: 1, Results: []itunes.Result{sampleResult()}}}
	handler := newTestHandler(ias, &fakeCatalogue{}, &fakeCrawler{})

	summary := annotatedGet(t, handler, "/api/v1/podcasts/?searchTerm=test")

	// Search has no cache to hit: iTunes response caching is out of scope for
	// this phase, and the log should say so rather than imply a miss.
	if summary["cache"] != "bypass" {
		t.Errorf("cache = %v, want bypass", summary["cache"])
	}
}

func TestGetEpisodesActivatesTheFeed(t *testing.T) {
	catalogue := &fakeCatalogue{feed: sampleFeed(true), episodes: []store.Episode{{Guid: "a"}}}

	rec := get(t, newTestHandler(&fakeItunes{}, catalogue, &fakeCrawler{}), "/api/v1/podcasts/1234567/episodes")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Requesting episodes is what puts a feed into the crawl rotation.
	if len(catalogue.requested) != 1 || catalogue.requested[0] != 7 {
		t.Errorf("marked %v as requested, want [7]", catalogue.requested)
	}
}

func TestGetEpisodesSurvivesActivationFailure(t *testing.T) {
	catalogue := &fakeCatalogue{
		feed:         sampleFeed(true),
		episodes:     []store.Episode{{Guid: "a"}},
		requestedErr: errors.New("database is down"),
	}

	rec := get(t, newTestHandler(&fakeItunes{}, catalogue, &fakeCrawler{}), "/api/v1/podcasts/1234567/episodes")

	// The caller wanted episodes, and we have them. Failing to record the
	// request must not turn that into an error.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 despite activation failing", rec.Code)
	}
}

func TestSearchDoesNotActivateAnything(t *testing.T) {
	ias := &fakeItunes{response: itunes.SearchResponse{ResultCount: 1, Results: []itunes.Result{sampleResult()}}}
	catalogue := &fakeCatalogue{}

	rec := get(t, newTestHandler(ias, catalogue, &fakeCrawler{}), "/api/v1/podcasts/?searchTerm=test")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The whole point: a search stores metadata but schedules no crawling.
	if len(catalogue.requested) != 0 {
		t.Errorf("search activated feeds %v, want none", catalogue.requested)
	}
	if len(catalogue.upserted) != 1 {
		t.Errorf("search should still store the podcast, upserted %d", len(catalogue.upserted))
	}
}
