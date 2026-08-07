package errortracker

import (
	"fmt"
	"net/http"
	"runtime/debug"
)

// Handler wraps an http.Handler so a panic is reported, answered with a 500,
// and NOT propagated.
//
// Unlike Recover(), this deliberately does not re-panic: net/http already
// recovers per-connection, so re-panicking here would only mean the client gets
// a dropped connection instead of a 500, with no benefit.
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				mu.RLock()
				c := global
				mu.RUnlock()
				capturePanicWithRequest(c, rec, debug.Stack(), r)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func capturePanicWithRequest(c *Client, rec any, stack []byte, r *http.Request) {
	if !c.enabled() {
		return
	}
	defer func() { _ = recover() }()

	ev := c.newEvent(levelFatal, fmt.Sprint(rec))
	ev.Transaction = r.Method + " " + r.URL.Path
	ev.Exception = []exception{{
		Type:   panicType(rec),
		Value:  fmt.Sprint(rec),
		Frames: parseStack(stack, c.cfg.AppRoots),
	}}
	// Path, not URL: the query string is where tokens live. Access logs and
	// reverse proxies generally go to some trouble to strip them; re-introducing
	// them in an error report would undo that one layer down.
	ev.Request = &requestInfo{Method: r.Method, URL: r.URL.Path, Status: 500}
	ev.Context = map[string]any{"raw_stack": string(stack)}
	c.enqueue(ev)
}

// GinRecovery is the gin-flavoured equivalent, as a bare func so this package
// keeps its zero third-party dependencies.
//
//	r.Use(errortracker.GinRecovery(logger))
//
// It is typed loosely on purpose: gin.HandlerFunc is func(*gin.Context), and
// importing gin here would push that dependency onto every service that only
// wanted the plain http.Handler above.
func GinRecovery(onPanic func(rec any, stack []byte, method, path string)) func(rec any, stack []byte, method, path string) {
	return func(rec any, stack []byte, method, path string) {
		mu.RLock()
		c := global
		mu.RUnlock()

		if c.enabled() {
			func() {
				defer func() { _ = recover() }()
				ev := c.newEvent(levelFatal, fmt.Sprint(rec))
				ev.Transaction = method + " " + path
				ev.Exception = []exception{{
					Type:   panicType(rec),
					Value:  fmt.Sprint(rec),
					Frames: parseStack(stack, c.cfg.AppRoots),
				}}
				ev.Request = &requestInfo{Method: method, URL: path, Status: 500}
				ev.Context = map[string]any{"raw_stack": string(stack)}
				c.enqueue(ev)
			}()
		}
		if onPanic != nil {
			onPanic(rec, stack, method, path)
		}
	}
}
