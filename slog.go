package errortracker

import (
	"context"
	"log/slog"
)

// SlogHandler captures slog records at or above a threshold and forwards
// EVERYTHING to the wrapped handler unchanged.
//
// The forwarding is not a detail. A logging handler that swallows records to
// "capture" them silently removes lines from the container log -- which is both
// where an operator looks first and, once the shipper is running, a second
// input to this same tracker.
//
//	logger := slog.New(errortracker.NewSlogHandler(
//	    slog.NewJSONHandler(os.Stdout, nil), slog.LevelError))
type SlogHandler struct {
	next   slog.Handler
	min    slog.Level
	client *Client
	attrs  []slog.Attr
	group  string
}

func NewSlogHandler(next slog.Handler, min slog.Level) *SlogHandler {
	return &SlogHandler{next: next, min: min}
}

// WithClient targets a specific client instead of the package-level one.
func (h *SlogHandler) WithClient(c *Client) *SlogHandler {
	cp := *h
	cp.client = c
	return &cp
}

func (h *SlogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.min {
		// Capture must never be able to break logging, so a failure here is
		// swallowed and the record still goes to the wrapped handler below.
		func() {
			defer func() { _ = recover() }()
			h.capture(ctx, r)
		}()
	}
	return h.next.Handle(ctx, r)
}

func (h *SlogHandler) capture(ctx context.Context, r slog.Record) {
	c := h.client
	if c == nil {
		mu.RLock()
		c = global
		mu.RUnlock()
	}
	if !c.enabled() {
		return
	}

	var opts []Option
	var errAttr error

	collect := func(a slog.Attr) bool {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		// An `error` attr is the whole point: it carries the type and the wrap
		// chain, which is the difference between grouping on a message string
		// and grouping on an exception.
		if e, ok := a.Value.Any().(error); ok && errAttr == nil {
			errAttr = e
			return true
		}
		opts = append(opts, WithContext(key, a.Value.Any()))
		return true
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(collect)

	level := levelError
	if r.Level >= slog.LevelError+4 {
		level = levelFatal
	}
	opts = append(opts, WithLevel(level), WithLogger("slog"))

	if errAttr != nil {
		c.CaptureError(ctx, errAttr, append(opts, withMessage(r.Message))...)
		return
	}
	c.CaptureMessage(ctx, level, r.Message, opts...)
}

func withMessage(msg string) Option {
	return func(e *event) {
		if msg != "" {
			e.Message = msg
		}
	}
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.next = h.next.WithAttrs(attrs)
	cp.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &cp
}

func (h *SlogHandler) WithGroup(name string) slog.Handler {
	cp := *h
	cp.next = h.next.WithGroup(name)
	if h.group == "" {
		cp.group = name
	} else {
		cp.group = h.group + "." + name
	}
	return &cp
}
