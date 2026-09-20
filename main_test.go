package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/bencode"
	"github.com/xsaveopt/qbit_benchmark/internal/metainfo"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
	"github.com/xsaveopt/qbit_benchmark/internal/peer"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"0", 0},
		{"512B", 512},
		{"1KiB", 1 << 10},
		{"1MiB", 1 << 20},
		{"1GiB", 1 << 30},
		{"4GiB", 4 << 30},
		{"1KB", 1000},
		{"1MB", 1_000_000},
		{"1GB", 1_000_000_000},
		{"1K", 1 << 10},
		{"1M", 1 << 20},
		{"1G", 1 << 30},
		{"1.5GiB", 1 << 30 * 3 / 2},
		{"0.5MiB", 1 << 19},
		{"  2 MiB  ", 2 << 20},
		{"2mib", 2 << 20},
		{"2gb", 2_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSizeRejectsBadInput(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "MiB", "1.2.3GiB", "1XiB", "GiB1"} {
		t.Run(in, func(t *testing.T) {
			if _, err := parseSize(in); err == nil {
				t.Fatalf("parseSize(%q) returned no error", in)
			}
		})
	}
}

func TestParseSizeKeepsNegativesForTheCallerToReject(t *testing.T) {
	got, err := parseSize("-5MiB")
	if err != nil {
		t.Fatal(err)
	}
	if got != -5<<20 {
		t.Fatalf("parseSize = %d, want %d", got, -5<<20)
	}
	if _, err := buildTorrent("x", "-5MiB", "1MiB"); err == nil {
		t.Fatal("buildTorrent accepted a negative size")
	}
}

func TestBuildTorrent(t *testing.T) {
	tor, err := buildTorrent("bench", "1MiB", "256KiB")
	if err != nil {
		t.Fatal(err)
	}
	if tor.Name != "bench" || tor.TotalSize != 1<<20 || tor.PieceLength != 256<<10 {
		t.Fatalf("unexpected torrent %+v", tor)
	}
	if _, err := buildTorrent("x", "notasize", "1MiB"); err == nil {
		t.Fatal("expected an error for a bad size")
	}
	if _, err := buildTorrent("x", "1MiB", "notasize"); err == nil {
		t.Fatal("expected an error for a bad piece length")
	}
	if _, err := buildTorrent("x", "1MiB", "100KiB"); err == nil {
		t.Fatal("expected an error for a piece length that is not a multiple of 16KiB")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1 << 20, "1.00 MiB"},
		{1 << 30, "1.00 GiB"},
		{1 << 40, "1.00 TiB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := humanBytes(tc.in); got != tc.want {
				t.Fatalf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPortOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":6969", ":6969"},
		{"0.0.0.0:6969", ":6969"},
		{"127.0.0.1:80", ":80"},
		{"nonsense", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := portOf(tc.in); got != tc.want {
				t.Fatalf("portOf(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSeederEndpoint(t *testing.T) {
	listen := &net.TCPAddr{IP: net.IPv4zero, Port: 6881}

	ip, port, err := seederEndpoint("http://127.0.0.1:6969/announce", listen)
	if err != nil {
		t.Fatal(err)
	}
	if !ip.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("ip = %v, want 127.0.0.1", ip)
	}
	if port != 6881 {
		t.Fatalf("port = %d, want 6881", port)
	}
}

func TestSeederEndpointUsesTheActualListenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	_, port, err := seederEndpoint("http://127.0.0.1:6969/announce", ln.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if want := uint16(ln.Addr().(*net.TCPAddr).Port); port != want {
		t.Fatalf("port = %d, want %d", port, want)
	}
	if port == 0 {
		t.Fatal("port 0 was not resolved to the assigned port")
	}
}

func TestSeederEndpointRejectsUnusableAnnounce(t *testing.T) {
	listen := &net.TCPAddr{IP: net.IPv4zero, Port: 6881}
	cases := []struct {
		name     string
		announce string
	}{
		{"no host", "http:///announce"},
		{"empty", ""},
		{"ipv6 literal", "http://[::1]:6969/announce"},
		{"unresolvable host", "http://host.invalid:6969/announce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := seederEndpoint(tc.announce, listen); err == nil {
				t.Fatalf("seederEndpoint(%q) returned no error", tc.announce)
			}
		})
	}
}

func TestSeederEndpointRejectsNonTCPListener(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6881}
	if _, _, err := seederEndpoint("http://127.0.0.1:6969/announce", addr); err == nil {
		t.Fatal("expected an error for a non-TCP listener")
	}
}

const (
	subprocessEnv = "QBIT_BENCHMARK_TEST_SUBPROCESS"
	testAnnounce  = "http://127.0.0.1:6969/announce"
)

func TestMain(m *testing.M) {
	if os.Getenv(subprocessEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), subprocessEnv+"=1")
	cmd.Dir = t.TempDir()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatal(err)
	}
	return outBuf.String(), errBuf.String(), code
}

func newTestTorrent(t *testing.T, totalSize int64) *metainfo.Torrent {
	t.Helper()
	tor, err := metainfo.New("bench", totalSize, 16384)
	if err != nil {
		t.Fatal(err)
	}
	return tor
}

func writeTestTorrent(t *testing.T, tor *metainfo.Torrent) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qbench.torrent")
	if err := tor.WriteFile(path, testAnnounce); err != nil {
		t.Fatal(err)
	}
	return path
}

func startTestSeeder(t *testing.T, tor *metainfo.Torrent) (string, *metrics.App) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	m := metrics.NewApp()
	go func() { _ = peer.NewSeeder(tor, m).Serve(ln) }()
	return ln.Addr().String(), m
}

type handOffListener struct {
	conns  chan net.Conn
	addr   net.Addr
	closed chan struct{}
	once   sync.Once
}

func (l *handOffListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *handOffListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *handOffListener) Addr() net.Addr { return l.addr }

func startOneConnectionSeeder(t *testing.T, tor *metainfo.Torrent) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handOff := &handOffListener{conns: make(chan net.Conn), addr: ln.Addr(), closed: make(chan struct{})}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = handOff.Close()
	})
	go func() { _ = peer.NewSeeder(tor, metrics.NewApp()).Serve(handOff) }()
	go func() {
		served := false
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if served {
				_ = conn.Close()
				continue
			}
			served = true
			select {
			case handOff.conns <- conn:
			case <-handOff.closed:
				_ = conn.Close()
				return
			}
		}
	}()
	return ln.Addr().String()
}

type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type chanWriter struct{ writes chan string }

func (w chanWriter) Write(p []byte) (int, error) {
	w.writes <- string(p)
	return len(p), nil
}

func waitForOutput(t *testing.T, w *syncWriter, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := w.String(); strings.Contains(got, want) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never contained %q, got %q", want, w.String())
	return ""
}

var listenerLine = regexp.MustCompile(`tracker on (\S+), seeder on (\S+), metrics on (\S+)/metrics`)

func startServe(t *testing.T, args ...string) (out, errOut *syncWriter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out, errOut = &syncWriter{}, &syncWriter{}
	returned := make(chan error, 1)
	go func() { returned <- cmdServe(ctx, args, out, errOut) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-returned:
			if err != nil {
				t.Errorf("cmdServe returned %v, want nil after cancellation", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("cmdServe did not return after its context was cancelled")
		}
	})
	return out, errOut
}

func serveListeners(t *testing.T, out *syncWriter) (httpAddr, peerAddr string) {
	t.Helper()
	text := waitForOutput(t, out, "tracker on ")
	m := listenerLine.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("could not find the listener line in %q", text)
	}
	return m[1], m[2]
}

func getBody(t *testing.T, addr, path string) []byte {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func announcePath(infoHash [20]byte, peerID, port, left string) string {
	q := url.Values{}
	q.Set("info_hash", string(infoHash[:]))
	q.Set("peer_id", peerID)
	q.Set("port", port)
	q.Set("left", left)
	return "/announce?" + q.Encode()
}

func decodeAnnounce(t *testing.T, body []byte) (peers []string, complete int64) {
	t.Helper()
	v, err := bencode.Unmarshal(body)
	if err != nil {
		t.Fatal(err)
	}
	dict, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("announce response is %T, want a dict", v)
	}
	complete, _ = dict["complete"].(int64)
	raw, ok := dict["peers"].([]byte)
	if !ok {
		t.Fatalf("announce response has no compact peers: %#v", dict)
	}
	if len(raw)%6 != 0 {
		t.Fatalf("compact peer list is %d bytes, not a multiple of 6", len(raw))
	}
	for i := 0; i < len(raw); i += 6 {
		ip := net.IP(raw[i : i+4])
		port := binary.BigEndian.Uint16(raw[i+4 : i+6])
		peers = append(peers, net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
	}
	return peers, complete
}

func TestMainDispatchesSubcommands(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{"no arguments", nil, 2, "", "usage:"},
		{"version", []string{"version"}, 0, "qbit_benchmark dev", ""},
		{"short version flag", []string{"-v"}, 0, "qbit_benchmark dev", ""},
		{"long version flag", []string{"--version"}, 0, "qbit_benchmark dev", ""},
		{"unknown subcommand", []string{"bogus"}, 2, "", "usage:"},
		{"failing subcommand", []string{"gen", "-size", "notasize"}, 1, "", "error:"},
		{"undefined flag", []string{"gen", "-nosuchflag"}, 2, "", "flag provided but not defined"},
		{"leech without its required flags", []string{"leech"}, 1, "", "leech requires -torrent and -addr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args...)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout, stderr)
			}
			if tc.wantOut != "" && !strings.Contains(stdout, tc.wantOut) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout, tc.wantOut)
			}
			if tc.wantErr != "" && !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
		})
	}
}

func TestUsageNamesEverySubcommand(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	got := buf.String()
	for _, want := range []string{"qbit_benchmark gen", "qbit_benchmark serve", "qbit_benchmark leech", "qbit_benchmark version", "-announce", "-piece"} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, got)
		}
	}
}

func TestPrintTorrent(t *testing.T) {
	tor := newTestTorrent(t, 3*16384)
	ih := tor.InfoHash()
	cases := []struct {
		name  string
		path  string
		want  []string
		avoid []string
	}{
		{
			name: "with a path",
			path: "/tmp/qbench.torrent",
			want: []string{
				"torrent:  /tmp/qbench.torrent\n",
				"name:     bench\n",
				"size:     48.00 KiB (3 pieces of 16.00 KiB)\n",
				"infohash: " + hex.EncodeToString(ih[:]) + "\n",
				"announce: http://127.0.0.1:6969/announce\n",
			},
		},
		{
			name:  "without a path",
			path:  "",
			want:  []string{"name:     bench\n"},
			avoid: []string{"torrent:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printTorrent(&buf, tor, tc.path, "http://127.0.0.1:6969/announce")
			got := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("output %q does not contain %q", got, want)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(got, avoid) {
					t.Fatalf("output %q should not contain %q", got, avoid)
				}
			}
		})
	}
}

func TestCmdGenWritesTheTorrentItDescribes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.torrent")
	var out, errOut bytes.Buffer

	args := []string{"-name", "custom", "-size", "128KiB", "-piece", "32KiB", "-announce", "http://127.0.0.1:7000/announce", "-o", path}
	if err := cmdGen(args, &out, &errOut); err != nil {
		t.Fatal(err)
	}

	tor, announce, err := metainfo.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if tor.Name != "custom" || tor.TotalSize != 128<<10 || tor.PieceLength != 32<<10 {
		t.Fatalf("unexpected torrent %+v", tor)
	}
	if announce != "http://127.0.0.1:7000/announce" {
		t.Fatalf("announce = %q", announce)
	}
	ih := tor.InfoHash()
	for _, want := range []string{
		"torrent:  " + path,
		"name:     custom",
		"size:     128.00 KiB (4 pieces of 32.00 KiB)",
		"infohash: " + hex.EncodeToString(ih[:]),
		"announce: http://127.0.0.1:7000/announce",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not contain %q", out.String(), want)
		}
	}
	if errOut.Len() != 0 {
		t.Fatalf("gen wrote %q to stderr", errOut.String())
	}
}

func TestCmdGenFallsBackToItsDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer

	if err := cmdGen([]string{"-size", "64KiB", "-piece", "16KiB"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}

	tor, announce, err := metainfo.Load("qbench.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if tor.Name != "qbench" {
		t.Fatalf("default name = %q, want qbench", tor.Name)
	}
	if announce != "http://127.0.0.1:6969/announce" {
		t.Fatalf("default announce = %q", announce)
	}
	if !strings.Contains(out.String(), "torrent:  qbench.torrent") {
		t.Fatalf("output %q does not name the default output file", out.String())
	}
}

func TestCmdGenRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"bad size", []string{"-size", "notasize", "-o", filepath.Join(dir, "a.torrent")}},
		{"bad piece length", []string{"-size", "64KiB", "-piece", "3KiB", "-o", filepath.Join(dir, "b.torrent")}},
		{"negative size", []string{"-size", "-64KiB", "-o", filepath.Join(dir, "c.torrent")}},
		{"unwritable output path", []string{"-size", "64KiB", "-piece", "16KiB", "-o", filepath.Join(dir, "missing", "d.torrent")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := cmdGen(tc.args, &out, &errOut); err == nil {
				t.Fatalf("cmdGen(%v) returned no error", tc.args)
			}
			if out.Len() != 0 {
				t.Fatalf("cmdGen described a torrent it did not write: %q", out.String())
			}
		})
	}
}

func TestCmdLeechReportsEveryConnectionAndTheAggregate(t *testing.T) {
	tor := newTestTorrent(t, 4*16384)
	path := writeTestTorrent(t, tor)

	cases := []struct {
		name  string
		args  []string
		conns int64
	}{
		{"one connection", []string{"-n", "1"}, 1},
		{"two connections", []string{"-n", "2"}, 2},
		{"default connection count", nil, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, m := startTestSeeder(t, tor)
			var out, errOut bytes.Buffer

			args := append([]string{"-torrent", path, "-addr", addr}, tc.args...)
			if err := cmdLeech(args, &out, &errOut); err != nil {
				t.Fatal(err)
			}

			got := out.String()
			want := []string{fmt.Sprintf("pulling from %s with %d connections...", addr, tc.conns)}
			for i := int64(0); i < tc.conns; i++ {
				want = append(want, fmt.Sprintf("conn %d: %s in ", i, humanBytes(tor.TotalSize)))
			}
			want = append(want, "aggregate: "+humanBytes(tc.conns*tor.TotalSize)+" in ")
			for _, w := range want {
				if !strings.Contains(got, w) {
					t.Fatalf("output %q does not contain %q", got, w)
				}
			}
			if strings.Contains(got, "connections failed") {
				t.Fatalf("output reported failures: %q", got)
			}
			if !strings.Contains(got, "MB/s)") {
				t.Fatalf("output %q does not report throughput", got)
			}
			if served, want := m.BytesServed.Value(), tc.conns*tor.TotalSize; served != want {
				t.Fatalf("seeder served %d bytes, want %d", served, want)
			}
		})
	}
}

func TestCmdLeechReportsPartialFailure(t *testing.T) {
	tor := newTestTorrent(t, 2*16384)
	path := writeTestTorrent(t, tor)
	addr := startOneConnectionSeeder(t, tor)

	var out, errOut bytes.Buffer
	if err := cmdLeech([]string{"-torrent", path, "-addr", addr, "-n", "2"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		"aggregate: " + humanBytes(tor.TotalSize) + " in ",
		"1 of 2 connections failed; many clients accept only one connection per IP",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q does not contain %q", got, want)
		}
	}
}

func TestCmdLeechFailsWhenEveryConnectionFails(t *testing.T) {
	tor := newTestTorrent(t, 16384)
	path := writeTestTorrent(t, tor)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var out, errOut bytes.Buffer
	err = cmdLeech([]string{"-torrent", path, "-addr", addr, "-n", "2"}, &out, &errOut)
	if err == nil {
		t.Fatal("cmdLeech returned no error when nothing was listening")
	}
	if !strings.Contains(err.Error(), "all 2 connections failed") {
		t.Fatalf("error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "conn 0: ") || strings.Contains(got, "aggregate:") {
		t.Fatalf("output %q should report the failures and no aggregate", got)
	}
}

func TestCmdLeechRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	tor := newTestTorrent(t, 16384)
	path := writeTestTorrent(t, tor)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no flags", nil, "leech requires -torrent and -addr"},
		{"no torrent", []string{"-addr", "127.0.0.1:6881"}, "leech requires -torrent and -addr"},
		{"no address", []string{"-torrent", path}, "leech requires -torrent and -addr"},
		{"zero connections", []string{"-torrent", path, "-addr", "127.0.0.1:6881", "-n", "0"}, "-n must be at least 1"},
		{"negative connections", []string{"-torrent", path, "-addr", "127.0.0.1:6881", "-n", "-3"}, "-n must be at least 1"},
		{"missing torrent file", []string{"-torrent", filepath.Join(dir, "nope.torrent"), "-addr", "127.0.0.1:6881"}, "nope.torrent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := cmdLeech(tc.args, &out, &errOut)
			if err == nil {
				t.Fatalf("cmdLeech(%v) returned no error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("cmdLeech printed %q before failing", out.String())
			}
		})
	}
}

func TestReportProgressWritesOnEveryTick(t *testing.T) {
	var served atomic.Int64
	tick := make(chan time.Time)
	done := make(chan struct{})
	writes := make(chan string, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		reportProgress(chanWriter{writes: writes}, served.Load, tick, done)
	}()

	cases := []struct {
		total int64
		want  string
	}{
		{1024, "\rserved 1.00 KiB, 1.00 KiB/s        "},
		{3 << 10, "\rserved 3.00 KiB, 2.00 KiB/s        "},
		{3 << 10, "\rserved 3.00 KiB, 0 B/s        "},
	}
	for _, tc := range cases {
		served.Store(tc.total)
		tick <- time.Now()
		if got := <-writes; got != tc.want {
			t.Fatalf("progress line = %q, want %q", got, tc.want)
		}
	}

	close(done)
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("reportProgress did not return once done was closed")
	}
}

func TestNewBenchServerAdvertisesTheSeederAndExposesMetrics(t *testing.T) {
	tor := newTestTorrent(t, 2*16384)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	var warnings bytes.Buffer
	srv := newBenchServer(tor, "http://127.0.0.1:6969/announce", ln, &warnings)
	if warnings.Len() != 0 {
		t.Fatalf("unexpected warning %q", warnings.String())
	}
	seedPort := uint16(ln.Addr().(*net.TCPAddr).Port)
	if !srv.seedIP.Equal(net.ParseIP("127.0.0.1")) || srv.seedPort != seedPort {
		t.Fatalf("seeder advertised as %v:%d, want 127.0.0.1:%d", srv.seedIP, srv.seedPort, seedPort)
	}

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics returned %d", rec.Code)
	}
	for _, want := range []string{"qbb_bytes_served_total 0", "qbb_tracker_announces_total 0", "qbb_active_connections 0"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("metrics body does not contain %q:\n%s", want, rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, announcePath(tor.InfoHash(), "-QB5000-leecher01", "6881", "100"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /announce returned %d", rec.Code)
	}
	peers, complete := decodeAnnounce(t, rec.Body.Bytes())
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(seedPort)))
	if len(peers) != 1 || peers[0] != want {
		t.Fatalf("announce returned peers %v, want [%s]", peers, want)
	}
	if complete != 1 {
		t.Fatalf("announce reported %d seeders, want 1", complete)
	}
	if got := srv.metrics.Announces.Value(); got != 1 {
		t.Fatalf("announce counter = %d, want 1", got)
	}
}

func TestNewBenchServerWarnsWhenTheSeederCannotBeAdvertised(t *testing.T) {
	tor := newTestTorrent(t, 16384)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	var warnings bytes.Buffer
	srv := newBenchServer(tor, "http://[::1]:6969/announce", ln, &warnings)
	if !strings.Contains(warnings.String(), "warning: not advertising the seeder on the tracker") {
		t.Fatalf("warning = %q", warnings.String())
	}
	if srv.seedIP != nil || srv.seedPort != 0 {
		t.Fatalf("seeder was advertised as %v:%d", srv.seedIP, srv.seedPort)
	}

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, announcePath(tor.InfoHash(), "-QB5000-leecher01", "6881", "100"), nil))
	peers, complete := decodeAnnounce(t, rec.Body.Bytes())
	if len(peers) != 0 || complete != 0 {
		t.Fatalf("announce handed out %v with %d seeders, want none", peers, complete)
	}
}

func TestCmdServeRunsTheTrackerSeederAndMetrics(t *testing.T) {
	tor := newTestTorrent(t, 4*16384)
	path := writeTestTorrent(t, tor)

	out, errOut := startServe(t,
		"-torrent", path,
		"-http", "127.0.0.1:0",
		"-peer", "127.0.0.1:0",
		"-announce", "http://127.0.0.1:6969/announce",
	)
	httpAddr, peerAddr := serveListeners(t, out)
	if got := errOut.String(); got != "" {
		t.Fatalf("serve warned: %q", got)
	}

	_, peerPort, err := net.SplitHostPort(peerAddr)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"torrent:  " + path,
		"announce: http://127.0.0.1:6969/announce",
		"advertising seeder to the swarm as 127.0.0.1:" + peerPort,
		"add the .torrent to qBittorrent to start the download benchmark",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not contain %q", out.String(), want)
		}
	}

	if body := string(getBody(t, httpAddr, "/metrics")); !strings.Contains(body, "qbb_bytes_served_total 0") {
		t.Fatalf("metrics before the transfer:\n%s", body)
	}

	peers, complete := decodeAnnounce(t, getBody(t, httpAddr, announcePath(tor.InfoHash(), "-QB5000-leecher01", "6881", "100")))
	want := net.JoinHostPort("127.0.0.1", peerPort)
	if len(peers) != 1 || peers[0] != want {
		t.Fatalf("tracker returned peers %v, want [%s]", peers, want)
	}
	if complete != 1 {
		t.Fatalf("tracker reported %d seeders, want 1", complete)
	}

	res, err := peer.NewLeecher(tor).Download(peerAddr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != tor.TotalSize {
		t.Fatalf("leeched %d bytes, want %d", res.Bytes, tor.TotalSize)
	}

	body := string(getBody(t, httpAddr, "/metrics"))
	for _, want := range []string{
		fmt.Sprintf("qbb_bytes_served_total %d", tor.TotalSize),
		"qbb_tracker_announces_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics after the transfer do not contain %q:\n%s", want, body)
		}
	}

	progress := waitForOutput(t, out, "\rserved ")
	if !strings.Contains(progress, "\rserved "+humanBytes(tor.TotalSize)+", ") {
		t.Fatalf("progress line does not report the bytes served: %q", progress)
	}
}

func TestCmdServeGeneratesATorrentWhenNoneIsGiven(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "generated.torrent")

	out, errOut := startServe(t,
		"-name", "generated",
		"-size", "64KiB",
		"-piece", "16KiB",
		"-o", outPath,
		"-http", "127.0.0.1:0",
		"-peer", "127.0.0.1:0",
	)
	httpAddr, _ := serveListeners(t, out)
	if got := errOut.String(); got != "" {
		t.Fatalf("serve warned: %q", got)
	}

	tor, announce, err := metainfo.Load(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if tor.Name != "generated" || tor.TotalSize != 64<<10 {
		t.Fatalf("unexpected torrent %+v", tor)
	}
	if announce != "http://127.0.0.1:0/announce" {
		t.Fatalf("announce = %q, want it derived from -http", announce)
	}
	got := out.String()
	if !strings.Contains(got, "generated "+outPath) {
		t.Fatalf("output %q does not report the generated file", got)
	}
	if strings.Contains(got, "torrent:  ") {
		t.Fatalf("output %q names a torrent path that was never given", got)
	}

	if body := string(getBody(t, httpAddr, "/metrics")); !strings.Contains(body, "qbb_swarm_peers 0") {
		t.Fatalf("metrics body:\n%s", body)
	}
}

func TestCmdServeWarnsWhenTheSeederCannotBeAdvertised(t *testing.T) {
	tor := newTestTorrent(t, 16384)
	path := writeTestTorrent(t, tor)

	out, errOut := startServe(t,
		"-torrent", path,
		"-http", "127.0.0.1:0",
		"-peer", "127.0.0.1:0",
		"-announce", "http://[::1]:6969/announce",
	)
	serveListeners(t, out)

	if got := errOut.String(); !strings.Contains(got, "warning: not advertising the seeder on the tracker") {
		t.Fatalf("stderr = %q", got)
	}
	if got := out.String(); strings.Contains(got, "advertising seeder to the swarm") {
		t.Fatalf("output %q claims the seeder was advertised", got)
	}
}

func TestCmdServeReportsSetupFailures(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"missing torrent", []string{"-torrent", filepath.Join(dir, "nope.torrent"), "-http", "127.0.0.1:0", "-peer", "127.0.0.1:0"}},
		{"bad size", []string{"-size", "notasize", "-o", filepath.Join(dir, "a.torrent"), "-http", "127.0.0.1:0", "-peer", "127.0.0.1:0"}},
		{"unwritable output path", []string{"-size", "64KiB", "-piece", "16KiB", "-o", filepath.Join(dir, "missing", "b.torrent"), "-http", "127.0.0.1:0", "-peer", "127.0.0.1:0"}},
		{"bad peer address", []string{"-size", "64KiB", "-piece", "16KiB", "-o", filepath.Join(dir, "c.torrent"), "-http", "127.0.0.1:0", "-peer", "nonsense"}},
		{"bad tracker address", []string{"-size", "64KiB", "-piece", "16KiB", "-o", filepath.Join(dir, "d.torrent"), "-http", "nonsense", "-peer", "127.0.0.1:0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := cmdServe(context.Background(), tc.args, io.Discard, io.Discard); err == nil {
				t.Fatalf("cmdServe(%v) returned no error", tc.args)
			}
		})
	}
}
