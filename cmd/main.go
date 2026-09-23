package main

import (
	"log/slog"
	"os"

	"github.com/shanejwalsh/starhane-fm-server/cmd/api"
	"github.com/shanejwalsh/starhane-fm-server/logging"
)

func main() {
	logger := logging.New(os.Stdout, logging.ConfigFromEnv())
	slog.SetDefault(logger)

	port := "8000"

	server := api.NewAPIServer(port, logger)
	if err := server.Start(); err != nil {
		logger.Error("server stopped", slog.Any("error", err))
		os.Exit(1)
	}
}
