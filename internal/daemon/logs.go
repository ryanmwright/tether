package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/ryanmwright/tether/internal/api"
)

const logRingSize = 500

// logRing keeps the most recent log entries for clients and passes each new
// one to publish.
type logRing struct {
	mu      sync.Mutex
	entries []api.LogEntry
	publish func(api.LogEntry)
}

func (r *logRing) add(e api.LogEntry) {
	r.mu.Lock()
	if len(r.entries) == logRingSize {
		r.entries = slices.Delete(r.entries, 0, 1)
	}
	r.entries = append(r.entries, e)
	r.mu.Unlock()
	r.publish(e)
}

// recent returns up to n of the latest entries, oldest first.
func (r *logRing) recent(n int) []api.LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.entries) {
		n = len(r.entries)
	}
	return slices.Clone(r.entries[len(r.entries)-n:])
}

// logTee is a slog.Handler that records entries in a logRing as well as
// passing them to the daemon's own handler. The ring keeps info and above
// even when the daemon's own output is quieter; debug entries only when the
// daemon logs them too.
type logTee struct {
	inner  slog.Handler
	ring   *logRing
	attrs  []api.LogAttr
	prefix string // from WithGroup
}

func (h *logTee) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.inner.Enabled(ctx, l)
}

func (h *logTee) Handle(ctx context.Context, r slog.Record) error {
	e := api.LogEntry{Time: r.Time, Level: r.Level.String(), Message: r.Message, Attrs: slices.Clone(h.attrs)}
	r.Attrs(func(a slog.Attr) bool {
		e.Attrs = appendAttr(e.Attrs, h.prefix, a)
		return true
	})
	h.ring.add(e)
	if !h.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *logTee) WithAttrs(as []slog.Attr) slog.Handler {
	attrs := slices.Clone(h.attrs)
	for _, a := range as {
		attrs = appendAttr(attrs, h.prefix, a)
	}
	return &logTee{inner: h.inner.WithAttrs(as), ring: h.ring, attrs: attrs, prefix: h.prefix}
}

func (h *logTee) WithGroup(name string) slog.Handler {
	return &logTee{inner: h.inner.WithGroup(name), ring: h.ring, attrs: h.attrs, prefix: h.prefix + name + "."}
}

func appendAttr(attrs []api.LogAttr, prefix string, a slog.Attr) []api.LogAttr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			attrs = appendAttr(attrs, prefix+a.Key+".", ga)
		}
		return attrs
	}
	return append(attrs, api.LogAttr{Key: prefix + a.Key, Value: fmt.Sprint(a.Value.Any())})
}
