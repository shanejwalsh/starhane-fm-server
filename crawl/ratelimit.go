package crawl

import (
	"context"
	"net/url"
	"sync"

	"golang.org/x/time/rate"
)

// HostLimiter rate-limits per host.
//
// One publisher often serves hundreds of feeds from one host, so a global limit
// would be both too slow across hosts and too fast against any single one.
type HostLimiter struct {
	rps   rate.Limit
	burst int

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

// NewHostLimiter allows rps requests per second to each host, with the given
// burst.
func NewHostLimiter(rps float64, burst int) *HostLimiter {
	if burst < 1 {
		burst = 1
	}
	return &HostLimiter{
		rps:      rate.Limit(rps),
		burst:    burst,
		limiters: make(map[string]*rate.Limiter),
	}
}

// Wait blocks until a request to rawURL's host is allowed, or ctx is done.
func (h *HostLimiter) Wait(ctx context.Context, rawURL string) error {
	return h.forHost(hostOf(rawURL)).Wait(ctx)
}

func (h *HostLimiter) forHost(host string) *rate.Limiter {
	h.mu.Lock()
	defer h.mu.Unlock()

	limiter, ok := h.limiters[host]
	if !ok {
		limiter = rate.NewLimiter(h.rps, h.burst)
		h.limiters[host] = limiter
	}
	return limiter
}

// hostOf extracts a URL's host. An unparseable URL gets its own bucket keyed by
// the raw string, which is harmless: the fetch is about to fail anyway.
func hostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	return parsed.Host
}
