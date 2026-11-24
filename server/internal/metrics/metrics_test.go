package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	joins := r.Counter("ambivox_joins_total", "Peers that joined a room.")
	r.Gauge("ambivox_rooms", "Open rooms.", func() float64 { return 2 })
	r.Collect(func() map[string]float64 { return map[string]float64{"ambivox_media_b": 1.5, "ambivox_media_a": 3} })
	joins.Inc()
	joins.Add(2)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	want := `# HELP ambivox_joins_total Peers that joined a room.
# TYPE ambivox_joins_total counter
ambivox_joins_total 3
# HELP ambivox_rooms Open rooms.
# TYPE ambivox_rooms gauge
ambivox_rooms 2
# TYPE ambivox_media_a untyped
ambivox_media_a 3
# TYPE ambivox_media_b untyped
ambivox_media_b 1.5
`
	if got := rec.Body.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type = %q", ct)
	}
}

func TestCountersAreSafeForConcurrentUse(t *testing.T) {
	c := NewRegistry().Counter("n", "n")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				c.Inc()
			}
		})
	}
	wg.Wait()
	if c.Value() != 8000 {
		t.Errorf("value = %d", c.Value())
	}
}
