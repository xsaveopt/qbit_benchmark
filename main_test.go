package main

import (
	"net"
	"testing"
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
