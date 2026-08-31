package srvhttp_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	srvhttp "github.com/omcrgnt/srv-http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slok/go-http-metrics/metrics"
)

// TestHTTPMetrics_concurrentRegisterAndObserve reproduces the scenario a
// plain (non-atomic) rec field can't survive: HTTPMetrics is a singleton
// shared across every srv-http.Server, so RegisterMetrics on it can run
// concurrently with Observe*/AddInflightRequests calls other, already-
// started servers are making from their own request-handling goroutines.
// Run with -race: a plain field here would be a data race on a 2-word
// interface value (torn type/data read), not just an "occasional nil".
func TestHTTPMetrics_concurrentRegisterAndObserve(t *testing.T) {
	m := &srvhttp.HTTPMetrics{}
	stop := make(chan struct{})

	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					m.ObserveHTTPRequestDuration(context.Background(), metrics.HTTPReqProperties{
						Service: "test", ID: "id", Method: "GET", Code: "200",
					}, 0)
					m.AddInflightRequests(context.Background(), metrics.HTTPProperties{Service: "test", ID: "id"}, 1)
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		reg := prometheus.NewRegistry()
		if err := m.RegisterMetrics(reg); err != nil {
			close(stop)
			readers.Wait()
			t.Fatal(err)
		}
	}

	close(stop)
	readers.Wait()
}

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
