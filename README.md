# errortracker-go

The Go client for a self-hosted error tracker. Zero third-party dependencies —
everything it needs is in the standard library.

```
go get github.com/samiruteisyo/errortracker-go
```

## Use

Set one environment variable:

```
ET_DSN=http://et_myapp_ab12cd34@error-tracker-app:8080
```

and two lines in `main`:

```go
import errortracker "github.com/samiruteisyo/errortracker-go"

errortracker.Init(errortracker.Config{})   // reads $ET_DSN
defer errortracker.Close(ctx)              // BEFORE closing your database pool
```

That is the whole integration. Panics in an HTTP handler and errors logged at
`ERROR` still need a line each to hook — see below.

**Point the DSN at an internal address, not your public domain.** An
error-reporting path that requires your edge proxy to be up is useless in
exactly the incident where you need it, and it costs a TLS handshake per report.

## The contract

**Nothing here ever blocks the caller, and nothing here ever panics.** A library
that can stall or crash its host turns a partial outage into a total one. The
failure mode is that you lose a report — never that you lose the process.

Concretely:

- Delivery is a buffered channel and one goroutine. On a full queue the report
  is **dropped and counted**, never queued at the caller's expense.
- An unset or malformed DSN yields an **inert client**, not an error. A service
  must start and serve traffic whether or not error reporting is configured.
- `Init` never returns an error and never fails a boot.

## Capturing

```go
// An error you are handling.
errortracker.CaptureError(ctx, err)

// With context.
errortracker.CaptureError(ctx, err,
    errortracker.WithTransaction("POST /bookings"),
    errortracker.WithTag("tenant", tenantID))

// A panic, in any goroutine you spawn.
go func() {
    defer errortracker.Recover()   // reports, then re-panics
    ...
}()
```

`Recover()` re-panics after reporting, so it does not change your program's
behaviour. `RecoverAndContinue()` swallows the panic instead — use it only where
you would have recovered anyway.

### net/http

```go
mux := http.NewServeMux()
srv := &http.Server{Handler: errortracker.Handler(mux)}
```

Reports the panic with the request's method, path and a 500, then returns 500.
Chi and anything else taking `func(http.Handler) http.Handler` works the same
way: `r.Use(errortracker.Handler)`.

### Gin

`GinRecovery` wraps your existing recovery callback rather than replacing it, so
it composes with whatever you already log:

```go
r.Use(middleware.StructuredRecovery(logger, errortracker.GinRecovery(nil)))
```

### slog

Captures every record at or above a threshold while forwarding all of them
unchanged to the handler you already have:

```go
base := slog.NewJSONHandler(os.Stdout, nil)
slog.SetDefault(slog.New(errortracker.NewSlogHandler(base, slog.LevelError)))
```

This is worth wiring even if you already capture panics. A handled error —
`logger.Error("insert failed", "err", err)` — prints a message and **no stack
frames**, so anything scraping your stdout can only group it by message text.
This hooks the log site and walks the stack there, which is the difference
between *"insert failed, somewhere"* and *"insert failed at
`internal/repository/leave.go:88`"*.

## Configuration

Everything below `DSN` is optional.

| Field | Default |
|---|---|
| `DSN` | `$ET_DSN` |
| `Environment` | `$APP_ENV` |
| `Release` | `$ET_RELEASE` |
| `ServerName` | `os.Hostname()` |
| `AppRoots` | the main module path, from build info |
| `QueueSize` / `BatchSize` | 256 / 20 |
| `Flush` / `Timeout` | 5s / 5s |
| `OnError` | silence |

`AppRoots` marks frames as `in_app`, which decides **which frame an error is
grouped by**. The default — your main module path — is right for a single-module
service. The server recomputes it from the project's own configuration anyway,
and that is authoritative; setting it here only improves the SDK's own guess.

`OnError` defaults to silence deliberately. The obvious alternative, logging the
failure, risks an infinite loop when the logger is the thing being captured: one
unreachable endpoint would become a delivery failure, which would be logged,
which would be captured, which would fail to deliver.

## Shutdown

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
errortracker.Close(ctx)   // then close your pool
```

Errors captured *during* shutdown are often the most interesting ones, so close
this before the resources they will mention.

## License

MIT.
