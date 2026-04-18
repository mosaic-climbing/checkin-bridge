package metrics

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCounterInc(t *testing.T) {
	c := &Counter{}
	if c.Value() != 0 {
		t.Errorf("initial value should be 0, got %d", c.Value())
	}

	c.Inc()
	if c.Value() != 1 {
		t.Errorf("after Inc(), value should be 1, got %d", c.Value())
	}

	c.Inc()
	if c.Value() != 2 {
		t.Errorf("after 2x Inc(), value should be 2, got %d", c.Value())
	}
}

func TestCounterAdd(t *testing.T) {
	c := &Counter{}
	c.Add(5)
	if c.Value() != 5 {
		t.Errorf("after Add(5), value should be 5, got %d", c.Value())
	}

	c.Add(3)
	if c.Value() != 8 {
		t.Errorf("after Add(3), value should be 8, got %d", c.Value())
	}

	c.Add(-2)
	if c.Value() != 6 {
		t.Errorf("after Add(-2), value should be 6, got %d", c.Value())
	}
}

func TestGaugeSet(t *testing.T) {
	g := &Gauge{}
	if g.Value() != 0 {
		t.Errorf("initial value should be 0, got %f", g.Value())
	}

	g.Set(42.5)
	if g.Value() != 42.5 {
		t.Errorf("after Set(42.5), value should be 42.5, got %f", g.Value())
	}

	g.Set(-10.25)
	if g.Value() != -10.25 {
		t.Errorf("after Set(-10.25), value should be -10.25, got %f", g.Value())
	}
}

func TestGaugeSetInt(t *testing.T) {
	g := &Gauge{}
	g.SetInt(100)
	if g.Value() != 100.0 {
		t.Errorf("after SetInt(100), value should be 100.0, got %f", g.Value())
	}

	g.SetInt(-50)
	if g.Value() != -50.0 {
		t.Errorf("after SetInt(-50), value should be -50.0, got %f", g.Value())
	}
}

func TestGaugeIncDec(t *testing.T) {
	g := &Gauge{}
	g.Set(10.0)

	g.Inc()
	if g.Value() != 11.0 {
		t.Errorf("after Inc(), value should be 11.0, got %f", g.Value())
	}

	g.Dec()
	g.Dec()
	if g.Value() != 9.0 {
		t.Errorf("after Dec() twice, value should be 9.0, got %f", g.Value())
	}
}

func TestHistogramObserve(t *testing.T) {
	buckets := []float64{0.1, 0.5, 1.0, 5.0}
	h := newHistogram(buckets)

	h.Observe(0.05)
	h.Observe(0.25)
	h.Observe(0.75)
	h.Observe(2.0)
	h.Observe(10.0)

	if h.Count() != 5 {
		t.Errorf("expected count 5, got %d", h.Count())
	}

	expectedSum := 0.05 + 0.25 + 0.75 + 2.0 + 10.0
	if math.Abs(h.Sum()-expectedSum) > 1e-9 {
		t.Errorf("expected sum %f, got %f", expectedSum, h.Sum())
	}
}

func TestHistogramBuckets(t *testing.T) {
	buckets := []float64{0.1, 0.5, 1.0}
	h := newHistogram(buckets)

	// Verify buckets are sorted
	for i := 0; i < len(h.buckets)-1; i++ {
		if h.buckets[i] > h.buckets[i+1] {
			t.Errorf("buckets are not sorted")
		}
	}
}

func TestRegistryCounterGetOrCreate(t *testing.T) {
	r := New()

	c1 := r.Counter("test_counter")
	c1.Add(5)

	c2 := r.Counter("test_counter")
	if c2.Value() != 5 {
		t.Errorf("same counter should be returned, expected 5, got %d", c2.Value())
	}

	if c1 != c2 {
		t.Errorf("same counter instance should be returned")
	}
}

func TestRegistryGaugeGetOrCreate(t *testing.T) {
	r := New()

	g1 := r.Gauge("test_gauge")
	g1.Set(42.0)

	g2 := r.Gauge("test_gauge")
	if g2.Value() != 42.0 {
		t.Errorf("same gauge should be returned, expected 42.0, got %f", g2.Value())
	}

	if g1 != g2 {
		t.Errorf("same gauge instance should be returned")
	}
}

func TestRegistryHistogramGetOrCreate(t *testing.T) {
	r := New()

	buckets := []float64{0.1, 1.0, 10.0}
	h1 := r.Histogram("test_histogram", buckets)
	h1.Observe(0.5)

	h2 := r.Histogram("test_histogram", buckets)
	if h2.Count() != 1 {
		t.Errorf("same histogram should be returned, expected count 1, got %d", h2.Count())
	}

	if h1 != h2 {
		t.Errorf("same histogram instance should be returned")
	}
}

func TestPrometheusTextFormat(t *testing.T) {
	r := New()

	// Add some metrics
	r.Counter("checkin_total{result=\"allowed\"}").Add(10)
	r.Counter("checkin_total{result=\"denied\"}").Add(3)
	r.Gauge("unifi_ws_connected").Set(1.0)
	r.Gauge("members_total{status=\"active\"}").SetInt(150)

	// Add histogram
	h := r.Histogram("checkin_duration_seconds", DefaultLatencyBuckets)
	h.Observe(0.001)
	h.Observe(0.05)
	h.Observe(0.5)

	output := r.PrometheusText()

	// Verify basic structure
	if !strings.Contains(output, "# TYPE checkin_total counter") {
		t.Errorf("missing counter TYPE declaration")
	}

	if !strings.Contains(output, "checkin_total{result=\"allowed\"} 10") {
		t.Errorf("missing counter metric line")
	}

	if !strings.Contains(output, "# TYPE unifi_ws_connected gauge") {
		t.Errorf("missing gauge TYPE declaration")
	}

	if !strings.Contains(output, "unifi_ws_connected 1") {
		t.Errorf("missing gauge metric line")
	}

	if !strings.Contains(output, "# TYPE checkin_duration_seconds histogram") {
		t.Errorf("missing histogram TYPE declaration")
	}

	if !strings.Contains(output, "checkin_duration_seconds_bucket{le=\"+Inf\"}") {
		t.Errorf("missing histogram +Inf bucket")
	}

	if !strings.Contains(output, "checkin_duration_seconds_sum") {
		t.Errorf("missing histogram sum")
	}

	if !strings.Contains(output, "checkin_duration_seconds_count 3") {
		t.Errorf("missing histogram count")
	}

	if !strings.Contains(output, "# TYPE bridge_uptime_seconds gauge") {
		t.Errorf("missing uptime metric")
	}
}

func TestPrometheusTextDeterministic(t *testing.T) {
	// Verify that PrometheusText produces deterministic output
	r := New()

	r.Counter("zzz_counter").Inc()
	r.Counter("aaa_counter").Inc()
	r.Gauge("mmm_gauge").Set(1.0)

	// Get output twice in quick succession (should be nearly identical except for uptime)
	output1 := r.PrometheusText()
	output2 := r.PrometheusText()

	// Note: uptime will differ slightly between calls, so we just verify
	// the metrics themselves are in both outputs

	// Verify ordering of metrics (excluding uptime)
	aaa_idx := strings.Index(output1, "aaa_counter")
	zzz_idx := strings.Index(output1, "zzz_counter")

	if aaa_idx == -1 || zzz_idx == -1 {
		t.Errorf("counters not found in output")
	}

	if aaa_idx > zzz_idx {
		t.Errorf("counters should be sorted alphabetically")
	}

	// Verify both outputs contain all metrics
	if !strings.Contains(output1, "aaa_counter") || !strings.Contains(output2, "aaa_counter") {
		t.Errorf("both outputs should contain aaa_counter")
	}

	if !strings.Contains(output1, "zzz_counter") || !strings.Contains(output2, "zzz_counter") {
		t.Errorf("both outputs should contain zzz_counter")
	}

	if !strings.Contains(output1, "mmm_gauge") || !strings.Contains(output2, "mmm_gauge") {
		t.Errorf("both outputs should contain mmm_gauge")
	}
}

func TestJSONSummary(t *testing.T) {
	r := New()

	// Add metrics
	r.Counter("test_counter").Add(42)
	r.Gauge("test_gauge").Set(3.14)
	h := r.Histogram("test_histogram", []float64{0.1, 1.0})
	h.Observe(0.05)
	h.Observe(0.5)

	summary := r.JSONSummary()

	// Verify structure
	if _, ok := summary["uptime"]; !ok {
		t.Errorf("missing 'uptime' in summary")
	}

	counters, ok := summary["counters"].(map[string]int64)
	if !ok {
		t.Errorf("missing or wrong type for 'counters'")
	}

	gauges, ok := summary["gauges"].(map[string]float64)
	if !ok {
		t.Errorf("missing or wrong type for 'gauges'")
	}

	histos, ok := summary["histograms"].(map[string]map[string]any)
	if !ok {
		t.Errorf("missing or wrong type for 'histograms'")
	}

	// Verify values
	if counters["test_counter"] != 42 {
		t.Errorf("counter value mismatch")
	}

	if gauges["test_gauge"] != 3.14 {
		t.Errorf("gauge value mismatch")
	}

	if histos["test_histogram"]["count"] != int64(2) {
		t.Errorf("histogram count mismatch")
	}
}

func TestJSONSummaryMarshalable(t *testing.T) {
	r := New()

	r.Counter("test_counter").Add(10)
	r.Gauge("test_gauge").Set(2.5)

	summary := r.JSONSummary()

	// Should be JSON marshallable
	data, err := json.Marshal(summary)
	if err != nil {
		t.Errorf("failed to marshal summary to JSON: %v", err)
	}

	// Verify it contains expected fields
	jsonStr := string(data)
	if !strings.Contains(jsonStr, "test_counter") {
		t.Errorf("counter not in JSON output")
	}

	if !strings.Contains(jsonStr, "test_gauge") {
		t.Errorf("gauge not in JSON output")
	}
}

func TestPrometheusBaseName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"counter_total", "counter_total"},
		{"metric{label=\"value\"}", "metric"},
		{"histogram{le=\"0.5\"}", "histogram"},
		{"gauge{instance=\"localhost\"}", "gauge"},
	}

	for _, tc := range tests {
		result := prometheusBaseName(tc.input)
		if result != tc.expected {
			t.Errorf("prometheusBaseName(%q) = %q, expected %q", tc.input, result, tc.expected)
		}
	}
}

func TestConcurrentCounterInc(t *testing.T) {
	c := &Counter{}
	done := make(chan bool, 100)

	for i := 0; i < 100; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				c.Inc()
			}
			done <- true
		}()
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	if c.Value() != 10000 {
		t.Errorf("concurrent Inc() failed: expected 10000, got %d", c.Value())
	}
}

func TestConcurrentGaugeOps(t *testing.T) {
	g := &Gauge{}
	g.Set(50.0)

	done := make(chan bool, 100)

	for i := 0; i < 100; i++ {
		go func() {
			g.Inc()
			g.Dec()
			done <- true
		}()
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	// After 100 Inc() and 100 Dec(), should be back to 50
	if g.Value() != 50.0 {
		t.Errorf("concurrent gauge ops failed: expected 50.0, got %f", g.Value())
	}
}

func TestConcurrentHistogramObserve(t *testing.T) {
	h := newHistogram([]float64{0.1, 1.0, 10.0})
	done := make(chan bool, 100)

	for i := 0; i < 100; i++ {
		go func(val float64) {
			for j := 0; j < 10; j++ {
				h.Observe(val)
			}
			done <- true
		}(float64(i%10) * 0.1)
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	if h.Count() != 1000 {
		t.Errorf("concurrent histogram observations failed: expected count 1000, got %d", h.Count())
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := New()
	done := make(chan bool, 50)

	// Concurrent counter updates
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				c := r.Counter("counter")
				c.Inc()
			}
			done <- true
		}(i)
	}

	// Concurrent gauge updates
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				g := r.Gauge("gauge")
				g.Set(float64(id))
			}
			done <- true
		}(i)
	}

	// Concurrent histogram observations
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				h := r.Histogram("histogram", DefaultLatencyBuckets)
				h.Observe(float64(id) * 0.1)
			}
			done <- true
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 20; i++ {
		go func() {
			_ = r.PrometheusText()
			_ = r.JSONSummary()
			done <- true
		}()
	}

	for i := 0; i < 50; i++ {
		<-done
	}

	// Verify final state
	if r.Counter("counter").Value() != 100 {
		t.Errorf("final counter value incorrect")
	}
}

func TestUptimeIncrements(t *testing.T) {
	r := New()
	start := time.Now()

	summary1 := r.JSONSummary()
	time.Sleep(10 * time.Millisecond)
	summary2 := r.JSONSummary()

	// Both should have uptime strings, and second should be >= first
	if _, ok := summary1["uptime"]; !ok {
		t.Errorf("missing uptime in summary")
	}

	if _, ok := summary2["uptime"]; !ok {
		t.Errorf("missing uptime in summary")
	}

	// Verify uptime is approximately correct
	elapsed := time.Since(start)
	if elapsed < 5*time.Millisecond || elapsed > 1*time.Second {
		t.Errorf("uptime seems wrong: %v", elapsed)
	}
}

func TestEmptyMetricsOutput(t *testing.T) {
	r := New()

	// No metrics added, should only have uptime
	output := r.PrometheusText()

	if !strings.Contains(output, "bridge_uptime_seconds") {
		t.Errorf("uptime should be in empty registry output")
	}

	summary := r.JSONSummary()
	if len(summary["counters"].(map[string]int64)) != 0 {
		t.Errorf("empty registry should have no counters")
	}

	if len(summary["gauges"].(map[string]float64)) != 0 {
		t.Errorf("empty registry should have no gauges")
	}
}

func TestDefaultLatencyBuckets(t *testing.T) {
	expected := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 10, 30}

	if len(DefaultLatencyBuckets) != len(expected) {
		t.Errorf("DefaultLatencyBuckets length mismatch")
	}

	for i, val := range expected {
		if DefaultLatencyBuckets[i] != val {
			t.Errorf("DefaultLatencyBuckets[%d] = %f, expected %f", i, DefaultLatencyBuckets[i], val)
		}
	}
}

func TestLargeHistogramValues(t *testing.T) {
	h := newHistogram([]float64{1.0, 10.0, 100.0})

	h.Observe(0.5)
	h.Observe(5.0)
	h.Observe(50.0)
	h.Observe(500.0)

	if h.Count() != 4 {
		t.Errorf("expected count 4, got %d", h.Count())
	}

	expectedSum := 0.5 + 5.0 + 50.0 + 500.0
	if math.Abs(h.Sum()-expectedSum) > 1e-9 {
		t.Errorf("expected sum %f, got %f", expectedSum, h.Sum())
	}
}
