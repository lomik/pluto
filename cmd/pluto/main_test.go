package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lomik/pluto/pkg/config"
	"github.com/lomik/pluto/pkg/listen"
	"github.com/prometheus/client_golang/prometheus"
)

func TestRegisterRoutes(t *testing.T) {
	const (
		shared = "127.0.0.1:19095"
		other  = "127.0.0.1:19096"
	)

	type check struct {
		addr, method, path, body string
		status                   int
		contains, location       string
	}

	tests := []struct {
		name   string
		setup  func(*config.Config)
		checks []check
	}{
		{
			name: "all enabled on one address, prometheus at root",
			setup: func(cfg *config.Config) {
				cfg.Insert.Enabled = true
				cfg.Debug.Enabled = true
				cfg.Debug.Metrics = true
				cfg.Debug.Pprof = true
				cfg.Prometheus.Enabled = true
				cfg.Prometheus.RoutePrefix = "/"
			},
			checks: []check{
				{shared, http.MethodPost, "/api/v1/write", "not snappy", http.StatusBadRequest, "", ""},
				{shared, http.MethodGet, "/metrics", "", http.StatusOK, "go_build_info", ""},
				{shared, http.MethodGet, "/debug/pprof/", "", http.StatusOK, "Types of profiles", ""},
				{shared, http.MethodGet, "/-/healthy", "", http.StatusOK, "Healthy", ""},
				{shared, http.MethodGet, "/", "", http.StatusFound, "", "/query"},
				{shared, http.MethodGet, "/api/v1/status/buildinfo", "", http.StatusOK, "success", ""},
			},
		},
		{
			name: "prometheus prefix and distinct address",
			setup: func(cfg *config.Config) {
				cfg.Insert.Enabled = true
				cfg.Debug.Enabled = true
				cfg.Debug.Pprof = true
				cfg.Prometheus.Enabled = true
				cfg.Prometheus.Listen = other
				cfg.Prometheus.RoutePrefix = "/prom"
			},
			checks: []check{
				{shared, http.MethodPost, "/api/v1/write", "not snappy", http.StatusBadRequest, "", ""},
				{shared, http.MethodGet, "/debug/pprof/", "", http.StatusOK, "Types of profiles", ""},
				{shared, http.MethodGet, "/metrics", "", http.StatusNotFound, "", ""},
				{shared, http.MethodGet, "/prom/-/healthy", "", http.StatusNotFound, "", ""},
				{other, http.MethodGet, "/prom/-/healthy", "", http.StatusOK, "Healthy", ""},
				{other, http.MethodGet, "/prom/", "", http.StatusFound, "", "/prom/query"},
				{other, http.MethodGet, "/prom/api/v1/status/buildinfo", "", http.StatusOK, "success", ""},
				{other, http.MethodPost, "/api/v1/write", "not snappy", http.StatusNotFound, "", ""},
			},
		},
		{
			name: "prometheus prefix shares the insert and debug address",
			setup: func(cfg *config.Config) {
				cfg.Insert.Enabled = true
				cfg.Debug.Enabled = true
				cfg.Debug.Metrics = true
				cfg.Prometheus.Enabled = true
				cfg.Prometheus.RoutePrefix = "/prom"
			},
			checks: []check{
				{shared, http.MethodPost, "/api/v1/write", "not snappy", http.StatusBadRequest, "", ""},
				{shared, http.MethodGet, "/metrics", "", http.StatusOK, "go_build_info", ""},
				{shared, http.MethodGet, "/prom/-/ready", "", http.StatusOK, "Ready", ""},
				{shared, http.MethodGet, "/prom/api/v1/status/buildinfo", "", http.StatusOK, "success", ""},
				{shared, http.MethodGet, "/-/ready", "", http.StatusNotFound, "", ""},
			},
		},
		{
			name: "exact debug metrics route takes precedence over prometheus prefix",
			setup: func(cfg *config.Config) {
				cfg.Debug.Enabled = true
				cfg.Debug.Metrics = true
				cfg.Prometheus.Enabled = true
				cfg.Prometheus.RoutePrefix = "/metrics"
			},
			checks: []check{
				{shared, http.MethodGet, "/metrics", "", http.StatusOK, "go_build_info", ""},
				{shared, http.MethodGet, "/metrics/", "", http.StatusFound, "", "/metrics/query"},
				{shared, http.MethodGet, "/metrics/-/ready", "", http.StatusOK, "Ready", ""},
			},
		},
		{
			name: "all optional routes disabled",
			setup: func(cfg *config.Config) {
				cfg.Debug.Enabled = true
			},
			checks: []check{
				{shared, http.MethodPost, "/api/v1/write", "not snappy", http.StatusNotFound, "", ""},
				{shared, http.MethodGet, "/metrics", "", http.StatusNotFound, "", ""},
				{shared, http.MethodGet, "/debug/pprof/", "", http.StatusNotFound, "", ""},
				{shared, http.MethodGet, "/-/healthy", "", http.StatusNotFound, "", ""},
			},
		},
		{
			name: "disabled debug does not expose metrics or pprof",
			setup: func(cfg *config.Config) {
				cfg.Insert.Enabled = true
				cfg.Debug.Metrics = true
				cfg.Debug.Pprof = true
			},
			checks: []check{
				{shared, http.MethodPost, "/api/v1/write", "not snappy", http.StatusBadRequest, "", ""},
				{shared, http.MethodGet, "/metrics", "", http.StatusNotFound, "", ""},
				{shared, http.MethodGet, "/debug/pprof/", "", http.StatusNotFound, "", ""},
			},
		},
	}

	// prom.New registers scrape collectors globally. Give each setup its own registry,
	// just as separate Pluto processes would have, without leaking state between tests.
	oldRegisterer, oldGatherer := prometheus.DefaultRegisterer, prometheus.DefaultGatherer
	defer func() {
		prometheus.DefaultRegisterer, prometheus.DefaultGatherer = oldRegisterer, oldGatherer
	}()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			prometheus.DefaultRegisterer, prometheus.DefaultGatherer = registry, registry
			cfg := &config.Config{}
			cfg.Insert.Listen = shared
			cfg.Debug.Listen = shared
			cfg.Prometheus.Listen = shared
			cfg.Prometheus.ExternalURL = "http://localhost:19095"
			tt.setup(cfg)

			manager := listen.NewHTTP()
			if err := registerRoutes(context.Background(), cfg, manager); err != nil {
				t.Fatalf("registerRoutes: %v", err)
			}
			for _, c := range tt.checks {
				req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
				rec := httptest.NewRecorder()
				manager.Mux(c.addr).ServeHTTP(rec, req)
				if rec.Code != c.status {
					t.Errorf("%s %s on %s: status = %d, want %d; body = %q", c.method, c.path, c.addr, rec.Code, c.status, rec.Body.String())
				}
				if c.contains != "" && !strings.Contains(rec.Body.String(), c.contains) {
					t.Errorf("%s on %s: body = %q, want substring %q", c.path, c.addr, rec.Body.String(), c.contains)
				}
				if c.location != "" && rec.Header().Get("Location") != c.location {
					t.Errorf("%s on %s: Location = %q, want %q", c.path, c.addr, rec.Header().Get("Location"), c.location)
				}
			}
		})
	}
}

func TestRegisterRoutesReportsPrometheusError(t *testing.T) {
	registry := prometheus.NewRegistry()
	oldRegisterer := prometheus.DefaultRegisterer
	prometheus.DefaultRegisterer = registry
	defer func() { prometheus.DefaultRegisterer = oldRegisterer }()

	cfg := &config.Config{}
	cfg.Prometheus.Enabled = true
	cfg.Prometheus.Listen = "127.0.0.1:19096"
	cfg.Prometheus.ExternalURL = "http://%"
	if err := registerRoutes(context.Background(), cfg, listen.NewHTTP()); err == nil {
		t.Fatal("registerRoutes accepted a malformed Prometheus external URL")
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMainExitsOnSignal(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			testMainExitsOnSignal(t, sig)
		})
	}
}

func testMainExitsOnSignal(t *testing.T, sig os.Signal) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	configContents := fmt.Sprintf("insert:\n  enabled: false\nprometheus:\n  enabled: false\ndebug:\n  enabled: true\n  listen: %s\n  pprof: true\n  metrics: false\n", addr)
	if err := os.WriteFile(configFile, []byte(configContents), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainSignalHelper$")
	// The helper passes -dev together with an explicit -config: the flag must
	// not prevent the requested configuration from loading.
	cmd.Env = append(os.Environ(), "PLUTO_SIGNAL_HELPER="+configFile)
	var output safeBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/debug/pprof/")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("server did not start: last error %v; output: %s", err, output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s exit: %v; output: %s", sig, err, output.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("server did not exit on %s; output: %s", sig, output.String())
	}
}

func TestMainReportsOccupiedListenAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	configContents := fmt.Sprintf("insert:\n  enabled: true\n  listen: %s\nprometheus:\n  enabled: false\ndebug:\n  enabled: false\n", listener.Addr().String())
	if err := os.WriteFile(configFile, []byte(configContents), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainSignalHelper$")
	cmd.Env = append(os.Environ(), "PLUTO_SIGNAL_HELPER="+configFile)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("occupied listener hung: %v; output: %s", ctx.Err(), output)
	}
	if _, ok := err.(*exec.ExitError); !ok || !strings.Contains(string(output), "address already in use") {
		t.Fatalf("occupied listener: got %v, output %s; want bind error and nonzero exit", err, output)
	}
}

func TestMainReportsListenError(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	configContents := "insert:\n  enabled: false\nprometheus:\n  enabled: false\ndebug:\n  enabled: true\n  listen: '127.0.0.1:notaport'\n  pprof: true\n  metrics: false\n"
	if err := os.WriteFile(configFile, []byte(configContents), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainSignalHelper$")
	cmd.Env = append(os.Environ(), "PLUTO_SIGNAL_HELPER="+configFile)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid listen address unexpectedly succeeded: %s", output)
	}
	if _, ok := err.(*exec.ExitError); !ok || !strings.Contains(string(output), "notaport") {
		t.Fatalf("invalid listen address: got %v, output %s; want nonzero exit with listen error", err, output)
	}
}

func TestMainSignalHelper(t *testing.T) {
	configFile := os.Getenv("PLUTO_SIGNAL_HELPER")
	if configFile == "" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("pluto", flag.ExitOnError)
	os.Args = []string{"pluto", "-dev", "-config", configFile}
	main()
}
