// Package metrics is a small, dependency-free Prometheus text-exposition
// registry. It implements exactly what the gateway exports: counters, gauges
// and histograms with a fixed label set per metric, plus gauges read at
// scrape time. It is deliberately not a general-purpose client library:
// there are no summaries, no exemplars, no push gateway and no collectors
// beyond these.
//
// Every series is atomics-only on the hot path. The registry's mutex guards
// only the map of label combinations, and only the first touch of a new
// combination takes the write lock.
//
// Allocation note for callers: With(vals...) joins the label values into a
// map key, which costs one allocation per call. Any call site whose label set
// is constant for the life of a request must resolve its series once and
// reuse it rather than calling With in a loop — a per-chunk With in a
// streaming handler is the mistake this note exists to prevent.
package metrics

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// namePattern is Prometheus' metric and label name grammar.
var namePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// sep joins label values into a map key. It is a unit separator: no label
// value may contain it, so distinct label tuples cannot collide.
const sep = "\x1f"

// defaultMaxSeries caps the label combinations one registry will hold. It is
// deliberately low: the metric set is designed so that cardinality grows with
// the size of the installation (projects, models, stores) and not with
// traffic, and an install that legitimately exceeds it should raise the cap
// deliberately rather than discover unbounded memory growth in production.
const defaultMaxSeries = 5000

// Options configures a registry.
type Options struct {
	// MaxSeries caps the total number of label combinations across every
	// metric. Beyond it new combinations are dropped, counted in
	// ragmux_metrics_series_dropped_total and reported through
	// OnSeriesDropped, so a pathological label value cannot grow the process
	// without bound and cannot do it quietly. Zero selects the default.
	MaxSeries int
}

// collector renders one metric family.
type collector interface {
	appendTo(b []byte) []byte
}

// Registry holds every metric the process exports. Registration order is
// output order, so writing needs no sort.
type Registry struct {
	mu        sync.RWMutex
	cols      []collector
	names     map[string]struct{}
	maxSeries int

	series  atomic.Int64
	dropped atomic.Uint64
	// lastNotify holds the Unix nanosecond stamp of the most recent
	// OnSeriesDropped call, which is how the report is rate limited.
	lastNotify atomic.Int64
	// OnSeriesDropped, when set, is called when a new label combination is
	// refused, at most once per notifyInterval, with the metric that was
	// refused and the running total of refusals. It exists so the owner can
	// log it; the registry itself does not log.
	OnSeriesDropped func(metric string, dropped uint64)
}

// New creates an empty registry.
func New(opts Options) *Registry {
	if opts.MaxSeries <= 0 {
		opts.MaxSeries = defaultMaxSeries
	}
	return &Registry{names: map[string]struct{}{}, maxSeries: opts.MaxSeries}
}

// register records a family under its name. A duplicate name or a malformed
// name panics: both are programming errors that a startup test catches, and
// neither has a sensible runtime behaviour.
func (r *Registry) register(name string, labels []string, c collector) {
	if !namePattern.MatchString(name) {
		panic(fmt.Sprintf("metrics: invalid metric name %q", name))
	}
	for _, l := range labels {
		if !namePattern.MatchString(l) {
			panic(fmt.Sprintf("metrics: invalid label name %q on metric %q", l, name))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.names[name]; dup {
		panic(fmt.Sprintf("metrics: metric %q registered twice", name))
	}
	r.names[name] = struct{}{}
	r.cols = append(r.cols, c)
}

// notifyInterval is the shortest gap between two OnSeriesDropped calls. It
// keeps a hot loop of refusals from becoming the log itself while still
// letting a standing problem re-announce itself.
const notifyInterval = time.Minute

// admitSeries reserves room for one new label combination. It reports false
// once the registry is at its cap, which is how a runaway label value degrades
// into a counted drop instead of unbounded growth.
//
// Refusing is deliberately all this does: there is no eviction. Evicting the
// least recently used series would keep the cap from freezing legitimate
// metrics, but it would cost a write on every observation -- a lock or an
// atomic store on the scrape path -- and a counter that is evicted and then
// comes back starts from zero, which Prometheus reads as a counter reset and
// silently turns into a wrong rate(). Visibly missing data is preferred to
// quietly wrong data, so the drop is reported instead: counted in
// ragmux_metrics_series_dropped_total and handed to OnSeriesDropped, which the
// owner logs at error level.
func (r *Registry) admitSeries(metric string) bool {
	if r.series.Add(1) > int64(r.maxSeries) {
		r.series.Add(-1)
		r.notifyDropped(metric, r.dropped.Add(1))
		return false
	}
	return true
}

// notifyDropped reports a refused series to the owner, at most once per
// notifyInterval. Reporting only the very first refusal would leave a process
// that has been losing series for a week with one log line from the day it
// started; the counter carries the total, the log carries the alarm.
func (r *Registry) notifyDropped(metric string, dropped uint64) {
	if r.OnSeriesDropped == nil {
		return
	}
	now := time.Now().UnixNano()
	last := r.lastNotify.Load()
	if last != 0 && now-last < int64(notifyInterval) {
		return
	}
	// Losing the swap means another goroutine is reporting this window.
	if !r.lastNotify.CompareAndSwap(last, now) {
		return
	}
	r.OnSeriesDropped(metric, dropped)
}

// Dropped reports how many label combinations have been refused.
func (r *Registry) Dropped() uint64 { return r.dropped.Load() }

// ---- counter ----

// Counter is a monotonically increasing integer series.
type Counter struct{ v atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n.
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Value reports the current count.
func (c *Counter) Value() uint64 { return c.v.Load() }

// FloatCounter is a monotonically increasing float series. It exists for
// money: a token count is an integer but a cost is not.
type FloatCounter struct{ bits atomic.Uint64 }

// Add adds v. Negative values are ignored: a counter that goes backwards
// breaks every rate() over it, so a sign error is dropped rather than stored.
func (c *FloatCounter) Add(v float64) {
	if v <= 0 || math.IsNaN(v) {
		return
	}
	for {
		old := c.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if c.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Value reports the current total.
func (c *FloatCounter) Value() float64 { return math.Float64frombits(c.bits.Load()) }

// Gauge is a value that goes up and down.
type Gauge struct{ bits atomic.Uint64 }

// Set replaces the value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add adds v, which may be negative.
func (g *Gauge) Add(v float64) {
	for {
		old := g.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if g.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Inc adds one.
func (g *Gauge) Inc() { g.Add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.Add(-1) }

// Value reports the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

// Histogram counts observations into cumulative buckets.
type Histogram struct {
	bounds []float64 // upper bounds, ascending, without +Inf
	counts []atomic.Uint64
	sum    atomic.Uint64 // float64 bits
	total  atomic.Uint64
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) {
		return
	}
	i := sort.SearchFloat64s(h.bounds, v)
	// SearchFloat64s returns the first index whose bound is >= v, which is the
	// bucket v belongs in because Prometheus buckets are inclusive upper
	// bounds. len(bounds) is the implicit +Inf bucket.
	h.counts[i].Add(1)
	h.total.Add(1)
	for {
		old := h.sum.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sum.CompareAndSwap(old, next) {
			return
		}
	}
}

// ---- vectors ----

// vec is the shared machinery of every labelled family.
type vec[T any] struct {
	reg    *Registry
	name   string
	help   string
	typ    string
	labels []string
	mk     func() *T

	mu     sync.RWMutex
	series map[string]*T
	order  []string
	// overflow is handed out once the registry is at its series cap, so a
	// caller never has to nil-check and a dropped series silently discards
	// its observations.
	overflow *T
}

func newVec[T any](r *Registry, name, help, typ string, labels []string, mk func() *T) *vec[T] {
	v := &vec[T]{reg: r, name: name, help: help, typ: typ, labels: labels, mk: mk,
		series: map[string]*T{}, overflow: mk()}
	r.register(name, labels, v)
	return v
}

func (v *vec[T]) with(vals []string) *T {
	if len(vals) != len(v.labels) {
		panic(fmt.Sprintf("metrics: metric %q takes %d label values, got %d",
			v.name, len(v.labels), len(vals)))
	}
	key := strings.Join(vals, sep)
	v.mu.RLock()
	s, ok := v.series[key]
	v.mu.RUnlock()
	if ok {
		return s
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.series[key]; ok {
		return s
	}
	if !v.reg.admitSeries(v.name) {
		return v.overflow
	}
	s = v.mk()
	v.series[key] = s
	v.order = append(v.order, key)
	return s
}

// snapshot returns the series in insertion order so successive scrapes keep a
// stable ordering.
func (v *vec[T]) snapshot() ([]string, []*T) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	keys := make([]string, len(v.order))
	copy(keys, v.order)
	out := make([]*T, len(keys))
	for i, k := range keys {
		out[i] = v.series[k]
	}
	return keys, out
}

// CounterVec is a counter family.
type CounterVec struct{ v *vec[Counter] }

// With resolves the series for these label values, in the order the labels
// were declared. A wrong count panics: it is a programming error.
func (c *CounterVec) With(vals ...string) *Counter { return c.v.with(vals) }

// FloatCounterVec is a float counter family.
type FloatCounterVec struct{ v *vec[FloatCounter] }

// With resolves the series for these label values.
func (c *FloatCounterVec) With(vals ...string) *FloatCounter { return c.v.with(vals) }

// GaugeVec is a gauge family.
type GaugeVec struct{ v *vec[Gauge] }

// With resolves the series for these label values.
func (g *GaugeVec) With(vals ...string) *Gauge { return g.v.with(vals) }

// HistogramVec is a histogram family.
type HistogramVec struct{ v *vec[Histogram] }

// With resolves the series for these label values.
func (h *HistogramVec) With(vals ...string) *Histogram { return h.v.with(vals) }

// Counter registers an integer counter family.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	return &CounterVec{newVec(r, name, help, "counter", labels, func() *Counter { return &Counter{} })}
}

// FloatCounter registers a float counter family.
func (r *Registry) FloatCounter(name, help string, labels ...string) *FloatCounterVec {
	return &FloatCounterVec{newVec(r, name, help, "counter", labels, func() *FloatCounter { return &FloatCounter{} })}
}

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{newVec(r, name, help, "gauge", labels, func() *Gauge { return &Gauge{} })}
}

// Histogram registers a histogram family. bounds are inclusive upper bounds
// and must be sorted ascending without a +Inf entry, which is implicit.
func (r *Registry) Histogram(name, help string, bounds []float64, labels ...string) *HistogramVec {
	if len(bounds) == 0 {
		panic(fmt.Sprintf("metrics: histogram %q needs at least one bucket bound", name))
	}
	if !sort.Float64sAreSorted(bounds) {
		panic(fmt.Sprintf("metrics: histogram %q bounds are not sorted ascending", name))
	}
	for _, b := range bounds {
		if math.IsInf(b, 1) {
			panic(fmt.Sprintf("metrics: histogram %q must not declare +Inf; it is implicit", name))
		}
	}
	fixed := make([]float64, len(bounds))
	copy(fixed, bounds)
	return &HistogramVec{newVec(r, name, help, "histogram", labels, func() *Histogram {
		return &Histogram{bounds: fixed, counts: make([]atomic.Uint64, len(fixed)+1)}
	})}
}

// ---- scrape-time gauges ----

// funcGauge is a single gauge evaluated while the response is written.
type funcGauge struct {
	name, help string
	f          func() float64
}

// GaugeFunc registers a gauge evaluated at scrape time. f must be cheap and
// must not block or query the database: it runs inline while the scrape
// response is built.
func (r *Registry) GaugeFunc(name, help string, f func() float64) {
	g := &funcGauge{name: name, help: help, f: f}
	r.register(name, nil, g)
}

// funcCounter is a counter read at scrape time. It exists for totals owned by
// another package, which hands out a value rather than a series to increment.
type funcCounter struct {
	name, help string
	f          func() uint64
}

// CounterFunc registers a counter evaluated at scrape time. f must be cheap,
// and must be monotonic: exporting a value that can fall as a counter breaks
// every rate() over it.
func (r *Registry) CounterFunc(name, help string, f func() uint64) {
	r.register(name, nil, &funcCounter{name: name, help: help, f: f})
}

// cachedGaugeVec is a labelled gauge whose values cost a query, refreshed at
// most once per TTL however often the endpoint is scraped. Without this a
// scrape storm becomes a query storm.
type cachedGaugeVec struct {
	name, help string
	label      string
	ttl        time.Duration
	f          func() (map[string]float64, error)

	mu       sync.Mutex
	vals     map[string]float64
	loadedAt time.Time
	lastErr  error
}

// CachedGaugeFunc registers a single-label gauge family whose values come
// from f, refreshed at most once per ttl. f runs inline on the scrape that
// finds the cache stale, so it must have its own timeout.
func (r *Registry) CachedGaugeFunc(name, help, label string, ttl time.Duration, f func() (map[string]float64, error)) {
	if !namePattern.MatchString(label) {
		panic(fmt.Sprintf("metrics: invalid label name %q on metric %q", label, name))
	}
	c := &cachedGaugeVec{name: name, help: help, label: label, ttl: ttl, f: f}
	r.register(name, []string{label}, c)
}

func (c *cachedGaugeVec) load() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vals != nil && time.Since(c.loadedAt) < c.ttl {
		return c.vals
	}
	vals, err := c.f()
	c.loadedAt = time.Now()
	if err != nil {
		// Keep serving the previous values: a scrape that briefly cannot
		// reach the database should read stale rather than report zero, which
		// an alert would read as "the backlog drained".
		c.lastErr = err
		return c.vals
	}
	c.lastErr = nil
	c.vals = vals
	return vals
}
