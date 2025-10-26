package podcast

import (
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/shanejwalsh/itunes-xml-parser/feeds"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"
	"github.com/shanejwalsh/starhane-fm-server/utils"
)

const (
	SERVICE_PATH = "/podcasts"
	INDEX_PATH   = "/"
	EPISODE_PATH = "/{podcastId}"
)

type Handler struct {
	itunesParserService *itunes.ItunesApiServices
	feedService         *feeds.RssFeedService
}

func NewHandler(ias *itunes.ItunesApiServices, fs *feeds.RssFeedService) *Handler {

	return &Handler{
		itunesParserService: ias,
		feedService:         fs,
	}
}

func (h *Handler) RegisterRoutes(router *mux.Router) {

	subrouter := router.PathPrefix(SERVICE_PATH).Subrouter()

	subrouter.StrictSlash(true)

	subrouter.HandleFunc(INDEX_PATH, h.getPodcasts).Methods("GET")
	subrouter.HandleFunc(EPISODE_PATH, h.getPodcast).Methods("GET")
}

func (h *Handler) getPodcasts(res http.ResponseWriter, req *http.Request) {

	ias := h.itunesParserService
	searchTerm := req.URL.Query().Get("searchTerm")

	itunesRes, err := ias.Search(searchTerm)

	if err != nil {
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	utils.WriteJson(res, http.StatusOK, itunesRes)
}

type Podcast struct {
	Id          int       `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Url         string    `json:"url"`
	Episodes    []Episode `json:"episodes"`
}

type Episode struct {
	Id          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Url         string `json:"url"`
}

func (h *Handler) getPodcast(res http.ResponseWriter, req *http.Request) {

	vars := mux.Vars(req)
	podcastId := vars["podcastId"]

	ias := h.itunesParserService
	fs := h.feedService

	parsedId, err := strconv.Atoi(podcastId)

	if err != nil {
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	itunesRes, err := ias.FindById(parsedId)

	if err != nil {
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	if itunesRes.ResultCount != 1 {
		utils.WriteJson(res, http.StatusNotFound, "Podcast not found")
		return
	}

	podcast := itunesRes.Results[0]

	episodes, err := fs.GetFeed(podcast.FeedURL)

	if err != nil {
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	utils.WriteJson(res, http.StatusOK, episodes)
}
