package prom

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneOfOne/xxhash"
	"github.com/lomik/pluto/pkg/config"
	"github.com/lomik/pluto/pkg/sql"
	"github.com/lomik/rowbinary"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

type clickhouseCall struct {
	contentType string
	body        []byte
}

func (c clickhouseCall) fields(t *testing.T) map[string][]byte {
	t.Helper()
	if !strings.HasPrefix(c.contentType, "multipart/form-data;") {
		return map[string][]byte{"query": c.body}
	}
	_, params, err := mime.ParseMediaType(c.contentType)
	if err != nil {
		t.Fatal(err)
	}
	r := multipart.NewReader(bytes.NewReader(c.body), params["boundary"])
	fields := make(map[string][]byte)
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		fields[p.FormName()], err = io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
	}
	return fields
}

func fakeQuerier(t *testing.T, respond func(clickhouseCall) (int, []byte)) (*Querier, <-chan clickhouseCall) {
	t.Helper()
	calls := make(chan clickhouseCall, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := clickhouseCall{r.Header.Get("Content-Type"), body}
		calls <- call
		status, response := respond(call)
		w.WriteHeader(status)
		_, _ = w.Write(response)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{ClickHouse: config.ClickHouse{DSN: server.URL}}
	cfg.Extension.HTTPClient = server.Client()
	cfg.Select.TableSeries = "series_test"
	cfg.Select.TableSamples = "samples_test"
	cfg.Select.SeriesPartitionMs = 86400000
	cfg.Select.AutocompleteLookback = 2 * time.Hour
	return &Querier{config: cfg, mint: 1000, maxt: 9000}, calls
}

func binaryRows(t *testing.T, columns []rowbinary.Any, rows ...[]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	opts := []rowbinary.FormatOption{rowbinary.RowBinary}
	for i, col := range columns {
		opts = append(opts, rowbinary.C(fmt.Sprintf("col%d", i), col))
	}
	w := rowbinary.NewFormatWriter(&buf, opts...)
	for _, row := range rows {
		if err := w.WriteAny(row...); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func oneSeries(t *testing.T, id string, lb labels.Labels) []byte {
	return binaryRows(t, []rowbinary.Any{rowbinary.String, ColumnLabels}, []any{id, lb})
}

func requireQueryContains(t *testing.T, call clickhouseCall, parts ...string) {
	t.Helper()
	q := string(call.fields(t)["query"])
	for _, part := range parts {
		if !strings.Contains(q, part) {
			t.Errorf("query missing %q: %s", part, q)
		}
	}
}

func TestSelectSeriesSQLAndRowBinary(t *testing.T) {
	lb := labels.FromStrings("__name__", "http_requests_total", "job", "api")
	response := oneSeries(t, "id1", lb)
	q, calls := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, response })
	q.config.Select.SeriesMaterializedLabels = []string{"job"}
	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "__name__", "http_requests_total"),
		labels.MustNewMatcher(labels.MatchEqual, "job", "a'b"),
		labels.MustNewMatcher(labels.MatchNotEqual, "env", "dev"),
		labels.MustNewMatcher(labels.MatchRegexp, "region", "eu|us"),
		labels.MustNewMatcher(labels.MatchNotRegexp, "instance", "bad.*"),
	}
	got, err := q.selectSeries(context.Background(), &storage.SelectHints{Start: 1000, End: 9000}, matchers)
	if err != nil || !labels.Equal(got["id1"], lb) {
		t.Fatalf("selectSeries = %v, %v", got, err)
	}
	requireQueryContains(t, <-calls, "FROM series_test", "`timestamp_min` >= -86399000", "`timestamp_min` <= 9000",
		"`timestamp_max` >= 1000", "`name` = 'http_requests_total'", "`label_job` = 'a\\'b'",
		"arrayElement(labels, 'env') != 'dev'", "match(arrayElement(labels, 'region'), '^(?:eu|us)$')",
		"NOT match(arrayElement(labels, 'instance'), '^(?:bad.*)$')", "GROUP BY id")
}

func TestSelectSeriesMalformedResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"truncated id", []byte{4, 'a'}},
		{"truncated labels", []byte{1, 'a', 1, 3, 'j'}},
		{"huge labels count", []byte{1, 'a', 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 1}},
		{"valid row then truncated row", append(oneSeries(t, "id", labels.FromStrings("job", "ok")), 3, 'i')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, tc.body })
			got, err := q.selectSeries(context.Background(), &storage.SelectHints{Start: 1000, End: 9000}, nil)
			if err == nil || got != nil {
				t.Fatalf("malformed series must return error, got %v, %v", got, err)
			}
		})
	}
}

func TestSelectSeriesHTTPError(t *testing.T) {
	q, _ := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 500, []byte("clickhouse unavailable") })
	set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1000, End: 9000})
	if set.Next() || set.Err() == nil || !strings.Contains(set.Err().Error(), "clickhouse unavailable") {
		t.Fatalf("Select must return a failed series set, got %v", set.Err())
	}
}

func TestSelectNilHintsUsesQuerierBounds(t *testing.T) {
	response := oneSeries(t, "id", labels.FromStrings("job", "api"))
	q, calls := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 200, binaryRows(t, []rowbinary.Any{rowbinary.UInt32, rowbinary.Int64, rowbinary.Float64},
				[]any{xxhash.ChecksumString32("id"), int64(1000), float64(3)})
		}
		return 200, response
	})
	set := q.Select(context.Background(), false, nil)
	if err := set.Err(); err != nil || !set.Next() {
		t.Fatalf("Select(nil) failed: %v", err)
	}
	requireQueryContains(t, <-calls, "`timestamp_max` >= 1000", "`timestamp_min` <= 9000")
	requireQueryContains(t, <-calls, "timestamp >= 1000-1000", "timestamp <= 9000", "intDiv(timestamp-1000, 1000)")
}

func TestSelectSampleRowsAndMultipartIDs(t *testing.T) {
	lbA := labels.FromStrings("job", "api")
	lbB := labels.FromStrings("job", "web")
	seriesResponse := binaryRows(t, []rowbinary.Any{rowbinary.String, ColumnLabels},
		[]any{"id-a", lbA}, []any{"id-a2", lbA}, []any{"id-b", lbB})
	var sampleBuf bytes.Buffer
	w := rowbinary.NewFormatWriter(&sampleBuf, rowbinary.RowBinary,
		rowbinary.C("hash", rowbinary.UInt32), rowbinary.C("timestamp", rowbinary.Int64), rowbinary.C("value", rowbinary.Float64))
	for _, s := range []struct {
		id string
		ts int64
		v  float64
	}{
		{"id-a", 4000, 4}, {"id-b", 3000, 3}, {"id-a2", 2000, 2}, {"id-a", 1000, 1},
	} {
		if err := w.WriteAny(xxhash.ChecksumString32(s.id), s.ts, s.v); err != nil {
			t.Fatal(err)
		}
	}
	q, calls := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 200, sampleBuf.Bytes()
		}
		return 200, seriesResponse
	})
	set := q.Select(context.Background(), true, &storage.SelectHints{Start: 1000, End: 9000, Step: 2000})
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	if !set.Next() || !labels.Equal(set.At().Labels(), lbA) {
		t.Fatalf("first series = %v", set.At())
	}
	it := set.At().Iterator(nil)
	for _, want := range []sample{{1000, 1}, {2000, 2}, {4000, 4}} {
		if it.Next() != chunkenc.ValFloat {
			t.Fatalf("missing sample %+v", want)
		}
		ts, val := it.At()
		if ts != want.timestamp || val != want.value || it.AtT() != want.timestamp {
			t.Errorf("sample = %v,%v, want %+v", ts, val, want)
		}
	}
	if it.Next() != chunkenc.ValNone || it.Err() != nil {
		t.Fatalf("iterator should be exhausted, err=%v", it.Err())
	}
	if !set.Next() || !labels.Equal(set.At().Labels(), lbB) {
		t.Fatalf("second series = %v, err=%v", set.At(), set.Err())
	}
	if iterator := set.At().Iterator(nil); iterator.Next() != chunkenc.ValFloat || iterator.AtT() != 3000 || iterator.Next() != chunkenc.ValNone {
		t.Fatal("second series samples invalid")
	}
	if set.Next() || set.Err() != nil {
		t.Fatalf("unexpected extra series or error: %v", set.Err())
	}
	<-calls
	call := <-calls
	fields := call.fields(t)
	for field, want := range map[string]string{"ids_format": "RowBinary", "ids_structure": "id String"} {
		if string(fields[field]) != want {
			t.Errorf("%s = %q, want %q", field, fields[field], want)
		}
	}
	reader := rowbinary.NewFormatReader(bufio.NewReader(bytes.NewReader(fields["ids"])), rowbinary.RowBinary, rowbinary.C("id", rowbinary.String))
	ids := map[string]bool{}
	for reader.Next() {
		id, err := rbRead(reader, rowbinary.String)
		if err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	if reader.Err() != nil || len(ids) != 3 || !ids["id-a"] || !ids["id-a2"] || !ids["id-b"] {
		t.Errorf("multipart ids = %v, err=%v", ids, reader.Err())
	}
	requireQueryContains(t, call, "FROM samples_test", "xxHash32(`id`) as id_hash",
		"timestamp >= 1000-2000", "timestamp <= 9000", "intDiv(timestamp-1000, 2000)")
}

func TestSelectUnknownSampleID(t *testing.T) {
	response := oneSeries(t, "known", labels.FromStrings("job", "api"))
	q, _ := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 200, binaryRows(t, []rowbinary.Any{rowbinary.UInt32, rowbinary.Int64, rowbinary.Float64},
				[]any{xxhash.ChecksumString32("unknown"), int64(1000), float64(3)})
		}
		return 200, response
	})
	set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1000, End: 9000})
	if set.Next() || set.Err() == nil {
		t.Fatalf("unknown hash must be rejected, err=%v", set.Err())
	}
}

func TestSelectMalformedSample(t *testing.T) {
	response := oneSeries(t, "known", labels.FromStrings("job", "api"))
	hash := binaryRows(t, []rowbinary.Any{rowbinary.UInt32}, []any{xxhash.ChecksumString32("known")})
	valid := binaryRows(t, []rowbinary.Any{rowbinary.UInt32, rowbinary.Int64, rowbinary.Float64},
		[]any{xxhash.ChecksumString32("known"), int64(1000), float64(1)})
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"hash", []byte{1, 2}},
		{"timestamp", append(bytes.Clone(hash), 1, 2)},
		{"value", append(bytes.Clone(hash), binaryRows(t, []rowbinary.Any{rowbinary.Int64}, []any{int64(1000)})...)},
		{"second row", append(bytes.Clone(valid), hash[:2]...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
				if strings.HasPrefix(call.contentType, "multipart/") {
					return 200, tc.body
				}
				return 200, response
			})
			set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1000, End: 9000})
			if set.Next() || set.Err() == nil {
				t.Fatalf("truncated sample must return error, got %v", set.Err())
			}
		})
	}
}

func TestSelectSampleHTTPError(t *testing.T) {
	response := oneSeries(t, "id", labels.FromStrings("job", "api"))
	q, _ := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 500, []byte("samples unavailable")
		}
		return 200, response
	})
	set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1000, End: 9000})
	if set.Next() || set.Err() == nil || !strings.Contains(set.Err().Error(), "samples unavailable") {
		t.Fatalf("sample HTTP error not propagated: %v", set.Err())
	}
}

func TestSelectSeriesFunctionSkipsSamples(t *testing.T) {
	response := binaryRows(t, []rowbinary.Any{rowbinary.String, ColumnLabels},
		[]any{"id-z", labels.FromStrings("job", "z")}, []any{"id-a", labels.FromStrings("job", "a")})
	q, calls := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, response })
	set := q.Select(context.Background(), true, &storage.SelectHints{Start: 1000, End: 9000, Func: "series"})
	if !set.Next() || set.At().Labels().Get("job") != "a" || !set.Next() || set.At().Labels().Get("job") != "z" || set.Next() || set.Err() != nil {
		t.Fatalf("series result not returned: %v", set.Err())
	}
	if len(calls) != 1 {
		t.Errorf("series function issued %d requests, want one", len(calls))
	}
}

func TestSelectEmptySeriesSkipsSampleRequest(t *testing.T) {
	q, calls := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, nil })
	set := q.Select(context.Background(), true, &storage.SelectHints{Start: 1000, End: 9000})
	if next := set.Next(); next || set.Err() != nil || len(calls) != 1 {
		t.Fatalf("empty series: Next=%v Err=%v requests=%d", next, set.Err(), len(calls))
	}
}

func TestSelectUInt32TimestampSmallStep(t *testing.T) {
	response := oneSeries(t, "id", labels.FromStrings("job", "api"))
	q, calls := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 200, binaryRows(t, []rowbinary.Any{rowbinary.UInt32, rowbinary.UInt32, rowbinary.Float64},
				[]any{xxhash.ChecksumString32("id"), uint32(2), float64(4)})
		}
		return 200, response
	})
	q.config.Select.SamplesTimestampUInt32 = true
	set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1500, End: 4500, Step: 500})
	if !set.Next() || set.Err() != nil {
		t.Fatalf("no uint32 sample, err=%v", set.Err())
	}
	it := set.At().Iterator(nil)
	if it.Next() != chunkenc.ValFloat {
		t.Fatal("no sample")
	}
	ts, value := it.At()
	if ts != 2000 || value != 4 {
		t.Errorf("sample = %v,%v, want 2000,4", ts, value)
	}
	<-calls
	requireQueryContains(t, <-calls, "timestamp >= 1-1", "timestamp <= 4", "intDiv(timestamp-1, 1)")
}

func TestSelectHashCollisionFallsBackToFullIDs(t *testing.T) {
	const idA, idB = "35566", "149360" // xxHash32 collision.
	if xxhash.ChecksumString32(idA) != xxhash.ChecksumString32(idB) {
		t.Fatal("test IDs must collide")
	}
	seriesResponse := binaryRows(t, []rowbinary.Any{rowbinary.String, ColumnLabels},
		[]any{idA, labels.FromStrings("job", "a")}, []any{idB, labels.FromStrings("job", "b")})
	q, calls := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 200, binaryRows(t, []rowbinary.Any{rowbinary.String, rowbinary.Int64, rowbinary.Float64},
				[]any{idA, int64(1000), float64(1)}, []any{idB, int64(2000), float64(2)})
		}
		return 200, seriesResponse
	})
	set := q.Select(context.Background(), true, &storage.SelectHints{Start: 1000, End: 9000})
	for _, name := range []string{"a", "b"} {
		if !set.Next() || set.At().Labels().Get("job") != name {
			t.Fatalf("missing series %q, err=%v", name, set.Err())
		}
	}
	if set.Next() || set.Err() != nil {
		t.Fatalf("extra series, err=%v", set.Err())
	}
	<-calls
	requireQueryContains(t, <-calls, "`id` as id_hash")
}

func TestSelectRejectsUnknownFullID(t *testing.T) {
	seriesResponse := binaryRows(t, []rowbinary.Any{rowbinary.String, ColumnLabels},
		[]any{"35566", labels.FromStrings("job", "a")}, []any{"149360", labels.FromStrings("job", "b")})
	q, _ := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 200, binaryRows(t, []rowbinary.Any{rowbinary.String, rowbinary.Int64, rowbinary.Float64},
				[]any{"unknown", int64(1000), float64(1)})
		}
		return 200, seriesResponse
	})
	set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1000, End: 9000})
	if set.Next() || set.Err() == nil || !strings.Contains(set.Err().Error(), "unknown series") {
		t.Fatalf("unexpected unknown ID result: %v", set.Err())
	}
}

func TestSelectLabelsDoNotCollide(t *testing.T) {
	// The old labelsMapKey joined pairs with '&' and '=' and collapsed these two distinct label sets.
	first := labels.FromStrings("a", "1&b=2")
	second := labels.FromStrings("a", "1", "b", "2")
	if labelsMapKey(first) == labelsMapKey(second) {
		t.Fatal("distinct label sets have the same key")
	}
	response := binaryRows(t, []rowbinary.Any{rowbinary.String, ColumnLabels}, []any{"one", first}, []any{"two", second})
	q, _ := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if strings.HasPrefix(call.contentType, "multipart/") {
			return 200, binaryRows(t, []rowbinary.Any{rowbinary.UInt32, rowbinary.Int64, rowbinary.Float64},
				[]any{xxhash.ChecksumString32("one"), int64(1000), float64(1)},
				[]any{xxhash.ChecksumString32("two"), int64(1000), float64(2)})
		}
		return 200, response
	})
	set := q.Select(context.Background(), true, &storage.SelectHints{Start: 1000, End: 9000})
	if !set.Next() || !labels.Equal(set.At().Labels(), second) {
		t.Fatalf("wrong first series: %v", set.At())
	}
	if !set.Next() || !labels.Equal(set.At().Labels(), first) || set.Next() || set.Err() != nil {
		t.Fatalf("wrong second series: %v, %v", set.At(), set.Err())
	}
}

func TestLabelQueriesNilHintsAndMalformedResponse(t *testing.T) {
	oldNow := timeNow
	timeNow = func() time.Time { return time.UnixMilli(10_000_000) }
	t.Cleanup(func() { timeNow = oldNow })
	q, calls := fakeQuerier(t, func(call clickhouseCall) (int, []byte) {
		if bytes.Contains(call.body, []byte("mapKeys(labels)")) {
			return 200, binaryRows(t, []rowbinary.Any{rowbinary.String}, []any{"job"}, []any{"region"})
		}
		return 200, binaryRows(t, []rowbinary.Any{rowbinary.String}, []any{"api"}, []any{"web"})
	})
	match := labels.MustNewMatcher(labels.MatchEqual, "region", "west")
	names, _, err := q.LabelNames(context.Background(), nil, match)
	if err != nil || strings.Join(names, ",") != "job,region" {
		t.Fatalf("LabelNames = %v, %v", names, err)
	}
	requireQueryContains(t, <-calls, "mapKeys(labels)", "`timestamp_min` <= 10000000", "`timestamp_max` >= 2800000",
		"arrayElement(labels, 'region') = 'west'")
	values, _, err := q.LabelValues(context.Background(), "job", nil)
	if err != nil || strings.Join(values, ",") != "api,web" {
		t.Fatalf("LabelValues = %v, %v", values, err)
	}
	requireQueryContains(t, <-calls, "arrayElement(labels, 'job')", "GROUP BY value", "ORDER BY value")
	_, _, err = q.LabelValues(context.Background(), "__name__", nil)
	if err != nil {
		t.Fatal(err)
	}
	requireQueryContains(t, <-calls, "name", "AS value")

	bad, _ := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, []byte{5, 'a'} })
	if result, _, err := bad.LabelNames(context.Background(), nil); err == nil || result != nil {
		t.Fatalf("malformed label names = %v, %v", result, err)
	}
	if result, _, err := bad.LabelValues(context.Background(), "job", nil); err == nil || result != nil {
		t.Fatalf("malformed label values = %v, %v", result, err)
	}
}

func TestLabelQueriesHTTPAndDiscoveryErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Querier)
	}{
		{"http error", func(*Querier) {}},
		{"discovery error", func(q *Querier) {
			q.config.Extension.ClickHouseDiscovery = func(context.Context, string) (string, error) {
				return "", fmt.Errorf("discovery unavailable")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 500, []byte("clickhouse unavailable") })
			tc.setup(q)
			if got, _, err := q.LabelNames(context.Background(), &storage.LabelHints{Limit: 5}); err == nil || got != nil {
				t.Fatalf("LabelNames = %v, %v", got, err)
			}
			if got, _, err := q.LabelValues(context.Background(), "job", &storage.LabelHints{Limit: 5}); err == nil || got != nil {
				t.Fatalf("LabelValues = %v, %v", got, err)
			}
		})
	}
}

func TestSelectDiscoveryError(t *testing.T) {
	q, calls := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, nil })
	q.config.Extension.ClickHouseDiscovery = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("discovery unavailable")
	}
	set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1000, End: 9000})
	if set.Next() || set.Err() == nil || len(calls) != 0 {
		t.Fatalf("discovery error = %v; requests=%d", set.Err(), len(calls))
	}
}

func TestSelectSampleDiscoveryError(t *testing.T) {
	response := oneSeries(t, "id", labels.FromStrings("job", "api"))
	q, calls := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, response })
	var mu sync.Mutex
	requests := 0
	q.config.Extension.ClickHouseDiscovery = func(_ context.Context, dsn string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests == 2 {
			return "", fmt.Errorf("samples discovery unavailable")
		}
		return dsn, nil
	}
	set := q.Select(context.Background(), false, &storage.SelectHints{Start: 1000, End: 9000})
	if set.Next() || set.Err() == nil || len(calls) != 1 {
		t.Fatalf("sample discovery error = %v; requests=%d", set.Err(), len(calls))
	}
}

func TestSeriesMatcherRegexpAlternationAnchorsBothBranches(t *testing.T) {
	q, _ := fakeQuerier(t, func(clickhouseCall) (int, []byte) { return 200, nil })
	for _, kind := range []labels.MatchType{labels.MatchRegexp, labels.MatchNotRegexp} {
		matcher := labels.MustNewMatcher(kind, "job", "api|web")
		where := sql.NewWhere()
		q.whereMatchLabels(context.Background(), config.ConfigSeries{}, where, []*labels.Matcher{matcher})
		text := where.String()
		if !strings.Contains(text, "'^(?:api|web)$'") {
			t.Errorf("regexp matcher produced %q; expected anchored alternation", text)
		}
	}
}
