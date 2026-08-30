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
				logger := slog.Default().With("srv", label)      // TODO mcrgnt: make properly logger
				return context.WithValue(ctx, "srvhttp", logger) // TODO mcrgnt: make properly logger
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
func (r *Server[T]) DisableGate() { r.gateDisabled = true }

func (r *Server[T]) Deps() []any {
	var t T
	return []any{
		t,
		(*metrics.Recorder)(nil),
		(*gate)(nil),
	}
}

func (r *Server[T]) Inject(args []any) {
	for _, arg := range args {
		switch v := arg.(type) {
		case T:
			r.Handler = v
		case metrics.Recorder:
			r.recorder = v
		case gate:
			r.gate = v
		}
	}
}

func (t *Server[T]) Start(ctx context.Context) (func(context.Context) error, error) {
	select {
	case <-ctx.Done():
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

func (t *Server[T]) stop(ctx context.Context) error {
	if err := t.Shutdown(ctx); err != nil {
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

func (t *Server[T]) HealthCheck(_ context.Context) error {
	return t.err.Load()
}

// ProbeReady reports traffic readiness (SDI duck typing; no ops import).
// v1: same as HealthCheck — non-nil if Serve failed after Start.
func (t *Server[T]) ProbeReady(ctx context.Context) error {
	return t.HealthCheck(ctx)
}
