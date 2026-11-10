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

func TestRemoteReadUsesConfiguredConcurrencyLimit(t *testing.T) {
	cfg := &config.Config{}
	cfg.Prometheus.ExternalURL = "http://example.test"
	cfg.Prometheus.RoutePrefix = "/"
	cfg.Prometheus.RemoteReadConcurrencyLimit = 1
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mux http.ServeMux
	p.Register(&mux)
	payload, err := (&prompb.ReadRequest{}).Marshal()
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
	if rec.Code != http.StatusOK {
		response, _ := io.ReadAll(rec.Result().Body)
		t.Fatalf("remote read status %d, body %q", rec.Code, strings.TrimSpace(string(response)))
	}
}
