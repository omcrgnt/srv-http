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

	server := built.(*Server[*chi.Mux])
	server.Inject([]any{r, recorder})

	stop, err := server.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stop(context.Background())
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
	if strings.Contains(metricsStr, `service="value:\"`) {
		t.Error("metrics: service label must use Label.Value, not proto String()")
	}
	if !strings.Contains(metricsStr, `service="test_srv"`) {
		t.Errorf("metrics: want service=test_srv in body, got excerpt: %.200s", metricsStr)
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

	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := server.HealthCheck(t.Context()); err != nil {
		t.Errorf("HealthCheck after graceful close: %v", err)
	}
}

func TestInject(t *testing.T) {
	mux := chi.NewRouter()
	rec := promrecorder.NewRecorder(promrecorder.Config{Registry: prometheus.NewRegistry()})

	s := &Server[*chi.Mux]{}
	deps := s.Deps()

	if got, want := reflect.TypeOf(deps[0]), reflect.TypeOf((*chi.Mux)(nil)); got != want {
		t.Errorf("Deps()[0] type = %v, want %v", got, want)
	}
	if got, want := reflect.TypeOf(deps[1]), reflect.TypeOf((*metrics.Recorder)(nil)); got != want {
		t.Errorf("Deps()[1] type = %v, want %v", got, want)
	}
	if got, want := reflect.TypeOf(deps[2]), reflect.TypeOf((*gate)(nil)); got != want {
		t.Errorf("Deps()[2] type = %v, want %v", got, want)
	}

	fg := &fakeGate{ready: true}
	s.Inject([]any{mux, rec, gate(fg)})

	if s.Handler != mux {
		t.Error("Inject: Handler not set")
	}
	if s.recorder != rec {
		t.Error("Inject: recorder not set")
	}
	if s.gate != fg {
		t.Error("Inject: gate not set")
	}
}

type fakeGate struct{ ready bool }

func (g *fakeGate) Ready() bool { return g.ready }

func TestConfig_Build_gate(t *testing.T) {
	table := []struct {
		name       string
		gate       *fakeGate
		wantStatus int
	}{
		{name: "no gate wired", gate: nil, wantStatus: http.StatusOK},
		{name: "gate not ready", gate: &fakeGate{ready: false}, wantStatus: http.StatusServiceUnavailable},
		{name: "gate ready", gate: &fakeGate{ready: true}, wantStatus: http.StatusOK},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Get("/ping", func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte("pong"))
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
			server := built.(*Server[*chi.Mux])

			deps := []any{r, promrecorder.NewRecorder(promrecorder.Config{Registry: prometheus.NewRegistry()})}
			if tc.gate != nil {
				deps = append(deps, gate(tc.gate))
			}
			server.Inject(deps)

			stop, err := server.Start(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stop(context.Background()) })

			addr := server.listener.Addr().String()
			time.Sleep(50 * time.Millisecond)

			resp, err := (&http.Client{}).Get(fmt.Sprintf("http://%s/ping", addr))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestConfig_Build_gate_disabled(t *testing.T) {
	// A gate-not-ready would normally 503 (see TestConfig_Build_gate) — this
	// proves DisableGate suppresses that check even with a gate injected,
	// the case ops needs its own readiness endpoint to never be blocked by.
	r := chi.NewRouter()
	r.Get("/ping", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("pong"))
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
	server := built.(*Server[*chi.Mux])
	server.DisableGate()

	rec := promrecorder.NewRecorder(promrecorder.Config{Registry: prometheus.NewRegistry()})
	server.Inject([]any{r, rec, gate(&fakeGate{ready: false})})

	stop, err := server.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	addr := server.listener.Addr().String()
	time.Sleep(50 * time.Millisecond)

	resp, err := (&http.Client{}).Get(fmt.Sprintf("http://%s/ping", addr))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (DisableGate should suppress the gate check)", resp.StatusCode, http.StatusOK)
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

	s := &Server[*chi.Mux]{
		listener: ln,
		initFn:   func(context.Context, *Server[*chi.Mux]) {},
	}

	cleanup, err := s.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Start: got %v, want context.Canceled", err)
	}
	if cleanup != nil {
		t.Error("Start: expected nil cleanup on failure")
	}
	// Start returns no cleanup on this path, so the listener Build already
	// opened would otherwise never be closed by anything — Accept must fail
	// on a closed listener. Bounded deadline instead of a bare blocking
	// Accept: if this regresses to leaking the listener again, Accept would
	// otherwise block forever waiting for a connection that never comes,
	// hanging the whole test run instead of failing it.
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := ln.Accept(); err == nil {
		t.Error("Start: listener was not closed on the already-cancelled-ctx path — fd leak")
	} else if !strings.Contains(err.Error(), "use of closed network connection") {
		t.Errorf("Start: Accept error = %v, want \"use of closed network connection\" (got a deadline timeout instead — listener was never closed)", err)
	}
}

// TestClose_isGraceful proves *Server[T]'s own Close shadows the http.Server
// it embeds: without that shadow, a bare server.Close() call promotes to
// http.Server.Close (hard reset, drops in-flight connections instantly)
// instead of the graceful Shutdown-based stop Start's cleanup uses. A hard
// close would make this test observe closeDone fire immediately, before
// release is ever closed.
func TestClose_isGraceful(t *testing.T) {
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

	server := built.(*Server[*chi.Mux])
	server.Inject([]any{r, recorder})

	if _, err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

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

	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close() }()

	select {
	case <-closeDone:
		t.Fatal("Close() returned before the in-flight request finished — got the embedded http.Server's hard Close, not the graceful stop")
	case <-time.After(100 * time.Millisecond):
		// still blocked waiting for the in-flight request, as expected of a
		// graceful shutdown.
	}

	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
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

	s := &Server[*chi.Mux]{
		listener: ln,
		initFn:   func(context.Context, *Server[*chi.Mux]) {},
		Server: http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	}

	if _, err := s.Start(context.Background()); err != nil {
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

func TestProbeReady_serveError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server[*chi.Mux]{
		listener: ln,
		initFn:   func(context.Context, *Server[*chi.Mux]) {},
		Server: http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	}

	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.ProbeReady(context.Background()); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ProbeReady: expected serve error, got nil")
}

func TestProbeReady_matchesHealthCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &Server[*chi.Mux]{
		listener: ln,
		initFn:   func(context.Context, *Server[*chi.Mux]) {},
		Server: http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	}

	ctx := context.Background()
	stop, err := s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	if err := s.ProbeReady(ctx); err != nil {
		t.Fatalf("ProbeReady after Start: %v", err)
	}
	if err := s.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck after Start: %v", err)
	}
}

// TestProbeReady_gateNotReady: HealthCheck alone (t.err) can't see this —
// Serve hasn't failed, only the gate hasn't opened yet. Without ProbeReady
// also consulting the gate, a k8s readiness probe would mark this pod Ready
// while every real request is still answered 503 by the gate handler.
func TestProbeReady_gateNotReady(t *testing.T) {
	r := chi.NewRouter()
	rec := promrecorder.NewRecorder(promrecorder.Config{Registry: prometheus.NewRegistry()})

	cfg := Config[*chi.Mux]{
		Label: common.Label{Value: "test_srv"},
		Host:  common.Host{Value: "127.0.0.1"},
		Port:  common.Port{Value: 0},
	}
	built, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	server := built.(*Server[*chi.Mux])
	server.Inject([]any{r, rec, gate(&fakeGate{ready: false})})

	ctx := context.Background()
	stop, err := server.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	if err := server.ProbeReady(ctx); err == nil {
		t.Fatal("ProbeReady: expected error while gate is not ready, got nil")
	}
	if err := server.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: got %v, want nil (gate readiness is ProbeReady's concern, not HealthCheck's)", err)
	}
}

// TestProbeReady_gateDisabled_ignoresNotReadyGate mirrors
// TestConfig_Build_gate_disabled at the ProbeReady level: DisableGate must
// suppress the gate check here too, not just in the request handler.
func TestProbeReady_gateDisabled_ignoresNotReadyGate(t *testing.T) {
	r := chi.NewRouter()
	rec := promrecorder.NewRecorder(promrecorder.Config{Registry: prometheus.NewRegistry()})

	cfg := Config[*chi.Mux]{
		Label: common.Label{Value: "test_srv"},
		Host:  common.Host{Value: "127.0.0.1"},
		Port:  common.Port{Value: 0},
	}
	built, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	server := built.(*Server[*chi.Mux])
	server.DisableGate()
	server.Inject([]any{r, rec, gate(&fakeGate{ready: false})})

	ctx := context.Background()
	stop, err := server.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	if err := server.ProbeReady(ctx); err != nil {
		t.Fatalf("ProbeReady: got %v, want nil (DisableGate should suppress the gate check)", err)
	}
}

func TestStop_cancelledContext(t *testing.T) {
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

	server := built.(*Server[*chi.Mux])
	server.Inject([]any{r, recorder})

	stop, err := server.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		_ = stop(context.Background())
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

	if err := stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with cancelled context: got %v, want context.Canceled", err)
	}
}

func TestServer_BuildConfig(t *testing.T) {
	slot := &Server[*chi.Mux]{}
	mat, err := slot.BuildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mat.(*Config[*chi.Mux]); !ok {
		t.Fatalf("BuildConfig: got %T, want *Config[*chi.Mux]", mat)
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
