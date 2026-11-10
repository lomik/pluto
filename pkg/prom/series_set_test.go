package prom

import (
	"math"
	"os"
	"os/exec"
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

func TestSeriesIteratorSeekAndNext(t *testing.T) {
	s := &series{samples: []sample{{100, 1}, {200, 2}, {300, 3}}}
	for _, tc := range []struct {
		name   string
		seek   int64
		wantTs int64
	}{
		{"before first", 1, 100},
		{"at first", 100, 100},
		{"between", 150, 200},
		{"last", 300, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := s.Iterator(nil)
			if it.Seek(tc.seek) != chunkenc.ValFloat {
				t.Fatalf("Seek(%d) did not find a sample", tc.seek)
			}
			if ts, _ := it.At(); ts != tc.wantTs || it.AtT() != tc.wantTs {
				t.Errorf("Seek(%d) = %d, want %d", tc.seek, ts, tc.wantTs)
			}
		})
	}
	it := s.Iterator(nil)
	if it.Next() != chunkenc.ValFloat || it.Seek(50) != chunkenc.ValFloat || it.AtT() != 100 {
		t.Fatal("Seek must not move backward")
	}
	if it.Seek(200) != chunkenc.ValFloat || it.Next() != chunkenc.ValFloat || it.AtT() != 300 {
		t.Fatal("Next after Seek should advance to 300")
	}
	if it.Seek(1000) != chunkenc.ValNone || it.Next() != chunkenc.ValNone || it.Err() != nil {
		t.Fatalf("exhausted iterator has error %v", it.Err())
	}
	if it.Seek(100) != chunkenc.ValNone {
		t.Fatal("exhausted iterator must not move backward")
	}
	if empty := (&series{}).Iterator(nil); empty.Next() != chunkenc.ValNone || empty.Seek(0) != chunkenc.ValNone {
		t.Fatal("empty iterator returned sample")
	}
}

func TestFloatIteratorHistogramAccessPanicsWithoutExitingProcess(t *testing.T) {
	if os.Getenv("PLUTO_PROM_HISTOGRAM_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFloatIteratorHistogramAccessPanicsWithoutExitingProcess$")
		cmd.Env = append(os.Environ(), "PLUTO_PROM_HISTOGRAM_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("histogram access exited the process: %v\n%s", err, output)
		}
		return
	}

	it := (&series{samples: []sample{{100, 1}}}).Iterator(nil)
	if it.Next() != chunkenc.ValFloat {
		t.Fatal("no float sample")
	}
	for name, call := range map[string]func(){
		"histogram":       func() { it.AtHistogram(nil) },
		"float histogram": func() { it.AtFloatHistogram(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("histogram access on float sample must fail explicitly")
				}
			}()
			call()
		})
	}
}

func TestMakeSeriesSetSortAndRate(t *testing.T) {
	set, err := makeSeriesSet([]series{{labels: labels.FromStrings("job", "api"), samples: []sample{{300, 3}, {100, 1}}}},
		&storage.SelectHints{Func: "rate", Step: 100})
	if err != nil || !set.Next() {
		t.Fatalf("makeSeriesSet failed: %v", err)
	}
	it := set.At().Iterator(nil)
	for _, want := range []sample{{100, 1}, {300, 3}} {
		if it.Next() != chunkenc.ValFloat {
			t.Fatal("missing sample")
		}
		ts, value := it.At()
		if ts != want.timestamp || value != want.value {
			t.Errorf("sample = %d,%v, want %+v", ts, value, want)
		}
	}
	if it.Next() != chunkenc.ValFloat || it.AtT() != 400 {
		t.Fatal("missing rate staleness sample")
	}
	_, value := it.At()
	if !math.IsNaN(value) {
		t.Errorf("rate staleness value = %v", value)
	}
	if it.Next() != chunkenc.ValNone || set.Next() || set.Err() != nil {
		t.Fatalf("unexpected series set state: %v", set.Err())
	}
}

func TestHistogramFilteringDoesNotCombineOtherLabels(t *testing.T) {
	// Without separators, these two different label sets share a grouping key.
	data := []series{
		{labels: labels.FromStrings("a", "xb=y", "le", "1"), samples: []sample{{100, 1}}},
		{labels: labels.FromStrings("a", "x", "b", "y", "le", "2"), samples: []sample{{200, 2}}},
	}
	set, err := makeSeriesSet(data, &storage.SelectHints{})
	if err != nil {
		t.Fatal(err)
	}
	for set.Next() {
		if set.At().Iterator(nil).Next() != chunkenc.ValFloat {
			t.Fatal("unrelated histogram bucket incorrectly filtered")
		}
	}
	if set.Err() != nil {
		t.Fatal(set.Err())
	}
}

func TestSeriesSetErrorsAndBounds(t *testing.T) {
	set := emptySeriesSet()
	if set.At() != nil || set.Next() || set.Err() != nil {
		t.Fatal("empty set must be empty")
	}
	err := &testError{}
	set = errorSeriesSet(err)
	if set.At() != nil || set.Next() || set.Err() != err {
		t.Fatal("error set must retain its error")
	}
	set = newLabelsSeriesSet([]labels.Labels{labels.FromStrings("job", "api")})
	if set.At() != nil || !set.Next() || set.At().Labels().Get("job") != "api" || set.Next() {
		t.Fatal("labels-only set bounds incorrect")
	}
}

type testError struct{}

func (*testError) Error() string { return "test error" }
