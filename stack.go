package errortracker

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	levelFatal   = "fatal"
	levelError   = "error"
	levelWarning = "warning"
)

// Wire types. Kept private and duplicated from internal/model rather than
// imported: this package must be copyable into another repository with no
// dependency on the tracker's own module.
type envelope struct {
	SentAt time.Time `json:"sent_at"`
	SDK    sdkInfo   `json:"sdk"`
	Events []event   `json:"events"`
}

type sdkInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type event struct {
	OccurredAt  *time.Time     `json:"occurred_at,omitempty"`
	Level       string         `json:"level"`
	Source      string         `json:"source"`
	Environment string         `json:"environment,omitempty"`
	Release     string         `json:"release,omitempty"`
	ServerName  string         `json:"server_name,omitempty"`
	Transaction string         `json:"transaction,omitempty"`
	Message     string         `json:"message"`
	Logger      string         `json:"logger,omitempty"`
	Exception   []exception    `json:"exception,omitempty"`
	Request     *requestInfo   `json:"request,omitempty"`
	Tags        map[string]any `json:"tags,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	Fingerprint []string       `json:"fingerprint,omitempty"`
}

type exception struct {
	Type   string  `json:"type"`
	Value  string  `json:"value"`
	Module string  `json:"module,omitempty"`
	Frames []frame `json:"frames,omitempty"`
}

type frame struct {
	File     string `json:"file"`
	Function string `json:"function"`
	Line     int    `json:"line,omitempty"`
	Module   string `json:"module,omitempty"`
	InApp    *bool  `json:"in_app,omitempty"`
}

type requestInfo struct {
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`
	Status int    `json:"status,omitempty"`
}

const maxFrames = 64

// sdkPkg is this package's import path, e.g.
// "github.com/samiruteisyo/errortracker-go". Resolved from
// the runtime so it stays correct when the package is vendored into another
// repository under a different module path.
var sdkPkg = func() string {
	pcs := make([]uintptr, 1)
	if runtime.Callers(1, pcs) == 0 {
		return ""
	}
	f, _ := runtime.CallersFrames(pcs).Next()
	// "path/to/pkg.funcName" -> "path/to/pkg"
	name := f.Function
	slash := strings.LastIndexByte(name, '/')
	if dot := strings.IndexByte(name[slash+1:], '.'); dot >= 0 {
		return name[:slash+1+dot]
	}
	return name
}()

// isSDKFrame reports whether a function belongs to this package, by exact
// package path rather than by name substring.
func isSDKFrame(function string) bool {
	if sdkPkg == "" {
		return false
	}
	return strings.HasPrefix(function, sdkPkg+".")
}

// buildChain unwraps an error into the chain the server expects: INNERMOST
// FIRST.
//
// The server fingerprints on element 0, so the order is load-bearing. Go's
// Unwrap walks outermost-to-innermost, which is the opposite, hence the
// reversal at the end. Getting this backwards groups every wrapped error by
// whichever handler wrapped it -- a bug that looks like "grouping is bad"
// rather than like an ordering mistake.
func buildChain(err error, appRoots []string, skip int) []exception {
	var chain []exception
	frames := captureFrames(skip+1, appRoots)

	for e := err; e != nil; e = errors.Unwrap(e) {
		exc := exception{
			Type:  typeName(e),
			Value: e.Error(),
		}
		// Only the innermost link gets the stack: the frames were captured at
		// the report site, so attaching them to every link would claim each
		// wrapper was thrown there.
		chain = append(chain, exc)
		// Bound the walk. A cyclic Unwrap is rare but it exists, and an
		// unbounded loop inside an error reporter is a very unfunny bug.
		if len(chain) >= 16 {
			break
		}
	}

	// Reverse: Unwrap yields outermost-first, the wire format wants innermost-first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if len(chain) > 0 {
		chain[0].Frames = frames
	}
	return chain
}

func typeName(err error) string {
	if err == nil {
		return ""
	}
	t := reflectTypeName(err)
	if t == "" {
		return "error"
	}
	return t
}

// captureFrames walks the caller's stack, innermost first.
func captureFrames(skip int, appRoots []string) []frame {
	pcs := make([]uintptr, maxFrames)
	// +2 skips runtime.Callers and captureFrames itself.
	n := runtime.Callers(skip+2, pcs)
	if n == 0 {
		return nil
	}
	it := runtime.CallersFrames(pcs[:n])

	var out []frame
	for {
		f, more := it.Next()
		if f.Function == "" && f.File == "" {
			if !more {
				break
			}
			continue
		}
		// Frames belonging to this package are noise in every report it
		// produces, so they never reach the wire.
		//
		// Matched against this package's OWN import path, resolved at init from
		// the runtime rather than hard-coded. A substring match on
		// "/errortracker." would also delete a caller's frames if they happened
		// to have a package of that name -- which is not hypothetical, since
		// the natural thing to call a wrapper around this SDK is errortracker.
		if !isSDKFrame(f.Function) {
			out = append(out, newFrame(f.File, f.Function, f.Line, appRoots))
		}
		if !more {
			break
		}
	}
	return out
}

func newFrame(file, function string, line int, appRoots []string) frame {
	fr := frame{File: file, Function: function, Line: line}
	if mod := moduleOf(function); mod != "" {
		fr.Module = mod
	}
	// The SDK's opinion, which the server honours when present. Go can be
	// definitive here because the module path comes from build info, unlike the
	// server's prefix match against a file path.
	in := isInApp(file, function, appRoots)
	fr.InApp = &in
	return fr
}

func isInApp(file, function string, appRoots []string) bool {
	hay := file + "\x00" + function
	if strings.Contains(file, "/go/pkg/mod/") ||
		strings.Contains(file, "/usr/local/go/src/") ||
		strings.HasPrefix(function, "runtime.") {
		return false
	}
	for _, r := range appRoots {
		if r != "" && strings.Contains(hay, r) {
			return true
		}
	}
	return false
}

func moduleOf(function string) string {
	i := strings.LastIndex(function, "/")
	if i < 0 {
		return ""
	}
	rest := function[i+1:]
	if j := strings.Index(rest, "."); j >= 0 {
		return function[:i+1] + rest[:j]
	}
	return function[:i]
}

// parseStack reads the runtime's panic stack text.
//
// Needed because by the time a deferred recover runs, the panicking frames are
// gone from the goroutine's call stack -- runtime.Callers would report the
// deferred function and nothing about where the panic actually came from.
//
// The format is pairs of lines:
//
//	main.doThing(0x1400012c008, 0x2)
//	        /app/internal/thing.go:42 +0x1a4
//
// Machinery is dropped by POSITION, not by name: everything up to and including
// the `panic(...)` frame is the recover path and the runtime, and everything
// after it is the code that actually panicked. That rule is what makes the
// culprit the first frame in the report.
//
// The earlier version filtered by package-name substring instead, and dropped
// any frame whose import path contained "/errortracker." -- which silently
// deleted the panicking function whenever a caller's own package happened to be
// named that. Position is unambiguous; a name match never was.
func parseStack(stack []byte, appRoots []string) []frame {
	lines := strings.Split(string(stack), "\n")

	type parsed struct {
		fn, file string
		line     int
	}
	var all []parsed

	for i := 0; i+1 < len(lines); i++ {
		fn := strings.TrimSpace(lines[i])
		loc := lines[i+1]
		if fn == "" || !strings.HasPrefix(loc, "\t") {
			continue
		}
		if strings.HasPrefix(fn, "goroutine ") {
			continue
		}
		createdBy := strings.HasPrefix(fn, "created by ")
		if createdBy {
			fn = strings.TrimPrefix(fn, "created by ")
			if p := strings.Index(fn, " in goroutine"); p > 0 {
				fn = fn[:p]
			}
		}
		// Strip the argument list: "main.doThing(0x14...)" -> "main.doThing"
		if p := strings.IndexByte(fn, '('); p > 0 {
			fn = fn[:p]
		}

		loc = strings.TrimSpace(loc)
		if p := strings.Index(loc, " +0x"); p > 0 {
			loc = loc[:p]
		}
		file, line := loc, 0
		if c := strings.LastIndexByte(loc, ':'); c > 0 {
			file = loc[:c]
			line, _ = strconv.Atoi(loc[c+1:])
		}

		all = append(all, parsed{fn: fn, file: file, line: line})
		i++ // consume the location line
	}

	// Find the panic frame and start after it. Both spellings occur: the
	// synthetic `panic` frame the runtime prints for a user panic, and
	// `runtime.gopanic` in some builds.
	start := 0
	for i, p := range all {
		if p.fn == "panic" || p.fn == "runtime.gopanic" {
			start = i + 1
		}
	}

	out := make([]frame, 0, len(all)-start)
	for _, p := range all[start:] {
		// After the panic frame, runtime entries are the goroutine's launch
		// path (runtime.main, runtime.goexit) -- real, but never the culprit.
		if strings.HasPrefix(p.fn, "runtime.") {
			continue
		}
		out = append(out, newFrame(p.file, p.fn, p.line, appRoots))
		if len(out) >= maxFrames {
			break
		}
	}
	return out
}

func panicType(r any) string {
	if err, ok := r.(error); ok {
		return typeName(err)
	}
	return "panic"
}
