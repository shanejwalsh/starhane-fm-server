package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestMiddlewareLogsRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Level: "debug", Format: "json"})

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		FromContext(r.Context()).InfoContext(r.Context(), "in handler")
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/brew?kind=earl-grey", nil)
	req.Header.Set(RequestIDHeader, "abc123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); got != "abc123" {
		t.Fatalf("response %s = %q, want %q", RequestIDHeader, got, "abc123")
	}

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2:\n%s", len(lines), buf.String())
	}

	var inHandler, summary map[string]any
	if err := json.Unmarshal(lines[0], &inHandler); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &summary); err != nil {
		t.Fatal(err)
	}

	if inHandler["request_id"] != "abc123" {
		t.Errorf("handler log request_id = %v, want abc123", inHandler["request_id"])
	}

	want := map[string]any{
		"msg":        "request completed",
		"level":      "WARN",
		"request_id": "abc123",
		"method":     "GET",
		"path":       "/brew",
		"query":      "kind=earl-grey",
		"status":     float64(http.StatusTeapot),
		"bytes":      float64(5),
	}
	for k, v := range want {
		if summary[k] != v {
			t.Errorf("summary[%q] = %v, want %v", k, summary[k], v)
		}
	}
}

func TestMiddlewareRecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{})

	handler := Middleware(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Error("expected a generated request ID")
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"msg":"panic recovered"`)) {
		t.Errorf("panic not logged:\n%s", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]string{"": "INFO", "debug": "DEBUG", "WARN": "WARN", "error": "ERROR", "nonsense": "INFO"}
	for in, want := range cases {
		if got := parseLevel(in).String(); got != want {
			t.Errorf("parseLevel(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestMiddlewareIncludesHandlerAnnotations(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Level: "debug", Format: "json"})

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AnnotateCache(r.Context(), CacheHit, slog.Int("episodes", 13))
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/episodes", nil))

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	// The annotation must ride on the existing summary line rather than adding
	// a second one — one line per request is the whole point.
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1:\n%s", len(lines), buf.String())
	}

	var summary map[string]any
	if err := json.Unmarshal(lines[0], &summary); err != nil {
		t.Fatal(err)
	}
	if summary["cache"] != "hit" {
		t.Errorf("cache = %v, want hit", summary["cache"])
	}
	if summary["episodes"] != float64(13) {
		t.Errorf("episodes = %v, want 13", summary["episodes"])
	}
}

func TestAnnotateOutsideARequestIsANoop(t *testing.T) {
	// Handlers share code with the crawler, which has no request context.
	// Annotating there must not panic.
	AnnotateCache(context.Background(), CacheMiss)
	Annotate(context.Background(), slog.String("k", "v"))
}

func TestMiddlewareAnnotationsAreConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json"})

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				Annotate(r.Context(), slog.Int("n", i))
			}()
		}
		wg.Wait()
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var summary map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &summary); err != nil {
		t.Fatalf("summary line is not valid JSON: %v\n%s", err, buf.String())
	}
}
