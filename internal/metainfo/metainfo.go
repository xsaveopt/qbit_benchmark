package metainfo

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand"
	"os"
	"runtime"
	"sync"

	"github.com/xsaveopt/qbit_benchmark/internal/bencode"
)

const seedKey = "x_qbb_seed"

type Torrent struct {
	Name        string
	PieceLength int64
	TotalSize   int64
	Seed        [16]byte

	pieces   []byte
	infoHash [20]byte
}

func New(name string, totalSize, pieceLength int64) (*Torrent, error) {
	if totalSize <= 0 {
		return nil, errors.New("metainfo: total size must be positive")
	}
	if pieceLength <= 0 || pieceLength%16384 != 0 {
		return nil, errors.New("metainfo: piece length must be a positive multiple of 16384")
	}
	t := &Torrent{Name: name, TotalSize: totalSize, PieceLength: pieceLength}
	if _, err := rand.Read(t.Seed[:]); err != nil {
		return nil, err
	}
	t.computePieces()
	if err := t.computeInfoHash(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Torrent) NumPieces() int64 {
	return (t.TotalSize + t.PieceLength - 1) / t.PieceLength
}

func (t *Torrent) PieceSize(index int) int {
	if int64(index) == t.NumPieces()-1 {
		if rem := t.TotalSize % t.PieceLength; rem != 0 {
			return int(rem)
		}
	}
	return int(t.PieceLength)
}

func (t *Torrent) InfoHash() [20]byte { return t.infoHash }

func (t *Torrent) PieceData(index int) []byte {
	buf := make([]byte, t.PieceSize(index))
	a := binary.LittleEndian.Uint64(t.Seed[0:8])
	b := binary.LittleEndian.Uint64(t.Seed[8:16])
	src := mrand.NewSource(int64(a ^ b ^ uint64(index)*0x9E3779B97F4A7C15))
	_, _ = mrand.New(src).Read(buf)
	return buf
}

func (t *Torrent) Block(index, begin, length int) []byte {
	data := t.PieceData(index)
	if begin < 0 || begin >= len(data) {
		return nil
	}
	end := begin + length
	if end > len(data) {
		end = len(data)
	}
	return data[begin:end]
}

func (t *Torrent) computePieces() {
	n := int(t.NumPieces())
	t.pieces = make([]byte, n*20)
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			h := sha1.Sum(t.PieceData(idx))
			copy(t.pieces[idx*20:], h[:])
		}(i)
	}
	wg.Wait()
}

func (t *Torrent) infoDict() map[string]any {
	return map[string]any{
		"name":         t.Name,
		"piece length": t.PieceLength,
		"length":       t.TotalSize,
		"pieces":       t.pieces,
		seedKey:        t.Seed[:],
	}
}

func (t *Torrent) computeInfoHash() error {
	b, err := bencode.Marshal(t.infoDict())
	if err != nil {
		return err
	}
	t.infoHash = sha1.Sum(b)
	return nil
}

func (t *Torrent) Marshal(announce string) ([]byte, error) {
	return bencode.Marshal(map[string]any{
		"announce": announce,
		"info":     t.infoDict(),
	})
}

func (t *Torrent) WriteFile(path, announce string) error {
	b, err := t.Marshal(announce)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func Load(path string) (t *Torrent, announce string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	v, err := bencode.Unmarshal(raw)
	if err != nil {
		return nil, "", err
	}
	top, ok := v.(map[string]any)
	if !ok {
		return nil, "", errors.New("metainfo: top level is not a dict")
	}
	info, ok := top["info"].(map[string]any)
	if !ok {
		return nil, "", errors.New("metainfo: missing info dict")
	}
	seed, ok := info[seedKey].([]byte)
	if !ok || len(seed) != 16 {
		return nil, "", fmt.Errorf("metainfo: not a qbit_benchmark torrent (missing %s)", seedKey)
	}
	name, _ := info["name"].([]byte)
	pieceLen, ok1 := info["piece length"].(int64)
	length, ok2 := info["length"].(int64)
	pieces, ok3 := info["pieces"].([]byte)
	if !ok1 || !ok2 || !ok3 {
		return nil, "", errors.New("metainfo: malformed info dict")
	}
	out := &Torrent{
		Name:        string(name),
		PieceLength: pieceLen,
		TotalSize:   length,
		pieces:      pieces,
	}
	copy(out.Seed[:], seed)
	if err := out.computeInfoHash(); err != nil {
		return nil, "", err
	}
	ann, _ := top["announce"].([]byte)
	return out, string(ann), nil
}
