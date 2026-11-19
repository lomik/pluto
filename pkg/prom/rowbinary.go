package prom

import "github.com/lomik/rowbinary"

// rbRead scans the next value of type tp from r.
func rbRead[V any](r *rowbinary.FormatReader, tp rowbinary.Type[V]) (V, error) {
	var v V
	err := rowbinary.Scan(r, tp, &v)
	return v, err
}
