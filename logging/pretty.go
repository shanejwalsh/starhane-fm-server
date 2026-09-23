package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ANSI escapes. Kept as named constants so the colour choices below read as
// intent rather than as numbers.
const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiRed     = "\033[31m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiMagenta = "\033[35m"
	ansiCyan    = "\033[36m"
	ansiGrey    = "\033[90m"
)

// messageWidth is the column the attributes start at. Messages longer than this
// push their attributes right rather than being truncated.
const messageWidth = 26

// PrettyHandler writes human-readable, optionally coloured logs for a terminal.
//
// It is for development only. Production uses the JSON handler, because a log
// aggregator wants fields, not alignment.
type PrettyHandler struct {
	opts  *slog.HandlerOptions
	color bool

	mu *sync.Mutex
	w  io.Writer

	// attrs and groups carry what WithAttrs and WithGroup have accumulated.
	attrs  []slog.Attr
	groups []string
}

// NewPrettyHandler returns a handler writing to w.
//
// Colour is used only when w is a terminal and the environment has not opted
// out, so piping to a file or a pager stays clean.
func NewPrettyHandler(w io.Writer, opts *slog.HandlerOptions) *PrettyHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}
	return &PrettyHandler{
		opts:  opts,
		color: shouldColor(w),
		mu:    &sync.Mutex{},
		w:     w,
	}
}

// shouldColor reports whether to emit ANSI escapes.
//
// It honours the NO_COLOR convention (https://no-color.org) and allows
// FORCE_COLOR for the case where output is piped but colour is still wanted.
func shouldColor(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if _, ok := os.LookupEnv("FORCE_COLOR"); ok {
		return true
	}

	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	// A character device is a terminal; a regular file or pipe is not.
	return info.Mode()&os.ModeCharDevice != 0
}

func (h *PrettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	minimum := slog.LevelInfo
	if h.opts.Level != nil {
		minimum = h.opts.Level.Level()
	}
	return level >= minimum
}

func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := h.clone()
	for _, attr := range attrs {
		next.attrs = append(next.attrs, qualify(h.groups, attr))
	}
	return next
}

func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.clone()
	next.groups = append(next.groups, name)
	return next
}

func (h *PrettyHandler) clone() *PrettyHandler {
	return &PrettyHandler{
		opts:   h.opts,
		color:  h.color,
		mu:     h.mu,
		w:      h.w,
		attrs:  slices.Clip(h.attrs),
		groups: slices.Clip(h.groups),
	}
}

// qualify prefixes an attribute's key with the groups it was opened under, so
// nested attributes stay distinguishable on one line.
func qualify(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 {
		return attr
	}
	attr.Key = strings.Join(groups, ".") + "." + attr.Key
	return attr
}

func (h *PrettyHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder

	timestamp := record.Time
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	line.WriteString(h.paint(ansiGrey, timestamp.Format("15:04:05.000")))
	line.WriteByte(' ')

	line.WriteString(h.paint(levelColor(record.Level), levelLabel(record.Level)))
	line.WriteByte(' ')

	message := record.Message
	line.WriteString(h.paint(ansiBold, message))
	if padding := messageWidth - len(message); padding > 0 {
		line.WriteString(strings.Repeat(" ", padding))
	}

	for _, attr := range h.attrs {
		h.writeAttr(&line, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		h.writeAttr(&line, qualify(h.groups, attr))
		return true
	})

	line.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line.String())
	return err
}

func (h *PrettyHandler) writeAttr(line *strings.Builder, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}

	// A group expands into its members rather than printing as a struct.
	if attr.Value.Kind() == slog.KindGroup {
		for _, member := range attr.Value.Group() {
			h.writeAttr(line, qualify([]string{attr.Key}, member))
		}
		return
	}

	line.WriteByte(' ')
	line.WriteString(h.paint(ansiDim, attr.Key+"="))
	line.WriteString(h.paint(valueColor(attr), formatValue(attr.Value)))
}

// valueColor highlights the few fields worth spotting at a glance while
// watching a crawl or a request log scroll past.
func valueColor(attr slog.Attr) string {
	switch attr.Key {
	case "error":
		return ansiRed

	case "cache":
		switch attr.Value.String() {
		case string(CacheHit):
			return ansiGreen
		case string(CacheMiss):
			return ansiYellow
		}

	case "status", "last_status_code":
		switch code := attr.Value.Int64(); {
		case code >= 500:
			return ansiRed
		case code >= 400:
			return ansiYellow
		case code >= 200:
			return ansiGreen
		}

	case "outcome":
		switch attr.Value.String() {
		case "changed":
			return ansiCyan
		case "not_modified":
			return ansiGrey
		case "moved":
			return ansiMagenta
		case "gone", "failed":
			return ansiRed
		case "rate_limited":
			return ansiYellow
		}
	}
	return ""
}

func formatValue(v slog.Value) string {
	var s string
	switch v.Kind() {
	case slog.KindDuration:
		s = formatDuration(v.Duration())
	case slog.KindTime:
		s = v.Time().Format(time.RFC3339)
	default:
		s = v.String()
	}

	// Quote anything that would otherwise break key=value scanning.
	if s == "" || strings.ContainsAny(s, " \t\"=") {
		return strconv.Quote(s)
	}
	return s
}

// formatDuration trims durations to something readable: nanosecond precision on
// a 900ms fetch is noise.
func formatDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.String()
	}
}

func levelLabel(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERRO"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBG"
	}
}

func levelColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return ansiRed
	case level >= slog.LevelWarn:
		return ansiYellow
	case level >= slog.LevelInfo:
		return ansiGreen
	default:
		return ansiGrey
	}
}

// paint wraps s in an ANSI colour when colour is enabled.
func (h *PrettyHandler) paint(color, s string) string {
	if !h.color || color == "" {
		return s
	}
	return color + s + ansiReset
}

var _ slog.Handler = (*PrettyHandler)(nil)
