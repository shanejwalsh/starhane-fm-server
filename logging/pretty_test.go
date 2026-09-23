package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func prettyLine(t *testing.T, log func(*slog.Logger)) string {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(NewPrettyHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log(logger)
	return strings.TrimSpace(buf.String())
}

func TestPrettyHandlerFormatsAttributes(t *testing.T) {
	line := prettyLine(t, func(l *slog.Logger) {
		l.Info("feed crawled", slog.Int64("feed_id", 3), slog.String("outcome", "changed"))
	})

	for _, want := range []string{"INFO", "feed crawled", "feed_id=3", "outcome=changed"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not contain %q", line, want)
		}
	}
}

func TestPrettyHandlerIsPlainWhenNotATerminal(t *testing.T) {
	line := prettyLine(t, func(l *slog.Logger) {
		l.Error("boom", slog.String("error", "nope"))
	})

	// A bytes.Buffer is not a terminal, so the output must stay free of escape
	// sequences — otherwise piping logs to a file fills it with junk.
	if strings.Contains(line, "\033[") {
		t.Errorf("line contains ANSI escapes when writing to a non-terminal: %q", line)
	}
}

func TestPrettyHandlerRespectsNoColor(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	var buf bytes.Buffer
	if !shouldColor(&buf) {
		t.Fatal("FORCE_COLOR should enable colour even for a non-terminal")
	}

	// NO_COLOR wins over FORCE_COLOR.
	t.Setenv("NO_COLOR", "1")
	if shouldColor(&buf) {
		t.Error("NO_COLOR should disable colour")
	}
}

func TestPrettyHandlerColoursWhenForced(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")

	var buf bytes.Buffer
	logger := slog.New(NewPrettyHandler(&buf, nil))
	logger.Info("request completed", slog.String("cache", "hit"), slog.Int("status", 200))
	line := buf.String()

	if !strings.Contains(line, ansiGreen+"hit"+ansiReset) {
		t.Errorf("a cache hit should be green: %q", line)
	}
	if !strings.Contains(line, ansiGreen+"200"+ansiReset) {
		t.Errorf("a 2xx status should be green: %q", line)
	}
}

func TestPrettyHandlerQuotesValuesNeedingIt(t *testing.T) {
	line := prettyLine(t, func(l *slog.Logger) {
		l.Info("msg", slog.String("version", "18.6 (Homebrew)"), slog.String("empty", ""))
	})

	// Unquoted spaces would break key=value scanning.
	if !strings.Contains(line, `version="18.6 (Homebrew)"`) {
		t.Errorf("value with spaces was not quoted: %q", line)
	}
	if !strings.Contains(line, `empty=""`) {
		t.Errorf("empty value was not quoted: %q", line)
	}
}

func TestPrettyHandlerWithAttrsAndGroups(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewPrettyHandler(&buf, nil)).
		With(slog.String("service", "crawler")).
		WithGroup("feed").
		With(slog.Int64("id", 7))

	logger.Info("crawled", slog.String("host", "example.com"))
	line := buf.String()

	for _, want := range []string{"service=crawler", "feed.id=7", "feed.host=example.com"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not contain %q", line, want)
		}
	}
}

func TestPrettyHandlerExpandsInlineGroups(t *testing.T) {
	line := prettyLine(t, func(l *slog.Logger) {
		l.Info("msg", slog.Group("db", slog.Int("conns", 8), slog.String("name", "starhane_fm")))
	})

	for _, want := range []string{"db.conns=8", "db.name=starhane_fm"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not contain %q", line, want)
		}
	}
}

func TestPrettyHandlerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewPrettyHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	logger.Info("should not appear")
	logger.Warn("should appear")

	if strings.Contains(buf.String(), "should not appear") {
		t.Error("an info line was emitted by a warn-level handler")
	}
	if !strings.Contains(buf.String(), "should appear") {
		t.Error("the warn line is missing")
	}
}

func TestFormatDurationIsReadable(t *testing.T) {
	cases := map[time.Duration]string{
		368383416 * time.Nanosecond: "368.4ms",
		2146583 * time.Nanosecond:   "2.1ms",
		1500 * time.Millisecond:     "1.5s",
		90 * time.Second:            "1m30s",
		400 * time.Nanosecond:       "400ns",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestNewSelectsHandlerByFormat(t *testing.T) {
	// A buffer is never a terminal, so an unset format must give JSON —
	// production must not depend on the format variable being set.
	var buf bytes.Buffer
	logger := New(&buf, Config{})
	logger.Info("hello", slog.String("k", "v"))
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Errorf("unset format on a non-terminal should be JSON, got %q", buf.String())
	}

	buf.Reset()
	New(&buf, Config{Format: "pretty"}).Info("hello", slog.String("k", "v"))
	if strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Errorf("pretty format produced JSON: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "k=v") {
		t.Errorf("pretty format missing attributes: %q", buf.String())
	}

	buf.Reset()
	New(&buf, Config{Format: "text"}).Info("hello")
	if !strings.Contains(buf.String(), "level=INFO") {
		t.Errorf("text format should still be the stdlib TextHandler: %q", buf.String())
	}
}

func TestPrettyHandlerIsSafeForConcurrentUse(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewPrettyHandler(&buf, nil))

	done := make(chan struct{})
	for i := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 50 {
				logger.With(slog.Int("worker", i)).Info("crawled", slog.String("host", "example.com"))
			}
		}()
	}
	for range 8 {
		<-done
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 400 {
		t.Fatalf("got %d lines, want 400", len(lines))
	}
	// Interleaved writes would produce lines missing their message.
	for _, line := range lines {
		if !strings.Contains(line, "crawled") {
			t.Fatalf("interleaved output: %q", line)
		}
	}
}

func TestPrettyHandlerContextIsUnused(t *testing.T) {
	var buf bytes.Buffer
	h := NewPrettyHandler(&buf, nil)
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info should be enabled by default")
	}
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug should be disabled by default")
	}
}
