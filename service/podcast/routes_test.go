package podcast

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/shanejwalsh/itunes-xml-parser/feeds"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGetPodcastsNoSearchTerm(t *testing.T) {
	for name, body := range map[string]string{
		"empty results":   `{"resultCount":0,"results":[]}`,
		"missing results": `{"resultCount":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			var upstream string
			orig := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				upstream = r.URL.String()
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = orig })

			router := mux.NewRouter()
			NewHandler(itunes.NewItunesApiServices(), feeds.NewRssFeedService()).RegisterRoutes(router)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/podcasts/", nil))

			t.Logf("upstream: %s", upstream)
			t.Logf("response: %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))

			if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
				t.Errorf("got %d %q, want 200 []", rec.Code, rec.Body.String())
			}
		})
	}
}
