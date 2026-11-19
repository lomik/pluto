package insert

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/lomik/pluto/pkg/insert/id"
	"github.com/lomik/pluto/pkg/insert/labels"
	"github.com/lomik/rowbinary"
	"github.com/prometheus/prometheus/prompb"
)

type insertRow struct {
	id, name string
	labels   []labels.Bytes
	time     int64
	value    float64
}

func testWriteRequest(t *testing.T, series ...prompb.TimeSeries) []byte {
	t.Helper()
	raw, err := (&prompb.WriteRequest{Timeseries: series}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeInsertRows(t *testing.T, body []byte) []insertRow {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(body))
	count, err := rowbinary.VarintRead(reader)
	if err != nil || count != 5 {
		t.Fatalf("column count: %d, %v", count, err)
	}
	for _, name := range []string{"id", "name", "labels", "timestamp", "value"} {
		var got string
		err := rowbinary.String.Scan(reader, &got)
		if err != nil || got != name {
			t.Fatalf("column name: %q, %v; want %q", got, err, name)
		}
	}
	for _, typ := range []string{"String", "String", "Map(String, String)", "Int64", "Float64"} {
		var got string
		err := rowbinary.String.Scan(reader, &got)
		if err != nil || got != typ {
			t.Fatalf("column type: %q, %v; want %q", got, err, typ)
		}
	}

	r := rowbinary.NewFormatReader(reader, rowbinary.RowBinary,
		rowbinary.C("id", rowbinary.String), rowbinary.C("name", rowbinary.String),
		rowbinary.C("labels", labels.ColumnBytes),
		rowbinary.C("timestamp", rowbinary.Int64), rowbinary.C("value", rowbinary.Float64))
	var rows []insertRow
	for r.Next() {
		var row insertRow
		if err := rowbinary.Scan(r, rowbinary.String, &row.id); err != nil {
			t.Fatal(err)
		}
		if err := rowbinary.Scan(r, rowbinary.String, &row.name); err != nil {
			t.Fatal(err)
		}
		if err := rowbinary.Scan(r, labels.ColumnBytes, &row.labels); err != nil {
			t.Fatal(err)
		}
		if err := rowbinary.Scan(r, rowbinary.Int64, &row.time); err != nil {
			t.Fatal(err)
		}
		if err := rowbinary.Scan(r, rowbinary.Float64, &row.value); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPayloadToRowBinary(t *testing.T) {
	series := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "job", Value: "a&b=雪"}, {Name: "__name__", Value: "requests_total"}},
			Samples: []prompb.Sample{{Timestamp: -123, Value: 1.25}, {Timestamp: 1234567890, Value: -2.5}},
		},
		{Labels: []prompb.Label{{Name: "__name__", Value: "other"}}, Samples: []prompb.Sample{{Timestamp: 17, Value: 0}}},
		{Labels: []prompb.Label{{Name: "empty"}}, Samples: nil},
	}
	var buf bytes.Buffer
	if err := payloadToRowBinary(testWriteRequest(t, series...), &buf, id.NewNameWithSha256()); err != nil {
		t.Fatal(err)
	}

	first := []labels.Bytes{{Name: []byte("__name__"), Value: []byte("requests_total")}, {Name: []byte("job"), Value: []byte("a&b=雪")}}
	second := []labels.Bytes{{Name: []byte("__name__"), Value: []byte("other")}}
	h := id.NewNameWithSha256()
	h.Update(first)
	firstID := string(h.ID())
	h.Update(second)
	secondID := string(h.ID())
	want := []insertRow{
		{firstID, "requests_total", first, -123, 1.25},
		{firstID, "requests_total", first, 1234567890, -2.5},
		{secondID, "other", second, 17, 0},
	}
	if got := decodeInsertRows(t, buf.Bytes()); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows: got %#v; want %#v", got, want)
	}
}

func TestPayloadToRowBinaryEmptyMalformedAndPoolReuse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		wantErr bool
	}{
		{"empty bytes", nil, false},
		{"empty write request", testWriteRequest(t), false},
		{"series without labels", testWriteRequest(t, prompb.TimeSeries{Samples: []prompb.Sample{{Value: 1}}}), false},
		{"series without samples", testWriteRequest(t, prompb.TimeSeries{Labels: []prompb.Label{{Name: "__name__", Value: "x"}}}), false},
		{"truncated message", []byte{0x0a, 0x03, 0x0a}, true},
		{"malformed nested message", []byte{0x0a, 0x03, 0x0a, 0x01, 0x80}, true},
		{"invalid tag", []byte{0}, true},
		{"truncated varint", []byte{0x80}, true},
		{"overflowing varint", bytes.Repeat([]byte{0xff}, 11), true},
		{"truncated unknown field", []byte{0x52, 0x04, 0x01}, true},
		{"oversized unknown length", []byte{0x52, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, true},
		{"nested oversized unknown length", []byte{0x0a, 0x0b, 0x52, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, true},
		{"label oversized unknown length", []byte{0x0a, 0x0d, 0x0a, 0x0b, 0x52, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, true},
		{"opaque unknown field", []byte{0x52, 0x02, 0xff, 0xff}, false},
		{"oversized field number", []byte{0xf8, 0xff, 0xff, 0xff, 0x1f, 0x01}, true},
		{"wrong timeseries wire type", []byte{0x08, 0x01}, true},
		{"wrong sample value wire type", []byte{0x0a, 0x04, 0x12, 0x02, 0x08, 0x01}, true},
		{"wrong timestamp wire type", []byte{0x0a, 0x04, 0x12, 0x02, 0x12, 0x00}, true},
		{"truncated unknown group", []byte{0x53, 0x08, 0x01}, true},
		{"invalid wire type", []byte{0x0f}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := payloadToRowBinary(tc.payload, &buf, id.NewNameWithSha256())
			if (err != nil) != tc.wantErr {
				t.Fatalf("error: %v, wantErr: %v", err, tc.wantErr)
			}
			if tc.wantErr && (!errors.Is(err, errInvalidPayload) || buf.Len() != 0) {
				t.Fatalf("malformed payload must fail before writing: error=%v, written=%d", err, buf.Len())
			}
			if !tc.wantErr && len(decodeInsertRows(t, buf.Bytes())) != 0 {
				t.Fatal("expected no rows")
			}
		})
	}

	var buf bytes.Buffer
	good := testWriteRequest(t, prompb.TimeSeries{Labels: []prompb.Label{{Name: "__name__", Value: "fresh"}}, Samples: []prompb.Sample{{Timestamp: 44}}})
	if err := payloadToRowBinary(good, &buf, id.NewNameWithSha256()); err != nil {
		t.Fatal(err)
	}
	rows := decodeInsertRows(t, buf.Bytes())
	if len(rows) != 1 || rows[0].name != "fresh" || len(rows[0].labels) != 1 || rows[0].time != 44 {
		t.Fatalf("pooled timeseries retained old data: %#v", rows)
	}
}

func FuzzPayloadToRowBinary(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{0x0a, 0x04, 0x0a, 0x02, 0x08, 0x01})
	f.Add([]byte{0x52, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})
	valid, err := (&prompb.WriteRequest{Timeseries: []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "metric"}},
		Samples: []prompb.Sample{{Timestamp: 123, Value: 1}},
	}}}).Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Fuzz(func(t *testing.T, raw []byte) {
		// Any byte sequence must return normally, even if the payload is invalid.
		_ = payloadToRowBinary(raw, io.Discard, id.NewNoop())
	})
}

type rejectWriter struct{}

func (rejectWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPayloadToRowBinaryWriteError(t *testing.T) {
	err := payloadToRowBinary(nil, rejectWriter{}, id.NewNoop())
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("header write: got %v, want closed pipe", err)
	}

	var header bytes.Buffer
	if err := payloadToRowBinary(nil, &header, id.NewNoop()); err != nil {
		t.Fatal(err)
	}
	writer := &failAfterWriter{remaining: header.Len()}
	payload := testWriteRequest(t, prompb.TimeSeries{Labels: []prompb.Label{{Name: "__name__", Value: "m"}}, Samples: []prompb.Sample{{Value: 1}}})
	if err := payloadToRowBinary(payload, writer, id.NewNoop()); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("row write: got %v, want closed pipe", err)
	}
}

type failAfterWriter struct{ remaining int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, io.ErrClosedPipe
	}
	if len(p) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, io.ErrClosedPipe
	}
	w.remaining -= len(p)
	return len(p), nil
}
