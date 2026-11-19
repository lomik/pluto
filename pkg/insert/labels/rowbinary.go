package labels

import (
	rb "github.com/lomik/rowbinary"
	"github.com/prometheus/prometheus/prompb"
)

var ColumnPrompb rb.Type[[]prompb.Label] = rb.MakeTypeWrapAny[[]prompb.Label](&typeColumnPrompb{})

type typeColumnPrompb struct {
}

// Scan implements rb.BaseType.
func (t *typeColumnPrompb) Scan(r rb.Reader, dst *[]prompb.Label) error {
	n, err := rb.VarintRead(r)
	if err != nil {
		return err
	}

	ret := make([]prompb.Label, 0)
	for i := uint64(0); i < n; i++ {
		k, err := readString(r)
		if err != nil {
			return err
		}
		v, err := readString(r)
		if err != nil {
			return err
		}
		ret = append(ret, prompb.Label{Name: k, Value: v})
	}

	*dst = ret
	return nil
}

// Binary implements rb.BaseType.
func (t *typeColumnPrompb) Binary() []byte {
	return rb.Map(rb.String, rb.String).Binary()
}

// String implements rb.BaseType.
func (t *typeColumnPrompb) String() string {
	return "Map(String, String)"
}

// Write implements rb.BaseType.
func (t *typeColumnPrompb) Write(w rb.Writer, value []prompb.Label) error {
	err := rb.VarintWrite(w, uint64(len(value)))
	if err != nil {
		return err
	}
	for i := 0; i < len(value); i++ {
		err = rb.String.Write(w, value[i].Name)
		if err != nil {
			return err
		}

		err = rb.String.Write(w, value[i].Value)
		if err != nil {
			return err
		}
	}

	return nil
}
