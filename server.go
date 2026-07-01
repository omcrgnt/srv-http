package srvhttp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	common "github.com/omcrgnt/proto/gen/go/common/v1"
	"github.com/slok/go-http-metrics/metrics"
	"github.com/slok/go-http-metrics/middleware"
	"github.com/slok/go-http-metrics/middleware/std"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/atomic"
)

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
	return &srv[T]{
		initFn: func(ctx context.Context, t *srv[T]) {
			mdlw := middleware.New(middleware.Config{
				Recorder: t.recorder,
				Service:  cfg.Label.GetValue(),
			})
			t.Handler = std.Handler("", mdlw, t.Handler)

			t.Handler = otelhttp.NewHandler(t.Handler, cfg.Label.GetValue())

			t.BaseContext = func(net.Listener) context.Context {
				logger := slog.Default().With("srv", cfg.Label.GetValue()) // TODO mcrgnt: make properly logger
				return context.WithValue(ctx, "srvhttp", logger)         // TODO mcrgnt: make properly logger
			}
		},
		Server:   http.Server{},
		listener: listener,
	}, nil
}

type srv[T http.Handler] struct {
	initFn func(context.Context, *srv[T])
	http.Server
	listener net.Listener
	err      atomic.Error

	handler  T
	recorder metrics.Recorder
}

func (r *srv[T]) Deps() []any {
	var t T
	return []any{
		t,
		(*metrics.Recorder)(nil),
	}
}

func (r *srv[T]) Inject(args []any) {
	for _, arg := range args {
		switch v := arg.(type) {
		case T:
			r.Handler = v
		case metrics.Recorder:
			r.recorder = v
		}
	}
}

func (t *srv[T]) Start(ctx context.Context) error {
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

func (t *srv[T]) Close(ctx context.Context) error {
	if err := t.Shutdown(ctx); err != nil {
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

func (t *srv[T]) HealthCheck(_ context.Context) error {
	return t.err.Load()
}
