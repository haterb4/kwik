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
	// scheduling for flush
	flushInterval    time.Duration
	minFramesForSend int
	firstEnqueueTime map[protocol.PathID]time.Time
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
	p := &Packer{
		queues:           make(map[protocol.PathID][]*protocol.Frame),
		maxPacketSize:    maxPacketSize,
		logger:           logger.NewLogger(logger.LogLevelDebug).WithComponent("PACKER"),
		nextPacketSeq:    make(map[protocol.PathID]uint64),
		pending:          make(map[protocol.PathID]map[uint64]*pendingPacket),
		paths:            make(map[protocol.PathID]Path),
		flushInterval:    20 * time.Millisecond,
		minFramesForSend: 2,
		firstEnqueueTime: make(map[protocol.PathID]time.Time),
		// improved defaults for better reliability
		maxAttempts:   8,    // More attempts before giving up
		baseBackoffMs: 250,  // Higher base backoff for QUIC
		maxBackoffMs:  5000, // Higher max backoff
	}
	return p
}

// SubmitFrame queues a frame for the given path and attempts to assemble a packet
// containing as many whole frames as possible (without splitting frames) up to
// maxPacketSize. The assembled packet is written to the quic stream corresponding
// to the first frame's StreamID.
// SubmitFrame dispatches to the client or server submitter depending on path role.

func (p *Packer) SubmitFrame(path Path, f *protocol.Frame) error {
	pid := path.PathID()

	p.logger.Debug("SubmitFrame enter", "path", pid, "stream", f.StreamID, "payload_len", len(f.Payload))

	p.mu.Lock()
	p.queues[pid] = append(p.queues[pid], f)
	p.logger.Debug("SubmitFrame queued", "path", pid, "queue_len", len(p.queues[pid]))

	frames := p.queues[pid]
	var buf bytes.Buffer
	var assembled []*protocol.Frame
	size := 0

	for _, fr := range frames {
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
		p.logger.Debug("SubmitFrame no frames assembled", "path", pid)
		p.mu.Unlock()
		return nil
	}

	// remove assembled frames from queue now that we're sending
	p.queues[pid] = frames[len(assembled):]
	if len(p.queues[pid]) == 0 {
		delete(p.firstEnqueueTime, pid)
	}
	p.mu.Unlock()

	// log assembled summary
	totalPayload := 0
	for _, af := range assembled {
		totalPayload += len(af.Payload)
	}
	p.logger.Debug("SubmitFrame assembled", "path", pid, "assembled_frames", len(assembled), "total_payload", totalPayload, "encoded_size", size)

	// write length-prefixed packet body to path. use first frame's StreamID as carrier
	packet := buf.Bytes()
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(packet)))
	out := append(lenBuf[:], packet...)

	// ensure the target quic stream exists on the path
	carrierStreamID := assembled[0].StreamID

	if carrierStreamID != protocol.StreamID(0) {
		if stream, ok := path.(interface{ HasStream(protocol.StreamID) bool }); ok {
			if !stream.HasStream(carrierStreamID) {
				if err := path.OpenStream(carrierStreamID); err != nil {
					p.logger.Debug("failed to open stream for packer", "path", pid, "stream", carrierStreamID, "err", err)
				}
			}
		} else {
			if err := path.OpenStream(carrierStreamID); err != nil {
				p.logger.Debug("failed to open stream for packer", "path", pid, "stream", carrierStreamID, "err", err)
			}
		}
	}

	p.mu.Lock()
	_, pathExists := p.paths[pid]
	p.mu.Unlock()

	if !pathExists {
		p.logger.Warn("Skipping write to unregistered path", "path", pid)
		return protocol.NewPathNotExistsError(pid)
	}

	p.seqMu.Lock()
	seq := p.nextPacketSeq[pid] + 1
	p.nextPacketSeq[pid] = seq
	p.seqMu.Unlock()

	p.logger.Debug("SubmitFrame assigned seq", "path", pid, "seq", seq)

	p.mu.Lock()
	if _, ok := p.pending[pid]; !ok {
		p.pending[pid] = make(map[uint64]*pendingPacket)
	}
	packetBody := packet
	p.pending[pid][seq] = &pendingPacket{body: packetBody, carrier: carrierStreamID}
	pendingCount := len(p.pending[pid])
	p.mu.Unlock()

	p.logger.Debug("SubmitFrame stored pending", "path", pid, "seq", seq, "pending_count", pendingCount)

	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], seq)
	outWithSeq := append(seqBuf[:], out...)

	p.logger.Debug("submit packet", "path", pid, "frames", len(assembled), "seq", seq, "size", len(outWithSeq), "carrier", carrierStreamID)

	start := time.Now()
	_, err := path.WriteStream(carrierStreamID, outWithSeq)
	dur := time.Since(start)

	if err != nil {
		p.logger.Warn("write returned error (submit)", "path", pid, "seq", seq, "dur_ms", dur.Milliseconds(), "err", err)
	} else {
		p.logger.Debug("write returned ok (submit)", "path", pid, "seq", seq, "dur_ms", dur.Milliseconds())
	}

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
		for {
			type task struct {
				pathID  protocol.PathID
				seq     uint64
				body    []byte
				carrier protocol.StreamID
			}
			var tasks []task

			// collect tasks under lock
			p.mu.Lock()
			for pid, mp := range p.pending {
				for seq, packet := range mp {
					now := time.Now()
					if packet.nextRetry.After(now) {
						continue
					}
					if packet.attempts >= p.maxAttempts {
						delete(mp, seq)
						p.metricsMu.Lock()
						p.droppedCount++
						p.metricsMu.Unlock()
						continue
					}
					tasks = append(tasks, task{pathID: pid, seq: seq, body: packet.body, carrier: packet.carrier})
					packet.attempts++
					p.metricsMu.Lock()
					if packet.attempts == 1 {
						p.sentCount++
					} else {
						p.resentCount++
					}
					p.metricsMu.Unlock()
				}
			}
			p.mu.Unlock()

			// execute tasks outside the lock
			for _, t := range tasks {
				var err error
				if resend != nil {
					err = resend(t.pathID, t.seq, t.body)
				} else if path, ok := p.paths[t.pathID]; ok {
					var seqBuf [8]byte
					binary.BigEndian.PutUint64(seqBuf[:], t.seq)
					var lenBuf [4]byte
					binary.BigEndian.PutUint32(lenBuf[:], uint32(len(t.body)))
					out := append(seqBuf[:], append(lenBuf[:], t.body...)...)
					_, err = path.WriteStream(t.carrier, out)
				}

				// update retry schedule under lock
				p.mu.Lock()
				if mp, ok := p.pending[t.pathID]; ok {
					if packet, ok2 := mp[t.seq]; ok2 {
						if err != nil {
							backoff := p.baseBackoffMs << (packet.attempts - 1)
							if backoff > p.maxBackoffMs {
								backoff = p.maxBackoffMs
							}
							jitter := backoff / 4
							if jitter > 0 {
								backoff += int(time.Now().UnixNano() % int64(jitter))
							}
							packet.nextRetry = time.Now().Add(time.Duration(backoff) * time.Millisecond)
						} else {
							waitTime := p.baseBackoffMs * (1 << packet.attempts)
							if waitTime > p.maxBackoffMs {
								waitTime = p.maxBackoffMs
							}
							packet.nextRetry = time.Now().Add(time.Duration(waitTime) * time.Millisecond)
						}
						if packet.attempts >= p.maxAttempts {
							delete(mp, t.seq)
							p.metricsMu.Lock()
							p.droppedCount++
							p.metricsMu.Unlock()
						}
					}
				}
				p.mu.Unlock()
			}

			time.Sleep(500 * time.Millisecond)
		}
	}()

}

// RegisterPath registers a Path with the Packer for retransmit tracking.
func (p *Packer) RegisterPath(path Path) {
	pid := path.PathID()
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.paths[pid]; ok {
		p.logger.Debug("Path already registered with packer", "pathID", pid)
		return
	}

	p.seqMu.Lock()
	if _, ok := p.nextPacketSeq[pid]; !ok {
		p.nextPacketSeq[pid] = 0
	}
	p.seqMu.Unlock()

	if _, ok := p.pending[pid]; !ok {
		p.pending[pid] = make(map[uint64]*pendingPacket)
	}

	p.paths[pid] = path
	p.logger.Debug("Registered path with packer", "pathID", pid)
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

type pendingPacket struct {
	body      []byte
	carrier   protocol.StreamID
	attempts  int
	nextRetry time.Time
}
