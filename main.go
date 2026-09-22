package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/metainfo"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
	"github.com/xsaveopt/qbit_benchmark/internal/peer"
	"github.com/xsaveopt/qbit_benchmark/internal/tracker"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:], os.Stdout, os.Stderr)
	case "serve":
		err = cmdServe(context.Background(), os.Args[2:], os.Stdout, os.Stderr)
	case "leech":
		err = cmdLeech(os.Args[2:], os.Stdout, os.Stderr)
	case "healthcheck":
		err = cmdHealthcheck(os.Args[2:], os.Stderr)
	case "version", "-v", "--version":
		fmt.Println("qbit_benchmark", version)
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `qbit_benchmark - generate test torrents and benchmark a qBittorrent client

usage:
  qbit_benchmark gen    -size 1GiB -piece 256KiB -announce http://HOST:6969/announce -o qbench.torrent
  qbit_benchmark serve  -torrent qbench.torrent -http :6969 -peer :6881
  qbit_benchmark leech  -torrent qbench.torrent -addr HOST:PORT -n 4
  qbit_benchmark healthcheck -addr 127.0.0.1:6969
  qbit_benchmark version

serve and healthcheck both default -http/-addr from the QBIT_HTTP_ADDR env var when set.`)
}

func cmdGen(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	fs.SetOutput(errOut)
	name := fs.String("name", "qbench", "torrent name")
	size := fs.String("size", "1GiB", "total size (e.g. 512MiB, 4GiB)")
	piece := fs.String("piece", "256KiB", "piece length (multiple of 16KiB)")
	announce := fs.String("announce", "http://127.0.0.1:6969/announce", "tracker announce URL")
	outPath := fs.String("o", "qbench.torrent", "output .torrent path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, err := buildTorrent(*name, *size, *piece)
	if err != nil {
		return err
	}
	if err := t.WriteFile(*outPath, *announce); err != nil {
		return err
	}
	printTorrent(out, t, *outPath, *announce)
	return nil
}

func cmdServe(ctx context.Context, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.SetOutput(errOut)
	torrentPath := fs.String("torrent", "", "existing .torrent to seed (else one is generated)")
	name := fs.String("name", "qbench", "torrent name when generating")
	size := fs.String("size", "1GiB", "total size when generating")
	piece := fs.String("piece", "256KiB", "piece length when generating")
	outPath := fs.String("o", "qbench.torrent", "where to write a generated .torrent")
	httpAddr := fs.String("http", httpAddrDefault(), "tracker HTTP listen address (env QBIT_HTTP_ADDR)")
	peerAddr := fs.String("peer", ":6881", "seeder TCP listen address")
	announce := fs.String("announce", "", "tracker announce URL to embed (default derived from -http)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ann := *announce
	if ann == "" {
		ann = "http://127.0.0.1" + portOf(*httpAddr) + "/announce"
	}

	var t *metainfo.Torrent
	if *torrentPath != "" {
		loaded, _, err := metainfo.Load(*torrentPath)
		if err != nil {
			return err
		}
		t = loaded
	} else {
		built, err := buildTorrent(*name, *size, *piece)
		if err != nil {
			return err
		}
		if err := built.WriteFile(*outPath, ann); err != nil {
			return err
		}
		t = built
		_, _ = fmt.Fprintf(out, "generated %s\n", *outPath)
	}

	peerLn, err := net.Listen("tcp", *peerAddr)
	if err != nil {
		return err
	}
	defer func() { _ = peerLn.Close() }()

	httpLn, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		return err
	}

	srv := newBenchServer(t, ann, peerLn, errOut)

	printTorrent(out, t, *torrentPath, ann)
	_, _ = fmt.Fprintf(out, "tracker on %s, seeder on %s, metrics on %s/metrics\n", httpLn.Addr(), peerLn.Addr(), httpLn.Addr())
	if srv.seedIP != nil {
		_, _ = fmt.Fprintf(out, "advertising seeder to the swarm as %s\n", net.JoinHostPort(srv.seedIP.String(), strconv.Itoa(int(srv.seedPort))))
	}
	_, _ = fmt.Fprintln(out, "add the .torrent to qBittorrent to start the download benchmark")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	var progress sync.WaitGroup
	progress.Add(1)
	go func() {
		defer progress.Done()
		reportProgress(out, srv.metrics.BytesServed.Value, ticker.C, done)
	}()
	defer progress.Wait()
	defer close(done)

	go func() {
		_ = srv.seeder.Serve(peerLn)
		srv.seederUp.Store(false)
	}()

	hs := &http.Server{Handler: srv.mux, ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() { _ = hs.Close() })
	defer stop()
	if err := hs.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type benchServer struct {
	metrics  *metrics.App
	seeder   *peer.Seeder
	mux      *http.ServeMux
	seedIP   net.IP
	seedPort uint16
	seederUp atomic.Bool
}

func newBenchServer(t *metainfo.Torrent, announce string, peerLn net.Listener, errOut io.Writer) *benchServer {
	m := metrics.NewApp()
	tr := tracker.New(m)
	srv := &benchServer{metrics: m, seeder: peer.NewSeeder(t, m), mux: http.NewServeMux()}
	srv.seederUp.Store(true)

	ip, port, err := seederEndpoint(announce, peerLn.Addr())
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: not advertising the seeder on the tracker: %v\n", err)
	} else {
		tr.AddSeeder(t.InfoHash(), ip, port)
		srv.seedIP, srv.seedPort = ip, port
	}

	srv.mux.HandleFunc("/announce", tr.Announce)
	srv.mux.Handle("/metrics", m.Handler())
	srv.mux.HandleFunc("/health", srv.handleHealth)
	return srv
}

func (srv *benchServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !srv.seederUp.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "degraded")
		return
	}
	_, _ = io.WriteString(w, "up")
}

func reportProgress(w io.Writer, served func() int64, tick <-chan time.Time, done <-chan struct{}) {
	var last int64
	for {
		select {
		case <-done:
			return
		case <-tick:
			cur := served()
			_, _ = fmt.Fprintf(w, "\rserved %s, %s/s        ", humanBytes(cur), humanBytes(cur-last))
			last = cur
		}
	}
}

func cmdLeech(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("leech", flag.ExitOnError)
	fs.SetOutput(errOut)
	torrentPath := fs.String("torrent", "", "the .torrent the target is seeding (required)")
	addr := fs.String("addr", "", "target peer host:port to pull from (required)")
	n := fs.Int("n", 4, "number of parallel connections")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *torrentPath == "" || *addr == "" {
		return errors.New("leech requires -torrent and -addr")
	}
	if *n < 1 {
		return fmt.Errorf("leech: -n must be at least 1, got %d", *n)
	}
	t, _, err := metainfo.Load(*torrentPath)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "pulling from %s with %d connections...\n", *addr, *n)
	results := make([]peer.Result, *n)
	errs := make([]error, *n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = peer.NewLeecher(t).Download(*addr)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var total int64
	failed := 0
	var firstErr error
	for i := range results {
		if errs[i] != nil {
			failed++
			if firstErr == nil {
				firstErr = errs[i]
			}
			_, _ = fmt.Fprintf(out, "conn %d: %v\n", i, errs[i])
			continue
		}
		total += results[i].Bytes
		_, _ = fmt.Fprintf(out, "conn %d: %s in %s (%.1f MB/s)\n", i, humanBytes(results[i].Bytes), results[i].Duration.Round(time.Millisecond), results[i].MBps())
	}
	if failed == len(results) {
		return fmt.Errorf("all %d connections failed: %w", failed, firstErr)
	}
	agg := float64(total) / 1e6 / elapsed.Seconds()
	_, _ = fmt.Fprintf(out, "aggregate: %s in %s (%.1f MB/s)\n", humanBytes(total), elapsed.Round(time.Millisecond), agg)
	if failed > 0 {
		_, _ = fmt.Fprintf(out, "%d of %d connections failed; many clients accept only one connection per IP\n", failed, len(results))
	}
	return nil
}

func httpAddrDefault() string {
	if v := os.Getenv("QBIT_HTTP_ADDR"); v != "" {
		return v
	}
	return ":6969"
}

func cmdHealthcheck(args []string, errOut io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	fs.SetOutput(errOut)
	addr := fs.String("addr", "127.0.0.1"+portOf(httpAddrDefault()), "tracker HTTP address to probe (env QBIT_HTTP_ADDR)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + *addr + "/health")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

func buildTorrent(name, size, piece string) (*metainfo.Torrent, error) {
	total, err := parseSize(size)
	if err != nil {
		return nil, fmt.Errorf("size: %w", err)
	}
	pieceLen, err := parseSize(piece)
	if err != nil {
		return nil, fmt.Errorf("piece: %w", err)
	}
	return metainfo.New(name, total, pieceLen)
}

func printTorrent(w io.Writer, t *metainfo.Torrent, path, announce string) {
	ih := t.InfoHash()
	if path != "" {
		_, _ = fmt.Fprintf(w, "torrent:  %s\n", path)
	}
	_, _ = fmt.Fprintf(w, "name:     %s\n", t.Name)
	_, _ = fmt.Fprintf(w, "size:     %s (%d pieces of %s)\n", humanBytes(t.TotalSize), t.NumPieces(), humanBytes(t.PieceLength))
	_, _ = fmt.Fprintf(w, "infohash: %s\n", hex.EncodeToString(ih[:]))
	_, _ = fmt.Fprintf(w, "announce: %s\n", announce)
}

func seederEndpoint(announce string, listen net.Addr) (net.IP, uint16, error) {
	u, err := url.Parse(announce)
	if err != nil {
		return nil, 0, fmt.Errorf("announce URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return nil, 0, fmt.Errorf("announce URL %q has no host", announce)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving %q: %w", host, err)
	}
	var ip net.IP
	for _, candidate := range ips {
		if v4 := candidate.To4(); v4 != nil {
			ip = v4
			break
		}
	}
	if ip == nil {
		return nil, 0, fmt.Errorf("%q has no IPv4 address", host)
	}
	tcp, ok := listen.(*net.TCPAddr)
	if !ok {
		return nil, 0, errors.New("seeder is not listening on TCP")
	}
	return ip, uint16(tcp.Port), nil
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return ""
	}
	return ":" + port
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, errors.New("empty")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, err
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
