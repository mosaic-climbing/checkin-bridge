package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Registry holds all application metrics.
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
	startTime  time.Time
}

// New creates a new metrics registry.
func New() *Registry {
	return &Registry{
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
		startTime:  time.Now(),
	}
}

// Counter is a monotonically increasing value.
type Counter struct {
	value atomic.Int64
}

// Inc increments the counter by 1.
func (c *Counter) Inc() { c.value.Add(1) }

// Add increments the counter by n.
func (c *Counter) Add(n int64) { c.value.Add(n) }

// Value returns the current counter value.
func (c *Counter) Value() int64 { return c.value.Load() }

// Gauge is a value that can go up and down.
type Gauge struct {
	value atomic.Int64 // stored as float64 bits
}

// Set sets the gauge to a float64 value.
func (g *Gauge) Set(v float64) { g.value.Store(int64(math.Float64bits(v))) }

// SetInt sets the gauge to an int64 value (converted to float64).
func (g *Gauge) SetInt(v int64) { g.value.Store(int64(math.Float64bits(float64(v)))) }

// Inc increments the gauge by 1 (as float64).
func (g *Gauge) Inc() {
	for {
		old := g.value.Load()
		oldF := math.Float64frombits(uint64(old))
		newF := oldF + 1
		if g.value.CompareAndSwap(old, int64(math.Float64bits(newF))) {
			break
		}
	}
}

// Dec decrements the gauge by 1 (as float64).
func (g *Gauge) Dec() {
	for {
		old := g.value.Load()
		oldF := math.Float64frombits(uint64(old))
		newF := oldF - 1
		if g.value.CompareAndSwap(old, int64(math.Float64bits(newF))) {
			break
		}
	}
}

// Value returns the current gauge value.
func (g *Gauge) Value() float64 { return math.Float64frombits(uint64(g.value.Load())) }

// Histogram tracks value distributions using pre-defined buckets.
// All fields are accessed via atomics; no mutex needed.
type Histogram struct {
	buckets []float64      // upper bounds
	counts  []atomic.Int64 // one per bucket + 1 for +Inf
	sum     atomic.Int64   // sum as float64 bits
	count   atomic.Int64
}

// newHistogram creates a new histogram with specified bucket boundaries.
func newHistogram(buckets []float64) *Histogram {
	sortedBuckets := make([]float64, len(buckets))
	copy(sortedBuckets, buckets)
	sort.Float64s(sortedBuckets)

	h := &Histogram{
		buckets: sortedBuckets,
		counts:  make([]atomic.Int64, len(sortedBuckets)+1), // +1 for +Inf
	}
	return h
}

// Observe records an observation in the histogram.
func (h *Histogram) Observe(v float64) {
	h.count.Add(1)

	// Atomic add to sum using CAS loop
	for {
		old := h.sum.Load()
		oldF := math.Float64frombits(uint64(old))
		newF := oldF + v
		if h.sum.CompareAndSwap(old, int64(math.Float64bits(newF))) {
			break
		}
	}

	// Increment all buckets >= v
	for i, bound := range h.buckets {
		if v <= bound {
			h.counts[i].Add(1)
		}
	}

	// +Inf bucket is always incremented
	h.counts[len(h.buckets)].Add(1)
}

// Count returns the total number of observations.
func (h *Histogram) Count() int64 { return h.count.Load() }

// Sum returns the sum of all observations.
func (h *Histogram) Sum() float64 {
	return math.Float64frombits(uint64(h.sum.Load()))
}

// Registry methods

// Counter returns or creates a counter with the given name.
func (r *Registry) Counter(name string) *Counter {
	r.mu.RLock()
	if c, ok := r.counters[name]; ok {
		r.mu.RUnlock()
		return c
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if c, ok := r.counters[name]; ok {
		return c
	}

	c := &Counter{}
	r.counters[name] = c
	return c
}

// Gauge returns or creates a gauge with the given name.
func (r *Registry) Gauge(name string) *Gauge {
	r.mu.RLock()
	if g, ok := r.gauges[name]; ok {
		r.mu.RUnlock()
		return g
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if g, ok := r.gauges[name]; ok {
		return g
	}

	g := &Gauge{}
	r.gauges[name] = g
	return g
}

// Histogram returns or creates a histogram with the given name and buckets.
func (r *Registry) Histogram(name string, buckets []float64) *Histogram {
	r.mu.RLock()
	if h, ok := r.histograms[name]; ok {
		r.mu.RUnlock()
		return h
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if h, ok := r.histograms[name]; ok {
		return h
	}

	h := newHistogram(buckets)
	r.histograms[name] = h
	return h
}

// DefaultLatencyBuckets are standard buckets for latency histograms (in seconds).
// Values: 1ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 5s, 10s, 30s
var DefaultLatencyBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 10, 30}

// PrometheusText returns all metrics in Prometheus text exposition format.
func (r *Registry) PrometheusText() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var sb strings.Builder

	// Sort names for deterministic output
	counterNames := sortedKeys(r.counters)
	gaugeNames := sortedKeys(r.gauges)
	histNames := sortedKeys(r.histograms)

	// Counters
	for _, name := range counterNames {
		c := r.counters[name]
		baseName := prometheusBaseName(name)
		sb.WriteString(fmt.Sprintf("# TYPE %s counter\n", baseName))
		sb.WriteString(fmt.Sprintf("%s %d\n", name, c.Value()))
	}

	// Gauges
	for _, name := range gaugeNames {
		g := r.gauges[name]
		baseName := prometheusBaseName(name)
		sb.WriteString(fmt.Sprintf("# TYPE %s gauge\n", baseName))
		sb.WriteString(fmt.Sprintf("%s %g\n", name, g.Value()))
	}

	// Histograms
	for _, name := range histNames {
		h := r.histograms[name]
		sb.WriteString(fmt.Sprintf("# TYPE %s histogram\n", name))

		// Cumulative bucket counts
		var cumulative int64
		for i, bound := range h.buckets {
			cumulative += h.counts[i].Load()
			sb.WriteString(fmt.Sprintf("%s_bucket{le=\"%g\"} %d\n", name, bound, cumulative))
		}

		// +Inf bucket
		cumulative += h.counts[len(h.buckets)].Load()
		sb.WriteString(fmt.Sprintf("%s_bucket{le=\"+Inf\"} %d\n", name, cumulative))

		// Sum and count
		sb.WriteString(fmt.Sprintf("%s_sum %g\n", name, h.Sum()))
		sb.WriteString(fmt.Sprintf("%s_count %d\n", name, h.count.Load()))
	}

	// Uptime gauge
	sb.WriteString("# TYPE bridge_uptime_seconds gauge\n")
	sb.WriteString(fmt.Sprintf("bridge_uptime_seconds %g\n", time.Since(r.startTime).Seconds()))

	return sb.String()
}

// JSONSummary returns a map suitable for JSON serialization (for /health or admin API).
func (r *Registry) JSONSummary() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := map[string]any{
		"uptime": time.Since(r.startTime).String(),
	}

	// Counters
	counters := make(map[string]int64)
	for name, c := range r.counters {
		counters[name] = c.Value()
	}
	result["counters"] = counters

	// Gauges
	gauges := make(map[string]float64)
	for name, g := range r.gauges {
		gauges[name] = g.Value()
	}
	result["gauges"] = gauges

	// Histograms
	histos := make(map[string]map[string]any)
	for name, h := range r.histograms {
		histos[name] = map[string]any{
			"count": h.count.Load(),
			"sum":   h.Sum(),
		}
	}
	result["histograms"] = histos

	return result
}

// prometheusBaseName extracts the base metric name (before labels).
func prometheusBaseName(name string) string {
	if idx := strings.IndexByte(name, '{'); idx != -1 {
		return name[:idx]
	}
	return name
}

// sortedKeys returns sorted keys from a map for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
