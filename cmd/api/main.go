// Command api serves the HTTP API.
package main

import (
	"log/slog"
	"os"

	"github.com/shanejwalsh/starhane-fm-server/logging"
	"github.com/shanejwalsh/starhane-fm-server/server"
)

func main() {
	logger := logging.New(os.Stdout, logging.ConfigFromEnv())
	slog.SetDefault(logger)

	port := "8000"

	srv := server.NewAPIServer(port, logger)
	if err := srv.Start(); err != nil {
		logger.Error("server stopped", slog.Any("error", err))
		os.Exit(1)
	}
}
