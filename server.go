package srvhttp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/omcrgnt/app"
	common "github.com/omcrgnt/proto/gen/go/common/v1"

	"github.com/slok/go-http-metrics/metrics"
	"github.com/slok/go-http-metrics/middleware"
	"github.com/slok/go-http-metrics/middleware/std"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/atomic"
)

// Config is the HTTP server spec (Label, Host, Port); ecfg fills before Build.
type Config[T http.Handler] struct {
	Label common.Label
	Host  common.Host
	Port  common.Port
}

// loggerCtxKey is an unexported type, not a string: context.WithValue's own
// docs require this to avoid collisions between packages using the same
// string key.
type loggerCtxKey struct{}

func (cfg *Config[T]) Build() (any, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Host.Value, cfg.Port.Value))
	if err != nil {
		return nil, err
	}
	label := cfg.Label.GetValue()
	return &Server[T]{
		initFn: func(ctx context.Context, t *Server[T]) {
			handler := t.Handler
			if t.gate != nil && !t.gateDisabled {
				handler = gateHandler(t.gate, handler)
			}
			mdlw := middleware.New(middleware.Config{
				Recorder: t.recorder,
				Service:  label,
			})
			t.Handler = std.Handler("", mdlw, handler)
			t.Handler = otelhttp.NewHandler(t.Handler, label)
			t.BaseContext = func(net.Listener) context.Context {
				logger := slog.Default().With("srv", label) // TODO mcrgnt: make properly logger
				return context.WithValue(ctx, loggerCtxKey{}, logger)
			}
		},
		Server:   http.Server{},
		listener: listener,
	}, nil
}

// gate reports whether traffic should be let through — no runner import;
// duck-typed against runner.Gate's Ready() bool.
type gate interface{ Ready() bool }

// gateHandler answers 503 instead of calling next while g reports not ready.
func gateHandler(g gate, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Server is the HTTP server resource bound to handler type T.
// Catalog field: *Server[T] (Configurable); materialized *Server[T] is the runtime instance after [Config].Build.
// Runtime methods: Start (returns a cleanup that stops the server), HealthCheck, ProbeReady.
// Close also exists (see its own doc comment) but only to shadow the
// embedded http.Server's promoted Close — prefer the cleanup Start returns.
type Server[T http.Handler] struct {
	initFn func(context.Context, *Server[T])
	http.Server
	listener net.Listener
	err      atomic.Error

	recorder     metrics.Recorder
	gate         gate
	gateDisabled bool
}

func (*Server[T]) BuildConfig() (app.Materializer, error) {
	return &Config[T]{}, nil
}

// DisableGate permanently turns off gate-checking for this instance — for
// servers that must never be traffic-gated (e.g. ops's own readiness
// endpoint, which would otherwise mask its own status behind a blanket 503,
// and could false-fail a liveness check sharing the same listener).
//
// Must be called before Start, synchronously by the same caller that wires
// this Server: initFn reads t.gate/t.gateDisabled once, while building the
// request pipeline, and never again — a call after Start has already run
// is a silent no-op.
func (t *Server[T]) DisableGate() { t.gateDisabled = true }

func (t *Server[T]) Deps() []any {
	var handler T
	return []any{
		handler,
		(*metrics.Recorder)(nil),
		(*gate)(nil),
	}
}

func (t *Server[T]) Inject(args []any) {
	for _, arg := range args {
		switch v := arg.(type) {
		case T:
			t.Handler = v
		case metrics.Recorder:
			t.recorder = v
		case gate:
			t.gate = v
		}
	}
}

func (t *Server[T]) Start(ctx context.Context) (func(context.Context) error, error) {
	select {
	case <-ctx.Done():
		// Build already opened t.listener; on this early-return path nothing
		// else will ever close it (Start returns no cleanup on failure), so
		// it must be closed here or the fd leaks.
		_ = t.listener.Close()
		return nil, ctx.Err()
	default:
		t.initFn(ctx, t)

		go func() {
			if err := t.Serve(t.listener); err != nil {
				if !errors.Is(err, http.ErrServerClosed) {
					t.err.Store(err)
				}
			}
		}()
		return t.stop, nil
	}
}

// stop is the cleanup Start returns. http.Server.Shutdown never itself
// returns ErrServerClosed — that's what Serve/ListenAndServe return once
// Shutdown has been called on them — so this is just t.Shutdown(ctx).
func (t *Server[T]) stop(ctx context.Context) error {
	return t.Shutdown(ctx)
}

// Close shadows the http.Server.Close this type embeds: without an
// explicitly declared Close of its own, Server[T] promotes the embedded
// http.Server's zero-arg, non-graceful Close() error — a hard reset that
// drops in-flight connections instead of the graceful stop above. Anyone
// calling the old Close(ctx) convention by habit gets a compile error, not
// a silently different shutdown.
func (t *Server[T]) Close() error {
	return t.stop(context.Background())
}

func (t *Server[T]) HealthCheck(_ context.Context) error {
	return t.err.Load()
}

// ProbeReady reports traffic readiness (SDI duck typing; no ops import):
// non-nil if Serve failed after Start (same as HealthCheck), or if a gate
// is wired, enabled, and not yet open — a caller relying only on
// HealthCheck would see this Server as ready while every real request is
// still answered 503 by the gate handler.
func (t *Server[T]) ProbeReady(ctx context.Context) error {
	if err := t.HealthCheck(ctx); err != nil {
		return err
	}
	if t.gate != nil && !t.gateDisabled && !t.gate.Ready() {
		return errors.New("srvhttp: gate not ready")
	}
	return nil
}
