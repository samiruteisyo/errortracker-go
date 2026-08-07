package errortracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector stands in for the ingest endpoint.
type collector struct {
	mu   sync.Mutex
	envs []envelope
	key  string
}

func (c *collector) start(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.envs = append(c.envs, env)
		c.key = r.Header.Get("X-ET-Key")
		c.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
}

func (c *collector) events() []event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []event
	for _, e := range c.envs {
		out = append(out, e.Events...)
	}
	return out
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(Config{
		Endpoint: srv.URL, Key: "et_test_key", Environment: "test",
		ServerName: "test-host", AppRoots: []string{"error_tracker"},
		Flush: 10 * time.Millisecond, BatchSize: 100,
		OnError: func(err error) { t.Errorf("delivery failed: %v", err) },
	})
}

// The chain must arrive INNERMOST FIRST -- the server fingerprints on element
// 0. Go's Unwrap walks the other way, so getting this backwards would group
// every wrapped error by whichever handler wrapped it: a bug that looks like
// "grouping is bad" rather than like an ordering mistake.
func TestErrorChainIsInnermostFirst(t *testing.T) {
	c := &collector{}
	srv := c.start(t)
	defer srv.Close()

	client := newTestClient(t, srv)
	base := errors.New("connection refused")
	wrapped := fmt.Errorf("loading employee: %w", fmt.Errorf("querying db: %w", base))

	client.CaptureError(context.Background(), wrapped)
	client.Close(context.Background())

	evs := c.events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	chain := evs[0].Exception
	if len(chain) != 3 {
		t.Fatalf("expected a 3-link chain, got %d: %+v", len(chain), chain)
	}
	if chain[0].Value != "connection refused" {
		t.Errorf("chain[0] should be the innermost error, got %q", chain[0].Value)
	}
	if !strings.Contains(chain[2].Value, "loading employee") {
		t.Errorf("chain[2] should be the outermost error, got %q", chain[2].Value)
	}
	// Only the innermost link carries frames: they were captured at the report
	// site, so attaching them to a wrapper would claim it was thrown there.
	if len(chain[0].Frames) == 0 {
		t.Error("innermost link has no frames")
	}
	if len(chain[1].Frames) != 0 {
		t.Error("a wrapper link carries frames it did not produce")
	}
}

// A panic's frames cannot come from runtime.Callers -- by the time a deferred
// recover runs those frames are gone. They are parsed from the runtime's stack
// text instead, and this proves the parse actually finds the culprit.
func TestPanicStackNamesTheCulprit(t *testing.T) {
	c := &collector{}
	srv := c.start(t)
	defer srv.Close()

	client := newTestClient(t, srv)
	mu.Lock()
	old := global
	global = client
	mu.Unlock()
	defer func() { mu.Lock(); global = old; mu.Unlock() }()

	func() {
		defer RecoverAndContinue()
		mustPanicHere()
	}()
	client.Close(context.Background())

	evs := c.events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].Level != levelFatal {
		t.Errorf("a panic should be fatal, got %q", evs[0].Level)
	}

	joined := ""
	for _, f := range evs[0].Exception[0].Frames {
		joined += f.Function + " "
	}
	if !strings.Contains(joined, "mustPanicHere") {
		t.Errorf("panic frames do not name the panicking function: %s", joined)
	}
	// The runtime's own machinery is identical in every panic report and says
	// nothing about this one.
	if strings.Contains(joined, "runtime.gopanic") {
		t.Errorf("runtime frames leaked into the report: %s", joined)
	}
}

func mustPanicHere() { panic("deliberate test panic") }

// The single most important property of this package: an unreachable or slow
// endpoint must cost a dropped report, never a blocked caller.
func TestEnqueueNeverBlocks(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // never returns until the test releases it
	}))
	defer srv.Close()
	defer close(blocked)

	client := New(Config{
		Endpoint: srv.URL, Key: "k", QueueSize: 4,
		Flush: time.Millisecond, Timeout: 50 * time.Millisecond,
		OnError: func(error) {},
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			client.CaptureError(context.Background(), errors.New("boom"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CaptureError blocked on a hung endpoint")
	}

	_, dropped, _, _ := client.Stats()
	if dropped == 0 {
		t.Error("expected drops on a 4-slot queue with 1000 reports")
	}
}

// An unconfigured SDK must be inert, not fatal. A service has to start and
// serve traffic whether or not error reporting is set up.
func TestUnconfiguredClientIsInert(t *testing.T) {
	client := New(Config{})
	client.CaptureError(context.Background(), errors.New("boom"))
	client.Close(context.Background())

	enqueued, _, sent, failed := client.Stats()
	if enqueued != 0 || sent != 0 || failed != 0 {
		t.Errorf("unconfigured client did work: enqueued=%d sent=%d failed=%d", enqueued, sent, failed)
	}
}

// A nil client is what CaptureError sees before Init runs. It must be safe.
func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	c.CaptureError(context.Background(), errors.New("boom"))
	c.Close(context.Background())
	c.Flush(time.Millisecond)
}

func TestHandlerReportsPanicAndReturns500(t *testing.T) {
	c := &collector{}
	srv := c.start(t)
	defer srv.Close()

	client := newTestClient(t, srv)
	mu.Lock()
	old := global
	global = client
	mu.Unlock()
	defer func() { mu.Lock(); global = old; mu.Unlock() }()

	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler exploded")
	}))
	rec := httptest.NewRecorder()
	// Deliberately includes a query string, to prove it is not recorded.
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/thing?token=SECRETCANARY", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
	client.Close(context.Background())

	evs := c.events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].Transaction != "GET /api/thing" {
		t.Errorf("transaction = %q", evs[0].Transaction)
	}
	// The query string is where tokens live. Whatever strips them upstream must
	// not be undone by an error report one layer down.
	blob, _ := json.Marshal(evs[0])
	if strings.Contains(string(blob), "SECRETCANARY") {
		t.Error("query string leaked into the report")
	}
}

func TestInApp(t *testing.T) {
	roots := []string{"example.com/app"}
	cases := []struct {
		file, fn string
		want     bool
		why      string
	}{
		{"example.com/app/internal/db.go", "db.Connect", true, "our code"},
		{"/go/pkg/mod/github.com/jackc/pgx/v5/conn.go", "pgx.exec", false, "a module dependency"},
		{"/usr/local/go/src/net/http/server.go", "http.serve", false, "the standard library"},
		{"", "runtime.gopanic", false, "the runtime"},
		{"example.com/other/x.go", "other.Do", false, "a different project"},
	}
	for _, c := range cases {
		if got := isInApp(c.file, c.fn, roots); got != c.want {
			t.Errorf("isInApp(%q, %q) = %v, want %v (%s)", c.file, c.fn, got, c.want, c.why)
		}
	}
}

// The SDK's own frames are noise in every report it produces and must never
// reach the wire. Matched by exact package path -- a substring match on the
// package NAME would also delete a caller's frames if they happened to name
// their own wrapper package errortracker.
func TestSDKFramesAreFilteredByExactPackagePath(t *testing.T) {
	if sdkPkg == "" {
		t.Fatal("could not resolve this package's own import path")
	}
	if !isSDKFrame(sdkPkg + ".CaptureError") {
		t.Error("failed to recognise its own frame")
	}
	// The case the old substring match got wrong.
	if isSDKFrame("github.com/someoneelse/errortracker.Report") {
		t.Error("a different package named errortracker was misidentified as ours")
	}
	if isSDKFrame("example.com/app/internal/db.Connect") {
		t.Error("user code was misidentified as SDK code")
	}
}
