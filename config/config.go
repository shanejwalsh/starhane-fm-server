// Package config loads every setting the API, crawler and migrate commands
// need from the environment.
//
// There is no dotenv library here on purpose: the makefile loads .env for local
// development, and production reads real environment variables directly.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shanejwalsh/starhane-fm-server/logging"
)

// Defaults. Every one of these can be overridden by the matching env var.
const (
	DefaultPort = "8000"

	DefaultDBMaxConns = 10
	DefaultDBMinConns = 1

	DefaultCrawlerWorkers      = 8
	DefaultCrawlerBatchSize    = 20
	DefaultCrawlerPollInterval = 30 * time.Second
	DefaultCrawlerHTTPTimeout  = 30 * time.Second
	DefaultCrawlerMaxBodyBytes = 20 << 20 // 20 MiB
	DefaultCrawlerHostRPS      = 1.0
	DefaultCrawlerHostBurst    = 2
	DefaultCrawlerUserAgent    = "starhane-fm/1.0 (+https://github.com/shanejwalsh/starhane-fm-server)"

	DefaultMinCheckInterval = 1 * time.Hour
	DefaultMaxCheckInterval = 24 * time.Hour
	DefaultMaxFailures      = 10
	DefaultDeadRecheck      = 30 * 24 * time.Hour
	DefaultLeaseDuration    = 15 * time.Minute

	DefaultItunesTimeout   = 10 * time.Second
	DefaultSyncCrawlBudget = 20 * time.Second
)

// Config is the whole application's configuration.
type Config struct {
	Port        string
	DatabaseURL string
	Log         logging.Config
	DB          DB
	Crawler     Crawler
	Itunes      Itunes
}

// DB configures the pgx connection pool. Pool sizes are set explicitly because
// the API and the crawler share one database's connection limit.
type DB struct {
	MaxConns int32
	MinConns int32
}

// Crawler configures feed crawling. The same settings drive both the crawler
// binary and the API's synchronous first crawl.
type Crawler struct {
	Workers      int
	BatchSize    int
	PollInterval time.Duration

	HTTPTimeout  time.Duration
	MaxBodyBytes int64
	UserAgent    string

	// HostRPS and HostBurst bound how hard we hit any single host, so one
	// publisher serving hundreds of feeds is not hammered.
	HostRPS   float64
	HostBurst int

	// MinInterval and MaxInterval clamp adaptive scheduling.
	MinInterval time.Duration
	MaxInterval time.Duration

	// MaxFailures is how many consecutive failures mark a feed dead.
	MaxFailures int
	// DeadRecheck is how often a dead feed is retried.
	DeadRecheck time.Duration
	// LeaseDuration is how far ahead a claimed feed's next check is pushed, so
	// a crashed worker's feeds return to the queue rather than being lost.
	LeaseDuration time.Duration

	// SyncCrawlBudget bounds the API's first-request crawl of a cold feed.
	SyncCrawlBudget time.Duration
}

// Itunes configures the upstream iTunes Search API client.
type Itunes struct {
	Timeout time.Duration
	// SearchLimit caps results per search. Zero uses the iTunes default of 50.
	SearchLimit int
	Country     string
}

// Load reads configuration from the environment. It reports every problem it
// finds at once rather than failing on the first one.
func Load() (Config, error) {
	var errs []error
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := Config{
		Port:        stringOr("PORT", DefaultPort),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		Log:         logging.ConfigFromEnv(),
	}

	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		fail(errors.New("DATABASE_URL is required"))
	}

	maxConns, err := intOr("DB_MAX_CONNS", DefaultDBMaxConns)
	fail(err)
	minConns, err := intOr("DB_MIN_CONNS", DefaultDBMinConns)
	fail(err)
	if maxConns < 1 {
		fail(fmt.Errorf("DB_MAX_CONNS must be at least 1, got %d", maxConns))
	}
	if minConns > maxConns {
		fail(fmt.Errorf("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", minConns, maxConns))
	}
	cfg.DB = DB{MaxConns: int32(maxConns), MinConns: int32(minConns)}

	workers, err := intOr("CRAWLER_WORKERS", DefaultCrawlerWorkers)
	fail(err)
	if workers < 1 {
		fail(fmt.Errorf("CRAWLER_WORKERS must be at least 1, got %d", workers))
	}
	batchSize, err := intOr("CRAWLER_BATCH_SIZE", DefaultCrawlerBatchSize)
	fail(err)
	if batchSize < 1 {
		fail(fmt.Errorf("CRAWLER_BATCH_SIZE must be at least 1, got %d", batchSize))
	}
	pollInterval, err := durationOr("CRAWLER_POLL_INTERVAL", DefaultCrawlerPollInterval)
	fail(err)
	httpTimeout, err := durationOr("CRAWLER_HTTP_TIMEOUT", DefaultCrawlerHTTPTimeout)
	fail(err)
	maxBody, err := int64Or("CRAWLER_MAX_BODY_BYTES", DefaultCrawlerMaxBodyBytes)
	fail(err)
	if maxBody < 1 {
		fail(fmt.Errorf("CRAWLER_MAX_BODY_BYTES must be positive, got %d", maxBody))
	}
	hostRPS, err := floatOr("CRAWLER_HOST_RPS", DefaultCrawlerHostRPS)
	fail(err)
	if hostRPS <= 0 {
		fail(fmt.Errorf("CRAWLER_HOST_RPS must be positive, got %v", hostRPS))
	}
	hostBurst, err := intOr("CRAWLER_HOST_BURST", DefaultCrawlerHostBurst)
	fail(err)
	if hostBurst < 1 {
		fail(fmt.Errorf("CRAWLER_HOST_BURST must be at least 1, got %d", hostBurst))
	}
	minInterval, err := durationOr("CRAWLER_MIN_INTERVAL", DefaultMinCheckInterval)
	fail(err)
	maxInterval, err := durationOr("CRAWLER_MAX_INTERVAL", DefaultMaxCheckInterval)
	fail(err)
	if minInterval > maxInterval {
		fail(fmt.Errorf("CRAWLER_MIN_INTERVAL (%s) must not exceed CRAWLER_MAX_INTERVAL (%s)", minInterval, maxInterval))
	}
	maxFailures, err := intOr("CRAWLER_MAX_FAILURES", DefaultMaxFailures)
	fail(err)
	deadRecheck, err := durationOr("CRAWLER_DEAD_RECHECK", DefaultDeadRecheck)
	fail(err)
	lease, err := durationOr("CRAWLER_LEASE_DURATION", DefaultLeaseDuration)
	fail(err)
	syncBudget, err := durationOr("CRAWLER_SYNC_BUDGET", DefaultSyncCrawlBudget)
	fail(err)

	cfg.Crawler = Crawler{
		Workers:         workers,
		BatchSize:       batchSize,
		PollInterval:    pollInterval,
		HTTPTimeout:     httpTimeout,
		MaxBodyBytes:    maxBody,
		UserAgent:       stringOr("CRAWLER_USER_AGENT", DefaultCrawlerUserAgent),
		HostRPS:         hostRPS,
		HostBurst:       hostBurst,
		MinInterval:     minInterval,
		MaxInterval:     maxInterval,
		MaxFailures:     maxFailures,
		DeadRecheck:     deadRecheck,
		LeaseDuration:   lease,
		SyncCrawlBudget: syncBudget,
	}

	itunesTimeout, err := durationOr("ITUNES_TIMEOUT", DefaultItunesTimeout)
	fail(err)
	searchLimit, err := intOr("ITUNES_SEARCH_LIMIT", 0)
	fail(err)
	cfg.Itunes = Itunes{
		Timeout:     itunesTimeout,
		SearchLimit: searchLimit,
		Country:     os.Getenv("ITUNES_COUNTRY"),
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func stringOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func intOr(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback, fmt.Errorf("%s: %q is not an integer", key, raw)
	}
	return v, nil
}

func int64Or(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback, fmt.Errorf("%s: %q is not an integer", key, raw)
	}
	return v, nil
}

func floatOr(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback, fmt.Errorf("%s: %q is not a number", key, raw)
	}
	return v, nil
}

func durationOr(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fallback, fmt.Errorf("%s: %q is not a duration (try 30s, 5m, 24h)", key, raw)
	}
	return v, nil
}
