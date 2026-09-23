package logging

import (
	"context"
	"log/slog"
	"sync"
)

// CacheResult says where a response's data came from.
type CacheResult string

const (
	// CacheHit means the response was served entirely from Postgres, with no
	// upstream call.
	CacheHit CacheResult = "hit"
	// CacheMiss means something had to be fetched from iTunes or a feed before
	// the response could be served.
	CacheMiss CacheResult = "miss"
	// CacheBypass means the endpoint always goes upstream. Search does, until
	// iTunes responses are cached.
	CacheBypass CacheResult = "bypass"
)

type annotationsKey struct{}

// annotations collects attributes a handler wants on the request's summary
// line. The middleware writes that line after the handler returns, so the
// attributes have to be gathered somewhere both can reach.
type annotations struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func (a *annotations) add(attrs ...slog.Attr) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attrs = append(a.attrs, attrs...)
}

func (a *annotations) collect() []slog.Attr {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attrs
}

// withAnnotations returns a context carrying a fresh annotation collector.
func withAnnotations(ctx context.Context) (context.Context, *annotations) {
	a := &annotations{}
	return context.WithValue(ctx, annotationsKey{}, a), a
}

// Annotate adds attributes to the request-completed summary line.
//
// It is how a handler reports something the middleware cannot see for itself —
// whether the catalogue could answer the request, say — without emitting a
// second log line for it. Outside a request it does nothing.
func Annotate(ctx context.Context, attrs ...slog.Attr) {
	if a, ok := ctx.Value(annotationsKey{}).(*annotations); ok {
		a.add(attrs...)
	}
}

// AnnotateCache records where a response's data came from.
func AnnotateCache(ctx context.Context, result CacheResult, attrs ...slog.Attr) {
	Annotate(ctx, append([]slog.Attr{slog.String("cache", string(result))}, attrs...)...)
}
