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
			mdlw := middleware.New(middleware.Config{
				Recorder: t.recorder,
				Service:  label,
			})
			t.Handler = std.Handler("", mdlw, t.Handler)
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

// Server is the HTTP server resource bound to handler type T.
// Catalog field: *Server[T] (Configurable); materialized *Server[T] is the runtime instance after [Config].Build.
// Runtime methods: Start, Close, HealthCheck, ProbeReady.
type Server[T http.Handler] struct {
	initFn func(context.Context, *Server[T])
	http.Server
	listener net.Listener
	err      atomic.Error

	recorder metrics.Recorder
}

func (*Server[T]) BuildConfig() (app.Materializer, error) {
	return &Config[T]{}, nil
}

func (r *Server[T]) Deps() []any {
	var t T
	return []any{
		t,
		(*metrics.Recorder)(nil),
	}
}

func (r *Server[T]) Inject(args []any) {
	for _, arg := range args {
		switch v := arg.(type) {
		case T:
			r.Handler = v
		case metrics.Recorder:
			r.recorder = v
		}
	}
}

func (t *Server[T]) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		t.initFn(ctx, t)

		go func() {
			if err := t.Serve(t.listener); err != nil {
				if !errors.Is(err, http.ErrServerClosed) {
					t.err.Store(err)
				}
			}
		}()
		return nil
	}
}

func (t *Server[T]) Close(ctx context.Context) error {
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
