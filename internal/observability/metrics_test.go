package observability

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCounterAndNilSafety(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "help")
	c.Inc()
	c.Inc()
	if c.Value() != 2 || r.Counter("x_total", "ignored") != c {
		t.Fatal("counter must be shared by name")
	}
	var nilCounter *Counter
	nilCounter.Inc() // must not panic
	var nilHist *Histogram
	nilHist.Observe(time.Second)
	var nilReg *Registry
	nilReg.Counter("a", "b").Inc()
	nilReg.Histogram("a", "b").Observe(time.Second)
}

func TestCountersAreSafeForConcurrentUse(t *testing.T) {
	c := NewRegistry().Counter("c_total", "h")
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Inc()
			}
		}()
	}
	wg.Wait()
	if c.Value() != 5000 {
		t.Fatalf("value = %d", c.Value())
	}
}

func TestHistogramBucketsAndRendering(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("lat_seconds", "latency")
	h.Observe(500 * time.Microsecond) // le 0.001
	h.Observe(30 * time.Millisecond)  // le 0.05
	h.Observe(10 * time.Second)       // +Inf
	r.Counter("b_total", "second").Inc()
	r.Counter("a_total", "first").Inc()

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE a_total counter\na_total 1", "# TYPE lat_seconds histogram",
		`lat_seconds_bucket{le="0.001"} 1`, `lat_seconds_bucket{le="0.05"} 2`, `lat_seconds_bucket{le="+Inf"} 3`,
		"lat_seconds_count 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "a_total") > strings.Index(body, "b_total") {
		t.Error("counters must be sorted by name")
	}
}
