package server

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/seanmmitchell/gos3/internal/config"
)

// LogControl changes the level and format of a logger created by NewLogger
// at run time (the logging settings are editable through the admin API).
// It is safe for concurrent use.
type LogControl struct {
	level slog.LevelVar
	json  atomic.Bool
}

// NewLogger returns a logger writing to w in the given format ("text" or
// "json") at the given level, plus the LogControl that changes both later.
// Unknown levels mean info and unknown formats text.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, *LogControl) {
	c := &LogControl{}
	c.Set(level, format)
	opts := &slog.HandlerOptions{Level: &c.level}
	h := &switchHandler{
		ctl:  c,
		text: slog.NewTextHandler(w, opts),
		json: slog.NewJSONHandler(w, opts),
	}
	return slog.New(h), c
}

// Set applies a level (debug|info|warn|error) and a format (text|json).
// Invalid values leave the respective setting unchanged.
func (c *LogControl) Set(level, format string) {
	if c == nil {
		return
	}
	if l, err := config.ParseLogLevel(level); err == nil {
		c.level.Set(l)
	}
	switch format {
	case "json":
		c.json.Store(true)
	case "text":
		c.json.Store(false)
	}
}

// Level returns the current level.
func (c *LogControl) Level() slog.Level { return c.level.Level() }

// JSON reports whether the logger currently writes JSON.
func (c *LogControl) JSON() bool { return c.json.Load() }

// switchHandler forwards to a text or JSON handler depending on the current
// LogControl format. Attributes and groups are applied to both so switching
// keeps them.
type switchHandler struct {
	ctl  *LogControl
	text slog.Handler
	json slog.Handler
}

func (h *switchHandler) current() slog.Handler {
	if h.ctl.json.Load() {
		return h.json
	}
	return h.text
}

func (h *switchHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.current().Enabled(ctx, l)
}

func (h *switchHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.current().Handle(ctx, r)
}

func (h *switchHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &switchHandler{ctl: h.ctl, text: h.text.WithAttrs(attrs), json: h.json.WithAttrs(attrs)}
}

func (h *switchHandler) WithGroup(name string) slog.Handler {
	return &switchHandler{ctl: h.ctl, text: h.text.WithGroup(name), json: h.json.WithGroup(name)}
}
