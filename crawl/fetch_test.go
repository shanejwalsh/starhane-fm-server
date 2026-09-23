package crawl

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testFeedBody = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Test</title></channel></rss>`

func testFetcher() *Fetcher {
	return NewFetcher(FetcherOptions{
		Timeout:      5 * time.Second,
		UserAgent:    "starhane-fm-test/1.0 (+https://example.com)",
		MaxBodyBytes: 1 << 20,
		// Fast enough not to slow the tests, still exercising the limiter.
		HostRPS:   1000,
		HostBurst: 100,
	})
}

func fetch(t *testing.T, f *Fetcher, url, etag, lastModified string, knownHash []byte) FetchResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return f.Fetch(ctx, url, etag, lastModified, knownHash)
}

func TestFetchChangedBodyReturnsHashAndValidators(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Wed, 01 Jan 2025 00:00:00 GMT")
		_, _ = w.Write([]byte(testFeedBody))
	}))
	defer srv.Close()

	got := fetch(t, testFetcher(), srv.URL, "", "", nil)

	if got.Outcome != OutcomeChanged {
		t.Fatalf("outcome = %q, want %q (err: %v)", got.Outcome, OutcomeChanged, got.Err)
	}
	if string(got.Body) != testFeedBody {
		t.Errorf("body = %q, want the feed document", string(got.Body))
	}
	want := sha256.Sum256([]byte(testFeedBody))
	if string(got.BodyHash) != string(want[:]) {
		t.Error("body hash does not match the body")
	}
	if got.ETag != `"v1"` {
		t.Errorf("etag = %q, want %q", got.ETag, `"v1"`)
	}
	if got.LastModified != "Wed, 01 Jan 2025 00:00:00 GMT" {
		t.Errorf("last-modified = %q, unexpected", got.LastModified)
	}
}

func TestFetchSendsConditionalHeadersAndHandles304(t *testing.T) {
	var gotINM, gotIMS, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotINM = r.Header.Get("If-None-Match")
		gotIMS = r.Header.Get("If-Modified-Since")
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	got := fetch(t, testFetcher(), srv.URL, `"v1"`, "Wed, 01 Jan 2025 00:00:00 GMT", nil)

	if got.Outcome != OutcomeNotModified {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeNotModified)
	}
	if gotINM != `"v1"` {
		t.Errorf("If-None-Match = %q, want %q", gotINM, `"v1"`)
	}
	if gotIMS != "Wed, 01 Jan 2025 00:00:00 GMT" {
		t.Errorf("If-Modified-Since = %q, unexpected", gotIMS)
	}
	// A descriptive agent with a contact URL is what lets a publisher tell us
	// apart from a scraper.
	if !strings.Contains(gotUA, "starhane-fm-test") || !strings.Contains(gotUA, "https://") {
		t.Errorf("User-Agent = %q, want a descriptive agent with a contact URL", gotUA)
	}
	// A 304 need not repeat the validators, so the old ones must be kept.
	if got.ETag != `"v1"` {
		t.Errorf("etag after 304 = %q, want the previous %q to be kept", got.ETag, `"v1"`)
	}
	if got.Body != nil {
		t.Error("a 304 should carry no body")
	}
}

func TestFetchTreatsUnchangedBodyAsNotModified(t *testing.T) {
	// Plenty of hosts ignore conditional GET and send 200 with the same bytes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(testFeedBody))
	}))
	defer srv.Close()

	sum := sha256.Sum256([]byte(testFeedBody))
	got := fetch(t, testFetcher(), srv.URL, "", "", sum[:])

	if got.Outcome != OutcomeNotModified {
		t.Fatalf("outcome = %q, want %q — the body hash should have short-circuited the parse", got.Outcome, OutcomeNotModified)
	}
	if got.Body != nil {
		t.Error("an unchanged body should not be handed back for parsing")
	}
}

func TestFetchReportsPermanentRedirectsWithoutFollowing(t *testing.T) {
	var target *httptest.Server
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(testFeedBody))
	}))
	defer target.Close()

	for _, status := range []int{http.StatusMovedPermanently, http.StatusPermanentRedirect} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/moved.xml", status)
		}))

		got := fetch(t, testFetcher(), srv.URL, "", "", nil)
		srv.Close()

		if got.Outcome != OutcomeMoved {
			t.Errorf("status %d: outcome = %q, want %q", status, got.Outcome, OutcomeMoved)
		}
		if got.NewURL != target.URL+"/moved.xml" {
			t.Errorf("status %d: new url = %q, want %q", status, got.NewURL, target.URL+"/moved.xml")
		}
	}
}

func TestFetchFollowsTemporaryRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(testFeedBody))
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	got := fetch(t, testFetcher(), srv.URL, "", "", nil)

	// A temporary redirect says nothing about where the feed lives, so it is
	// followed rather than recorded.
	if got.Outcome != OutcomeChanged {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeChanged)
	}
	if string(got.Body) != testFeedBody {
		t.Error("expected the body from the redirect target")
	}
}

func TestFetchResolvesRelativeRedirectLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/new/feed.xml")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	got := fetch(t, testFetcher(), srv.URL+"/old/feed.xml", "", "", nil)

	if got.Outcome != OutcomeMoved {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeMoved)
	}
	if got.NewURL != srv.URL+"/new/feed.xml" {
		t.Errorf("new url = %q, want it resolved against the old one", got.NewURL)
	}
}

func TestFetchHonoursRetryAfterOn429(t *testing.T) {
	t.Run("delta seconds", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		got := fetch(t, testFetcher(), srv.URL, "", "", nil)
		if got.Outcome != OutcomeRateLimited {
			t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeRateLimited)
		}
		if got.RetryAfter != 2*time.Minute {
			t.Errorf("retry after = %s, want 2m", got.RetryAfter)
		}
	})

	t.Run("http date", func(t *testing.T) {
		when := time.Now().Add(90 * time.Second).UTC()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", when.Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		got := fetch(t, testFetcher(), srv.URL, "", "", nil)
		if got.Outcome != OutcomeRateLimited {
			t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeRateLimited)
		}
		// Allow slack for the round trip and second-granularity formatting.
		if got.RetryAfter < 80*time.Second || got.RetryAfter > 95*time.Second {
			t.Errorf("retry after = %s, want roughly 90s", got.RetryAfter)
		}
	})

	t.Run("absent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		got := fetch(t, testFetcher(), srv.URL, "", "", nil)
		if got.Outcome != OutcomeRateLimited {
			t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeRateLimited)
		}
		if got.RetryAfter != 0 {
			t.Errorf("retry after = %s, want 0 so the caller applies its own backoff", got.RetryAfter)
		}
	})
}

func TestFetchReportsGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	got := fetch(t, testFetcher(), srv.URL, "", "", nil)
	if got.Outcome != OutcomeGone {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeGone)
	}
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	big := strings.Repeat("a", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{
		Timeout:      5 * time.Second,
		UserAgent:    "test",
		MaxBodyBytes: 1000,
		HostRPS:      1000,
		HostBurst:    100,
	})

	got := fetch(t, f, srv.URL, "", "", nil)

	if got.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeFailed)
	}
	// An oversized feed must fail rather than parse as a truncated document,
	// which would look like a feed whose episodes had been deleted.
	if !errors.Is(got.Err, ErrBodyTooLarge) {
		t.Errorf("error = %v, want ErrBodyTooLarge", got.Err)
	}
	if got.Body != nil {
		t.Error("an oversized response should not yield a partial body")
	}
}

func TestFetchAcceptsBodyExactlyAtTheLimit(t *testing.T) {
	body := strings.Repeat("a", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{
		Timeout: 5 * time.Second, UserAgent: "test", MaxBodyBytes: 1000, HostRPS: 1000, HostBurst: 100,
	})

	got := fetch(t, f, srv.URL, "", "", nil)
	if got.Outcome != OutcomeChanged {
		t.Fatalf("outcome = %q, want %q (err: %v)", got.Outcome, OutcomeChanged, got.Err)
	}
	if len(got.Body) != 1000 {
		t.Errorf("body length = %d, want 1000", len(got.Body))
	}
}

func TestFetchReportsServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))

		got := fetch(t, testFetcher(), srv.URL, "", "", nil)
		srv.Close()

		if got.Outcome != OutcomeFailed {
			t.Errorf("status %d: outcome = %q, want %q", status, got.Outcome, OutcomeFailed)
		}
		if got.StatusCode != status {
			t.Errorf("status %d: reported %d", status, got.StatusCode)
		}
	}
}

func TestFetchHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := testFetcher().Fetch(ctx, srv.URL, "", "", nil)
	if got.Outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeFailed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"60":                            time.Minute,
		"0":                             0,
		"-5":                            0,
		"tomorrow":                      0,
		"Wed, 01 Jan 2025 12:01:00 GMT": time.Minute,
		// A date already in the past means "retry now", not "retry in the past".
		"Wed, 01 Jan 2025 11:00:00 GMT": 0,
	}
	for header, want := range cases {
		if got := parseRetryAfter(header, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", header, got, want)
		}
	}
}

func TestHostLimiterIsPerHost(t *testing.T) {
	limiter := NewHostLimiter(1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// The first request to each host consumes that host's burst, so two
	// different hosts both proceed immediately. A single shared limiter would
	// have made the second wait a second.
	start := time.Now()
	if err := limiter.Wait(ctx, "https://a.example.com/feed.xml"); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Wait(ctx, "https://b.example.com/feed.xml"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("two different hosts took %s, want them not to share a limiter", elapsed)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://example.com/feed.xml":     "example.com",
		"http://example.com:8080/feed.xml": "example.com:8080",
		"https://sub.example.com/a/b?c=d":  "sub.example.com",
		"not a url":                        "not a url",
		"":                                 "",
	}
	for raw, want := range cases {
		if got := hostOf(raw); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", raw, got, want)
		}
	}
}
