// Package metrics is a minimal Prometheus text-format exporter.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

type Counter struct{ value atomic.Uint64 }

func (c *Counter) Inc()          { c.value.Add(1) }
func (c *Counter) Add(n uint64)  { c.value.Add(n) }
func (c *Counter) Value() uint64 { return c.value.Load() }

type metric struct {
	name, help, kind string
	read             func() float64
}

type Registry struct {
	mu         sync.Mutex
	metrics    []metric
	collectors []func() map[string]float64
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{}
	r.add(metric{name, help, "counter", func() float64 { return float64(c.Value()) }})
	return c
}

func (r *Registry) Gauge(name, help string, read func() float64) {
	r.add(metric{name, help, "gauge", read})
}

// Collect registers a source of untyped samples that is polled on every scrape.
func (r *Registry) Collect(fn func() map[string]float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, fn)
}

func (r *Registry) add(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, m)
}

func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	metrics := append([]metric(nil), r.metrics...)
	collectors := append([]func() map[string]float64(nil), r.collectors...)
	r.mu.Unlock()

	var written int64
	emit := func(format string, args ...any) error {
		n, err := fmt.Fprintf(w, format, args...)
		written += int64(n)
		return err
	}
	for _, m := range metrics {
		if err := emit("# HELP %s %s\n# TYPE %s %s\n%s %g\n", m.name, m.help, m.name, m.kind, m.name, m.read()); err != nil {
			return written, err
		}
	}
	for _, collect := range collectors {
		samples := collect()
		names := make([]string, 0, len(samples))
		for name := range samples {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := emit("# TYPE %s untyped\n%s %g\n", name, name, samples[name]); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = r.WriteTo(w)
}
