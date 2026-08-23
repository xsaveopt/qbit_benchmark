package tracker

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/bencode"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
)

var testInfoHash = [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}

func newTestTracker(t *testing.T) (*Tracker, *bytes.Buffer) {
	t.Helper()
	tr := New(metrics.NewApp())
	warnings := &bytes.Buffer{}
	tr.warn = warnings
	return tr, warnings
}

type announceParams struct {
	infoHash   [20]byte
	peerID     string
	port       string
	left       string
	event      string
	remoteAddr string
}

func announce(t *testing.T, tr *Tracker, p announceParams) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{}
	q.Set("info_hash", string(p.infoHash[:]))
	q.Set("peer_id", p.peerID)
	q.Set("port", p.port)
	if p.left != "" {
		q.Set("left", p.left)
	}
	if p.event != "" {
		q.Set("event", p.event)
	}
	req := httptest.NewRequest(http.MethodGet, "/announce?"+q.Encode(), nil)
	if p.remoteAddr != "" {
		req.RemoteAddr = p.remoteAddr
	}
	rec := httptest.NewRecorder()
	tr.Announce(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	v, err := bencode.Unmarshal(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("response is not valid bencode: %v", err)
	}
	dict, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("response is not a dict: %#v", v)
	}
	return dict
}

func peerList(t *testing.T, dict map[string]any) []string {
	t.Helper()
	raw, ok := dict["peers"].([]byte)
	if !ok {
		if dict["peers"] == nil {
			return nil
		}
		t.Fatalf("peers is not a byte string: %#v", dict["peers"])
	}
	if len(raw)%6 != 0 {
		t.Fatalf("compact peer list is %d bytes, not a multiple of 6", len(raw))
	}
	var out []string
	for i := 0; i < len(raw); i += 6 {
		ip := net.IP(raw[i : i+4])
		port := binary.BigEndian.Uint16(raw[i+4 : i+6])
		out = append(out, net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestAnnounceRejectsInvalidParams(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"no params", ""},
		{"missing info_hash", "peer_id=aaaaaaaaaaaaaaaaaaaa&port=6881"},
		{"missing peer_id", "info_hash=aaaaaaaaaaaaaaaaaaaa&port=6881"},
		{"missing port", "info_hash=aaaaaaaaaaaaaaaaaaaa&peer_id=bbbbbbbbbbbbbbbbbbbb"},
		{"port not numeric", "info_hash=aaaaaaaaaaaaaaaaaaaa&peer_id=bbbbbbbbbbbbbbbbbbbb&port=abc"},
		{"port zero", "info_hash=aaaaaaaaaaaaaaaaaaaa&peer_id=bbbbbbbbbbbbbbbbbbbb&port=0"},
		{"port negative", "info_hash=aaaaaaaaaaaaaaaaaaaa&peer_id=bbbbbbbbbbbbbbbbbbbb&port=-1"},
		{"port above 65535", "info_hash=aaaaaaaaaaaaaaaaaaaa&peer_id=bbbbbbbbbbbbbbbbbbbb&port=999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, _ := newTestTracker(t)
			req := httptest.NewRequest(http.MethodGet, "/announce?"+tc.query, nil)
			rec := httptest.NewRecorder()
			tr.Announce(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestAnnounceReturnsRegisteredSeeder(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.AddSeeder(testInfoHash, net.ParseIP("192.0.2.50"), 6881)

	rec := announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-aaaaaaaaaaaa",
		port:       "6882",
		left:       "1048576",
		remoteAddr: "192.0.2.10:40000",
	})
	dict := decodeResponse(t, rec)
	peers := peerList(t, dict)
	if !contains(peers, "192.0.2.50:6881") {
		t.Fatalf("seeder missing from peer list %v", peers)
	}
	if dict["complete"].(int64) != 1 {
		t.Fatalf("complete = %v, want 1", dict["complete"])
	}
	if dict["incomplete"].(int64) != 1 {
		t.Fatalf("incomplete = %v, want 1", dict["incomplete"])
	}
	if dict["interval"].(int64) <= 0 {
		t.Fatalf("interval = %v, want a positive value", dict["interval"])
	}
}

func TestAnnounceForOtherTorrentDoesNotGetSeeder(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.AddSeeder(testInfoHash, net.ParseIP("192.0.2.50"), 6881)

	other := [20]byte{99}
	rec := announce(t, tr, announceParams{
		infoHash:   other,
		peerID:     "-QB5000-aaaaaaaaaaaa",
		port:       "6882",
		left:       "1",
		remoteAddr: "192.0.2.10:40000",
	})
	dict := decodeResponse(t, rec)
	if peers := peerList(t, dict); len(peers) != 0 {
		t.Fatalf("got peers %v for an unrelated infohash", peers)
	}
	if dict["complete"].(int64) != 0 {
		t.Fatalf("complete = %v, want 0", dict["complete"])
	}
}

func TestAnnounceExcludesTheCallerFromItsOwnPeerList(t *testing.T) {
	tr, _ := newTestTracker(t)
	first := announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-aaaaaaaaaaaa",
		port:       "6881",
		left:       "1",
		remoteAddr: "192.0.2.10:40000",
	}
	if peers := peerList(t, decodeResponse(t, announce(t, tr, first))); len(peers) != 0 {
		t.Fatalf("first announce returned itself: %v", peers)
	}

	second := announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-bbbbbbbbbbbb",
		port:       "6882",
		left:       "1",
		remoteAddr: "192.0.2.11:40001",
	}
	peers := peerList(t, decodeResponse(t, announce(t, tr, second)))
	if len(peers) != 1 || peers[0] != "192.0.2.10:6881" {
		t.Fatalf("second announce got %v, want the first peer only", peers)
	}
}

func TestAnnounceCountsSeedersAndLeechers(t *testing.T) {
	tr, _ := newTestTracker(t)
	announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-seeder000000",
		port:       "6881",
		left:       "0",
		remoteAddr: "192.0.2.10:40000",
	})
	dict := decodeResponse(t, announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-leecher00000",
		port:       "6882",
		left:       "1048576",
		remoteAddr: "192.0.2.11:40001",
	}))
	if dict["complete"].(int64) != 1 {
		t.Fatalf("complete = %v, want 1", dict["complete"])
	}
	if dict["incomplete"].(int64) != 1 {
		t.Fatalf("incomplete = %v, want 1", dict["incomplete"])
	}
}

func TestAnnounceStoppedRemovesPeer(t *testing.T) {
	tr, _ := newTestTracker(t)
	leaving := announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-aaaaaaaaaaaa",
		port:       "6881",
		left:       "0",
		remoteAddr: "192.0.2.10:40000",
	}
	announce(t, tr, leaving)
	leaving.event = "stopped"
	announce(t, tr, leaving)

	dict := decodeResponse(t, announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-bbbbbbbbbbbb",
		port:       "6882",
		left:       "1",
		remoteAddr: "192.0.2.11:40001",
	}))
	if peers := peerList(t, dict); len(peers) != 0 {
		t.Fatalf("stopped peer is still listed: %v", peers)
	}
}

func TestExpiredPeersAreDropped(t *testing.T) {
	tr, _ := newTestTracker(t)
	announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-stale0000000",
		port:       "6881",
		left:       "1",
		remoteAddr: "192.0.2.10:40000",
	})

	tr.mu.Lock()
	for id, e := range tr.swarm[string(testInfoHash[:])] {
		e.seen = e.seen.Add(-peerTTL - time.Minute)
		tr.swarm[string(testInfoHash[:])][id] = e
	}
	tr.mu.Unlock()

	dict := decodeResponse(t, announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-fresh0000000",
		port:       "6882",
		left:       "1",
		remoteAddr: "192.0.2.11:40001",
	}))
	if peers := peerList(t, dict); len(peers) != 0 {
		t.Fatalf("expired peer is still listed: %v", peers)
	}
	tr.mu.Lock()
	remaining := len(tr.swarm[string(testInfoHash[:])])
	tr.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("swarm holds %d peers, want only the fresh one", remaining)
	}
}

func TestIPv6PeerIsWarnedAboutAndNotListed(t *testing.T) {
	tr, warnings := newTestTracker(t)
	tr.AddSeeder(testInfoHash, net.ParseIP("192.0.2.50"), 6881)

	v6 := announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-v6peer000000",
		port:       "6881",
		left:       "1",
		remoteAddr: "[2001:db8::1]:40000",
	}
	dict := decodeResponse(t, announce(t, tr, v6))
	if peers := peerList(t, dict); !contains(peers, "192.0.2.50:6881") {
		t.Fatalf("IPv6 peer did not get the IPv4 seeder: %v", peers)
	}
	if !strings.Contains(warnings.String(), "IPv6") {
		t.Fatalf("no IPv6 warning was emitted, got %q", warnings.String())
	}

	announce(t, tr, v6)
	if strings.Count(warnings.String(), "IPv6") != 1 {
		t.Fatalf("warning repeated for the same peer: %q", warnings.String())
	}

	dict = decodeResponse(t, announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QB5000-v4peer000000",
		port:       "6882",
		left:       "1",
		remoteAddr: "192.0.2.11:40001",
	}))
	peers := peerList(t, dict)
	if contains(peers, "[2001:db8::1]:6881") {
		t.Fatalf("IPv6 peer was handed out: %v", peers)
	}
	if len(peers) != 1 || peers[0] != "192.0.2.50:6881" {
		t.Fatalf("got %v, want the seeder only", peers)
	}
}

func TestSeederIsNotReturnedToItself(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.AddSeeder(testInfoHash, net.ParseIP("192.0.2.50"), 6881)

	dict := decodeResponse(t, announce(t, tr, announceParams{
		infoHash:   testInfoHash,
		peerID:     "-QBB000-seeder000000",
		port:       "6881",
		left:       "0",
		remoteAddr: "192.0.2.50:40000",
	}))
	if peers := peerList(t, dict); len(peers) != 0 {
		t.Fatalf("seeder was handed its own address: %v", peers)
	}
}

func TestConcurrentAnnouncesAreSafe(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.AddSeeder(testInfoHash, net.ParseIP("192.0.2.50"), 6881)

	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for k := 0; k < 25; k++ {
				announce(t, tr, announceParams{
					infoHash:   testInfoHash,
					peerID:     "-QB5000-peer" + strconv.Itoa(i) + strings.Repeat("0", 8-len(strconv.Itoa(i))),
					port:       "6881",
					left:       "1",
					remoteAddr: "192.0.2." + strconv.Itoa(i%200+1) + ":40000",
				})
			}
		}(i)
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}
