package errortracker

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// The DSN is the whole configuration of a reporting client in one string, so
// wiring a service is one environment variable rather than two fields and a URL
// path nobody remembers:
//
//	ET_DSN=http://et_hrm_ab12cd34@error-tracker-app:8080
//
// Shape borrowed from Sentry, which is the ergonomic every Go, PHP and Node
// developer already has the muscle memory for. It is NOT wire-compatible with
// Sentry and is not trying to be -- see model.Envelope for why half-
// compatibility is worse than none. Only the ergonomics are copied.
//
// Two things this format deliberately does NOT carry, both of which Sentry's
// does:
//
//   - A project id. Our key is `et_<slug>_<32 base62>` and already names its
//     project, so a separate id would be a second source of truth that can
//     disagree with the key.
//   - A path. The ingest endpoint is a property of the SERVER's API version, not
//     of the deployment, so baking it into every service's environment means
//     every service has to be edited to move it. It defaults to
//     DefaultIngestPath and can still be overridden for the unusual case.
//
// Point it at the Docker network name, NOT the public domain: an
// error-reporting path that requires your edge proxy to be up is useless in
// exactly the incident where it is needed, and it costs a TLS handshake per
// report.
const DefaultIngestPath = "/e/v1/envelope"

// EnvDSN is read when Config.DSN is empty. Matching SENTRY_DSN's behaviour of
// being picked up with no code at all is most of why that ergonomic feels good.
const EnvDSN = "ET_DSN"

// ErrEmptyDSN is returned by ParseDSN for an empty string. It is deliberately
// distinct from a malformed one: unset means "reporting is off here", which is
// a normal state for a laptop, while malformed means someone tried to configure
// it and made a typo -- and those two deserve different reactions from a caller
// that logs.
var ErrEmptyDSN = errors.New("errortracker: empty DSN")

// ParseDSN splits a DSN into the endpoint and key that Config already uses.
//
//	http://et_hrm_ab12cd34@error-tracker-app:8080
//	  -> http://error-tracker-app:8080/e/v1/envelope, et_hrm_ab12cd34
//
// The key travels in the userinfo position rather than as a query parameter so
// it cannot end up in a URL that gets logged whole: net/url redacts userinfo in
// String(), and the endpoint this returns has it removed entirely.
func ParseDSN(dsn string) (endpoint, key string, err error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", "", ErrEmptyDSN
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return "", "", fmt.Errorf("errortracker: bad DSN: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("errortracker: bad DSN: scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", "", errors.New("errortracker: bad DSN: no host")
	}
	if u.User == nil || u.User.Username() == "" {
		return "", "", errors.New("errortracker: bad DSN: no key (expected scheme://<key>@host)")
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		// A colon in the userinfo means the key was split in half and the second
		// half silently discarded, which would present as 401s that look like a
		// revoked key. Refuse it instead.
		return "", "", errors.New("errortracker: bad DSN: key must not contain ':'")
	}

	key = u.User.Username()
	path := u.Path
	if path == "" || path == "/" {
		path = DefaultIngestPath
	}
	// Rebuilt rather than string-trimmed so the key cannot survive into the
	// endpoint by way of some encoding the trim did not anticipate.
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: path}).String(), key, nil
}

// resolveDSN fills Endpoint and Key from the DSN when they were not given
// explicitly.
//
// Precedence is explicit fields, then Config.DSN, then $ET_DSN. Explicit wins
// because it is the more specific statement: a caller that passed an Endpoint
// meant it, and having a stray environment variable silently override it is the
// kind of surprise that costs an afternoon.
//
// A malformed DSN is reported through OnError and otherwise IGNORED, leaving an
// inert client. This is the same contract as Init's: the observability system
// must not be able to take down the thing it observes, and refusing to boot over
// a typo in an optional variable would do exactly that.
func resolveDSN(cfg *Config) {
	if cfg.Endpoint != "" && cfg.Key != "" {
		return
	}

	raw := cfg.DSN
	if raw == "" {
		raw = os.Getenv(EnvDSN)
	}
	if raw == "" {
		return
	}

	endpoint, key, err := ParseDSN(raw)
	if err != nil {
		if !errors.Is(err, ErrEmptyDSN) && cfg.OnError != nil {
			cfg.OnError(err)
		}
		return
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = endpoint
	}
	if cfg.Key == "" {
		cfg.Key = key
	}
}
