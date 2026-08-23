package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

type kind int

const (
	counterKind kind = iota
	gaugeKind
)

type metric struct {
	name string
	help string
	kind kind
	val  atomic.Int64
}

type Registry struct {
	mu      sync.Mutex
	metrics []*metric
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) register(name, help string, k kind) *metric {
	m := &metric{name: name, help: help, kind: k}
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
	return m
}

type Counter struct{ m *metric }

func (c Counter) Add(n int64)  { c.m.val.Add(n) }
func (c Counter) Inc()         { c.m.val.Add(1) }
func (c Counter) Value() int64 { return c.m.val.Load() }

type Gauge struct{ m *metric }

func (g Gauge) Set(n int64)  { g.m.val.Store(n) }
func (g Gauge) Inc()         { g.m.val.Add(1) }
func (g Gauge) Dec()         { g.m.val.Add(-1) }
func (g Gauge) Value() int64 { return g.m.val.Load() }

func (r *Registry) NewCounter(name, help string) Counter {
	return Counter{m: r.register(name, help, counterKind)}
}

func (r *Registry) NewGauge(name, help string) Gauge {
	return Gauge{m: r.register(name, help, gaugeKind)}
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		ms := make([]*metric, len(r.metrics))
		copy(ms, r.metrics)
		r.mu.Unlock()
		sort.Slice(ms, func(i, j int) bool { return ms[i].name < ms[j].name })

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		for _, m := range ms {
			t := "counter"
			if m.kind == gaugeKind {
				t = "gauge"
			}
			_, _ = fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help)
			_, _ = fmt.Fprintf(w, "# TYPE %s %s\n", m.name, t)
			_, _ = fmt.Fprintf(w, "%s %d\n", m.name, m.val.Load())
		}
	})
}

type App struct {
	BytesServed  Counter
	PiecesServed Counter
	Requests     Counter
	ActiveConns  Gauge
	Announces    Counter
	SwarmPeers   Gauge

	reg *Registry
}

func NewApp() *App {
	r := NewRegistry()
	return &App{
		reg:          r,
		BytesServed:  r.NewCounter("qbb_bytes_served_total", "Total bytes served to peers by the seeder."),
		PiecesServed: r.NewCounter("qbb_pieces_served_total", "Total piece blocks served to peers."),
		Requests:     r.NewCounter("qbb_requests_total", "Total piece requests received from peers."),
		ActiveConns:  r.NewGauge("qbb_active_connections", "Currently open seeder peer connections."),
		Announces:    r.NewCounter("qbb_tracker_announces_total", "Total tracker announce requests handled."),
		SwarmPeers:   r.NewGauge("qbb_swarm_peers", "Peers currently known to the tracker across all swarms."),
	}
}

func (a *App) Handler() http.Handler { return a.reg.Handler() }
