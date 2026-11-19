package labels

import (
	"encoding/binary"
	"io"

	rb "github.com/lomik/rowbinary"
)

// readString grows its buffer only as data arrives, never from the untrusted
// declared length, so a corrupted RowBinary length cannot exhaust memory.
func readString(r rb.Reader) (string, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	if n <= 4096 {
		buf := make([]byte, int(n))
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	}
	var buf []byte
	var chunk [4096]byte
	for n > 0 {
		count := uint64(len(chunk))
		if n < count {
			count = n
		}
		if _, err := io.ReadFull(r, chunk[:int(count)]); err != nil {
			return "", err
		}
		buf = append(buf, chunk[:int(count)]...)
		n -= count
	}
	return string(buf), nil
}
