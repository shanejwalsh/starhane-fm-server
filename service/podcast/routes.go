package podcast

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/shanejwalsh/itunes-xml-parser/feeds"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"

	"github.com/shanejwalsh/starhane-fm-server/types"
	"github.com/shanejwalsh/starhane-fm-server/utils"
)

const (
	SERVICE_PATH  = "/podcasts"
	INDEX_PATH    = "/"
	PODCAST_PATH  = "/{podcastId}"
	EPISODES_PATH = "/{podcastId}/episodes"
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
	subrouter.HandleFunc(PODCAST_PATH, h.getPodcast).Methods("GET")
	subrouter.HandleFunc(EPISODES_PATH, h.getEpisodes).Methods("GET")
}

func (h *Handler) getPodcasts(res http.ResponseWriter, req *http.Request) {

	searchTerm := req.URL.Query().Get("searchTerm")

	itunesRes, err := h.itunesParserService.Search(searchTerm)

	if err != nil {
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	podcasts := make([]types.Podcast, itunesRes.ResultCount)

	for i, podcast := range itunesRes.Results {
		podcasts[i] = utils.MapPodcast(&podcast)
	}

	utils.WriteJson(res, http.StatusOK, podcasts)
}

func (h *Handler) getPodcast(res http.ResponseWriter, req *http.Request) {

	vars := mux.Vars(req)
	podcastId := vars["podcastId"]
	parsedId, err := strconv.Atoi(podcastId)

	podcast, err := h.lookupPodcast(parsedId)

	if err != nil {
		utils.WriteJson(res, http.StatusNotFound, err.Error())
		return
	}

	utils.WriteJson(res, http.StatusOK, utils.MapPodcast(podcast))
}

func (h *Handler) getEpisodes(res http.ResponseWriter, req *http.Request) {

	vars := mux.Vars(req)
	podcastId := vars["podcastId"]
	parsedId, err := strconv.Atoi(podcastId)

	if err != nil {
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	podcast, err := h.lookupPodcast(parsedId)

	if err != nil {
		utils.WriteJson(res, http.StatusNotFound, err.Error())
		return
	}

	episodes, err := h.feedService.GetFeed(podcast.FeedURL)

	if err != nil {
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	utils.WriteJson(res, http.StatusOK, episodes)
}

func (h *Handler) lookupPodcast(id int) (*itunes.Result, error) {
	res, err := h.itunesParserService.FindById(id)
	if err != nil {
		return nil, err
	}
	if res.ResultCount != 1 {
		return nil, fmt.Errorf("expected 1 podcast, found %d", res.ResultCount)
	}
	return &res.Results[0], nil
}
