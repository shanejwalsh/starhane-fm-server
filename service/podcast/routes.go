package podcast

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"
	"golang.org/x/sync/singleflight"

	"github.com/shanejwalsh/starhane-fm-server/crawl"
	"github.com/shanejwalsh/starhane-fm-server/logging"
	"github.com/shanejwalsh/starhane-fm-server/store"
	"github.com/shanejwalsh/starhane-fm-server/types"
	"github.com/shanejwalsh/starhane-fm-server/utils"
)

const (
	INDEX_PATH    = "/"
	SERVICE_PATH  = "/podcasts"
	PODCAST_PATH  = "/{podcastId}"
	EPISODES_PATH = "/{podcastId}/episodes"
)

// ItunesService is the part of the iTunes client the handlers use. It is an
// interface so handlers can be tested without reaching the network.
type ItunesService interface {
	SearchWithContext(ctx context.Context, params itunes.SearchParams) (itunes.SearchResponse, error)
	FindByIdWithContext(ctx context.Context, id int) (itunes.SearchResponse, error)
}

// Catalogue is the part of the store the handlers use.
type Catalogue interface {
	UpsertPodcasts(ctx context.Context, podcasts []store.PodcastUpsert) error
	UpsertPodcastWithFeed(ctx context.Context, podcast store.PodcastUpsert) (store.Podcast, *store.Feed, error)
	PodcastWithFeedByItunesID(ctx context.Context, itunesID int64) (store.Podcast, *store.Feed, error)
	EpisodesByFeed(ctx context.Context, feedID int64) ([]store.Episode, error)
}

// FeedCrawler crawls a single feed. The API uses it only to fill a feed it has
// never seen before; everything after that is the crawler service's job.
type FeedCrawler interface {
	CrawlFeed(ctx context.Context, feed store.Feed) (crawl.Report, error)
}

type Handler struct {
	itunesParserService ItunesService
	catalogue           Catalogue
	crawler             FeedCrawler

	searchParams    itunes.SearchParams
	syncCrawlBudget time.Duration

	// coldCrawls collapses concurrent first-requests for the same feed into a
	// single crawl. Duplicate crawls across processes are harmless — every
	// write is an idempotent upsert — so this need not be distributed.
	coldCrawls singleflight.Group
}

// Options configures a Handler.
type Options struct {
	SearchLimit     int
	SearchCountry   string
	SyncCrawlBudget time.Duration
}

func NewHandler(ias ItunesService, catalogue Catalogue, crawler FeedCrawler, opts Options) *Handler {
	return &Handler{
		itunesParserService: ias,
		catalogue:           catalogue,
		crawler:             crawler,
		searchParams: itunes.SearchParams{
			Limit:   opts.SearchLimit,
			Country: opts.SearchCountry,
		},
		syncCrawlBudget: opts.SyncCrawlBudget,
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

	ctx := req.Context()
	searchTerm := req.URL.Query().Get("searchTerm")
	logger := logging.FromContext(ctx).With(slog.String("search_term", searchTerm))

	params := h.searchParams
	params.Term = searchTerm

	itunesRes, err := h.itunesParserService.SearchWithContext(ctx, params)

	if err != nil {
		logger.ErrorContext(ctx, "itunes search failed", slog.Any("error", err))
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	podcasts := make([]types.Podcast, len(itunesRes.Results))

	for i, podcast := range itunesRes.Results {
		podcasts[i] = utils.MapPodcast(&podcast)
	}

	h.rememberPodcasts(ctx, logger, itunesRes.Results)

	// Search always goes upstream: iTunes response caching is deliberately not
	// part of this phase.
	logging.AnnotateCache(ctx, logging.CacheBypass, slog.Int("results", len(podcasts)))

	logger.DebugContext(ctx, "itunes search succeeded", slog.Int("results", len(podcasts)))

	utils.WriteJson(res, http.StatusOK, podcasts)
}

func (h *Handler) getPodcast(res http.ResponseWriter, req *http.Request) {

	ctx := req.Context()
	vars := mux.Vars(req)
	podcastId := vars["podcastId"]
	logger := logging.FromContext(ctx).With(slog.String("podcast_id", podcastId))
	parsedId, err := strconv.Atoi(podcastId)

	if err != nil {
		logger.WarnContext(ctx, "invalid podcast id", slog.Any("error", err))
		utils.WriteJson(res, http.StatusBadRequest, err.Error())
		return
	}

	podcast, err := h.lookupPodcast(ctx, parsedId)

	if err != nil {
		logger.WarnContext(ctx, "podcast lookup failed", slog.Any("error", err))
		utils.WriteJson(res, http.StatusNotFound, err.Error())
		return
	}

	h.rememberPodcasts(ctx, logger, []itunes.Result{*podcast})

	logging.AnnotateCache(ctx, logging.CacheBypass)

	utils.WriteJson(res, http.StatusOK, utils.MapPodcast(podcast))
}

func (h *Handler) getEpisodes(res http.ResponseWriter, req *http.Request) {

	ctx := req.Context()
	vars := mux.Vars(req)
	podcastId := vars["podcastId"]
	logger := logging.FromContext(ctx).With(slog.String("podcast_id", podcastId))
	parsedId, err := strconv.Atoi(podcastId)

	if err != nil {
		logger.WarnContext(ctx, "invalid podcast id", slog.Any("error", err))
		utils.WriteJson(res, http.StatusBadRequest, err.Error())
		return
	}

	feed, fromCatalogue, err := h.resolveFeed(ctx, logger, int64(parsedId))

	if err != nil {
		if errors.Is(err, errNoFeed) {
			// A podcast with no feed URL could never be served, which is the
			// same failure this endpoint has always reported for one.
			logger.ErrorContext(ctx, "podcast has no feed url", slog.Any("error", err))
			utils.WriteJson(res, http.StatusInternalServerError, err.Error())
			return
		}
		logger.WarnContext(ctx, "podcast lookup failed", slog.Any("error", err))
		utils.WriteJson(res, http.StatusNotFound, err.Error())
		return
	}

	logger = logger.With(slog.Int64("feed_id", feed.ID))

	// A feed nobody has crawled yet is filled in now, once. After that the
	// crawler keeps it fresh and this endpoint only reads.
	crawled := feed.NeverCrawled()
	if crawled {
		if err := h.crawlColdFeed(ctx, logger, *feed); err != nil {
			logger.ErrorContext(ctx, "fetching rss feed failed",
				slog.String("feed_url", feed.URL),
				slog.Any("error", err),
			)
			utils.WriteJson(res, http.StatusInternalServerError, err.Error())
			return
		}
	}

	stored, err := h.catalogue.EpisodesByFeed(ctx, feed.ID)

	if err != nil {
		logger.ErrorContext(ctx, "reading episodes failed", slog.Any("error", err))
		utils.WriteJson(res, http.StatusInternalServerError, err.Error())
		return
	}

	episodes := make([]types.EpisodeResponse, len(stored))

	for i := range stored {
		episodes[i] = utils.MapStoredEpisode(&stored[i])
	}

	// Say plainly whether this request cost an upstream call. A hit is the
	// whole point of the catalogue; a miss should be rare after the first
	// request for a podcast.
	count := slog.Int("episodes", len(episodes))
	switch {
	case !fromCatalogue:
		// The podcast was not known, so iTunes had to be asked.
		logging.AnnotateCache(ctx, logging.CacheMiss, slog.String("miss_reason", "unknown_podcast"), count)
	case crawled:
		// Known podcast, but its feed had never been fetched.
		logging.AnnotateCache(ctx, logging.CacheMiss, slog.String("miss_reason", "uncrawled_feed"), count)
	default:
		logging.AnnotateCache(ctx, logging.CacheHit, count)
	}

	logger.DebugContext(ctx, "episodes served", slog.Int("episodes", len(episodes)))

	utils.WriteJson(res, http.StatusOK, episodes)
}

// errNoFeed means the podcast exists but has no feed URL to crawl.
var errNoFeed = errors.New("podcast has no feed url")

// resolveFeed finds a podcast's feed, preferring the catalogue and falling back
// to iTunes. It reports whether the catalogue could answer without going
// upstream.
//
// Reading the catalogue first is the whole point of phase 1: iTunes allows
// roughly twenty requests a minute per IP and every user shares this server's.
func (h *Handler) resolveFeed(ctx context.Context, logger *slog.Logger, itunesID int64) (*store.Feed, bool, error) {
	_, feed, err := h.catalogue.PodcastWithFeedByItunesID(ctx, itunesID)
	switch {
	case err == nil && feed != nil:
		return feed, true, nil

	case err == nil && feed == nil:
		// Known podcast, no feed. Ask iTunes once more in case it has since
		// published one.

	case errors.Is(err, pgx.ErrNoRows):
		// Not in the catalogue yet.

	default:
		logger.WarnContext(ctx, "could not read the catalogue, falling back to itunes",
			slog.Any("error", err))
	}

	result, err := h.lookupPodcast(ctx, int(itunesID))
	if err != nil {
		return nil, false, err
	}
	if !isPodcast(result) {
		// An iTunes ID that belongs to an artist or an album resolves fine but
		// has no feed. Storing it would put a row keyed on collectionId 0 in
		// the catalogue, which every other non-podcast would then collide with.
		return nil, false, errNoFeed
	}

	_, upserted, err := h.catalogue.UpsertPodcastWithFeed(ctx, utils.MapPodcastUpsert(result))
	if err != nil {
		return nil, false, fmt.Errorf("storing podcast: %w", err)
	}
	if upserted == nil {
		return nil, false, errNoFeed
	}
	return upserted, false, nil
}

// isPodcast reports whether an iTunes result is a podcast worth cataloguing.
//
// Search pins entity=podcast, but a lookup will happily resolve an artist or an
// album ID, and those come back with no collectionId and no feed URL.
func isPodcast(result *itunes.Result) bool {
	if result.CollectionID == 0 {
		return false
	}
	return result.Kind == "podcast" || result.FeedURL != ""
}

// crawlColdFeed crawls a feed the catalogue has never filled, so the first
// request for a podcast still returns its episodes.
func (h *Handler) crawlColdFeed(ctx context.Context, logger *slog.Logger, feed store.Feed) error {
	budget, cancel := context.WithTimeout(ctx, h.syncCrawlBudget)
	defer cancel()

	key := strconv.FormatInt(feed.ID, 10)
	_, err, shared := h.coldCrawls.Do(key, func() (any, error) {
		logger.InfoContext(budget, "crawling a feed for the first time", slog.String("feed_url", feed.URL))
		report, err := h.crawler.CrawlFeed(budget, feed)
		if err != nil {
			return nil, err
		}
		if report.Err != nil {
			return nil, report.Err
		}
		return nil, nil
	})

	if shared {
		logger.DebugContext(ctx, "joined an in-flight first crawl")
	}
	return err
}

// rememberPodcasts stores what iTunes just told us and schedules the feeds for
// crawling. This is the lazy catalogue: browsing the API is what fills it.
//
// A failure here is logged but never fails the request — the caller got their
// answer from iTunes regardless.
func (h *Handler) rememberPodcasts(ctx context.Context, logger *slog.Logger, results []itunes.Result) {
	if len(results) == 0 {
		return
	}

	upserts := make([]store.PodcastUpsert, 0, len(results))
	for i := range results {
		if !isPodcast(&results[i]) {
			continue
		}
		upserts = append(upserts, utils.MapPodcastUpsert(&results[i]))
	}
	if len(upserts) == 0 {
		return
	}

	if err := h.catalogue.UpsertPodcasts(ctx, upserts); err != nil {
		logger.WarnContext(ctx, "could not store podcasts in the catalogue",
			slog.Int("podcasts", len(upserts)),
			slog.Any("error", err),
		)
		return
	}
	logger.DebugContext(ctx, "catalogue updated", slog.Int("podcasts", len(upserts)))
}

func (h *Handler) lookupPodcast(ctx context.Context, id int) (*itunes.Result, error) {
	res, err := h.itunesParserService.FindByIdWithContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if res.ResultCount != 1 {
		return nil, fmt.Errorf("expected 1 podcast, found %d", res.ResultCount)
	}
	return &res.Results[0], nil
}
