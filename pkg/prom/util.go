package prom

import "github.com/prometheus/prometheus/model/labels"

func labelsMapKey(lb labels.Labels) string {
	return string(lb.Bytes(nil))
}
