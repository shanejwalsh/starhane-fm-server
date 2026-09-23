package crawl_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shanejwalsh/starhane-fm-server/config"
	"github.com/shanejwalsh/starhane-fm-server/crawl"
	"github.com/shanejwalsh/starhane-fm-server/internal/testdb"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

func testCrawlerConfig() config.Crawler {
	return config.Crawler{
		HTTPTimeout:  5 * time.Second,
		UserAgent:    "starhane-fm-test/1.0 (+https://example.com)",
		MaxBodyBytes: 1 << 20,
		HostRPS:      1000,
		HostBurst:    100,
		MinInterval:  time.Hour,
		MaxInterval:  24 * time.Hour,
		MaxFailures:  3,
		DeadRecheck:  30 * 24 * time.Hour,
	}
}

func newCrawler(t *testing.T) (*crawl.Crawler, *store.Store, context.Context) {
	t.Helper()

	pool := testdb.Setup(t)
	s := store.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return crawl.NewCrawler(s, testCrawlerConfig(), logger), s, ctx
}

// requestedFeed upserts a feed and activates it, which is what a request for
// its episodes does. Seeded feeds are dormant and never crawled.
func requestedFeed(t *testing.T, s *store.Store, ctx context.Context, url string) store.Feed {
	t.Helper()

	feed, err := s.UpsertFeed(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkFeedRequested(ctx, feed.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	activated, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	return activated
}

func feedDocument(channelExtra string, items ...string) string {
	body := ""
	for _, item := range items {
		body += item
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel><title>Test Podcast</title>%s%s</channel>
</rss>`, channelExtra, body)
}

func episodeItem(guid, title string) string {
	return fmt.Sprintf(`<item><guid>%s</guid><title>%s</title>
		<pubDate>Wed, 01 Jan 2025 00:00:00 +0000</pubDate>
		<enclosure url="https://example.com/%s.mp3" length="100" type="audio/mpeg"/></item>`, guid, title, guid)
}

func TestCrawlFeedStoresEpisodes(t *testing.T) {
	c, s, ctx := newCrawler(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One"), episodeItem("ep2", "Two")))
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	report, err := c.CrawlFeed(ctx, feed)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != crawl.OutcomeChanged {
		t.Fatalf("outcome = %q, want %q (err: %v)", report.Outcome, crawl.OutcomeChanged, report.Err)
	}
	if report.Episodes != 2 {
		t.Errorf("stored %d episodes, want 2", report.Episodes)
	}

	episodes, err := s.EpisodesByFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("read back %d episodes, want 2", len(episodes))
	}
	// Feed order, not date order: the two share a publication date.
	if episodes[0].Guid != "ep1" || episodes[1].Guid != "ep2" {
		t.Errorf("episode order = %q, %q, want ep1, ep2", episodes[0].Guid, episodes[1].Guid)
	}

	reloaded, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.NeverCrawled() {
		t.Error("feed should have a last_success_at after a successful crawl")
	}
	if reloaded.NextCheckAt.Before(time.Now()) {
		t.Error("a crawled feed should be scheduled into the future")
	}
}

func TestCrawlFeedIsIdempotent(t *testing.T) {
	c, s, ctx := newCrawler(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// A changing title forces a different body hash, so the second crawl
		// really does re-parse and re-upsert rather than short-circuiting.
		_, _ = io.WriteString(w, feedDocument("",
			episodeItem("ep1", fmt.Sprintf("One v%d", hits.Load())),
			episodeItem("ep2", "Two")))
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		current, err := s.FeedByID(ctx, feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CrawlFeed(ctx, current); err != nil {
			t.Fatal(err)
		}
	}

	total, err := s.CountEpisodes(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("after three crawls the feed has %d episode rows, want 2", total)
	}

	episodes, err := s.EpisodesByFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("serving %d episodes, want 2", len(episodes))
	}
	if episodes[0].Title != "One v3" {
		t.Errorf("title = %q, want the latest crawl's %q", episodes[0].Title, "One v3")
	}
}

func TestCrawlFeedNotModifiedSkipsReparse(t *testing.T) {
	c, s, ctx := newCrawler(t)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"constant"`)
		if r.Header.Get("If-None-Match") == `"constant"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One")))
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	first, err := c.CrawlFeed(ctx, feed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != crawl.OutcomeChanged {
		t.Fatalf("first crawl outcome = %q, want %q", first.Outcome, crawl.OutcomeChanged)
	}

	afterFirst, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CrawlFeed(ctx, afterFirst)
	if err != nil {
		t.Fatal(err)
	}
	if second.Outcome != crawl.OutcomeNotModified {
		t.Fatalf("second crawl outcome = %q, want %q", second.Outcome, crawl.OutcomeNotModified)
	}

	// Episodes must survive a 304 untouched: crawl_seq did not advance, so
	// what the first crawl stored is still what gets served.
	episodes, err := s.EpisodesByFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Errorf("serving %d episodes after a 304, want 1", len(episodes))
	}

	afterSecond, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterSecond.CheckInterval <= afterFirst.CheckInterval {
		t.Errorf("interval did not back off after no change: %s then %s",
			afterFirst.CheckInterval, afterSecond.CheckInterval)
	}
}

func TestCrawlFeedDropsRemovedEpisodes(t *testing.T) {
	c, s, ctx := newCrawler(t)

	var pass atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pass.Add(1) == 1 {
			_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One"), episodeItem("ep2", "Two")))
			return
		}
		_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One")))
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CrawlFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	current, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CrawlFeed(ctx, current); err != nil {
		t.Fatal(err)
	}

	// The publisher pulled an episode, so it stops being served...
	episodes, err := s.EpisodesByFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].Guid != "ep1" {
		t.Errorf("serving %d episodes, want just ep1", len(episodes))
	}
	// ...but its row survives for later.
	total, err := s.CountEpisodes(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("stored %d episode rows, want both retained", total)
	}
}

func TestCrawlFeedFollowsNewFeedURL(t *testing.T) {
	c, s, ctx := newCrawler(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, feedDocument("", episodeItem("moved1", "Moved")))
	}))
	defer target.Close()

	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, feedDocument(
			"<itunes:new-feed-url>"+target.URL+"</itunes:new-feed-url>",
			episodeItem("old1", "Old")))
	}))
	defer origin.Close()
	_ = origin

	feed, err := s.UpsertFeed(ctx, origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.CrawlFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	moved, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.URL != target.URL {
		t.Fatalf("feed url = %q, want it moved to %q", moved.URL, target.URL)
	}
	if moved.Status != store.StatusActive {
		t.Errorf("status = %q, want %q", moved.Status, store.StatusActive)
	}
	// Validators belonged to the old URL, so they must be dropped.
	if moved.ETag != "" || moved.LastModified != "" || moved.BodyHash != nil {
		t.Error("validators from the old URL should have been cleared")
	}
	// The move takes effect immediately rather than after the usual interval.
	if moved.NextCheckAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("next check = %s, want the moved feed to be due now", moved.NextCheckAt)
	}

	// Crawling again picks up the new location's episodes.
	if _, err := c.CrawlFeed(ctx, moved); err != nil {
		t.Fatal(err)
	}
	episodes, err := s.EpisodesByFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].Guid != "moved1" {
		t.Errorf("after the move the feed serves %+v, want the target's episode", episodes)
	}
}

func TestCrawlFeedRedirectsToFeedWeAlreadyHave(t *testing.T) {
	c, s, ctx := newCrawler(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, feedDocument("", episodeItem("t1", "Target")))
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer origin.Close()

	// Both URLs are already in the catalogue, so the origin cannot simply take
	// the target's URL — they would collide on the unique index.
	existing, err := s.UpsertFeed(ctx, target.URL)
	if err != nil {
		t.Fatal(err)
	}
	feed, err := s.UpsertFeed(ctx, origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.CrawlFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	stub, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stub.Status != store.StatusRedirected {
		t.Errorf("status = %q, want %q", stub.Status, store.StatusRedirected)
	}
	if stub.RedirectedToFeedID == nil || *stub.RedirectedToFeedID != existing.ID {
		t.Errorf("redirected_to_feed_id = %v, want %d", stub.RedirectedToFeedID, existing.ID)
	}
	if stub.URL != origin.URL {
		t.Errorf("url = %q, want the original %q to be kept", stub.URL, origin.URL)
	}
}

func TestCrawlFeedMarksGoneFeedDead(t *testing.T) {
	c, s, ctx := newCrawler(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CrawlFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	dead, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 410 is unambiguous, so one response is enough — no need to burn the
	// whole failure budget.
	if dead.Status != store.StatusDead {
		t.Errorf("status = %q, want %q after a single 410", dead.Status, store.StatusDead)
	}
	if dead.NextCheckAt.Before(time.Now().Add(24 * time.Hour)) {
		t.Error("a dead feed should be rechecked much later, not soon")
	}

	// A dead feed leaves the queue until its recheck comes due — it is not
	// written off permanently.
	claimed, err := s.ClaimDueFeeds(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range claimed {
		if f.ID == feed.ID {
			t.Error("a dead feed was claimed before its recheck was due")
		}
	}
}

func TestCrawlFeedMarksRepeatedlyFailingFeedDead(t *testing.T) {
	c, s, ctx := newCrawler(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// MaxFailures is 3 in the test config.
	for attempt := 1; attempt <= 3; attempt++ {
		current, err := s.FeedByID(ctx, feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CrawlFeed(ctx, current); err != nil {
			t.Fatal(err)
		}

		after, err := s.FeedByID(ctx, feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.ConsecutiveFailures != attempt {
			t.Errorf("after %d failures the count is %d", attempt, after.ConsecutiveFailures)
		}
		wantDead := attempt >= 3
		if isDead := after.Status == store.StatusDead; isDead != wantDead {
			t.Errorf("after %d failures status = %q, dead should be %v", attempt, after.Status, wantDead)
		}
	}
}

func TestCrawlFeedRateLimitedDoesNotCountAsFailure(t *testing.T) {
	c, s, ctx := newCrawler(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CrawlFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	after, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Being throttled is the host working as intended, not the feed being
	// broken, so it must not push the feed towards being marked dead.
	if after.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0 for a 429", after.ConsecutiveFailures)
	}
	if after.Status != store.StatusActive {
		t.Errorf("status = %q, want %q", after.Status, store.StatusActive)
	}
	// Retry-After was 5 minutes, so the next check should respect roughly that.
	delay := time.Until(after.NextCheckAt)
	if delay < 4*time.Minute || delay > 6*time.Minute {
		t.Errorf("next check in %s, want about 5m from Retry-After", delay)
	}
}

func TestCrawlFeedMalformedXMLIsAFailure(t *testing.T) {
	c, s, ctx := newCrawler(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html><body>we moved, see our website</body>")
	}))
	defer srv.Close()

	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CrawlFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	after, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", after.ConsecutiveFailures)
	}
	if after.LastError == "" {
		t.Error("the parse failure should have been recorded")
	}
	if !after.NeverCrawled() {
		t.Error("a feed that never parsed should still report NeverCrawled")
	}
}

func TestCrawlFeedActivatesADormantFeedEvenWhenTheCrawlFails(t *testing.T) {
	c, s, ctx := newCrawler(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// A dormant feed reaches the crawler only through the API's first-request
	// path, which has already activated it in the database. The struct handed
	// over still says dormant, and writing that back would undo the activation
	// and leave the feed asleep forever.
	feed, err := s.UpsertFeed(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if feed.Status != store.StatusDormant {
		t.Fatalf("a seeded feed should start dormant, got %q", feed.Status)
	}

	if _, err := c.CrawlFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	after, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != store.StatusActive {
		t.Errorf("status = %q, want %q so the crawler keeps retrying it", after.Status, store.StatusActive)
	}
}

func TestCrawlFeedPrunesEpisodesThatVanish(t *testing.T) {
	pool := testdb.Setup(t)
	s := store.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const grace = 2
	cfg := testCrawlerConfig()
	cfg.EpisodeGraceCrawls = grace

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := crawl.NewCrawler(s, cfg, logger)

	var pass atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pass.Add(1)
		if n == 1 {
			_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One"), episodeItem("ep2", "Two")))
			return
		}
		// ep2 is gone from here on. The title changes so the body hash differs
		// and every crawl really re-parses.
		_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", fmt.Sprintf("One v%d", n))))
	}))
	defer srv.Close()

	feed := requestedFeed(t, s, ctx, srv.URL)

	crawlOnce := func() crawl.Report {
		t.Helper()
		current, err := s.FeedByID(ctx, feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		report, err := c.CrawlFeed(ctx, current)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}

	crawlOnce()
	if n, _ := s.CountEpisodes(ctx, feed.ID); n != 2 {
		t.Fatalf("after the first crawl there are %d episode rows, want 2", n)
	}

	// Within the grace window the row survives, so one bad document cannot
	// erase a feed's history.
	for i := range grace {
		report := crawlOnce()
		if report.Pruned != 0 {
			t.Errorf("crawl %d pruned %d rows inside the grace window", i+1, report.Pruned)
		}
		if n, _ := s.CountEpisodes(ctx, feed.ID); n != 2 {
			t.Errorf("after crawl %d there are %d rows, want the vanished episode kept", i+1, n)
		}
	}

	// Past the window it goes.
	report := crawlOnce()
	if report.Pruned != 1 {
		t.Errorf("pruned %d rows, want 1", report.Pruned)
	}
	if n, _ := s.CountEpisodes(ctx, feed.ID); n != 1 {
		t.Errorf("%d episode rows remain, want 1", n)
	}

	remaining, err := s.EpisodesByFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Guid != "ep1" {
		t.Errorf("served %+v, want just ep1", remaining)
	}
}

func TestCrawlFeedDoesNotPruneWhenAFeedGoesEmpty(t *testing.T) {
	pool := testdb.Setup(t)
	s := store.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := testCrawlerConfig()
	cfg.EpisodeGraceCrawls = 1

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := crawl.NewCrawler(s, cfg, logger)

	var pass atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pass.Add(1) == 1 {
			_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One")))
			return
		}
		// A valid document with no items at all — far more likely a broken
		// origin than a podcast that deleted its entire back catalogue.
		_, _ = io.WriteString(w, feedDocument("<generator>glitch</generator>"))
	}))
	defer srv.Close()

	feed := requestedFeed(t, s, ctx, srv.URL)

	for range 4 {
		current, err := s.FeedByID(ctx, feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CrawlFeed(ctx, current); err != nil {
			t.Fatal(err)
		}
	}

	if n, _ := s.CountEpisodes(ctx, feed.ID); n != 1 {
		t.Errorf("%d episode rows, want the history kept when the feed went empty", n)
	}
}

func TestPruningDisabledByZeroGrace(t *testing.T) {
	pool := testdb.Setup(t)
	s := store.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := testCrawlerConfig()
	cfg.EpisodeGraceCrawls = 0

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := crawl.NewCrawler(s, cfg, logger)

	var pass atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pass.Add(1)
		if n == 1 {
			_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One"), episodeItem("ep2", "Two")))
			return
		}
		_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", fmt.Sprintf("One v%d", n))))
	}))
	defer srv.Close()

	feed := requestedFeed(t, s, ctx, srv.URL)
	for range 5 {
		current, err := s.FeedByID(ctx, feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CrawlFeed(ctx, current); err != nil {
			t.Fatal(err)
		}
	}

	if n, _ := s.CountEpisodes(ctx, feed.ID); n != 2 {
		t.Errorf("%d episode rows, want both kept with pruning disabled", n)
	}
}
