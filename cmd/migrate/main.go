// Command migrate applies the embedded database migrations.
//
// It is deliberately a separate binary rather than something the API or crawler
// does at startup: two services racing to migrate the same database is a bad
// time. On Railway this runs as the API service's pre-deploy command.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/shanejwalsh/starhane-fm-server/config"
	"github.com/shanejwalsh/starhane-fm-server/db"
	"github.com/shanejwalsh/starhane-fm-server/logging"
)

const usage = `usage: migrate <command>

commands:
  up              apply every pending migration
  down            roll back a single migration
  down --all      roll back every applied migration
  status          show the applied version and the available migrations
  force <version> set the recorded version without running anything, to
                  recover from a dirty state after a failed migration
`

func main() {
	logger := logging.New(os.Stdout, logging.ConfigFromEnv())
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("migrate failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	all := flag.Bool("all", false, "with down, roll back every migration")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return errors.New("no command given")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	migrator, err := db.NewMigrator(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer migrator.Close()

	switch command := args[0]; command {
	case "up":
		if err := migrator.Up(); err != nil {
			if errors.Is(err, db.ErrNoChange) {
				logger.Info("migrations already up to date")
				return nil
			}
			return fmt.Errorf("applying migrations: %w", err)
		}
		return report(logger, migrator, "migrations applied")

	case "down":
		rollback := migrator.Down
		message := "migration rolled back"
		if *all {
			rollback = migrator.DownAll
			message = "all migrations rolled back"
		}
		if err := rollback(); err != nil {
			if errors.Is(err, db.ErrNoChange) {
				logger.Info("nothing to roll back")
				return nil
			}
			return fmt.Errorf("rolling back: %w", err)
		}
		return report(logger, migrator, message)

	case "status":
		return report(logger, migrator, "migration status")

	case "force":
		if len(args) < 2 {
			return errors.New("force requires a version")
		}
		version, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("force: %q is not a version number", args[1])
		}
		if err := migrator.Force(version); err != nil {
			return fmt.Errorf("forcing version %d: %w", version, err)
		}
		return report(logger, migrator, "version forced")

	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func report(logger *slog.Logger, migrator *db.Migrator, message string) error {
	status, err := migrator.Status()
	if err != nil {
		return err
	}

	attrs := []any{
		slog.Uint64("version", uint64(status.Version)),
		slog.Bool("dirty", status.Dirty),
		slog.Any("available", status.Available),
	}
	if status.Dirty {
		logger.Error(message+" (schema is dirty: fix the failed migration by hand, then use `migrate force <version>`)", attrs...)
		return errors.New("schema is dirty")
	}
	logger.Info(message, attrs...)
	return nil
}
