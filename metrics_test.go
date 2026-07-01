package srvhttp_test

import (
	"context"
	"reflect"
	"testing"

	srvhttp "github.com/omcrgnt/srv-http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slok/go-http-metrics/metrics"
)

func TestHTTPMetrics_RegisterMetrics_andRecorder(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := &srvhttp.HTTPMetrics{}

	if err := m.RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}

	var rec metrics.Recorder = m
	rec.ObserveHTTPRequestDuration(context.Background(), metrics.HTTPReqProperties{
		Service: "test",
		ID:      "id",
		Method:  "GET",
		Code:    "200",
	}, 0)
	rec.AddInflightRequests(context.Background(), metrics.HTTPProperties{Service: "test", ID: "id"}, 1)
	rec.AddInflightRequests(context.Background(), metrics.HTTPProperties{Service: "test", ID: "id"}, -1)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(mfs) == 0 {
		t.Fatal("expected collectors registered")
	}
}

func TestHTTPMetrics_implementsContributor(t *testing.T) {
	var m srvhttp.HTTPMetrics
	var _ srvhttp.MetricsContributor = &m
	if typ := reflect.TypeOf((*srvhttp.MetricsContributor)(nil)).Elem(); typ.NumMethod() != 1 {
		t.Fatalf("MetricsContributor methods = %d", typ.NumMethod())
	}
}
