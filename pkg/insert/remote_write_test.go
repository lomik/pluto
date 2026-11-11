package insert

import (
	"bytes"
	"context"
	"errors"
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

func insertConfig(dsn string) *config.Config {
	cfg := &config.Config{ClickHouse: config.ClickHouse{DSN: dsn}}
	cfg.Insert.Table = "samples_null"
	return cfg
}

func TestPrometheusRemoteWrite(t *testing.T) {
	request := testWriteRequest(t, prompb.TimeSeries{
		Labels:  []prompb.Label{{Name: "region", Value: "eu"}, {Name: "__name__", Value: "http_requests"}},
		Samples: []prompb.Sample{{Timestamp: 101, Value: 2.5}, {Timestamp: 102, Value: 3}},
	})
	chBodies := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("ClickHouse method: %q", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ClickHouse read: %v", err)
		}
		chBodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := NewPrometheusRemoteWrite(Opts{Config: insertConfig(upstream.URL)})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/write", bytes.NewReader(snappy.Encode(nil, request)))
	r.Header.Set("Content-Encoding", "snappy")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %q", w.Code, w.Body.String())
	}
	body := <-chBodies
	const prefix = "INSERT INTO samples_null FORMAT RowBinaryWithNamesAndTypes\n"
	if !bytes.HasPrefix(body, []byte(prefix)) {
		t.Fatalf("ClickHouse query: %q", body)
	}
	rows := decodeInsertRows(t, body[len(prefix):])
	if len(rows) != 2 || rows[0].name != "http_requests" || rows[0].time != 101 || rows[0].value != 2.5 || rows[1].time != 102 || rows[1].id != rows[0].id || rows[1].value != 3 {
		t.Fatalf("rows: %#v", rows)
	}
	if len(rows[0].labels) != 2 || string(rows[0].labels[0].Name) != "__name__" || string(rows[0].labels[1].Name) != "region" {
		t.Fatalf("labels not sorted: %#v", rows[0].labels)
	}
}

func TestPrometheusRemoteWriteCloseConnections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	cfg := insertConfig(upstream.URL)
	cfg.Insert.CloseConnections = true
	server := httptest.NewServer(NewPrometheusRemoteWrite(Opts{Config: cfg}))
	defer server.Close()

	payload := testWriteRequest(t, prompb.TimeSeries{
		Labels:  []prompb.Label{{Name: "__name__", Value: "requests"}},
		Samples: []prompb.Sample{{Timestamp: 12, Value: 3}},
	})
	req, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(snappy.Encode(nil, payload)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("success should return HTTP 200 even when connection closes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !resp.Close {
		t.Fatalf("status=%d, Close=%t", resp.StatusCode, resp.Close)
	}
}

func TestPrometheusRemoteWriteUpstreamWriteError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force a write failure while the insert streams its rows.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unavailable")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	metric := prompb.TimeSeries{
		Labels:  []prompb.Label{{Name: "__name__", Value: "m"}},
		Samples: make([]prompb.Sample, 100000),
	}
	payload := testWriteRequest(t, metric)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(snappy.Encode(nil, payload))).WithContext(ctx)
	w := httptest.NewRecorder()
	NewPrometheusRemoteWrite(Opts{Config: insertConfig(upstream.URL)}).ServeHTTP(w, r)
	if err := ctx.Err(); err != nil {
		t.Fatalf("handler relied on the deadline to return: %v", err)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, body=%q; want 502", w.Code, w.Body.String())
	}
}

func TestPrometheusRemoteWriteErrors(t *testing.T) {
	upstreamCalled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled <- struct{}{}
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "clickhouse failure", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cases := []struct {
		name     string
		body     io.Reader
		config   *config.Config
		status   int
		message  string
		upstream bool
	}{
		{"read error", errorReader{}, insertConfig(upstream.URL), http.StatusBadRequest, "cannot read", false},
		{"invalid snappy", strings.NewReader("not snappy"), insertConfig(upstream.URL), http.StatusBadRequest, "snappy", false},
		{"invalid protobuf", bytes.NewReader(snappy.Encode(nil, []byte{0x0a, 0x04, 0x01})), insertConfig(upstream.URL), http.StatusBadRequest, "invalid protobuf payload", false},
		{"zero protobuf tag", bytes.NewReader(snappy.Encode(nil, []byte{0})), insertConfig(upstream.URL), http.StatusBadRequest, "invalid protobuf payload", false},
		{"wrong label wire type", bytes.NewReader(snappy.Encode(nil, []byte{0x0a, 0x04, 0x0a, 0x02, 0x08, 0x01})), insertConfig(upstream.URL), http.StatusBadRequest, "invalid protobuf payload", false},
		{"truncated unknown field", bytes.NewReader(snappy.Encode(nil, []byte{0x52, 0x04, 0x01})), insertConfig(upstream.URL), http.StatusBadRequest, "invalid protobuf payload", false},
		{"unsupported group", bytes.NewReader(snappy.Encode(nil, []byte{0x53, 0x08, 0x01})), insertConfig(upstream.URL), http.StatusBadRequest, "invalid protobuf payload", false},
		{"discovery error", bytes.NewReader(snappy.Encode(nil, testWriteRequest(t))), func() *config.Config {
			cfg := insertConfig(upstream.URL)
			cfg.Extension.ClickHouseDiscovery = func(context.Context, string) (string, error) { return "", errors.New("discovery failed") }
			return cfg
		}(), http.StatusBadGateway, "discovery failed", false},
		{"upstream error", bytes.NewReader(snappy.Encode(nil, testWriteRequest(t))), insertConfig(upstream.URL), http.StatusBadGateway, "clickhouse failure", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/write", tc.body)
			w := httptest.NewRecorder()
			NewPrometheusRemoteWrite(Opts{Config: tc.config}).ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(strings.ToLower(w.Body.String()), tc.message) {
				t.Fatalf("got status %d, body %q; want status %d and %q", w.Code, w.Body.String(), tc.status, tc.message)
			}
			select {
			case <-upstreamCalled:
				if !tc.upstream {
					t.Fatal("invalid request unexpectedly sent to ClickHouse")
				}
			default:
				if tc.upstream {
					t.Fatal("expected request at ClickHouse")
				}
			}
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("cannot read") }
