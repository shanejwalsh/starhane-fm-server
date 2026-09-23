// Package server wires the HTTP API together.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"

	"github.com/shanejwalsh/starhane-fm-server/config"
	"github.com/shanejwalsh/starhane-fm-server/crawl"
	"github.com/shanejwalsh/starhane-fm-server/logging"
	"github.com/shanejwalsh/starhane-fm-server/service/podcast"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

// shutdownGrace is how long in-flight requests get to finish after a signal.
const shutdownGrace = 20 * time.Second

type APIServer struct {
	cfg    config.Config
	store  *store.Store
	logger *slog.Logger
}

func NewAPIServer(cfg config.Config, s *store.Store, logger *slog.Logger) *APIServer {
	return &APIServer{
		cfg:    cfg,
		store:  s,
		logger: logger,
	}
}

// Start serves until ctx is cancelled, then drains in-flight requests.
func (s *APIServer) Start(ctx context.Context) error {

	cors := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: false,
	})

	router := mux.NewRouter()

	subrouter := router.PathPrefix("/api/v1").Subrouter()

	subrouter.StrictSlash(true)

	// A timeout on the iTunes client matters more than it looks: without one a
	// hung upstream request pins a handler indefinitely.
	ias := itunes.NewItunesApiServices(
		itunes.WithHTTPClient(&http.Client{Timeout: s.cfg.Itunes.Timeout}),
	)

	// The API crawls only feeds it has never seen; the crawler service does
	// everything after that. Sharing the implementation means a feed filled on
	// first request is stored exactly as the crawler would have stored it.
	crawler := crawl.NewCrawler(s.store, s.cfg.Crawler, s.logger)

	podcastHandler := podcast.NewHandler(ias, s.store, crawler, podcast.Options{
		SearchLimit:     s.cfg.Itunes.SearchLimit,
		SearchCountry:   s.cfg.Itunes.Country,
		SyncCrawlBudget: s.cfg.Crawler.SyncCrawlBudget,
	})

	podcastHandler.RegisterRoutes(subrouter)

	// Wrap the whole stack (rather than router.Use) so unmatched routes and
	// CORS preflight requests are logged too.
	handler := logging.Middleware(s.logger)(cors.Handler(router))

	server := &http.Server{
		Addr:              ":" + s.cfg.Port,
		Handler:           handler,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("starting server", slog.String("addr", server.Addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	s.logger.Info("shutdown signal received, draining requests",
		slog.Duration("grace", shutdownGrace))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-serveErr
}
