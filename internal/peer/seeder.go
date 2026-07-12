package peer

import (
	"encoding/binary"
	"net"

	"github.com/xsaveopt/qbit_benchmark/internal/metainfo"
	"github.com/xsaveopt/qbit_benchmark/internal/metrics"
)

type Seeder struct {
	t      *metainfo.Torrent
	peerID [20]byte
	m      *metrics.App
}

func NewSeeder(t *metainfo.Torrent, m *metrics.App) *Seeder {
	return &Seeder{t: t, peerID: NewPeerID(), m: m}
}

func (s *Seeder) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

func (s *Seeder) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	infoHash, err := readHandshake(conn)
	if err != nil || infoHash != s.t.InfoHash() {
		return
	}
	s.m.ActiveConns.Inc()
	defer s.m.ActiveConns.Dec()
	if err := writeHandshake(conn, s.t.InfoHash(), s.peerID); err != nil {
		return
	}
	if err := writeMessage(conn, msgBitfield, s.bitfield()); err != nil {
		return
	}
	for {
		msg, err := readMessage(conn)
		if err != nil {
			return
		}
		if msg == nil {
			continue
		}
		switch msg.id {
		case msgInterested:
			if err := writeMessage(conn, msgUnchoke, nil); err != nil {
				return
			}
		case msgRequest:
			if len(msg.payload) < 12 {
				continue
			}
			s.m.Requests.Inc()
			index := int(binary.BigEndian.Uint32(msg.payload[0:4]))
			begin := int(binary.BigEndian.Uint32(msg.payload[4:8]))
			length := int(binary.BigEndian.Uint32(msg.payload[8:12]))
			block := s.t.Block(index, begin, length)
			resp := make([]byte, 8+len(block))
			binary.BigEndian.PutUint32(resp[0:4], uint32(index))
			binary.BigEndian.PutUint32(resp[4:8], uint32(begin))
			copy(resp[8:], block)
			if err := writeMessage(conn, msgPiece, resp); err != nil {
				return
			}
			s.m.BytesServed.Add(int64(len(block)))
			s.m.PiecesServed.Inc()
		}
	}
}

func (s *Seeder) bitfield() []byte {
	n := int(s.t.NumPieces())
	bf := make([]byte, (n+7)/8)
	for i := 0; i < n; i++ {
		bf[i/8] |= 1 << (7 - uint(i%8))
	}
	return bf
}
