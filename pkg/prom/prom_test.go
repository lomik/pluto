package prom

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/lomik/pluto/pkg/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"
)

func Test_joinPrefix(t *testing.T) {
	type args struct {
		pfx string
		p   string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		// TODO: Add test cases.
		{name: "no prefix",
			args: args{pfx: "/", p: "/api/v1/labels"},
			want: "/api/v1/labels"},
		{name: "with prefix",
			args: args{pfx: "/prom", p: "/api/v1/labels"},
			want: "/prom/api/v1/labels"},
		{name: "with prefix no leading slash",
			args: args{pfx: "prom", p: "/api/v1/labels"},
			want: "/prom/api/v1/labels"},
		{name: "with prefix no trailing slash",
			args: args{pfx: "/prom", p: "api/v1/labels"},
			want: "/prom/api/v1/labels"},
		{name: "with prefix no slashes",
			args: args{pfx: "prom", p: "api/v1/labels"},
			want: "/prom/api/v1/labels"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinPrefix(tt.args.pfx, tt.args.p); got != tt.want {
				t.Errorf("joinPrefix(%#v,%#v) = %v, want %#v", tt.args.pfx, tt.args.p, got, tt.want)
			}
		})
	}
}

func newRemoteReadTestProm(t *testing.T) (*Prom, *httptest.Server) {
	t.Helper()

	// New registers scrape metrics globally. Keep this test serial and isolate
	// the registry so repeated runs do not leave duplicate collectors behind.
	oldRegisterer, oldGatherer := prometheus.DefaultRegisterer, prometheus.DefaultGatherer
	registry := prometheus.NewRegistry()
	prometheus.DefaultRegisterer, prometheus.DefaultGatherer = registry, registry
	t.Cleanup(func() {
		prometheus.DefaultRegisterer, prometheus.DefaultGatherer = oldRegisterer, oldGatherer
	})

	clickhouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(clickhouse.Close)

	cfg := &config.Config{}
	cfg.Prometheus.ExternalURL = "http://example.test"
	cfg.Prometheus.RoutePrefix = "/"
	cfg.Prometheus.RemoteReadConcurrencyLimit = 1
	cfg.ClickHouse.DSN = clickhouse.URL
	cfg.Extension.HTTPClient = clickhouse.Client()
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, clickhouse
}

func remoteReadRequest(t *testing.T, p *Prom, request *prompb.ReadRequest) *httptest.ResponseRecorder {
	t.Helper()
	var mux http.ServeMux
	p.Register(&mux)
	payload, err := request.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/read", bytes.NewReader(snappy.Encode(nil, payload))).WithContext(ctx)
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestRemoteReadUsesConfiguredConcurrencyLimit(t *testing.T) {
	p, _ := newRemoteReadTestProm(t)
	rec := remoteReadRequest(t, p, &prompb.ReadRequest{})
	if rec.Code != http.StatusOK {
		response, _ := io.ReadAll(rec.Result().Body)
		t.Fatalf("remote read status %d, body %q", rec.Code, strings.TrimSpace(string(response)))
	}
}

func TestRemoteReadSamplesAndStreamedChunkResponseTypes(t *testing.T) {
	t.Run("samples", func(t *testing.T) {
		p, _ := newRemoteReadTestProm(t)
		rec := remoteReadRequest(t, p, &prompb.ReadRequest{
			Queries:               []*prompb.Query{{StartTimestampMs: 0, EndTimestampMs: 1}},
			AcceptedResponseTypes: []prompb.ReadRequest_ResponseType{prompb.ReadRequest_SAMPLES},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("samples remote read status %d, body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	})

	t.Run("streamed chunks are rejected without panic", func(t *testing.T) {
		p, _ := newRemoteReadTestProm(t)
		rec := remoteReadRequest(t, p, &prompb.ReadRequest{
			Queries:               []*prompb.Query{{StartTimestampMs: 0, EndTimestampMs: 1}},
			AcceptedResponseTypes: []prompb.ReadRequest_ResponseType{prompb.ReadRequest_STREAMED_XOR_CHUNKS},
		})
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "streamed remote read is not supported") {
			t.Fatalf("streamed remote read status %d, body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	})
}
