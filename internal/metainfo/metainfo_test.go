package metainfo

import (
	"bytes"
	"crypto/sha1"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/xsaveopt/qbit_benchmark/internal/bencode"
)

func TestNewRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name      string
		totalSize int64
		pieceLen  int64
	}{
		{"zero size", 0, 16384},
		{"negative size", -1, 16384},
		{"zero piece", 1 << 20, 0},
		{"negative piece", 1 << 20, -16384},
		{"piece not multiple of 16KiB", 1 << 20, 100000},
		{"piece smaller than 16KiB", 1 << 20, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New("x", tc.totalSize, tc.pieceLen); err == nil {
				t.Fatalf("expected an error for size=%d piece=%d", tc.totalSize, tc.pieceLen)
			}
		})
	}
}

func TestNumPiecesAndPieceSize(t *testing.T) {
	tor, err := New("x", 3*(1<<20)+1234, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := tor.NumPieces(); got != 4 {
		t.Fatalf("NumPieces = %d, want 4", got)
	}
	for i := 0; i < 3; i++ {
		if got := tor.PieceSize(i); got != 1<<20 {
			t.Fatalf("PieceSize(%d) = %d, want %d", i, got, 1<<20)
		}
	}
	if got := tor.PieceSize(3); got != 1234 {
		t.Fatalf("PieceSize(3) = %d, want 1234", got)
	}
}

func TestExactMultipleHasFullFinalPiece(t *testing.T) {
	tor, err := New("x", 2*(1<<20), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := tor.NumPieces(); got != 2 {
		t.Fatalf("NumPieces = %d, want 2", got)
	}
	if got := tor.PieceSize(1); got != 1<<20 {
		t.Fatalf("PieceSize(1) = %d, want %d", got, 1<<20)
	}
}

func TestBlockMatchesPieceData(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, pieceLen := range []int64{16384, 65536, 1 << 20} {
		tor, err := New("x", pieceLen*3+12345, pieceLen)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < int(tor.NumPieces()); i++ {
			full := tor.PieceData(i)
			size := len(full)
			for k := 0; k < 200; k++ {
				begin := rng.Intn(size)
				length := rng.Intn(size-begin) + 1
				end := begin + length
				if end > size {
					end = size
				}
				if got := tor.Block(i, begin, length); !bytes.Equal(got, full[begin:end]) {
					t.Fatalf("piece=%d begin=%d length=%d does not match PieceData", i, begin, length)
				}
			}
		}
	}
}

func TestBlockClampsPastEndOfPiece(t *testing.T) {
	tor, err := New("x", 1<<20+500, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := tor.Block(1, 100, 16384)
	if len(got) != 400 {
		t.Fatalf("len = %d, want 400", len(got))
	}
	if !bytes.Equal(got, tor.PieceData(1)[100:500]) {
		t.Fatal("clamped block does not match PieceData")
	}
}

func TestBlockOutOfRange(t *testing.T) {
	tor, err := New("x", 2*(1<<20), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name                 string
		index, begin, length int
	}{
		{"negative index", -1, 0, 16384},
		{"index past end", 2, 0, 16384},
		{"negative begin", 0, -1, 16384},
		{"begin at piece end", 0, 1 << 20, 16384},
		{"zero length", 0, 0, 0},
		{"negative length", 0, 0, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tor.Block(tc.index, tc.begin, tc.length); got != nil {
				t.Fatalf("got %d bytes, want nil", len(got))
			}
		})
	}
}

func TestPieceHashesCoverPieceData(t *testing.T) {
	tor, err := New("x", 5*65536+77, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if len(tor.pieces) != int(tor.NumPieces())*20 {
		t.Fatalf("pieces length = %d, want %d", len(tor.pieces), tor.NumPieces()*20)
	}
	for i := 0; i < int(tor.NumPieces()); i++ {
		want := sha1.Sum(tor.PieceData(i))
		if !bytes.Equal(tor.pieces[i*20:i*20+20], want[:]) {
			t.Fatalf("hash for piece %d does not match its data", i)
		}
	}
}

func TestWriteFileLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qbench.torrent")
	announce := "http://192.0.2.10:6969/announce"
	orig, err := New("bench", 3*65536+11, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if err := orig.WriteFile(path, announce); err != nil {
		t.Fatal(err)
	}

	loaded, gotAnnounce, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotAnnounce != announce {
		t.Fatalf("announce = %q, want %q", gotAnnounce, announce)
	}
	if loaded.Name != orig.Name || loaded.TotalSize != orig.TotalSize || loaded.PieceLength != orig.PieceLength {
		t.Fatalf("metadata mismatch: %+v", loaded)
	}
	if loaded.Seed != orig.Seed {
		t.Fatal("seed did not survive the round trip")
	}
	if loaded.InfoHash() != orig.InfoHash() {
		t.Fatal("infohash differs after reload")
	}
	for i := 0; i < int(orig.NumPieces()); i++ {
		if !bytes.Equal(loaded.PieceData(i), orig.PieceData(i)) {
			t.Fatalf("piece %d differs after reload", i)
		}
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if _, _, err := Load(filepath.Join(dir, "absent.torrent")); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("not bencode", func(t *testing.T) {
		path := filepath.Join(dir, "junk.torrent")
		writeFile(t, path, []byte("this is not bencode"))
		if _, _, err := Load(path); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("foreign torrent without seed", func(t *testing.T) {
		raw, err := bencode.Marshal(map[string]any{
			"announce": "http://example.invalid/announce",
			"info": map[string]any{
				"name":         "other",
				"piece length": int64(65536),
				"length":       int64(65536),
				"pieces":       make([]byte, 20),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "foreign.torrent")
		writeFile(t, path, raw)
		if _, _, err := Load(path); err == nil {
			t.Fatal("expected an error for a torrent this tool did not generate")
		}
	})

	t.Run("seed of wrong length", func(t *testing.T) {
		raw, err := bencode.Marshal(map[string]any{
			"info": map[string]any{
				"name":         "short",
				"piece length": int64(65536),
				"length":       int64(65536),
				"pieces":       make([]byte, 20),
				seedKey:        make([]byte, 4),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "shortseed.torrent")
		writeFile(t, path, raw)
		if _, _, err := Load(path); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestDistinctTorrentsGetDistinctData(t *testing.T) {
	a, err := New("x", 65536, 65536)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("x", 65536, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if a.InfoHash() == b.InfoHash() {
		t.Fatal("two torrents ended up with the same infohash")
	}
	if bytes.Equal(a.PieceData(0), b.PieceData(0)) {
		t.Fatal("two torrents ended up with the same payload")
	}
}

func TestPiecesWithinATorrentDiffer(t *testing.T) {
	tor, err := New("x", 4*65536, 65536)
	if err != nil {
		t.Fatal(err)
	}
	first := tor.PieceData(0)
	for i := 1; i < 4; i++ {
		if bytes.Equal(first, tor.PieceData(i)) {
			t.Fatalf("piece %d repeats piece 0", i)
		}
	}
	chunkA := tor.Block(0, 0, chunkSize)
	chunkB := tor.Block(0, chunkSize, chunkSize)
	if bytes.Equal(chunkA, chunkB) {
		t.Fatal("consecutive chunks within a piece repeat")
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
