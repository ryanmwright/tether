// Package linelog turns a subprocess's stderr into log entries, remembering
// the last line to explain why the process failed.
package linelog

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
)

// Writer logs each line written to it and remembers the last one. While
// Quiet is set, lines are remembered but not logged.
type Writer struct {
	Quiet atomic.Bool

	mu       sync.Mutex
	log      *slog.Logger
	msg      string
	lastLine string
	part     []byte
}

// New logs lines as log.Warn(msg, "line", line).
func New(log *slog.Logger, msg string) *Writer {
	return &Writer{log: log, msg: msg}
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.part = append(w.part, p...)
	for {
		i := bytes.IndexByte(w.part, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(w.part[:i])); line != "" {
			if !w.Quiet.Load() {
				w.log.Warn(w.msg, "line", line)
			}
			w.lastLine = line
		}
		w.part = w.part[i+1:]
	}
	return len(p), nil
}

// Last returns the most recent line, including an unterminated one.
func (w *Writer) Last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := strings.TrimSpace(string(w.part)); s != "" {
		return s
	}
	return w.lastLine
}
