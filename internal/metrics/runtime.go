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

	sample := func(name string) func() float64 {
		s := []metrics.Sample{{Name: name}}
		return func() float64 {
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
