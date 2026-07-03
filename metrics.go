package srvhttp

import (
	"context"
	"time"

	"github.com/omcrgnt/res/unique"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slok/go-http-metrics/metrics"
	promrecorder "github.com/slok/go-http-metrics/metrics/prometheus"
)

// MetricsContributor registers HTTP metrics collectors into a shared registry.
// Same method signature as github.com/omcrgnt/ops/metrics.MetricsContributor.
type MetricsContributor interface {
	RegisterMetrics(reg *prometheus.Registry) error
}

// HTTPMetrics is a singleton pool resource: contributor + shared slok Recorder for all srv-http servers.
type HTTPMetrics struct {
	rec metrics.Recorder
}

func (m *HTTPMetrics) RegisterMetrics(reg *prometheus.Registry) error {
	m.rec = promrecorder.NewRecorder(promrecorder.Config{Registry: reg})
	return nil
}

func (m *HTTPMetrics) ObserveHTTPRequestDuration(ctx context.Context, props metrics.HTTPReqProperties, duration time.Duration) {
	if m.rec == nil {
		return
	}
	m.rec.ObserveHTTPRequestDuration(ctx, props, duration)
}

func (m *HTTPMetrics) ObserveHTTPResponseSize(ctx context.Context, props metrics.HTTPReqProperties, sizeBytes int64) {
	if m.rec == nil {
		return
	}
	m.rec.ObserveHTTPResponseSize(ctx, props, sizeBytes)
}

func (m *HTTPMetrics) AddInflightRequests(ctx context.Context, props metrics.HTTPProperties, quantity int) {
	if m.rec == nil {
		return
	}
	m.rec.AddInflightRequests(ctx, props, quantity)
}

var (
	_ MetricsContributor = (*HTTPMetrics)(nil)
	_ metrics.Recorder   = (*HTTPMetrics)(nil)
)

func init() {
	unique.MustAddFixed(&HTTPMetrics{})
}
