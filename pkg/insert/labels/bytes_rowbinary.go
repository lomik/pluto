package labels

import (
	"errors"
	"unsafe"

	rb "github.com/pluto-metrics/rowbinary"
)

var ColumnBytes rb.Type[[]Bytes] = &typeColumnBytes{}

type typeColumnBytes struct {
}

func unsafeBytesToString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// Read implements rb.Type.
func (t *typeColumnBytes) Read(r rb.Reader) ([]Bytes, error) {
	n, err := rb.UVarint.Read(r)
	if err != nil {
		return nil, err
	}

	ret := make([]Bytes, 0)
	for i := uint64(0); i < n; i++ {
		k, err := readString(r)
		if err != nil {
			return nil, err
		}
		v, err := readString(r)
		if err != nil {
			return nil, err
		}
		ret = append(ret, Bytes{Name: []byte(k), Value: []byte(v)})
	}

	return ret, nil
}

// ReadAny implements rb.Type.
func (t *typeColumnBytes) ReadAny(r rb.Reader) (any, error) {
	return t.Read(r)
}

// String implements rb.Type.
func (t *typeColumnBytes) String() string {
	return "Map(String, String)"
}

// Write implements rb.Type.
func (t *typeColumnBytes) Write(w rb.Writer, value []Bytes) error {
	err := rb.UVarint.Write(w, uint64(len(value)))
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

// WriteAny implements rb.Type.
func (t *typeColumnBytes) WriteAny(w rb.Writer, v any) error {
	value, ok := v.([]Bytes)
	if !ok {
		return errors.New("unexpected type")
	}
	return t.Write(w, value)
}
