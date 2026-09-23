package crawl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrBodyTooLarge reports that a feed exceeded the configured size cap. It is a
// failure rather than a truncated parse: half a feed would look like a feed
// whose episodes had been deleted.
var ErrBodyTooLarge = errors.New("feed body exceeded the size limit")

// Outcome classifies what a fetch produced.
type Outcome string

const (
	// OutcomeChanged means a new body was retrieved and should be parsed.
	OutcomeChanged Outcome = "changed"
	// OutcomeNotModified means the feed has not changed, either because the
	// server said 304 or because the body hashed to what we already had.
	OutcomeNotModified Outcome = "not_modified"
	// OutcomeMoved means the feed permanently moved to another URL.
	OutcomeMoved Outcome = "moved"
	// OutcomeGone means the feed is permanently unavailable.
	OutcomeGone Outcome = "gone"
	// OutcomeRateLimited means the host asked us to back off.
	OutcomeRateLimited Outcome = "rate_limited"
	// OutcomeFailed means a transport error or an unhelpful status code.
	OutcomeFailed Outcome = "failed"
)

// FetchResult is what one conditional GET produced.
type FetchResult struct {
	Outcome    Outcome
	StatusCode int

	// Body and BodyHash are set only when Outcome is OutcomeChanged.
	Body     []byte
	BodyHash []byte

	// ETag and LastModified are the validators to store for next time.
	ETag         string
	LastModified string

	// NewURL is set when Outcome is OutcomeMoved.
	NewURL string

	// RetryAfter is set when the host asked for a specific delay.
	RetryAfter time.Duration

	Err error
}

// Fetcher performs conditional, size-capped, rate-limited feed fetches.
type Fetcher struct {
	client       *http.Client
	limiter      *HostLimiter
	userAgent    string
	maxBodyBytes int64
}

// FetcherOptions configures a Fetcher.
type FetcherOptions struct {
	Timeout      time.Duration
	UserAgent    string
	MaxBodyBytes int64
	HostRPS      float64
	HostBurst    int
}

// NewFetcher builds a Fetcher with its own HTTP client.
//
// The client does not follow permanent redirects. A 301 or 308 is information
// worth keeping — it means the feed has moved and the stored URL should change
// — so it is surfaced rather than transparently followed.
func NewFetcher(opts FetcherOptions) *Fetcher {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: opts.Timeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			// Permanent redirects are reported to the caller; temporary ones
			// are followed as usual.
			if req.Response != nil {
				switch req.Response.StatusCode {
				case http.StatusMovedPermanently, http.StatusPermanentRedirect:
					return http.ErrUseLastResponse
				}
			}
			return nil
		},
	}

	return &Fetcher{
		client:       client,
		limiter:      NewHostLimiter(opts.HostRPS, opts.HostBurst),
		userAgent:    opts.UserAgent,
		maxBodyBytes: opts.MaxBodyBytes,
	}
}

// Fetch retrieves feedURL, sending the validators from a previous fetch so an
// unchanged feed costs a 304 rather than a download.
func (f *Fetcher) Fetch(ctx context.Context, feedURL, etag, lastModified string, knownHash []byte) FetchResult {
	if err := f.limiter.Wait(ctx, feedURL); err != nil {
		return FetchResult{Outcome: OutcomeFailed, Err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return FetchResult{Outcome: OutcomeFailed, Err: err}
	}

	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, text/xml;q=0.8, */*;q=0.5")
	// Accept-Encoding is deliberately not set: Transport adds gzip itself and
	// decompresses transparently, but only while the header is untouched.
	// Setting it by hand would hand us compressed bytes to hash and parse.
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	res, err := f.client.Do(req)
	if err != nil {
		return FetchResult{Outcome: OutcomeFailed, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
		res.Body.Close()
	}()

	result := FetchResult{
		StatusCode:   res.StatusCode,
		ETag:         res.Header.Get("ETag"),
		LastModified: res.Header.Get("Last-Modified"),
	}

	switch {
	case res.StatusCode == http.StatusNotModified:
		// Keep the validators we already had: a 304 need not repeat them.
		if result.ETag == "" {
			result.ETag = etag
		}
		if result.LastModified == "" {
			result.LastModified = lastModified
		}
		result.Outcome = OutcomeNotModified
		return result

	case res.StatusCode == http.StatusMovedPermanently || res.StatusCode == http.StatusPermanentRedirect:
		location := res.Header.Get("Location")
		target, err := resolveRedirect(feedURL, location)
		if err != nil {
			result.Outcome = OutcomeFailed
			result.Err = fmt.Errorf("permanent redirect to unusable location %q: %w", location, err)
			return result
		}
		result.Outcome = OutcomeMoved
		result.NewURL = target
		return result

	case res.StatusCode == http.StatusGone:
		result.Outcome = OutcomeGone
		result.Err = fmt.Errorf("feed is gone (410)")
		return result

	case res.StatusCode == http.StatusTooManyRequests:
		result.Outcome = OutcomeRateLimited
		result.RetryAfter = parseRetryAfter(res.Header.Get("Retry-After"), time.Now())
		result.Err = fmt.Errorf("rate limited (429)")
		return result

	case res.StatusCode < 200 || res.StatusCode > 299:
		result.Outcome = OutcomeFailed
		result.Err = fmt.Errorf("unexpected status %s", res.Status)
		return result
	}

	body, err := readCapped(res.Body, f.maxBodyBytes)
	if err != nil {
		result.Outcome = OutcomeFailed
		result.Err = err
		return result
	}

	sum := sha256.Sum256(body)
	result.BodyHash = sum[:]

	// Plenty of hosts ignore conditional GET and send a full body regardless.
	// Comparing hashes is what actually saves the parse.
	if len(knownHash) > 0 && bytes.Equal(knownHash, result.BodyHash) {
		result.Outcome = OutcomeNotModified
		return result
	}

	result.Outcome = OutcomeChanged
	result.Body = body
	return result
}

// readCapped reads at most limit bytes, reporting ErrBodyTooLarge if there were
// more. It reads one byte past the limit to tell "exactly at the cap" from
// "over it".
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading feed body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, limit)
	}
	return body, nil
}

// resolveRedirect turns a Location header, which may be relative, into an
// absolute URL.
func resolveRedirect(from, location string) (string, error) {
	if location == "" {
		return "", errors.New("no Location header")
	}

	base, err := url.Parse(from)
	if err != nil {
		return "", err
	}
	target, err := base.Parse(location)
	if err != nil {
		return "", err
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", target.Scheme)
	}
	return target.String(), nil
}

// parseRetryAfter reads a Retry-After header, which may be either a number of
// seconds or an HTTP date. It returns 0 when the header is absent or unusable,
// leaving the caller to apply its own backoff.
func parseRetryAfter(header string, now time.Time) time.Duration {
	if header == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}

	if when, err := http.ParseTime(header); err == nil {
		if delay := when.Sub(now); delay > 0 {
			return delay
		}
		return 0
	}
	return 0
}
