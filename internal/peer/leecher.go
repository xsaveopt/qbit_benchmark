package peer

import (
	"encoding/binary"
	"net"
	"time"

	"github.com/xsaveopt/qbit_benchmark/internal/metainfo"
)

const blockSize = 16 * 1024
const pipelineWindow = 64

type Result struct {
	Bytes    int64
	Duration time.Duration
}

func (r Result) MBps() float64 {
	if r.Duration <= 0 {
		return 0
	}
	return float64(r.Bytes) / 1e6 / r.Duration.Seconds()
}

type Leecher struct {
	t      *metainfo.Torrent
	peerID [20]byte
}

func NewLeecher(t *metainfo.Torrent) *Leecher {
	return &Leecher{t: t, peerID: NewPeerID()}
}

type blockReq struct {
	index, begin, length int
}

func (l *Leecher) Download(addr string) (Result, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = conn.Close() }()

	if err := writeHandshake(conn, l.t.InfoHash(), l.peerID); err != nil {
		return Result{}, err
	}
	if _, err := readHandshake(conn); err != nil {
		return Result{}, err
	}
	if err := writeMessage(conn, msgInterested, nil); err != nil {
		return Result{}, err
	}
	for {
		msg, err := readMessage(conn)
		if err != nil {
			return Result{}, err
		}
		if msg != nil && msg.id == msgUnchoke {
			break
		}
	}

	blocks := l.plan()
	start := time.Now()
	var got int64
	sendIdx := 0

	send := func() error {
		b := blocks[sendIdx]
		sendIdx++
		payload := make([]byte, 12)
		binary.BigEndian.PutUint32(payload[0:4], uint32(b.index))
		binary.BigEndian.PutUint32(payload[4:8], uint32(b.begin))
		binary.BigEndian.PutUint32(payload[8:12], uint32(b.length))
		return writeMessage(conn, msgRequest, payload)
	}

	for sendIdx < len(blocks) && sendIdx < pipelineWindow {
		if err := send(); err != nil {
			return Result{}, err
		}
	}

	for received := 0; received < len(blocks); {
		msg, err := readMessage(conn)
		if err != nil {
			return Result{}, err
		}
		if msg == nil || msg.id != msgPiece {
			continue
		}
		got += int64(len(msg.payload) - 8)
		received++
		if sendIdx < len(blocks) {
			if err := send(); err != nil {
				return Result{}, err
			}
		}
	}

	return Result{Bytes: got, Duration: time.Since(start)}, nil
}

func (l *Leecher) plan() []blockReq {
	var blocks []blockReq
	for i := 0; i < int(l.t.NumPieces()); i++ {
		ps := l.t.PieceSize(i)
		for begin := 0; begin < ps; begin += blockSize {
			length := blockSize
			if begin+length > ps {
				length = ps - begin
			}
			blocks = append(blocks, blockReq{index: i, begin: begin, length: length})
		}
	}
	return blocks
}
