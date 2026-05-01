package app

import (
	"fmt"

	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
)

// bgMetricsAdapter bridges bg.Group's tiny Metrics interface to the
// metrics.Registry. It encodes the goroutine name as a Prometheus
// label (`bg_goroutines_running{name="x"}`) so each tracked task gets
// its own time series while sharing one TYPE declaration.
//
// The adapter lives in this package (not in internal/bg) so the bg
// package stays free of any metrics-package dependency — bg can be
// reused or unit-tested without dragging in the registry.
type bgMetricsAdapter struct {
	reg *metrics.Registry
}

func (a bgMetricsAdapter) gauge(name string) *metrics.Gauge {
	// Quote the label value the way Prometheus expects so
	// PrometheusText() emits a syntactically valid line. The base
	// name is stripped by prometheusBaseName when the TYPE comment
	// is rendered, so all per-name series share one
	// `# TYPE bg_goroutines_running gauge` header.
	return a.reg.Gauge(fmt.Sprintf(`bg_goroutines_running{name=%q}`, name))
}

func (a bgMetricsAdapter) Inc(name string) { a.gauge(name).Inc() }
func (a bgMetricsAdapter) Dec(name string) { a.gauge(name).Dec() }
