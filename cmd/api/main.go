// Command api serves the HTTP API.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/shanejwalsh/starhane-fm-server/config"
	"github.com/shanejwalsh/starhane-fm-server/db"
	"github.com/shanejwalsh/starhane-fm-server/logging"
	"github.com/shanejwalsh/starhane-fm-server/server"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

func main() {
	logger := logging.New(os.Stdout, logging.ConfigFromEnv())
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
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

	// Migrations are applied by the migrate command, not here: the API and the
	// crawler would otherwise race to migrate the same database on deploy.
	return server.NewAPIServer(cfg, store.New(pool), logger).Start(ctx)
}
