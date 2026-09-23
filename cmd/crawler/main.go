// Command crawler keeps the episode catalogue fresh.
//
// It shares its packages and its database with the API, but runs as its own
// service: feed crawling should not compete with serving requests, and the two
// scale differently.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shanejwalsh/starhane-fm-server/config"
	"github.com/shanejwalsh/starhane-fm-server/crawl"
	"github.com/shanejwalsh/starhane-fm-server/db"
	"github.com/shanejwalsh/starhane-fm-server/logging"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

// shutdownGrace is how long in-flight crawls get to finish after a signal.
const shutdownGrace = 30 * time.Second

func main() {
	logger := logging.New(os.Stdout, logging.ConfigFromEnv())
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("crawler stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// The signal context is what makes shutdown graceful: it reaches every
	// fetch and every query, so nothing is left half-done.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	pool, err := db.NewPool(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	s := store.New(pool)
	crawler := crawl.NewCrawler(s, cfg.Crawler, logger)
	worker := crawl.NewWorker(s, crawler, cfg.Crawler, logger)

	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, finishing in-flight crawls",
			slog.Duration("grace", shutdownGrace))

		select {
		case err := <-done:
			return err
		case <-time.After(shutdownGrace):
			logger.Warn("in-flight crawls did not finish within the grace period")
			return nil
		}
	}
}
