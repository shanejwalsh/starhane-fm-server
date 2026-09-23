package crawl_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shanejwalsh/starhane-fm-server/crawl"
	"github.com/shanejwalsh/starhane-fm-server/internal/testdb"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

func TestWorkerCrawlsDueFeedsAndStopsOnCancel(t *testing.T) {
	pool := testdb.Setup(t)
	s := store.New(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		_, _ = io.WriteString(w, feedDocument("", episodeItem("ep1", "One")))
	}))
	defer srv.Close()

	const feedCount = 5
	for range feedCount {
		if _, err := s.UpsertFeed(ctx, srv.URL+"/"+time.Now().Format("150405.000000000")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	cfg := testCrawlerConfig()
	cfg.Workers = 3
	cfg.BatchSize = 10
	cfg.PollInterval = 20 * time.Millisecond
	cfg.LeaseDuration = time.Minute

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := crawl.NewWorker(s, crawl.NewCrawler(s, cfg, logger), cfg, logger)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()

	// Wait for the backlog to drain.
	deadline := time.Now().Add(10 * time.Second)
	for served.Load() < feedCount && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	stop()
	select {
	case err := <-done:
		// Cancellation is how the process is asked to stop, not a failure.
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if got := served.Load(); got < feedCount {
		t.Errorf("served %d feeds, want all %d crawled", got, feedCount)
	}

	// Every feed should now be scheduled into the future rather than still due.
	stillDue, err := s.ClaimDueFeeds(ctx, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillDue) != 0 {
		t.Errorf("%d feeds are still due after the worker drained the queue", len(stillDue))
	}
}
