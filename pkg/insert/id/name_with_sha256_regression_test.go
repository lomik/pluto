package id

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/lomik/pluto/pkg/insert/labels"
)

func TestNameWithSha256StableAcrossLabelOrderAndUpdates(t *testing.T) {
	one := []labels.Bytes{
		{Name: []byte("zone"), Value: []byte("west")},
		{Name: []byte("__name__"), Value: []byte("metric")},
		{Name: []byte("job"), Value: []byte("api")},
	}
	two := []labels.Bytes{
		{Name: []byte("job"), Value: []byte("api")},
		{Name: []byte("zone"), Value: []byte("west")},
		{Name: []byte("__name__"), Value: []byte("metric")},
	}
	h := NewNameWithSha256()
	h.Update(one)
	canonical := "__name__=metric&job=api&zone=west"
	sum := sha256.Sum256([]byte(canonical))
	want := "metric?" + hex.EncodeToString(sum[:])
	if string(h.ID()) != want || string(h.Name()) != "metric" {
		t.Fatalf("ID=%q name=%q, want ID=%q name=metric", h.ID(), h.Name(), want)
	}
	if string(one[0].Name) != "__name__" || string(one[1].Name) != "job" || string(one[2].Name) != "zone" {
		t.Fatalf("labels not sorted: %#v", one)
	}
	h.Update(two)
	if string(h.ID()) != want || string(h.Name()) != "metric" {
		t.Fatalf("shuffled labels changed ID or name: %q, %q", h.ID(), h.Name())
	}

	longName := strings.Repeat("m", 200)
	h.Update([]labels.Bytes{{Name: []byte("__name__"), Value: []byte(longName)}})
	if len(h.ID()) != len(longName)+65 || !bytes.Equal(h.Name(), []byte(longName)) {
		t.Fatalf("long metric name: ID=%q, name=%q", h.ID(), h.Name())
	}
	h.Update(two)
	if string(h.ID()) != want || string(h.Name()) != "metric" {
		t.Fatalf("reuse after long name changed ID or name: %q, %q", h.ID(), h.Name())
	}
	h.Update(nil)
	emptySum := sha256.Sum256(nil)
	if len(h.Name()) != 0 || string(h.ID()) != "?"+hex.EncodeToString(emptySum[:]) {
		t.Fatalf("empty labels did not clear the previous name: ID=%q name=%q", h.ID(), h.Name())
	}
}

// Known limitation: escaping or length framing would change IDs of persisted
// series. Preserve the existing algorithm until a migration is designed.
func TestNameWithSha256KnownDelimiterCollision(t *testing.T) {
	h := NewNameWithSha256()
	h.Update([]labels.Bytes{
		{Name: []byte("__name__"), Value: []byte("m")},
		{Name: []byte("a"), Value: []byte("b&c=d")},
	})
	first := string(h.ID())
	h.Update([]labels.Bytes{
		{Name: []byte("__name__"), Value: []byte("m")},
		{Name: []byte("a"), Value: []byte("b")},
		{Name: []byte("c"), Value: []byte("d")},
	})
	if first != string(h.ID()) {
		t.Fatalf("delimiter collision no longer reproduced: %q vs %q", first, h.ID())
	}
}

// Duplicate names are invalid Prometheus labels; their order is not stable
// with the existing name-only sort. Avoid altering previously persisted IDs.
func TestNameWithSha256KnownDuplicateLabelOrderDependence(t *testing.T) {
	h := NewNameWithSha256()
	h.Update([]labels.Bytes{{Name: []byte("x"), Value: []byte("two")}, {Name: []byte("x"), Value: []byte("one")}})
	first := string(h.ID())
	h.Update([]labels.Bytes{{Name: []byte("x"), Value: []byte("one")}, {Name: []byte("x"), Value: []byte("two")}})
	if first == string(h.ID()) {
		t.Fatalf("duplicate-label order dependence no longer reproduced: %q", first)
	}
}

func TestNoop(t *testing.T) {
	h := NewNoop()
	h.Update(nil)
	if string(h.ID()) != "noop" || string(h.Name()) != "noop" {
		t.Fatalf("ID=%q name=%q", h.ID(), h.Name())
	}
}
