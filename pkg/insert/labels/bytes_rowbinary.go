package labels

import (
	"unsafe"

	rb "github.com/lomik/rowbinary"
)

var ColumnBytes rb.Type[[]Bytes] = rb.MakeTypeWrapAny[[]Bytes](&typeColumnBytes{})

type typeColumnBytes struct {
}

func unsafeBytesToString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// Scan implements rb.BaseType.
func (t *typeColumnBytes) Scan(r rb.Reader, dst *[]Bytes) error {
	n, err := rb.VarintRead(r)
	if err != nil {
		return err
	}

	ret := make([]Bytes, 0)
	for i := uint64(0); i < n; i++ {
		k, err := readString(r)
		if err != nil {
			return err
		}
		v, err := readString(r)
		if err != nil {
			return err
		}
		ret = append(ret, Bytes{Name: []byte(k), Value: []byte(v)})
	}

	*dst = ret
	return nil
}

// Binary implements rb.BaseType.
func (t *typeColumnBytes) Binary() []byte {
	return rb.Map(rb.String, rb.String).Binary()
}

// String implements rb.BaseType.
func (t *typeColumnBytes) String() string {
	return "Map(String, String)"
}

// Write implements rb.BaseType.
func (t *typeColumnBytes) Write(w rb.Writer, value []Bytes) error {
	err := rb.VarintWrite(w, uint64(len(value)))
	if err != nil {
		return err
	}
	for i := 0; i < len(value); i++ {
		err = rb.String.Write(w, unsafeBytesToString(value[i].Name))
		if err != nil {
			return err
		}

		err = rb.String.Write(w, unsafeBytesToString(value[i].Value))
		if err != nil {
			return err
		}
	}

	return nil
}
