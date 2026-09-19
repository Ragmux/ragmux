package metrics

import (
	"runtime/metrics"
	"time"
)

// processStart is when this process began, for ragmux_process_start_time_seconds.
var processStart = time.Now()

// RegisterRuntime adds the Go runtime gauges.
//
// It reads through runtime/metrics rather than runtime.ReadMemStats: the
// latter stops the world on every call, which would make a fifteen-second
// scrape interval a fifteen-second global pause schedule.
func (r *Registry) RegisterRuntime(version, goVersion string) {
	build := r.Gauge("ragmux_build_info",
		"Always 1; the version and Go version are carried in the labels.", "version", "go_version")
	build.With(version, goVersion).Set(1)

	r.GaugeFunc("ragmux_process_start_time_seconds",
		"Start time of the process since the Unix epoch.",
		func() float64 { return float64(processStart.Unix()) })

	// sample returns a gauge function reading one runtime/metrics series.
	//
	// The []metrics.Sample is built inside the returned function, per call,
	// and never captured by the closure. That looks wasteful — one slice and
	// one Sample per scrape — and is the whole point: metrics.Read writes the
	// sampled value back into the slice it is handed, so a slice hoisted into
	// the closure would be a single buffer shared by every caller.
	//
	// Nothing serialises the scrape path. Two Prometheus servers, or one
	// server whose previous scrape has not finished, run these gauge
	// functions concurrently; with a shared slice both calls write s[0].Value
	// while both read it, which is a data race and, in practice, a gauge
	// reporting the other series' number.
	//
	// The race detector cannot see this one. metrics.Read reaches the runtime
	// through //go:linkname into runtime_readMetrics, and the runtime's own
	// code is not instrumented by -race, so the conflicting write is invisible
	// to it: a test that hammers two scrapes in parallel passes either way.
	// That is why this is a local allocation and a comment rather than a
	// regression test — the correctness argument here is static, not
	// observable.
	sample := func(name string) func() float64 {
		return func() float64 {
			s := []metrics.Sample{{Name: name}}
			metrics.Read(s)
			switch s[0].Value.Kind() {
			case metrics.KindUint64:
				return float64(s[0].Value.Uint64())
			case metrics.KindFloat64:
				return s[0].Value.Float64()
			default:
				return 0
			}
		}
	}

	r.GaugeFunc("ragmux_go_goroutines", "Goroutines that currently exist.",
		sample("/sched/goroutines:goroutines"))
	r.GaugeFunc("ragmux_go_memstats_heap_inuse_bytes", "Heap memory in use.",
		sample("/memory/classes/heap/objects:bytes"))
	r.GaugeFunc("ragmux_go_memstats_alloc_bytes_total", "Bytes allocated by the heap since start.",
		sample("/gc/heap/allocs:bytes"))
	r.GaugeFunc("ragmux_go_gc_cycles_total", "Completed garbage collection cycles.",
		sample("/gc/cycles/total:gc-cycles"))
}
