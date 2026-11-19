package labels

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	rb "github.com/lomik/rowbinary"
	"github.com/prometheus/prometheus/prompb"
)

func TestColumnsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []Bytes
	}{
		{"nil", nil},
		{"empty", []Bytes{}},
		{"empty key and value", []Bytes{{Name: []byte{}, Value: []byte{}}}},
		{"binary and unicode", []Bytes{{Name: []byte("__name__"), Value: []byte("雪\x00x")}, {Name: []byte("z=1"), Value: []byte("a&b")}}},
		{"multiple chunks", []Bytes{{Name: []byte(strings.Repeat("n", 8193)), Value: []byte(strings.Repeat("v", 16385))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := ColumnBytes.WriteAny(rb.NewWriter(&buf), tc.values); err != nil {
				t.Fatal(err)
			}
			wantProm := make([]prompb.Label, len(tc.values))
			for i, label := range tc.values {
				wantProm[i] = prompb.Label{Name: string(label.Name), Value: string(label.Value)}
			}
			var prom []prompb.Label
			err := ColumnPrompb.Scan(bufio.NewReader(bytes.NewReader(buf.Bytes())), &prom)
			if err != nil || !reflect.DeepEqual(prom, wantProm) {
				t.Fatalf("prompb read: %#v, %v; want %#v", prom, err, wantProm)
			}
			buf.Reset()
			if err := ColumnPrompb.WriteAny(rb.NewWriter(&buf), wantProm); err != nil {
				t.Fatal(err)
			}
			var got []Bytes
			err = ColumnBytes.Scan(bufio.NewReader(&buf), &got)
			wantBytes := tc.values
			if wantBytes == nil {
				wantBytes = []Bytes{}
			}
			if err != nil || !reflect.DeepEqual(got, wantBytes) {
				t.Fatalf("bytes read: %#v, %v; want %#v", got, err, wantBytes)
			}
		})
	}
}

func TestColumnReadMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"missing count", nil},
		{"overflowing count", bytes.Repeat([]byte{0xff}, 11)},
		{"oversized count", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{"oversized key length", []byte{1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{"truncated key", []byte{1, 2, 'a'}},
		{"overflowing key length", append([]byte{1}, bytes.Repeat([]byte{0xff}, 11)...)},
		{"oversized value length", []byte{1, 1, 'a', 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{"missing value", []byte{1, 1, 'a'}},
		{"truncated value", []byte{1, 1, 'a', 2, 'b'}},
		{"truncated large value", []byte{1, 1, 'a', 0x81, 0x20, 'b'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBytes []Bytes
			if err := ColumnBytes.Scan(bufio.NewReader(bytes.NewReader(tc.data)), &gotBytes); err == nil {
				t.Fatal("ColumnBytes.Scan accepted truncated data")
			}
			var gotProm []prompb.Label
			if err := ColumnPrompb.Scan(bufio.NewReader(bytes.NewReader(tc.data)), &gotProm); err == nil {
				t.Fatal("ColumnPrompb.Scan accepted truncated data")
			}
		})
	}
}

func TestColumnWriteErrors(t *testing.T) {
	if err := ColumnBytes.WriteAny(rb.NewWriter(io.Discard), "wrong type"); err == nil {
		t.Fatal("ColumnBytes.WriteAny accepted wrong type")
	}
	if err := ColumnPrompb.WriteAny(rb.NewWriter(io.Discard), "wrong type"); err == nil {
		t.Fatal("ColumnPrompb.WriteAny accepted wrong type")
	}
	for _, column := range []struct {
		name  string
		write func(rb.Writer) error
	}{
		{"bytes", func(w rb.Writer) error { return ColumnBytes.Write(w, []Bytes{{Name: []byte("a"), Value: []byte("b")}}) }},
		{"prompb", func(w rb.Writer) error { return ColumnPrompb.Write(w, []prompb.Label{{Name: "a", Value: "b"}}) }},
	} {
		t.Run(column.name, func(t *testing.T) {
			for _, remaining := range []int{0, 1, 3} {
				if err := column.write(rb.NewWriter(&failAfterWriter{remaining: remaining})); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("after %d bytes: error=%v, want closed pipe", remaining, err)
				}
			}
		})
	}
}

type failAfterWriter struct{ remaining int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, io.ErrClosedPipe
	}
	w.remaining -= len(p)
	return len(p), nil
}
