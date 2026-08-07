// Package errortracker_test exercises the SDK the way a consuming service does:
// from OUTSIDE the package, through the exported API only.
//
// That distinction is load-bearing here rather than stylistic. The SDK strips
// its own frames from every stack it captures, by package path -- so an
// internal test's frames are stripped too, and a test written in `package
// errortracker` cannot observe whether a CALLER's frames survive. It reports
// the standard library at the bottom of the stack and passes while the thing it
// meant to check is broken.
package errortracker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	errortracker "github.com/samiruteisyo/errortracker-go"
)

// captureOne runs fn against a client pointed at a test server and returns the
// single event it reported.
func captureOne(t *testing.T, fn func(c *errortracker.Client)) map[string]any {
	t.Helper()

	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := errortracker.New(errortracker.Config{Endpoint: srv.URL, Key: "et_t_k"})
	fn(c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("nothing was reported")
	}
	var env struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(bodies[0], &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	if len(env.Events) == 0 {
		t.Fatal("envelope carried no events")
	}
	return env.Events[0]
}

func innermostFrame(t *testing.T, ev map[string]any) string {
	t.Helper()
	excs, _ := ev["exception"].([]any)
	if len(excs) == 0 {
		t.Fatal("event carried no exception")
	}
	exc, _ := excs[0].(map[string]any)
	frames, _ := exc["frames"].([]any)
	if len(frames) == 0 {
		t.Fatal("exception carried no frames -- which is the entire point of the slog handler")
	}
	f, _ := frames[0].(map[string]any)
	fn, _ := f["function"].(string)
	return fn
}

// An error logged through slog must name the CALLER, not the standard library.
//
// SlogHandler.Handle captures the stack where it runs, so log/slog's own frames
// sit between the caller and this package. Stripping only the SDK's frames left
// "log/slog.(*Logger).log" innermost on every captured record: every issue's
// culprit read as the standard library, nothing was marked in_app, and every
// handled error in a service collapsed towards the same useless frame. Found
// against a real service, on the first error it reported, before this test
// existed.
func TestSlogHandlerNamesTheCallerNotTheStdlib(t *testing.T) {
	ev := captureOne(t, func(c *errortracker.Client) {
		logger := slog.New(errortracker.NewSlogHandler(
			slog.NewJSONHandler(io.Discard, nil), slog.LevelError).WithClient(c))
		logger.Error("insert failed", "err", errors.New("connection refused"))
	})

	fn := innermostFrame(t, ev)
	if strings.HasPrefix(fn, "log/slog.") || strings.HasPrefix(fn, "log.") {
		t.Fatalf("innermost frame is %q -- the standard library's logging plumbing, "+
			"not the code that logged the error", fn)
	}
	if !strings.Contains(fn, "TestSlogHandlerNamesTheCallerNotTheStdlib") {
		t.Fatalf("innermost frame is %q, want the function that called logger.Error", fn)
	}
}

// The same must hold for a message logged with no error attr.
func TestSlogHandlerNamesTheCallerForAMessage(t *testing.T) {
	ev := captureOne(t, func(c *errortracker.Client) {
		logger := slog.New(errortracker.NewSlogHandler(
			slog.NewJSONHandler(io.Discard, nil), slog.LevelError).WithClient(c))
		logger.Error("something went wrong", "detail", "no error value here")
	})

	fn := innermostFrame(t, ev)
	if strings.HasPrefix(fn, "log/slog.") || strings.HasPrefix(fn, "log.") {
		t.Fatalf("innermost frame is %q -- the standard library, not the caller", fn)
	}
	if !strings.Contains(fn, "TestSlogHandlerNamesTheCallerForAMessage") {
		t.Fatalf("innermost frame is %q, want the function that called logger.Error", fn)
	}
}

// A direct CaptureError must still name calling code -- the fix for the slog
// path must not have shifted the frames on the path that already worked.
//
// Asserted as "the innermost frame belongs to this test package" rather than to
// one named function: the compiler may inline the closure below into its caller,
// which changes the function name without changing anything that matters.
func TestCaptureErrorStillNamesTheCaller(t *testing.T) {
	ev := captureOne(t, func(c *errortracker.Client) {
		c.CaptureError(context.Background(), errors.New("direct"))
	})

	fn := innermostFrame(t, ev)
	if !strings.HasPrefix(fn, "github.com/samiruteisyo/errortracker-go_test.") {
		t.Fatalf("innermost frame is %q, want calling code in this test package", fn)
	}
	if strings.HasPrefix(fn, "log/slog.") || strings.HasPrefix(fn, "log.") {
		t.Fatalf("innermost frame is %q -- the standard library, not the caller", fn)
	}
}
