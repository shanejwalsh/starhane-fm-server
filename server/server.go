package server

import (
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"github.com/shanejwalsh/itunes-xml-parser/feeds"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"
	"github.com/shanejwalsh/starhane-fm-server/logging"
	"github.com/shanejwalsh/starhane-fm-server/service/podcast"
)

type APIServer struct {
	port   string
	logger *slog.Logger
}

func NewAPIServer(port string, logger *slog.Logger) *APIServer {
	return &APIServer{
		port:   port,
		logger: logger,
	}
}

func (s *APIServer) Start() error {

	cors := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: false,
	})

	router := mux.NewRouter()

	subrouter := router.PathPrefix("/api/v1").Subrouter()

	subrouter.StrictSlash(true)

	ias := itunes.NewItunesApiServices()
	fs := feeds.NewRssFeedService()

	podcastHandler := podcast.NewHandler(ias, fs)

	podcastHandler.RegisterRoutes(subrouter)

	// Wrap the whole stack (rather than router.Use) so unmatched routes and
	// CORS preflight requests are logged too.
	handler := logging.Middleware(s.logger)(cors.Handler(router))

	server := &http.Server{
		Addr:     ":" + s.port,
		Handler:  handler,
		ErrorLog: slog.NewLogLogger(s.logger.Handler(), slog.LevelError),
	}

	s.logger.Info("starting server", slog.String("addr", server.Addr))

	return server.ListenAndServe()
}
