package srvhttp

import (
	"context"
	"time"

	"github.com/omcrgnt/res/unique"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slok/go-http-metrics/metrics"
	promrecorder "github.com/slok/go-http-metrics/metrics/prometheus"
	"go.uber.org/atomic"
)

// MetricsContributor registers HTTP metrics collectors into a shared registry.
// Same method signature as github.com/omcrgnt/ops/metrics.MetricsContributor.
type MetricsContributor interface {
	RegisterMetrics(reg *prometheus.Registry) error
}

// HTTPMetrics is a singleton pool resource: contributor + shared slok Recorder
// for all srv-http servers. rec is an atomic.Pointer, not a plain field:
// RegisterMetrics can run concurrently with Observe*/AddInflightRequests
// from in-flight request goroutines on other srv-http.Server instances
// sharing this singleton — a plain metrics.Recorder field would be a data
// race on a 2-word interface value (torn type/data read), same class of bug
// [Server.err] already guards against with atomic.Error.
type HTTPMetrics struct {
	rec atomic.Pointer[metrics.Recorder]
}

func (m *HTTPMetrics) RegisterMetrics(reg *prometheus.Registry) error {
	rec := metrics.Recorder(promrecorder.NewRecorder(promrecorder.Config{Registry: reg}))
	m.rec.Store(&rec)
	return nil
}

func (m *HTTPMetrics) ObserveHTTPRequestDuration(ctx context.Context, props metrics.HTTPReqProperties, duration time.Duration) {
	rec := m.rec.Load()
	if rec == nil {
		return
	}
	(*rec).ObserveHTTPRequestDuration(ctx, props, duration)
}

func (m *HTTPMetrics) ObserveHTTPResponseSize(ctx context.Context, props metrics.HTTPReqProperties, sizeBytes int64) {
	rec := m.rec.Load()
	if rec == nil {
		return
	}
	(*rec).ObserveHTTPResponseSize(ctx, props, sizeBytes)
}

func (m *HTTPMetrics) AddInflightRequests(ctx context.Context, props metrics.HTTPProperties, quantity int) {
	rec := m.rec.Load()
	if rec == nil {
		return
	}
	(*rec).AddInflightRequests(ctx, props, quantity)
}

var (
	_ MetricsContributor = (*HTTPMetrics)(nil)
	_ metrics.Recorder   = (*HTTPMetrics)(nil)
)

func init() {
	unique.MustAddFixed(&HTTPMetrics{})
}
