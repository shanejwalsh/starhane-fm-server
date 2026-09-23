package crawl

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/shanejwalsh/starhane-fm-server/config"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

// Worker repeatedly claims due feeds and crawls them.
type Worker struct {
	store   *store.Store
	crawler *Crawler
	cfg     config.Crawler
	logger  *slog.Logger
}

// NewWorker builds a Worker.
func NewWorker(s *store.Store, crawler *Crawler, cfg config.Crawler, logger *slog.Logger) *Worker {
	return &Worker{store: s, crawler: crawler, cfg: cfg, logger: logger}
}

// Run claims and crawls feeds until ctx is cancelled.
//
// It returns nil on a clean shutdown: cancellation is how the process is asked
// to stop, not a failure.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.InfoContext(ctx, "crawler started",
		slog.Int("workers", w.cfg.Workers),
		slog.Int("batch_size", w.cfg.BatchSize),
		slog.Duration("poll_interval", w.cfg.PollInterval),
		slog.String("user_agent", w.cfg.UserAgent),
	)

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for ctx.Err() == nil {
		crawled, err := w.runBatch(ctx)
		if err != nil && ctx.Err() == nil {
			w.logger.ErrorContext(ctx, "crawl batch failed", slog.Any("error", err))
		}

		// Keep draining while there is work, so a large backlog is not
		// throttled to one batch per poll interval.
		if crawled > 0 {
			continue
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	w.logger.Info("crawler stopped")
	return nil
}

// runBatch claims one batch of due feeds and crawls them concurrently,
// returning how many were crawled.
func (w *Worker) runBatch(ctx context.Context) (int, error) {
	feeds, err := w.store.ClaimDueFeeds(ctx, w.cfg.BatchSize, w.cfg.LeaseDuration)
	if err != nil {
		return 0, err
	}
	if len(feeds) == 0 {
		return 0, nil
	}

	start := time.Now()
	w.logger.DebugContext(ctx, "claimed feeds", slog.Int("count", len(feeds)))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(w.cfg.Workers)

	for _, feed := range feeds {
		group.Go(func() error {
			if _, err := w.crawler.CrawlFeed(groupCtx, feed); err != nil {
				// A feed whose outcome could not be stored is worth logging,
				// but it must not abandon the rest of the batch.
				w.logger.ErrorContext(groupCtx, "could not record crawl",
					slog.Int64("feed_id", feed.ID),
					slog.Any("error", err),
				)
			}
			return nil
		})
	}

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return len(feeds), err
	}

	w.logger.InfoContext(ctx, "crawl batch finished",
		slog.Int("feeds", len(feeds)),
		slog.Duration("duration", time.Since(start)),
	)
	return len(feeds), nil
}
