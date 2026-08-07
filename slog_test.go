package errortracker

import "testing"

// The leading-only rule: an application frame must never be mistaken for the
// standard library's logging plumbing, and log/slog's own frames must be.
func TestLogPlumbingIsRecognisedByPackagePath(t *testing.T) {
	for _, fn := range []string{
		"example.com/app/internal/db.Connect",
		"example.com/app/logger.Error",
		"mylog.Error",
	} {
		if isLogPlumbingFrame(fn) {
			t.Errorf("%q was treated as logging plumbing", fn)
		}
	}
	for _, fn := range []string{
		"log/slog.(*Logger).log",
		"log/slog.Error",
		"log.Printf",
	} {
		if !isLogPlumbingFrame(fn) {
			t.Errorf("%q was not recognised as logging plumbing", fn)
		}
	}
}
