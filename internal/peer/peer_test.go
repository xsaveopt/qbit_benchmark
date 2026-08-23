package peer

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/metainfo"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
)

func startSeeder(t *testing.T, tor *metainfo.Torrent) (string, *metrics.App) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	m := metrics.NewApp()
	go func() { _ = NewSeeder(tor, m).Serve(ln) }()
	return ln.Addr().String(), m
}

func testTorrent(t *testing.T, totalSize int64) *metainfo.Torrent {
	t.Helper()
	tor, err := metainfo.New("test", totalSize, 65536)
	if err != nil {
		t.Fatal(err)
	}
	return tor
}

func TestNewPeerIDIsUniqueAndTagged(t *testing.T) {
	a := NewPeerID()
	b := NewPeerID()
	if a == b {
		t.Fatal("two peer ids came out identical")
	}
	if !bytes.HasPrefix(a[:], []byte("-QB5000-")) {
		t.Fatalf("peer id %q lacks the client prefix", a)
	}
}

func TestHandshakeRoundTrip(t *testing.T) {
	want := [20]byte{1, 2, 3}
	var buf bytes.Buffer
	if err := writeHandshake(&buf, want, NewPeerID()); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 68 {
		t.Fatalf("handshake is %d bytes, want 68", buf.Len())
	}
	got, err := readHandshake(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("infohash = %x, want %x", got, want)
	}
}

func TestReadHandshakeRejectsWrongProtocol(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(19)
	buf.WriteString("NotTorrent protocol")
	buf.Write(make([]byte, 48))
	if _, err := readHandshake(&buf); err == nil {
		t.Fatal("expected an error for a bad protocol string")
	}
}

func TestReadHandshakeRejectsTruncatedInput(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(19)
	buf.WriteString("BitTorrent")
	if _, err := readHandshake(&buf); err == nil {
		t.Fatal("expected an error for a truncated handshake")
	}
}

func TestMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{9, 8, 7}
	if err := writeMessage(&buf, msgPiece, payload); err != nil {
		t.Fatal(err)
	}
	msg, err := readMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg.id != msgPiece || !bytes.Equal(msg.payload, payload) {
		t.Fatalf("got id=%d payload=%v", msg.id, msg.payload)
	}
}

func TestKeepAliveDecodesAsNilMessage(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0})
	msg, err := readMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg != nil {
		t.Fatalf("keep-alive decoded as %#v, want nil", msg)
	}
}

func TestLeecherDownloadsWholeTorrent(t *testing.T) {
	tor := testTorrent(t, 5*65536+1234)
	addr, m := startSeeder(t, tor)

	res, err := NewLeecher(tor).Download(addr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != tor.TotalSize {
		t.Fatalf("downloaded %d bytes, want %d", res.Bytes, tor.TotalSize)
	}
	if res.Duration <= 0 {
		t.Fatal("duration was not measured")
	}
	if res.MBps() <= 0 {
		t.Fatal("throughput was not measured")
	}
	if got := m.BytesServed.Value(); got != tor.TotalSize {
		t.Fatalf("seeder counted %d bytes served, want %d", got, tor.TotalSize)
	}
	if got := m.Requests.Value(); got == 0 {
		t.Fatal("seeder counted no piece requests")
	}
}

func TestSeederServesTheCorrectBytes(t *testing.T) {
	tor := testTorrent(t, 3*65536)
	addr, _ := startSeeder(t, tor)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := writeHandshake(conn, tor.InfoHash(), NewPeerID()); err != nil {
		t.Fatal(err)
	}
	if _, err := readHandshake(conn); err != nil {
		t.Fatal(err)
	}

	bitfield, err := readMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if bitfield.id != msgBitfield {
		t.Fatalf("first message id = %d, want bitfield", bitfield.id)
	}
	for i := 0; i < int(tor.NumPieces()); i++ {
		if bitfield.payload[i/8]&(1<<(7-uint(i%8))) == 0 {
			t.Fatalf("bitfield does not claim piece %d", i)
		}
	}

	if err := writeMessage(conn, msgInterested, nil); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := readMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		if msg != nil && msg.id == msgUnchoke {
			break
		}
	}

	cases := []struct{ index, begin, length int }{
		{0, 0, 16384},
		{1, 16384, 16384},
		{2, 65536 - 4096, 4096},
		{0, 1000, 300},
	}
	for _, tc := range cases {
		req := make([]byte, 12)
		binary.BigEndian.PutUint32(req[0:4], uint32(tc.index))
		binary.BigEndian.PutUint32(req[4:8], uint32(tc.begin))
		binary.BigEndian.PutUint32(req[8:12], uint32(tc.length))
		if err := writeMessage(conn, msgRequest, req); err != nil {
			t.Fatal(err)
		}
		msg, err := readMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		if msg.id != msgPiece {
			t.Fatalf("response id = %d, want piece", msg.id)
		}
		gotIndex := binary.BigEndian.Uint32(msg.payload[0:4])
		gotBegin := binary.BigEndian.Uint32(msg.payload[4:8])
		if int(gotIndex) != tc.index || int(gotBegin) != tc.begin {
			t.Fatalf("got index=%d begin=%d, want %d and %d", gotIndex, gotBegin, tc.index, tc.begin)
		}
		want := tor.PieceData(tc.index)[tc.begin : tc.begin+tc.length]
		if !bytes.Equal(msg.payload[8:], want) {
			t.Fatalf("payload for piece %d at %d does not match the torrent data", tc.index, tc.begin)
		}
	}
}

func TestSeederRejectsWrongInfoHash(t *testing.T) {
	served := testTorrent(t, 65536)
	other := testTorrent(t, 65536)
	addr, _ := startSeeder(t, served)

	if _, err := NewLeecher(other).Download(addr); err == nil {
		t.Fatal("expected an error when the infohash does not match")
	}
}

func TestLeecherReportsDialFailure(t *testing.T) {
	tor := testTorrent(t, 65536)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if _, err := NewLeecher(tor).Download(addr); err == nil {
		t.Fatal("expected an error when nothing is listening")
	}
}

func TestParallelLeechersEachGetEverything(t *testing.T) {
	tor := testTorrent(t, 4*65536)
	addr, m := startSeeder(t, tor)

	const n = 4
	results := make([]Result, n)
	errs := make([]error, n)
	done := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			results[i], errs[i] = NewLeecher(tor).Download(addr)
			done <- i
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("connection %d failed: %v", i, errs[i])
		}
		if results[i].Bytes != tor.TotalSize {
			t.Fatalf("connection %d got %d bytes, want %d", i, results[i].Bytes, tor.TotalSize)
		}
	}
	if got, want := m.BytesServed.Value(), int64(n)*tor.TotalSize; got != want {
		t.Fatalf("seeder counted %d bytes served, want %d", got, want)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.ActiveConns.Value() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.ActiveConns.Value(); got != 0 {
		t.Fatalf("%d connections still counted as open", got)
	}
}

func TestResultMBpsWithoutDuration(t *testing.T) {
	if got := (Result{Bytes: 1000}).MBps(); got != 0 {
		t.Fatalf("MBps = %v, want 0", got)
	}
}
