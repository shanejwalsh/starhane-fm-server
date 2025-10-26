package api

import (
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"github.com/shanejwalsh/itunes-xml-parser/feeds"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"
	"github.com/shanejwalsh/starhane-fm-server/service/podcast"
)

type APIServer struct {
	port string
}

func NewAPIServer(port string) *APIServer {
	return &APIServer{
		port: port,
	}
}

func (s *APIServer) Start() error {

	cors := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	})

	router := mux.NewRouter()

	corsHandler := cors.Handler(router)
	router.Use(loggingMiddleware)

	subrouter := router.PathPrefix("/api/v1").Subrouter()

	subrouter.StrictSlash(true)

	ias := itunes.NewItunesApiServices()
	fs := feeds.NewRssFeedService()

	podcastHandler := podcast.NewHandler(ias, fs)

	podcastHandler.RegisterRoutes(subrouter)

	log.Println("listening on PORT", s.port)

	return http.ListenAndServe(":"+s.port, corsHandler)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
