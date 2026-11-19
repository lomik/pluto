package prom

import (
	rb "github.com/lomik/rowbinary"
	"github.com/prometheus/prometheus/model/labels"
)

var ColumnLabels rb.Type[labels.Labels] = rb.MakeTypeWrapAny[labels.Labels](&typeColumnLabels{})

type typeColumnLabels struct {
}

// Scan implements rb.BaseType.
func (t *typeColumnLabels) Scan(r rb.Reader, dst *labels.Labels) error {
	n, err := rb.VarintRead(r)
	if err != nil {
		return err
	}

	// The count comes from an untrusted response. Allocating from it before
	// reading the entries can exhaust memory even for a tiny malformed body.
	ss := make([]string, 0)

	for i := uint64(0); i < n; i++ {
		var k string
		if err := rb.String.Scan(r, &k); err != nil {
			return err
		}
		ss = append(ss, k)

		var v string
		if err := rb.String.Scan(r, &v); err != nil {
			return err
		}
		ss = append(ss, v)
	}

	*dst = labels.FromStrings(ss...)
	return nil
}

// Binary implements rb.BaseType.
func (t *typeColumnLabels) Binary() []byte {
	return rb.Map(rb.String, rb.String).Binary()
}

// String implements rb.BaseType.
func (t *typeColumnLabels) String() string {
	return "Map(String, String)"
}

// Write implements rb.BaseType.
func (t *typeColumnLabels) Write(w rb.Writer, value labels.Labels) error {
	err := rb.VarintWrite(w, uint64(value.Len()))
	if err != nil {
		return err
	}

	value.Range(func(l labels.Label) {
		if err != nil {
			return
		}
		err = rb.String.Write(w, l.Name)
		if err != nil {
			return
		}
		err = rb.String.Write(w, l.Value)
	})

	return err
}
