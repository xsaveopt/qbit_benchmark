package tracker

import (
	"encoding/binary"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/bencode"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
)

const peerTTL = 2 * time.Minute

type entry struct {
	ip   net.IP
	port uint16
	seen time.Time
}

type Tracker struct {
	mu    sync.Mutex
	swarm map[string]map[string]entry
	m     *metrics.App
}

func New(m *metrics.App) *Tracker {
	return &Tracker{swarm: make(map[string]map[string]entry), m: m}
}

func (tr *Tracker) Announce(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	infoHash := q.Get("info_hash")
	peerID := q.Get("peer_id")
	port, err := strconv.Atoi(q.Get("port"))
	if infoHash == "" || peerID == "" || err != nil {
		http.Error(w, "missing or invalid params", http.StatusBadRequest)
		return
	}
	tr.m.Announces.Inc()
	host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
	if splitErr != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)

	tr.mu.Lock()
	peers, ok := tr.swarm[infoHash]
	if !ok {
		peers = make(map[string]entry)
		tr.swarm[infoHash] = peers
	}
	peers[peerID] = entry{ip: ip, port: uint16(port), seen: time.Now()}

	var compact []byte
	now := time.Now()
	for id, p := range peers {
		if now.Sub(p.seen) > peerTTL {
			delete(peers, id)
			continue
		}
		if id == peerID {
			continue
		}
		v4 := p.ip.To4()
		if v4 == nil {
			continue
		}
		row := make([]byte, 6)
		copy(row[:4], v4)
		binary.BigEndian.PutUint16(row[4:], p.port)
		compact = append(compact, row...)
	}
	total := 0
	for _, ps := range tr.swarm {
		total += len(ps)
	}
	tr.mu.Unlock()
	tr.m.SwarmPeers.Set(int64(total))

	resp, err := bencode.Marshal(map[string]any{
		"interval": int64(30),
		"peers":    compact,
	})
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(resp)
}
