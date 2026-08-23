package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestCounter(t *testing.T) {
	c := NewRegistry().NewCounter("x_total", "help")
	if c.Value() != 0 {
		t.Fatalf("a fresh counter reads %d", c.Value())
	}
	c.Inc()
	c.Add(41)
	if c.Value() != 42 {
		t.Fatalf("Value = %d, want 42", c.Value())
	}
}

func TestGauge(t *testing.T) {
	g := NewRegistry().NewGauge("x", "help")
	g.Set(10)
	g.Inc()
	g.Dec()
	g.Dec()
	if g.Value() != 9 {
		t.Fatalf("Value = %d, want 9", g.Value())
	}
	g.Set(-3)
	if g.Value() != -3 {
		t.Fatalf("Value = %d, want -3", g.Value())
	}
}

func TestHandlerExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("qbb_test_total", "A test counter.")
	g := r.NewGauge("qbb_test_gauge", "A test gauge.")
	c.Add(7)
	g.Set(3)

	body := scrape(t, r.Handler())
	for _, want := range []string{
		"# HELP qbb_test_total A test counter.",
		"# TYPE qbb_test_total counter",
		"qbb_test_total 7",
		"# HELP qbb_test_gauge A test gauge.",
		"# TYPE qbb_test_gauge gauge",
		"qbb_test_gauge 3",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition is missing %q:\n%s", want, body)
		}
	}
}

func TestHandlerSortsMetricsByName(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("zzz_total", "z")
	r.NewCounter("aaa_total", "a")
	r.NewGauge("mmm", "m")

	body := scrape(t, r.Handler())
	first := strings.Index(body, "aaa_total")
	middle := strings.Index(body, "mmm")
	last := strings.Index(body, "zzz_total")
	if first > middle || middle > last {
		t.Fatalf("metrics are not in name order:\n%s", body)
	}
}

func TestAppRegistersEveryMetric(t *testing.T) {
	app := NewApp()
	app.BytesServed.Add(1024)
	app.PiecesServed.Inc()
	app.Requests.Inc()
	app.ActiveConns.Set(2)
	app.Announces.Inc()
	app.SwarmPeers.Set(5)

	body := scrape(t, app.Handler())
	for _, want := range []string{
		"qbb_bytes_served_total 1024",
		"qbb_pieces_served_total 1",
		"qbb_requests_total 1",
		"qbb_active_connections 2",
		"qbb_tracker_announces_total 1",
		"qbb_swarm_peers 5",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition is missing %q:\n%s", want, body)
		}
	}
}

func TestConcurrentUpdates(t *testing.T) {
	app := NewApp()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 100; k++ {
				app.BytesServed.Add(16384)
				app.ActiveConns.Inc()
				app.ActiveConns.Dec()
			}
		}()
	}
	wg.Wait()
	if got, want := app.BytesServed.Value(), int64(32*100*16384); got != want {
		t.Fatalf("BytesServed = %d, want %d", got, want)
	}
	if got := app.ActiveConns.Value(); got != 0 {
		t.Fatalf("ActiveConns = %d, want 0", got)
	}
}
