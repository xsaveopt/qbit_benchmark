package tracker

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/bencode"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
)

const peerTTL = 2 * time.Minute

type entry struct {
	ip     net.IP
	port   uint16
	seeder bool
	seen   time.Time
}

type Tracker struct {
	mu     sync.Mutex
	swarm  map[string]map[string]entry
	static map[string]entry
	warned map[string]struct{}
	m      *metrics.App
	warn   io.Writer
}

func New(m *metrics.App) *Tracker {
	return &Tracker{
		swarm:  make(map[string]map[string]entry),
		static: make(map[string]entry),
		warned: make(map[string]struct{}),
		m:      m,
		warn:   os.Stderr,
	}
}

func (tr *Tracker) AddSeeder(infoHash [20]byte, ip net.IP, port uint16) {
	tr.mu.Lock()
	tr.static[string(infoHash[:])] = entry{ip: ip, port: port, seeder: true}
	tr.mu.Unlock()
}

func (tr *Tracker) warnUnreachable(host string) {
	tr.mu.Lock()
	_, seen := tr.warned[host]
	if !seen {
		tr.warned[host] = struct{}{}
	}
	tr.mu.Unlock()
	if seen {
		return
	}
	_, _ = fmt.Fprintf(tr.warn, "warning: peer at %s announced over IPv6, which the tracker cannot hand out to other peers; it will only be reachable if it also connects over IPv4\n", host)
}

func (tr *Tracker) Announce(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	infoHash := q.Get("info_hash")
	peerID := q.Get("peer_id")
	port, err := strconv.Atoi(q.Get("port"))
	if infoHash == "" || peerID == "" || err != nil || port < 1 || port > 65535 {
		http.Error(w, "missing or invalid params", http.StatusBadRequest)
		return
	}
	tr.m.Announces.Inc()
	host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
	if splitErr != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip.To4() == nil {
		tr.warnUnreachable(host)
	}
	left, leftErr := strconv.ParseInt(q.Get("left"), 10, 64)
	seeder := leftErr == nil && left == 0

	tr.mu.Lock()
	peers, ok := tr.swarm[infoHash]
	if !ok {
		peers = make(map[string]entry)
		tr.swarm[infoHash] = peers
	}
	if q.Get("event") == "stopped" {
		delete(peers, peerID)
	} else {
		peers[peerID] = entry{ip: ip, port: uint16(port), seeder: seeder, seen: time.Now()}
	}

	var compact []byte
	var complete, incomplete int64
	now := time.Now()
	add := func(e entry) {
		v4 := e.ip.To4()
		if v4 == nil {
			return
		}
		row := make([]byte, 6)
		copy(row[:4], v4)
		binary.BigEndian.PutUint16(row[4:], e.port)
		compact = append(compact, row...)
	}
	for id, p := range peers {
		if now.Sub(p.seen) > peerTTL {
			delete(peers, id)
			continue
		}
		if p.seeder {
			complete++
		} else {
			incomplete++
		}
		if id == peerID {
			continue
		}
		add(p)
	}
	if s, ok := tr.static[infoHash]; ok {
		complete++
		if !s.ip.Equal(ip) || s.port != uint16(port) {
			add(s)
		}
	}
	total := 0
	for _, ps := range tr.swarm {
		total += len(ps)
	}
	tr.mu.Unlock()
	tr.m.SwarmPeers.Set(int64(total))

	resp, err := bencode.Marshal(map[string]any{
		"interval":     int64(30),
		"min interval": int64(10),
		"complete":     complete,
		"incomplete":   incomplete,
		"peers":        compact,
	})
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(resp)
}
