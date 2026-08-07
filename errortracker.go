// Package errortracker reports errors to the error tracker.
//
// Zero third-party dependencies, on purpose: this package is imported by every
// Go service in the estate, and a dependency here becomes a dependency
// everywhere. Everything it needs is in the standard library.
//
// The contract that matters: NOTHING HERE EVER BLOCKS THE CALLER AND NOTHING
// HERE EVER PANICS. An error-reporting library that can stall or crash its host
// turns a partial outage into a total one -- the failure mode is that you lose
// a report, never that you lose the process.
package errortracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sdkName    = "et-go"
	sdkVersion = "0.1.0"

	defaultQueueSize = 256
	defaultFlush     = 5 * time.Second
	defaultBatchSize = 20
	defaultTimeout   = 5 * time.Second
)

// Config configures the client.
//
// The short version, and all most services need:
//
//	errortracker.Init(errortracker.Config{})   // reads $ET_DSN
//	defer errortracker.Close(ctx)              // BEFORE pool.Close()
type Config struct {
	// DSN is endpoint and key in one string:
	// http://et_hrm_ab12cd34@error-tracker-app:8080
	//
	// When empty, $ET_DSN is used. Either way, an explicit Endpoint+Key below
	// takes precedence. A malformed DSN yields an inert client and a call to
	// OnError -- it never panics and never fails a boot. See dsn.go.
	DSN string

	// Endpoint is the ingest URL, e.g. http://error-tracker-app:8080/e/v1/envelope
	//
	// Point this at the Docker network name, NOT at the public domain. An
	// error-reporting path that requires your edge proxy to be up is useless in
	// the incident where you need it, and it costs a TLS handshake per report.
	//
	// Usually left unset in favour of DSN.
	Endpoint string
	Key      string

	Environment string
	Release     string
	ServerName  string

	// AppRoots marks frames as in_app. Optional: the server recomputes this
	// from the project's configuration, which is authoritative. Setting it here
	// only makes the SDK's own guess better for the module path case.
	AppRoots []string

	QueueSize int
	BatchSize int
	Flush     time.Duration
	Timeout   time.Duration

	// OnError is called when a report cannot be delivered. Optional; the
	// default is silence, because the obvious alternative -- logging the
	// failure -- risks an infinite loop when the logger is the thing being
	// captured.
	OnError func(error)

	HTTPClient *http.Client
}

// Client is a fire-and-forget reporter.
type Client struct {
	cfg    Config
	queue  chan event
	client *http.Client

	stop chan struct{}
	done chan struct{}
	once sync.Once

	enqueued atomic.Uint64
	dropped  atomic.Uint64
	sent     atomic.Uint64
	failed   atomic.Uint64
}

var (
	mu      sync.RWMutex
	global  *Client
	modPath string
)

func init() {
	// The module path is the most reliable in_app signal Go has -- far better
	// than prefix-matching file paths, which vary with the build environment.
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path != "" {
		modPath = bi.Main.Path
	}
}

// Init configures the package-level client.
//
// A missing endpoint or key is NOT an error: it returns a client that discards
// everything. A service must start and serve traffic whether or not error
// reporting is configured, and refusing to boot over an unset variable would
// mean the observability system can take down the thing it observes.
func Init(cfg Config) *Client {
	c := New(cfg)
	mu.Lock()
	global = c
	mu.Unlock()
	return c
}

func New(cfg Config) *Client {
	// Before the defaults below, so everything downstream sees a Config whose
	// Endpoint and Key are already resolved however they were supplied.
	resolveDSN(&cfg)

	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.Flush <= 0 {
		cfg.Flush = defaultFlush
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.ServerName == "" {
		cfg.ServerName, _ = os.Hostname()
	}
	if cfg.Environment == "" {
		cfg.Environment = os.Getenv("APP_ENV")
	}
	if cfg.Release == "" {
		cfg.Release = os.Getenv("ET_RELEASE")
	}
	if len(cfg.AppRoots) == 0 && modPath != "" {
		cfg.AppRoots = []string{modPath}
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}

	c := &Client{
		cfg:    cfg,
		queue:  make(chan event, cfg.QueueSize),
		client: httpClient,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if c.enabled() {
		go c.run()
	} else {
		close(c.done)
	}
	return c
}

func (c *Client) enabled() bool { return c != nil && c.cfg.Endpoint != "" && c.cfg.Key != "" }

// CaptureError reports an error. Never blocks, never panics.
func CaptureError(ctx context.Context, err error, opts ...Option) {
	mu.RLock()
	c := global
	mu.RUnlock()
	c.CaptureError(ctx, err, opts...)
}

func (c *Client) CaptureError(ctx context.Context, err error, opts ...Option) {
	if !c.enabled() || err == nil {
		return
	}
	// A panic inside the reporter must never reach the caller: it would turn a
	// handled error into a crash, in the one code path that exists to make
	// crashes visible.
	defer func() { _ = recover() }()

	ev := c.newEvent(levelError, err.Error())
	ev.Exception = buildChain(err, c.cfg.AppRoots, 2)
	for _, o := range opts {
		o(&ev)
	}
	c.enqueue(ev)
}

// CaptureMessage reports without an error value.
func (c *Client) CaptureMessage(ctx context.Context, level, msg string, opts ...Option) {
	if !c.enabled() {
		return
	}
	defer func() { _ = recover() }()

	ev := c.newEvent(level, msg)
	ev.Exception = []exception{{
		Type:   "message",
		Value:  msg,
		Frames: captureFrames(2, c.cfg.AppRoots),
	}}
	for _, o := range opts {
		o(&ev)
	}
	c.enqueue(ev)
}

// Recover reports a panic and RE-PANICS.
//
// Re-panicking is deliberate. Swallowing a panic here would change the
// program's behaviour to make it observable, which is the one thing an
// observability tool must not do: a process that was going to crash still
// crashes, it is just no longer silent about why.
//
//	defer errortracker.Recover()
func Recover() {
	if r := recover(); r != nil {
		mu.RLock()
		c := global
		mu.RUnlock()
		c.reportPanic(r, debug.Stack())
		c.Flush(2 * time.Second)
		panic(r)
	}
}

// RecoverAndContinue reports a panic and swallows it. For a worker loop where
// one bad job must not take down the pool.
func RecoverAndContinue() {
	if r := recover(); r != nil {
		mu.RLock()
		c := global
		mu.RUnlock()
		c.reportPanic(r, debug.Stack())
	}
}

func (c *Client) reportPanic(r any, stack []byte) {
	if !c.enabled() {
		return
	}
	defer func() { _ = recover() }()

	ev := c.newEvent(levelFatal, fmt.Sprint(r))
	ev.Exception = []exception{{
		Type:  panicType(r),
		Value: fmt.Sprint(r),
		// Parsed from the runtime's own stack text rather than
		// runtime.Callers: by the time a deferred recover runs, the panicking
		// frames are gone from the goroutine's call stack, so Callers would
		// report the deferred function and nothing useful.
		Frames: parseStack(stack, c.cfg.AppRoots),
	}}
	if ev.Context == nil {
		ev.Context = map[string]any{}
	}
	ev.Context["raw_stack"] = string(stack)
	c.enqueue(ev)
}

func (c *Client) newEvent(level, msg string) event {
	now := time.Now().UTC()
	return event{
		OccurredAt:  &now,
		Level:       level,
		Source:      "sdk",
		Environment: c.cfg.Environment,
		Release:     c.cfg.Release,
		ServerName:  c.cfg.ServerName,
		Message:     msg,
	}
}

// enqueue drops on a full queue rather than blocking.
//
// The alternative -- blocking until there is room -- means a slow or
// unreachable error tracker applies backpressure to every request handler in
// the calling service. That converts "the error tracker is down" into "the site
// is down", which is exactly backwards.
func (c *Client) enqueue(ev event) {
	c.enqueued.Add(1)
	select {
	case c.queue <- ev:
	default:
		c.dropped.Add(1)
	}
}

func (c *Client) run() {
	defer close(c.done)
	batch := make([]event, 0, c.cfg.BatchSize)
	tick := time.NewTicker(c.cfg.Flush)
	defer tick.Stop()

	for {
		select {
		case <-c.stop:
			for {
				select {
				case ev := <-c.queue:
					batch = append(batch, ev)
					if len(batch) >= c.cfg.BatchSize {
						c.send(batch)
						batch = batch[:0]
					}
				default:
					c.send(batch)
					return
				}
			}
		case ev := <-c.queue:
			batch = append(batch, ev)
			if len(batch) >= c.cfg.BatchSize {
				c.send(batch)
				batch = batch[:0]
			}
		case <-tick.C:
			if len(batch) > 0 {
				c.send(batch)
				batch = batch[:0]
			}
		}
	}
}

func (c *Client) send(batch []event) {
	if len(batch) == 0 {
		return
	}
	body, err := json.Marshal(envelope{
		SentAt: time.Now().UTC(),
		SDK:    sdkInfo{Name: sdkName, Version: sdkVersion},
		Events: batch,
	})
	if err != nil {
		c.failed.Add(uint64(len(batch)))
		c.onError(err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		c.failed.Add(uint64(len(batch)))
		c.onError(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ET-Key", c.cfg.Key)

	resp, err := c.client.Do(req)
	if err != nil {
		c.failed.Add(uint64(len(batch)))
		c.onError(err)
		return
	}
	defer resp.Body.Close()

	// No retry, deliberately. A retry queue in every reporting service is a
	// second unbounded buffer and a second source of duplicate reports, and the
	// events it would rescue are exactly the ones the server was too loaded to
	// accept. Dropping is the honest behaviour; the counter records it.
	if resp.StatusCode >= 300 {
		c.failed.Add(uint64(len(batch)))
		c.onError(fmt.Errorf("error tracker returned %s", resp.Status))
		return
	}
	c.sent.Add(uint64(len(batch)))
}

func (c *Client) onError(err error) {
	if c.cfg.OnError != nil {
		defer func() { _ = recover() }()
		c.cfg.OnError(err)
	}
}

// Flush blocks until the queue drains or the deadline passes.
func (c *Client) Flush(timeout time.Duration) {
	if !c.enabled() {
		return
	}
	deadline := time.After(timeout)
	for {
		if len(c.queue) == 0 {
			return
		}
		select {
		case <-deadline:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Close drains and stops. Call it BEFORE closing the database pool in a
// service's shutdown path -- an error captured during shutdown is often the
// most interesting one.
func (c *Client) Close(ctx context.Context) {
	if !c.enabled() {
		return
	}
	// A nil ctx is a caller mistake, but this runs in shutdown paths -- the one
	// place a half-built context most easily slips through, and the one place a
	// panic is least likely to be caught. The package contract is that nothing
	// here ever panics, so honour it rather than being right about ctx.
	if ctx == nil {
		ctx = context.Background()
	}
	c.once.Do(func() { close(c.stop) })
	select {
	case <-c.done:
	case <-ctx.Done():
	}
}

func Close(ctx context.Context) {
	mu.RLock()
	c := global
	mu.RUnlock()
	c.Close(ctx)
}

// Stats exposes the counters. dropped > 0 means the queue filled: the service
// is producing errors faster than they can be shipped, which is itself worth
// knowing.
func (c *Client) Stats() (enqueued, dropped, sent, failed uint64) {
	if c == nil {
		return 0, 0, 0, 0
	}
	return c.enqueued.Load(), c.dropped.Load(), c.sent.Load(), c.failed.Load()
}

// Option customises a single report.
type Option func(*event)

func WithTransaction(t string) Option { return func(e *event) { e.Transaction = t } }
func WithLogger(l string) Option      { return func(e *event) { e.Logger = l } }
func WithLevel(l string) Option       { return func(e *event) { e.Level = l } }

func WithTag(k string, v any) Option {
	return func(e *event) {
		if e.Tags == nil {
			e.Tags = map[string]any{}
		}
		e.Tags[k] = v
	}
}

func WithContext(k string, v any) Option {
	return func(e *event) {
		if e.Context == nil {
			e.Context = map[string]any{}
		}
		e.Context[k] = v
	}
}

// WithFingerprint overrides grouping entirely. Use it when the default rules
// split one operational problem across many issues -- but prefer a
// grouping_rules row on the server, which can be changed without a deploy.
func WithFingerprint(parts ...string) Option {
	return func(e *event) { e.Fingerprint = parts }
}

func WithRequest(method, url string, status int) Option {
	return func(e *event) {
		e.Request = &requestInfo{Method: method, URL: url, Status: status}
	}
}
