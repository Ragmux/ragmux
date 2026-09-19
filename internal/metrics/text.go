package metrics

import (
	"bytes"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// WriteTo renders every registered family in Prometheus' text exposition
// format, version 0.0.4.
//
// The whole response is built into a buffer and written once: a slow scraper
// must not hold a read lock on any series map while the kernel drains its
// socket.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.RLock()
	cols := make([]collector, len(r.cols))
	copy(cols, r.cols)
	r.mu.RUnlock()

	var b []byte
	for _, c := range cols {
		b = c.appendTo(b)
	}
	b = appendHeader(b, "ragmux_metrics_series_dropped_total",
		"Label combinations refused because the registry reached its series cap.", "counter")
	b = append(b, "ragmux_metrics_series_dropped_total "...)
	b = strconv.AppendUint(b, r.dropped.Load(), 10)
	b = append(b, '\n')

	n, err := w.Write(b)
	return int64(n), err
}

// Text renders the exposition into a string. It exists for tests.
func (r *Registry) Text() string {
	var buf bytes.Buffer
	_, _ = r.WriteTo(&buf)
	return buf.String()
}

func appendHeader(b []byte, name, help, typ string) []byte {
	b = append(b, "# HELP "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = appendEscapedHelp(b, help)
	b = append(b, '\n', '#', ' ', 'T', 'Y', 'P', 'E', ' ')
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, typ...)
	b = append(b, '\n')
	return b
}

// appendEscapedHelp escapes the two characters that would break a HELP line.
// A quote is legal in HELP and is left alone.
func appendEscapedHelp(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, c)
		}
	}
	return b
}

// appendLabels renders {a="1",b="2"}, or nothing when there are no labels.
// extra is appended as a final label, which is how histograms add le.
func appendLabels(b []byte, names []string, key string, extraName, extraVal string) []byte {
	if len(names) == 0 && extraName == "" {
		return b
	}
	b = append(b, '{')
	first := true
	if len(names) > 0 {
		vals := strings.Split(key, sep)
		for i, n := range names {
			if i >= len(vals) {
				break
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = append(b, n...)
			b = append(b, '=', '"')
			b = appendEscapedValue(b, vals[i])
			b = append(b, '"')
		}
	}
	if extraName != "" {
		if !first {
			b = append(b, ',')
		}
		b = append(b, extraName...)
		b = append(b, '=', '"')
		b = appendEscapedValue(b, extraVal)
		b = append(b, '"')
	}
	b = append(b, '}')
	return b
}

// appendEscapedValue escapes a label value per the exposition format.
func appendEscapedValue(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b = append(b, '\\', '\\')
		case '"':
			b = append(b, '\\', '"')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, c)
		}
	}
	return b
}

// appendFloat writes a value the way Prometheus expects: shortest round-trip
// decimal, with the infinities spelled out.
func appendFloat(b []byte, v float64) []byte {
	switch {
	case math.IsInf(v, 1):
		return append(b, "+Inf"...)
	case math.IsInf(v, -1):
		return append(b, "-Inf"...)
	case math.IsNaN(v):
		return append(b, "NaN"...)
	}
	return strconv.AppendFloat(b, v, 'g', -1, 64)
}

func (v *vec[T]) appendTo(b []byte) []byte {
	keys, series := v.snapshot()
	b = appendHeader(b, v.name, v.help, v.typ)
	for i, key := range keys {
		switch s := any(series[i]).(type) {
		case *Counter:
			b = append(b, v.name...)
			b = appendLabels(b, v.labels, key, "", "")
			b = append(b, ' ')
			b = strconv.AppendUint(b, s.Value(), 10)
			b = append(b, '\n')
		case *FloatCounter:
			b = append(b, v.name...)
			b = appendLabels(b, v.labels, key, "", "")
			b = append(b, ' ')
			b = appendFloat(b, s.Value())
			b = append(b, '\n')
		case *Gauge:
			b = append(b, v.name...)
			b = appendLabels(b, v.labels, key, "", "")
			b = append(b, ' ')
			b = appendFloat(b, s.Value())
			b = append(b, '\n')
		case *Histogram:
			b = appendHistogram(b, v.name, v.labels, key, s)
		}
	}
	return b
}

// appendHistogram renders the cumulative buckets, then _sum and _count. The
// counts are read once per bucket and accumulated here, so a concurrent
// Observe can land between two buckets; the result stays monotonic because
// buckets are summed in order.
func appendHistogram(b []byte, name string, labels []string, key string, h *Histogram) []byte {
	var cum uint64
	for i, bound := range h.bounds {
		cum += h.counts[i].Load()
		b = append(b, name...)
		b = append(b, "_bucket"...)
		b = appendLabels(b, labels, key, "le", string(appendFloat(nil, bound)))
		b = append(b, ' ')
		b = strconv.AppendUint(b, cum, 10)
		b = append(b, '\n')
	}
	cum += h.counts[len(h.bounds)].Load()
	b = append(b, name...)
	b = append(b, "_bucket"...)
	b = appendLabels(b, labels, key, "le", "+Inf")
	b = append(b, ' ')
	b = strconv.AppendUint(b, cum, 10)
	b = append(b, '\n')

	b = append(b, name...)
	b = append(b, "_sum"...)
	b = appendLabels(b, labels, key, "", "")
	b = append(b, ' ')
	b = appendFloat(b, math.Float64frombits(h.sum.Load()))
	b = append(b, '\n')

	b = append(b, name...)
	b = append(b, "_count"...)
	b = appendLabels(b, labels, key, "", "")
	b = append(b, ' ')
	b = strconv.AppendUint(b, cum, 10)
	b = append(b, '\n')
	return b
}

func (g *funcGauge) appendTo(b []byte) []byte {
	b = appendHeader(b, g.name, g.help, "gauge")
	b = append(b, g.name...)
	b = append(b, ' ')
	b = appendFloat(b, g.f())
	b = append(b, '\n')
	return b
}

func (c *cachedGaugeVec) appendTo(b []byte) []byte {
	vals := c.load()
	b = appendHeader(b, c.name, c.help, "gauge")
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	// Map order is random; sort so successive scrapes read the same.
	sort.Strings(keys)
	for _, k := range keys {
		b = append(b, c.name...)
		b = appendLabels(b, []string{c.label}, k, "", "")
		b = append(b, ' ')
		b = appendFloat(b, vals[k])
		b = append(b, '\n')
	}
	return b
}
