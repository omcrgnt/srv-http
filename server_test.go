package srvhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	common "github.com/omcrgnt/proto/gen/go/common/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/slok/go-http-metrics/metrics"
	promrecorder "github.com/slok/go-http-metrics/metrics/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestConfig_Build_integration(t *testing.T) {
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

	cfg := Config[*chi.Mux]{
		Label: common.Label{Value: "test_srv"},
		Host:  common.Host{Value: "127.0.0.1"},
		Port:  common.Port{Value: 0},
	}

	built, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}

	server := built.(*srv[*chi.Mux])
	server.Inject([]any{r, recorder})

	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close(context.Background())
	})

	addr := server.listener.Addr().String()

	time.Sleep(50 * time.Millisecond)

	client := &http.Client{}

	resp, err := client.Get(fmt.Sprintf("http://%s/ping", addr))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pong" {
		t.Errorf("expected pong, got %s", string(body))
	}

	resp, err = client.Get(fmt.Sprintf("http://%s/metrics", addr))
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	metricsStr := string(metricsBody)

	if !strings.Contains(metricsStr, "http_request_duration_seconds") {
		t.Error("metrics: missing http_request_duration_seconds")
	}
	if !strings.Contains(metricsStr, "http_request_duration_seconds_count") {
		t.Error("metrics: missing observation count after /ping")
	}

	spans := spanExporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no trace spans recorded")
	}
	for i, span := range spans {
		if span.Name != "GET" {
			t.Errorf("span[%d]: unexpected name %q, want GET", i, span.Name)
		}
	}

	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := server.HealthCheck(t.Context()); err != nil {
		t.Errorf("HealthCheck after graceful close: %v", err)
	}
}

func TestInject(t *testing.T) {
	mux := chi.NewRouter()
	rec := promrecorder.NewRecorder(promrecorder.Config{Registry: prometheus.NewRegistry()})

	s := &srv[*chi.Mux]{}
	deps := s.Deps()

	if got, want := reflect.TypeOf(deps[0]), reflect.TypeOf((*chi.Mux)(nil)); got != want {
		t.Errorf("Deps()[0] type = %v, want %v", got, want)
	}
	if got, want := reflect.TypeOf(deps[1]), reflect.TypeOf((*metrics.Recorder)(nil)); got != want {
		t.Errorf("Deps()[1] type = %v, want %v", got, want)
	}

	s.Inject([]any{mux, rec})

	if s.Handler != mux {
		t.Error("Inject: Handler not set")
	}
	if s.recorder != rec {
		t.Error("Inject: recorder not set")
	}
}

func TestStart_cancelledContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &srv[*chi.Mux]{
		listener: ln,
		initFn:   func(context.Context, *srv[*chi.Mux]) {},
	}

	err = s.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Start: got %v, want context.Canceled", err)
	}
}

func TestHealthCheck_serveError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	s := &srv[*chi.Mux]{
		listener: ln,
		initFn:   func(context.Context, *srv[*chi.Mux]) {},
		Server: http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	}

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.HealthCheck(context.Background()); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("HealthCheck: expected serve error, got nil")
}

func TestClose_cancelledContext(t *testing.T) {
	registry := prometheus.NewRegistry()
	recorder := promrecorder.NewRecorder(promrecorder.Config{Registry: registry})

	hold := make(chan struct{})
	release := make(chan struct{})

	r := chi.NewRouter()
	r.Get("/hold", func(w http.ResponseWriter, req *http.Request) {
		close(hold)
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})

	cfg := Config[*chi.Mux]{
		Label: common.Label{Value: "test_srv"},
		Host:  common.Host{Value: "127.0.0.1"},
		Port:  common.Port{Value: 0},
	}

	built, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}

	server := built.(*srv[*chi.Mux])
	server.Inject([]any{r, recorder})

	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		_ = server.Close(context.Background())
	})

	addr := server.listener.Addr().String()
	go func() {
		client := &http.Client{}
		resp, err := client.Get(fmt.Sprintf("http://%s/hold", addr))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	<-hold

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := server.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with cancelled context: got %v, want context.Canceled", err)
	}
}

func TestConfig_Build_listenError(t *testing.T) {
	cfg := Config[*chi.Mux]{
		Host: common.Host{Value: "127.0.0.1"},
		Port: common.Port{Value: 99999},
	}

	_, err := cfg.Build()
	if err == nil {
		t.Fatal("Build: expected listen error for invalid port")
	}
}
