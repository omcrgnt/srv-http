package srvhttp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	promrecorder "github.com/slok/go-http-metrics/metrics/prometheus"
	"github.com/slok/go-http-metrics/middleware"
	"github.com/slok/go-http-metrics/middleware/std"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestServer_FullIntegration(t *testing.T) {
	const label = "test-srv"

	spanExporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(spanExporter))
	otel.SetTracerProvider(tp)

	registry := prometheus.NewRegistry()
	recorder := promrecorder.NewRecorder(promrecorder.Config{Registry: registry})

	r := chi.NewRouter()
	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong"))
	})
	r.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()

	server := &srv[*chi.Mux]{
		Server:   http.Server{Handler: r},
		listener: listener,
	}
	server.recorder = recorder

	server.initFn = func(_ context.Context, t *srv[*chi.Mux]) {
		mdlw := middleware.New(middleware.Config{
			Recorder: server.recorder,
			Service:  label,
		})
		t.Handler = std.Handler("", mdlw, t.Handler)
		t.Handler = otelhttp.NewHandler(t.Handler, label)
	}

	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	client := &http.Client{}

	resp, err := client.Get(fmt.Sprintf("http://%s/ping", addr))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pong" {
		t.Errorf("Expected pong, got %s", string(body))
	}

	resp, err = client.Get(fmt.Sprintf("http://%s/metrics", addr))
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(metricsBody), "http_request_duration_seconds") {
		t.Error("Метрики не записались!")
	}

	spans := spanExporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("Трейсы не записались!")
	}

	if !strings.Contains(spans[0].Name, label) {
		t.Errorf("Unexpected span name: %s", spans[0].Name)
	}
}
