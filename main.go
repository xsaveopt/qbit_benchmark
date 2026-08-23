package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/metainfo"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
	"github.com/xsaveopt/qbit_benchmark/internal/peer"
	"github.com/xsaveopt/qbit_benchmark/internal/tracker"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "leech":
		err = cmdLeech(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("qbit_benchmark", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `qbit_benchmark - generate test torrents and benchmark a qBittorrent client

usage:
  qbit_benchmark gen    -size 1GiB -piece 256KiB -announce http://HOST:6969/announce -o qbench.torrent
  qbit_benchmark serve  -torrent qbench.torrent -http :6969 -peer :6881
  qbit_benchmark leech  -torrent qbench.torrent -addr HOST:PORT -n 4
  qbit_benchmark version`)
}

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	name := fs.String("name", "qbench", "torrent name")
	size := fs.String("size", "1GiB", "total size (e.g. 512MiB, 4GiB)")
	piece := fs.String("piece", "256KiB", "piece length (multiple of 16KiB)")
	announce := fs.String("announce", "http://127.0.0.1:6969/announce", "tracker announce URL")
	out := fs.String("o", "qbench.torrent", "output .torrent path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, err := buildTorrent(*name, *size, *piece)
	if err != nil {
		return err
	}
	if err := t.WriteFile(*out, *announce); err != nil {
		return err
	}
	printTorrent(t, *out, *announce)
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	torrentPath := fs.String("torrent", "", "existing .torrent to seed (else one is generated)")
	name := fs.String("name", "qbench", "torrent name when generating")
	size := fs.String("size", "1GiB", "total size when generating")
	piece := fs.String("piece", "256KiB", "piece length when generating")
	out := fs.String("o", "qbench.torrent", "where to write a generated .torrent")
	httpAddr := fs.String("http", ":6969", "tracker HTTP listen address")
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
		if err := built.WriteFile(*out, ann); err != nil {
			return err
		}
		t = built
		fmt.Printf("generated %s\n", *out)
	}

	ln, err := net.Listen("tcp", *peerAddr)
	if err != nil {
		return err
	}
	m := metrics.NewApp()
	seeder := peer.NewSeeder(t, m)
	tr := tracker.New(m)

	ip, seedPort, err := seederEndpoint(ann, ln.Addr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: not advertising the seeder on the tracker: %v\n", err)
	} else {
		tr.AddSeeder(t.InfoHash(), ip, seedPort)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/announce", tr.Announce)
	mux.Handle("/metrics", m.Handler())

	printTorrent(t, *torrentPath, ann)
	fmt.Printf("tracker on %s, seeder on %s, metrics on %s/metrics\n", *httpAddr, *peerAddr, *httpAddr)
	if ip != nil {
		fmt.Printf("advertising seeder to the swarm as %s\n", net.JoinHostPort(ip.String(), strconv.Itoa(int(seedPort))))
	}
	fmt.Println("add the .torrent to qBittorrent to start the download benchmark")

	go func() {
		var last int64
		for range time.Tick(time.Second) {
			cur := m.BytesServed.Value()
			fmt.Printf("\rserved %s, %s/s        ", humanBytes(cur), humanBytes(cur-last))
			last = cur
		}
	}()
	go func() { _ = seeder.Serve(ln) }()

	return http.ListenAndServe(*httpAddr, mux)
}

func cmdLeech(args []string) error {
	fs := flag.NewFlagSet("leech", flag.ExitOnError)
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

	fmt.Printf("pulling from %s with %d connections...\n", *addr, *n)
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
			fmt.Printf("conn %d: %v\n", i, errs[i])
			continue
		}
		total += results[i].Bytes
		fmt.Printf("conn %d: %s in %s (%.1f MB/s)\n", i, humanBytes(results[i].Bytes), results[i].Duration.Round(time.Millisecond), results[i].MBps())
	}
	if failed == len(results) {
		return fmt.Errorf("all %d connections failed: %w", failed, firstErr)
	}
	agg := float64(total) / 1e6 / elapsed.Seconds()
	fmt.Printf("aggregate: %s in %s (%.1f MB/s)\n", humanBytes(total), elapsed.Round(time.Millisecond), agg)
	if failed > 0 {
		fmt.Printf("%d of %d connections failed; many clients accept only one connection per IP\n", failed, len(results))
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

func printTorrent(t *metainfo.Torrent, path, announce string) {
	ih := t.InfoHash()
	if path != "" {
		fmt.Printf("torrent:  %s\n", path)
	}
	fmt.Printf("name:     %s\n", t.Name)
	fmt.Printf("size:     %s (%d pieces of %s)\n", humanBytes(t.TotalSize), t.NumPieces(), humanBytes(t.PieceLength))
	fmt.Printf("infohash: %s\n", hex.EncodeToString(ih[:]))
	fmt.Printf("announce: %s\n", announce)
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
