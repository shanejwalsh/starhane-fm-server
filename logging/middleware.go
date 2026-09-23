package logging

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// RequestIDHeader is the header used to read and propagate request IDs.
const RequestIDHeader = "X-Request-ID"

// Middleware attaches a request-scoped logger (tagged with a request ID) to
// each request's context, recovers from panics, and logs a summary line once
// the request completes.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			requestID := r.Header.Get(RequestIDHeader)
			if requestID == "" {
				requestID = newRequestID()
			}
			w.Header().Set(RequestIDHeader, requestID)

			reqLogger := logger.With(slog.String("request_id", requestID))
			ctx := WithContext(r.Context(), reqLogger)
			ctx, annotations := withAnnotations(ctx)

			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					reqLogger.ErrorContext(ctx, "panic recovered",
						slog.Any("panic", rec),
						slog.String("stack", string(debug.Stack())),
					)
					if !rw.wroteHeader {
						http.Error(rw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					}
				}

				level := slog.LevelInfo
				switch {
				case rw.status >= 500:
					level = slog.LevelError
				case rw.status >= 400:
					level = slog.LevelWarn
				}

				attrs := []slog.Attr{
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("query", r.URL.RawQuery),
					slog.String("remote_addr", r.RemoteAddr),
					slog.String("user_agent", r.UserAgent()),
					slog.Int("status", rw.status),
					slog.Int("bytes", rw.bytes),
					slog.Duration("duration", time.Since(start)),
				}
				// Whatever the handler reported about itself, so the summary
				// line says where the data came from rather than needing a
				// second line for it.
				attrs = append(attrs, annotations.collect()...)

				reqLogger.LogAttrs(ctx, level, "request completed", attrs...)
			}()

			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// responseWriter captures the status code and number of bytes written.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (rw *responseWriter) WriteHeader(status int) {
	if !rw.wroteHeader {
		rw.status = status
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	rw.wroteHeader = true
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
