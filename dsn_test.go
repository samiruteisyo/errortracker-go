package errortracker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseDSN(t *testing.T) {
	cases := []struct {
		name, dsn, endpoint, key string
	}{
		{
			"the common case: no path, default ingest path filled in",
			"http://et_hrm_ab12cd34@error-tracker-app:8080",
			"http://error-tracker-app:8080/e/v1/envelope", "et_hrm_ab12cd34",
		},
		{
			"https and no port",
			"https://et_self_xyz@errors.example.com",
			"https://errors.example.com/e/v1/envelope", "et_self_xyz",
		},
		{
			"an explicit path overrides the default",
			"http://et_a_b@host:8080/custom/ingest",
			"http://host:8080/custom/ingest", "et_a_b",
		},
		{
			"a bare trailing slash is not a path",
			"http://et_a_b@host:8080/",
			"http://host:8080/e/v1/envelope", "et_a_b",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			endpoint, key, err := ParseDSN(c.dsn)
			if err != nil {
				t.Fatalf("ParseDSN(%q) = %v", c.dsn, err)
			}
			if endpoint != c.endpoint {
				t.Errorf("endpoint = %q, want %q", endpoint, c.endpoint)
			}
			if key != c.key {
				t.Errorf("key = %q, want %q", key, c.key)
			}
		})
	}
}

// The endpoint must never carry the credential onward -- it ends up in logs,
// error messages and metrics labels.
func TestParseDSNStripsTheKeyFromTheEndpoint(t *testing.T) {
	endpoint, key, err := ParseDSN("http://et_secret_value@host:8080")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(endpoint, key) || strings.Contains(endpoint, "@") {
		t.Fatalf("endpoint %q still carries the key", endpoint)
	}
}

func TestParseDSNRejectsMalformed(t *testing.T) {
	for _, dsn := range []string{
		"error-tracker-app:8080",        // no scheme, no key
		"http://error-tracker-app:8080", // no key
		"ftp://et_a_b@host",             // wrong scheme
		"http://et_a_b@",                // no host
		"http://et_a:b@host",            // a ':' silently truncates the key
		"://et_a_b@host",                // unparseable
	} {
		if _, _, err := ParseDSN(dsn); err == nil {
			t.Errorf("ParseDSN(%q) succeeded, want an error", dsn)
		}
	}
}

// Unset and malformed are different states and callers react to them
// differently: unset is normal on a laptop, malformed is a typo.
func TestEmptyDSNIsItsOwnError(t *testing.T) {
	_, _, err := ParseDSN("   ")
	if !errors.Is(err, ErrEmptyDSN) {
		t.Fatalf("err = %v, want ErrEmptyDSN", err)
	}
}

func TestDSNConfiguresAClient(t *testing.T) {
	c := New(Config{DSN: "http://et_hrm_key@host:8080"})
	defer c.Close(context.Background())
	if !c.enabled() {
		t.Fatal("a client configured by DSN must be enabled")
	}
	if c.cfg.Endpoint != "http://host:8080/e/v1/envelope" || c.cfg.Key != "et_hrm_key" {
		t.Fatalf("endpoint=%q key=%q", c.cfg.Endpoint, c.cfg.Key)
	}
}

func TestDSNIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv(EnvDSN, "http://et_env_key@envhost:9090")
	c := New(Config{})
	defer c.Close(context.Background())
	if c.cfg.Key != "et_env_key" || c.cfg.Endpoint != "http://envhost:9090/e/v1/envelope" {
		t.Fatalf("endpoint=%q key=%q", c.cfg.Endpoint, c.cfg.Key)
	}
}

// Explicit fields are the more specific statement. A stray ET_DSN in the
// environment silently overriding them is the kind of surprise that costs an
// afternoon.
func TestExplicitEndpointAndKeyBeatTheDSN(t *testing.T) {
	t.Setenv(EnvDSN, "http://et_env_key@envhost:9090")
	c := New(Config{
		DSN:      "http://et_dsn_key@dsnhost:8080",
		Endpoint: "http://explicit:1234/e/v1/envelope",
		Key:      "et_explicit_key",
	})
	defer c.Close(context.Background())
	if c.cfg.Key != "et_explicit_key" || c.cfg.Endpoint != "http://explicit:1234/e/v1/envelope" {
		t.Fatalf("endpoint=%q key=%q", c.cfg.Endpoint, c.cfg.Key)
	}
}

// The contract from the package doc: nothing here ever fails a boot. A typo in
// an optional variable must produce an inert client and a report through
// OnError, not a panic and not a hard error.
func TestMalformedDSNLeavesAnInertClientAndReportsOnce(t *testing.T) {
	t.Setenv(EnvDSN, "not-a-dsn-at-all")

	var got []error
	c := New(Config{OnError: func(e error) { got = append(got, e) }})
	defer c.Close(context.Background())

	if c.enabled() {
		t.Fatal("a malformed DSN must not produce an enabled client")
	}
	if len(got) != 1 {
		t.Fatalf("OnError called %d times, want exactly 1", len(got))
	}

	// And it stays inert rather than panicking when used.
	c.CaptureMessage(context.Background(), "error", "should be discarded")
	c.CaptureError(context.Background(), errors.New("should be discarded"))
}

// "NOTHING HERE EVER PANICS" (package doc). Close runs in shutdown paths, which
// is where a half-built context most easily slips through and where a panic is
// least likely to be caught -- so a nil one must be tolerated rather than
// dereferenced.
func TestCloseToleratesANilContext(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Close(nil) panicked: %v", r)
			}
		}()
		New(Config{DSN: "http://et_a_b@127.0.0.1:1/e/v1/envelope"}).Close(nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close(nil) did not return")
	}
}

// An unset DSN is the normal laptop state and must be silent -- OnError is for
// things someone should look at.
func TestUnsetDSNIsSilent(t *testing.T) {
	t.Setenv(EnvDSN, "")

	var got []error
	c := New(Config{OnError: func(e error) { got = append(got, e) }})
	defer c.Close(context.Background())

	if c.enabled() {
		t.Fatal("no DSN must mean an inert client")
	}
	if len(got) != 0 {
		t.Fatalf("OnError called %d times for an unset DSN, want 0", len(got))
	}
}
