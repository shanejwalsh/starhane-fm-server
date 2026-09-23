package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shanejwalsh/starhane-fm-server/internal/testdb"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

func newStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	pool := testdb.Setup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	return store.New(pool), ctx
}

func TestUpsertFeedIsIdempotent(t *testing.T) {
	s, ctx := newStore(t)

	first, err := s.UpsertFeed(ctx, "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.UpsertFeed(ctx, "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}

	if first.ID != second.ID {
		t.Errorf("upserting the same URL made two feeds: %d and %d", first.ID, second.ID)
	}
	// A seeded feed starts dormant: a search seeds fifty of them and a user
	// opens at most one, so none are crawled until asked for.
	if first.Status != store.StatusDormant {
		t.Errorf("status = %q, want %q", first.Status, store.StatusDormant)
	}
	if !first.Dormant() {
		t.Error("a new feed should report Dormant")
	}
	if !first.NeverCrawled() {
		t.Error("a new feed should report NeverCrawled")
	}
	if first.ActivatedAt != nil {
		t.Errorf("activated_at = %v, want nil until requested", first.ActivatedAt)
	}
}

func TestUpsertPodcastWithFeed(t *testing.T) {
	s, ctx := newStore(t)

	in := store.PodcastUpsert{
		ItunesID:      1234567,
		Title:         "Test Podcast",
		ArtistName:    "Test Artist",
		ArtworkURL600: "https://example.com/600.jpg",
		Genres:        []string{"Technology", "News"},
		Explicit:      true,
		FeedURL:       "https://example.com/feed.xml",
	}

	podcast, feed, err := s.UpsertPodcastWithFeed(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if feed == nil {
		t.Fatal("expected a feed to be created")
	}
	if podcast.FeedID == nil || *podcast.FeedID != feed.ID {
		t.Errorf("podcast.feed_id = %v, want %d", podcast.FeedID, feed.ID)
	}
	if len(podcast.Genres) != 2 || podcast.Genres[0] != "Technology" {
		t.Errorf("genres = %v, want [Technology News]", podcast.Genres)
	}

	// A second upsert updates in place rather than duplicating.
	in.Title = "Renamed Podcast"
	again, _, err := s.UpsertPodcastWithFeed(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != podcast.ID {
		t.Errorf("upsert made a second podcast row: %d then %d", podcast.ID, again.ID)
	}
	if again.Title != "Renamed Podcast" {
		t.Errorf("title = %q, want %q", again.Title, "Renamed Podcast")
	}
}

func TestUpsertPodcastWithoutFeedURL(t *testing.T) {
	s, ctx := newStore(t)

	// iTunes does not always return a feedUrl. The podcast is still worth
	// storing; it just cannot be crawled.
	podcast, feed, err := s.UpsertPodcastWithFeed(ctx, store.PodcastUpsert{
		ItunesID: 999,
		Title:    "No Feed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if feed != nil {
		t.Errorf("expected no feed, got %+v", feed)
	}
	if podcast.FeedID != nil {
		t.Errorf("podcast.feed_id = %v, want nil", podcast.FeedID)
	}
}

func TestUpsertPodcastKeepsKnownFeedWhenResponseOmitsOne(t *testing.T) {
	s, ctx := newStore(t)

	withFeed := store.PodcastUpsert{ItunesID: 42, Title: "Show", FeedURL: "https://example.com/f.xml"}
	if _, _, err := s.UpsertPodcastWithFeed(ctx, withFeed); err != nil {
		t.Fatal(err)
	}

	// A later lookup that omits feedUrl must not erase what we already know.
	podcast, _, err := s.UpsertPodcastWithFeed(ctx, store.PodcastUpsert{ItunesID: 42, Title: "Show"})
	if err != nil {
		t.Fatal(err)
	}
	if podcast.FeedID == nil {
		t.Error("feed_id was cleared by a response with no feedUrl")
	}
}

func TestPodcastByItunesIDReportsMissing(t *testing.T) {
	s, ctx := newStore(t)

	_, err := s.PodcastByItunesID(ctx, 404404)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("error = %v, want pgx.ErrNoRows", err)
	}
}

func TestClaimDueFeedsLeasesAndSkipsNotDue(t *testing.T) {
	s, ctx := newStore(t)

	due := activeFeed(t, s, ctx, "https://example.com/due.xml")
	notDue := activeFeed(t, s, ctx, "https://example.com/later.xml")
	// Push the second feed into the future.
	notDue.NextCheckAt = time.Now().Add(time.Hour)
	if _, err := s.ApplyCrawl(ctx, store.CrawlResult{Feed: notDue}); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDueFeeds(ctx, 10, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d feeds, want 1", len(claimed))
	}
	if claimed[0].ID != due.ID {
		t.Errorf("claimed feed %d, want %d", claimed[0].ID, due.ID)
	}

	// The lease pushed it forward, so an immediate second claim finds nothing.
	again, err := s.ClaimDueFeeds(ctx, 10, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("claimed %d feeds on the second pass, want 0 — the lease did not hold", len(again))
	}
}

func TestClaimDueFeedsSkipsLockedRows(t *testing.T) {
	s, ctx := newStore(t)

	const feedCount = 6
	for i := range feedCount {
		activeFeed(t, s, ctx, fmt.Sprintf("https://example.com/%d.xml", i))
	}

	// Two concurrent claimers must between them see each feed exactly once:
	// that is what SKIP LOCKED buys, and it is what lets several crawler
	// processes share one queue without coordinating.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed []int64
		errs    []error
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			feeds, err := s.ClaimDueFeeds(ctx, feedCount, 15*time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for _, f := range feeds {
				claimed = append(claimed, f.ID)
			}
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Fatal(err)
	}
	if len(claimed) != feedCount {
		t.Errorf("claimed %d feeds in total, want %d", len(claimed), feedCount)
	}
	seen := map[int64]bool{}
	for _, id := range claimed {
		if seen[id] {
			t.Errorf("feed %d was claimed twice", id)
		}
		seen[id] = true
	}
}

// activeFeed upserts a feed and puts it into the crawl rotation, which is what
// a request for its episodes would do.
func activeFeed(t *testing.T, s *store.Store, ctx context.Context, url string) store.Feed {
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

func TestDormantFeedsAreNotCrawled(t *testing.T) {
	s, ctx := newStore(t)

	// This is the change that bounds growth: a search seeds fifty feeds and
	// none of them cost anything until somebody opens one.
	for i := range 5 {
		if _, err := s.UpsertFeed(ctx, fmt.Sprintf("https://example.com/seeded-%d.xml", i)); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := s.ClaimDueFeeds(ctx, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d seeded feeds, want none until they are requested", len(claimed))
	}
}

func TestMarkFeedRequestedActivatesADormantFeed(t *testing.T) {
	s, ctx := newStore(t)

	feed, err := s.UpsertFeed(ctx, "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}

	activated, err := s.MarkFeedRequested(ctx, feed.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !activated {
		t.Error("requesting a dormant feed should activate it")
	}

	woken, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if woken.Status != store.StatusActive {
		t.Errorf("status = %q, want %q", woken.Status, store.StatusActive)
	}
	if woken.ActivatedAt == nil || woken.LastRequestedAt == nil {
		t.Errorf("activated_at = %v, last_requested_at = %v, want both set", woken.ActivatedAt, woken.LastRequestedAt)
	}

	// It is now claimable, which it was not a moment ago.
	claimed, err := s.ClaimDueFeeds(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != feed.ID {
		t.Errorf("claimed %d feeds, want the one just activated", len(claimed))
	}
}

func TestMarkFeedRequestedIsThrottled(t *testing.T) {
	s, ctx := newStore(t)

	feed := activeFeed(t, s, ctx, "https://example.com/feed.xml")
	first := feed.LastRequestedAt
	if first == nil {
		t.Fatal("activation should have set last_requested_at")
	}

	// Within the throttle window a second request writes nothing, so a popular
	// podcast does not take a database write on every read.
	activated, err := s.MarkFeedRequested(ctx, feed.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated {
		t.Error("a feed already active and recently requested should not report activation")
	}

	after, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastRequestedAt.Equal(*first) {
		t.Errorf("last_requested_at moved from %s to %s inside the throttle window", first, after.LastRequestedAt)
	}

	// With no throttle it does write.
	if _, err := s.MarkFeedRequested(ctx, feed.ID, 0); err != nil {
		t.Fatal(err)
	}
	refreshed, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.LastRequestedAt.After(*first) {
		t.Error("last_requested_at should advance once the throttle window has passed")
	}
}

func TestDeadFeedsAreRetriedWhenDue(t *testing.T) {
	s, ctx := newStore(t)

	feed := activeFeed(t, s, ctx, "https://example.com/gone.xml")

	// Mark it dead with its recheck already due, exactly as the crawler would
	// leave a feed whose recheck interval has elapsed.
	feed.Status = store.StatusDead
	feed.NextCheckAt = time.Now().Add(-time.Minute)
	if _, err := s.ApplyCrawl(ctx, store.CrawlResult{Feed: feed}); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDueFeeds(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The claim query used to exclude dead feeds outright, so CRAWLER_DEAD_RECHECK
	// never took effect and a feed marked dead was written off permanently.
	if len(claimed) != 1 || claimed[0].ID != feed.ID {
		t.Errorf("claimed %d feeds, want the dead one to be retried once its recheck came due", len(claimed))
	}
}

func TestRedirectedFeedsAreNeverClaimed(t *testing.T) {
	s, ctx := newStore(t)

	target := activeFeed(t, s, ctx, "https://example.com/target.xml")
	stub := activeFeed(t, s, ctx, "https://example.com/old.xml")

	stub.Status = store.StatusRedirected
	stub.RedirectedToFeedID = &target.ID
	stub.NextCheckAt = time.Now().Add(-time.Hour)
	if _, err := s.ApplyCrawl(ctx, store.CrawlResult{Feed: stub}); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDueFeeds(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range claimed {
		if f.ID == stub.ID {
			t.Error("a redirect stub was claimed for crawling")
		}
	}
}
