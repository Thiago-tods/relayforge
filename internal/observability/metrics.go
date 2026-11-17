package observability

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Counter is a monotonically increasing counter. A nil *Counter ignores updates,
// so instrumented code works without a registry.
type Counter struct{ v atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() {
	if c != nil {
		c.v.Add(1)
	}
}

// Value returns the current count.
func (c *Counter) Value() uint64 {
	if c == nil {
		return 0
	}
	return c.v.Load()
}

// latencyBuckets are the histogram upper bounds in seconds.
var latencyBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Histogram records durations in fixed buckets. A nil *Histogram ignores updates.
type Histogram struct {
	counts   [12]atomic.Uint64 // len(latencyBuckets) + 1 (+Inf)
	sumNanos atomic.Uint64
	total    atomic.Uint64
}

// Observe records one duration.
func (h *Histogram) Observe(d time.Duration) {
	if h == nil {
		return
	}
	secs := d.Seconds()
	i := 0
	for i < len(latencyBuckets) && secs > latencyBuckets[i] {
		i++
	}
	h.counts[i].Add(1)
	h.sumNanos.Add(uint64(d.Nanoseconds()))
	h.total.Add(1)
}

// Count returns the number of observations.
func (h *Histogram) Count() uint64 {
	if h == nil {
		return 0
	}
	return h.total.Load()
}

type counterEntry struct {
	help string
	c    *Counter
}

type histogramEntry struct {
	help string
	h    *Histogram
}

// Registry holds metrics and renders them in the Prometheus text format. It uses
// no third-party dependency. Metric names are fixed identifiers; labels are not
// supported, so no payload, secret or personal data can end up in a metric.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]counterEntry
	histograms map[string]histogramEntry
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{counters: map[string]counterEntry{}, histograms: map[string]histogramEntry{}}
}

// Counter returns the counter with the given name, creating it on first use.
func (r *Registry) Counter(name, help string) *Counter {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.counters[name]; ok {
		return e.c
	}
	c := &Counter{}
	r.counters[name] = counterEntry{help: help, c: c}
	return c
}

// Histogram returns the histogram with the given name, creating it on first use.
func (r *Registry) Histogram(name, help string) *Histogram {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.histograms[name]; ok {
		return e.h
	}
	h := &Histogram{}
	r.histograms[name] = histogramEntry{help: help, h: h}
	return h
}

// Render renders all metrics sorted by name.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := r.counters[n]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", n, e.help, n, n, e.c.Value())
	}
	names = names[:0]
	for n := range r.histograms {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := r.histograms[n]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", n, e.help, n)
		var cumulative uint64
		for i, bound := range latencyBuckets {
			cumulative += e.h.counts[i].Load()
			fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", n, bound, cumulative)
		}
		cumulative += e.h.counts[len(latencyBuckets)].Load()
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n",
			n, cumulative, n, float64(e.h.sumNanos.Load())/1e9, n, e.h.total.Load())
	}
}

// Handler serves the metrics in the Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Render(w)
	})
}
