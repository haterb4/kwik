package transport

import (
	"bytes"
	"encoding/binary"
	"sync"
	"time"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type Packer struct {
	mu            sync.Mutex
	queues        map[protocol.PathID][]*protocol.Frame
	maxPacketSize int
	logger        logger.Logger
	// packet sequencing and pending tracking
	seqMu         sync.Mutex
	nextPacketSeq map[protocol.PathID]uint64
	pending       map[protocol.PathID]map[uint64]*pendingPacket // pending packets by pathID -> seq -> packet
	// registered paths
	paths map[protocol.PathID]Path
	// retransmit policy
	maxAttempts   int
	baseBackoffMs int
	maxBackoffMs  int
	// metrics
	metricsMu    sync.Mutex
	sentCount    int64
	resentCount  int64
	ackedCount   int64
	droppedCount int64
}

func NewPacker(maxPacketSize int) *Packer {
	return &Packer{
		queues:        make(map[protocol.PathID][]*protocol.Frame),
		maxPacketSize: maxPacketSize,
		logger:        logger.NewLogger(logger.LogLevelDebug).WithComponent("PACKER"),
		nextPacketSeq: make(map[protocol.PathID]uint64),
		pending:       make(map[protocol.PathID]map[uint64]*pendingPacket),
		paths:         make(map[protocol.PathID]Path),
		// sensible defaults
		maxAttempts:   5,
		baseBackoffMs: 100,
		maxBackoffMs:  2000,
	}
}

var defaultPacker *Packer

func SetDefaultPacker(p *Packer) { defaultPacker = p }
func GetDefaultPacker() *Packer  { return defaultPacker }

// SubmitFrame queues a frame for the given path and attempts to assemble a packet
// containing as many whole frames as possible (without splitting frames) up to
// maxPacketSize. The assembled packet is written to the quic stream corresponding
// to the first frame's StreamID.
func (p *Packer) SubmitFrame(path Path, f *protocol.Frame) error {
	pid := path.PathID()
	p.mu.Lock()
	p.queues[pid] = append(p.queues[pid], f)
	frames := p.queues[pid]
	var buf bytes.Buffer
	var assembled []*protocol.Frame
	size := 0
	for _, fr := range frames {
		// encoded size: 1 + 8 + 8 + 4 + payload
		encSize := 1 + 8 + 8 + 4 + len(fr.Payload)
		if encSize > p.maxPacketSize {
			p.mu.Unlock()
			return protocol.NewFrameTooLargeError(encSize, p.maxPacketSize)
		}
		if size+encSize > p.maxPacketSize {
			break
		}
		if err := protocol.EncodeFrame(&buf, fr); err != nil {
			p.mu.Unlock()
			return err
		}
		size += encSize
		assembled = append(assembled, fr)
	}
	if len(assembled) == 0 {
		p.mu.Unlock()
		return nil
	}
	// remove assembled frames from queue
	p.queues[pid] = frames[len(assembled):]
	p.mu.Unlock()

	// write length-prefixed packet body to path. use first frame's StreamID as carrier
	packet := buf.Bytes()
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(packet)))
	out := append(lenBuf[:], packet...)

	// ensure the target quic stream exists on the path
	carrierStreamID := assembled[0].StreamID
	// try to open the stream if needed; ignore errors that indicate it exists
	_ = path.OpenStream(carrierStreamID)

	// write
	// assign packet seq
	p.seqMu.Lock()
	seq := p.nextPacketSeq[pid] + 1
	p.nextPacketSeq[pid] = seq
	p.seqMu.Unlock()

	// store pending packet
	p.mu.Lock()
	if _, ok := p.pending[pid]; !ok {
		p.pending[pid] = make(map[uint64]*pendingPacket)
	}
	// store packet body (without 4-byte length prefix) but we will send len prefix + seq
	packetBody := packet
	p.pending[pid][seq] = &pendingPacket{body: packetBody, carrier: carrierStreamID}
	p.mu.Unlock()

	// final wire layout: 8-byte packetSeq + 4-byte len + packetBody
	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], seq)
	outWithSeq := append(seqBuf[:], out...)

	_, err := path.WriteStream(carrierStreamID, outWithSeq)
	if err != nil {
		p.logger.Error("failed to write packet", "path", pid, "err", err)
		return err
	}
	return nil
}

// OnAck processes acknowledgement ranges for a path and removes pending packets.
func (p *Packer) OnAck(pathID protocol.PathID, ranges []protocol.AckRange) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending, ok := p.pending[pathID]
	if !ok {
		return
	}
	for _, r := range ranges {
		start := r.Start
		for i := uint64(0); i < uint64(r.Count); i++ {
			seq := start + i
			if _, ok := pending[seq]; ok {
				delete(pending, seq)
				p.metricsMu.Lock()
				p.ackedCount++
				p.metricsMu.Unlock()
			}
		}
	}
}

// ResendRanges attempts to resend specific packet sequences on pathID immediately.
func (p *Packer) ResendRanges(pathID protocol.PathID, ranges []protocol.AckRange) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending, ok := p.pending[pathID]
	if !ok {
		return
	}
	path := p.paths[pathID]
	if path == nil {
		return
	}
	for _, r := range ranges {
		for i := uint64(0); i < uint64(r.Count); i++ {
			seq := r.Start + i
			if pkt, ok := pending[seq]; ok {
				var seqBuf [8]byte
				binary.BigEndian.PutUint64(seqBuf[:], seq)
				var lenBuf [4]byte
				binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pkt.body)))
				out := append(seqBuf[:], append(lenBuf[:], pkt.body...)...)
				_, err := path.WriteStream(pkt.carrier, out)
				if err != nil {
					p.logger.Warn("resend failed", "path", pathID, "seq", seq, "err", err)
				} else {
					p.metricsMu.Lock()
					p.resentCount++
					p.metricsMu.Unlock()
				}
			}
		}
	}
}

// MetricsSnapshot contains basic packer metrics.
type MetricsSnapshot struct {
	Sent    int64
	Resent  int64
	Acked   int64
	Dropped int64
}

func (p *Packer) GetMetrics() MetricsSnapshot {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	return MetricsSnapshot{Sent: p.sentCount, Resent: p.resentCount, Acked: p.ackedCount, Dropped: p.droppedCount}
}

// StartRetransmitLoop periodically resends unacked packets using the provided resend function.
func (p *Packer) StartRetransmitLoop(resend func(pathID protocol.PathID, seq uint64, packet []byte) error) {
	go func() {
		// naive loop
		for {
			p.mu.Lock()
			for pid, mp := range p.pending {
				for seq, packet := range mp {
					now := time.Now()
					if packet.nextRetry.After(now) {
						continue
					}
					if packet.attempts >= p.maxAttempts {
						// drop
						delete(mp, seq)
						p.metricsMu.Lock()
						p.droppedCount++
						p.metricsMu.Unlock()
						p.logger.Warn("dropping packet after max attempts", "path", pid, "seq", seq)
						continue
					}
					// attempt resend
					var err error
					if resend != nil {
						err = resend(pid, seq, packet.body)
					} else if path, ok := p.paths[pid]; ok {
						var seqBuf [8]byte
						binary.BigEndian.PutUint64(seqBuf[:], seq)
						var lenBuf [4]byte
						binary.BigEndian.PutUint32(lenBuf[:], uint32(len(packet.body)))
						out := append(seqBuf[:], append(lenBuf[:], packet.body...)...)
						_, err = path.WriteStream(packet.carrier, out)
					}
					packet.attempts++
					p.metricsMu.Lock()
					if packet.attempts == 1 {
						p.sentCount++
					} else {
						p.resentCount++
					}
					p.metricsMu.Unlock()
					if err != nil {
						// schedule next retry with exponential backoff
						backoff := p.baseBackoffMs << (packet.attempts - 1)
						if backoff > p.maxBackoffMs {
							backoff = p.maxBackoffMs
						}
						packet.nextRetry = time.Now().Add(time.Duration(backoff) * time.Millisecond)
					} else {
						// successful write; set nextRetry to now to allow waiting for ACK
						packet.nextRetry = time.Now().Add(time.Duration(p.baseBackoffMs) * time.Millisecond)
					}
				}
			}
			p.mu.Unlock()
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// RegisterPath allows packer to know about active paths (used by retransmit loop if needed)
func (p *Packer) RegisterPath(path Path) {
	pid := path.PathID()
	p.seqMu.Lock()
	if _, ok := p.nextPacketSeq[pid]; !ok {
		p.nextPacketSeq[pid] = 0
	}
	p.seqMu.Unlock()
	p.mu.Lock()
	if _, ok := p.pending[pid]; !ok {
		p.pending[pid] = make(map[uint64]*pendingPacket)
	}
	p.paths[pid] = path
	p.mu.Unlock()
}

// UnregisterPath removes a previously registered path from the packer.
func (p *Packer) UnregisterPath(pid protocol.PathID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.paths, pid)
	delete(p.pending, pid)
	p.seqMu.Lock()
	delete(p.nextPacketSeq, pid)
	p.seqMu.Unlock()
}

// GetRegisteredPath returns the registered Path for pathID or nil.
func (p *Packer) GetRegisteredPath(pathID protocol.PathID) Path {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paths[pathID]
}

// transportPath is an adapter to avoid import cycle with transport.Path interface
type transportPath interface {
	PathID() protocol.PathID
	OpenStream(streamID protocol.StreamID) error
	WriteStream(streamID protocol.StreamID, b []byte) (int, error)
}

type pendingPacket struct {
	body      []byte
	carrier   protocol.StreamID
	attempts  int
	nextRetry time.Time
}
