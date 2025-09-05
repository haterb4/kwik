package transport

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

// multiplexer collects packets from multiple paths concurrently, unpacks
// frames and delivers ordered frames to per-stream queues.
type Multiplexer struct {
	mu     sync.RWMutex
	queues map[protocol.StreamID]*streamQueue
	logger logger.Logger
	// received packet sequences per path for ACK generation
	received map[protocol.PathID]map[uint64]struct{}
	// last-seen mapping: for each stream, which path and packetSeq carried its most recent frame
	lastSeenPathForStream      map[protocol.StreamID]protocol.PathID
	lastSeenPacketSeqForStream map[protocol.StreamID]uint64
	// ack policy
	ackThreshold  int // number of packets to trigger immediate ack
	ackIntervalMs int
	// nack policy
	nackCooldownMs    int // minimum ms between NACKs per stream
	nackMaxRangeCount int // max ranges allowed in a NACK
	nackMaxTotalSeqs  uint64
	// metrics
	metricsMu     sync.Mutex
	ackSentCount  int64
	nackSentCount int64
	packer        *Packer
}

type streamQueue struct {
	mu          sync.Mutex
	framesBySeq map[uint64]*protocol.Frame
	// next expected sequence number for this stream
	expectedSeq uint64
	// gap and nack metrics
	gapCount   int64
	lastGapAt  time.Time
	lastNackAt time.Time
	cond       *sync.Cond
}

func NewMultiplexer(packer *Packer) *Multiplexer {
	return &Multiplexer{
		queues:                     make(map[protocol.StreamID]*streamQueue),
		logger:                     logger.NewLogger(logger.LogLevelDebug).WithComponent("MULTIPLEXER"),
		received:                   make(map[protocol.PathID]map[uint64]struct{}),
		ackThreshold:               32,  // Higher threshold before immediate ACK
		ackIntervalMs:              500, // Less frequent ACK intervals
		nackCooldownMs:             500, // Longer cooldown between NACKs
		nackMaxRangeCount:          64,
		nackMaxTotalSeqs:           4096,
		lastSeenPathForStream:      make(map[protocol.StreamID]protocol.PathID),
		lastSeenPacketSeqForStream: make(map[protocol.StreamID]uint64),
		packer:                     packer,
	}
}

// PushPacket accepts a raw packet (which is prefixed by 4-byte length when serialized
// on the wire). We assume callers pass the packet body (not the 4-byte len).
func (m *Multiplexer) PushPacket(pathID protocol.PathID, pkt []byte) error {
	// unpack: sequence of frames
	buf := bytes.NewReader(pkt)
	for {
		if buf.Len() == 0 {
			break
		}
		f, err := protocol.DecodeFrame(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			m.logger.Error("failed to decode frame", "path", pathID, "err", err)
			return err
		}
		// if this is a control frame (stream 0), route to the path's control handler
		if f.StreamID == 0 {
			// handle ACK frames specially
			if f.Type == protocol.FrameTypeAck {
				ranges, err := protocol.DecodeAckPayload(f.Payload)
				if err != nil {
					m.logger.Error("failed to decode ack payload", "err", err)
				} else {
					if m.packer != nil {
						m.packer.OnAck(pathID, ranges)
					}
				}
				continue
			}
			// handle NACK frames: request resend of ranges
			if f.Type == protocol.FrameTypeNack {
				ranges, err := protocol.DecodeAckPayload(f.Payload)
				if err != nil {
					m.logger.Error("failed to decode nack payload", "err", err)
				} else {
					if m.packer != nil {
						// instruct packer to resend those ranges on this path
						m.packer.ResendRanges(pathID, ranges)
					}
				}
				continue
			}

			// other control frames: try to deliver to registered path's control handler
			if path := m.packer.GetRegisteredPath(pathID); path != nil {
				// best effort; ignore errors
				_ = path.HandleControlFrame(f)
				continue
			}
			// fallback: if no path handler, fall through to enqueue (unlikely)
		}
		m.enqueueFrame(f)
	}
	return nil
}

// PushPacketWithSeq accepts a packet along with its sequence number.
func (m *Multiplexer) PushPacketWithSeq(pathID protocol.PathID, packetSeq uint64, pkt []byte) error {
	// record receipt for ACKs
	m.recordReceivedPacket(pathID, packetSeq)
	// decode frames here so we can record last-seen path/packet per stream
	buf := bytes.NewReader(pkt)
	for {
		if buf.Len() == 0 {
			break
		}
		f, err := protocol.DecodeFrame(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			m.logger.Error("failed to decode frame", "path", pathID, "err", err)
			return err
		}
		// record last-seen info for this stream
		m.mu.Lock()
		m.lastSeenPathForStream[f.StreamID] = pathID
		m.lastSeenPacketSeqForStream[f.StreamID] = packetSeq
		m.mu.Unlock()

		// handle ACK/NACK frames specially
		if f.Type == protocol.FrameTypeAck {
			ranges, err := protocol.DecodeAckPayload(f.Payload)
			if err != nil {
				m.logger.Error("failed to decode ack payload", "err", err)
			} else {
				if m.packer != nil {
					m.packer.OnAck(pathID, ranges)
				}
			}
			continue
		}
		if f.Type == protocol.FrameTypeNack {
			ranges, err := protocol.DecodeAckPayload(f.Payload)
			if err != nil {
				m.logger.Error("failed to decode nack payload", "err", err)
			} else {
				if m.packer != nil {
					m.packer.ResendRanges(pathID, ranges)
				}
			}
			continue
		}

		// If this is a control frame on the control stream (0), deliver it to the
		// registered path's control handler (handshake/ping/pong) so it can reply
		// immediately. This mirrors the behavior in PushPacket (no-seq path).
		if f.StreamID == 0 {
			if path := m.packer.GetRegisteredPath(pathID); path != nil {
				// log dispatching control frame to path
				m.logger.Debug("dispatching control frame to path handler", "path", pathID, "type", f.Type, "len", len(f.Payload))
				if err := path.HandleControlFrame(f); err != nil {
					m.logger.Warn("path HandleControlFrame returned error", "path", pathID, "err", err)
				}
				continue
			} else {
				m.logger.Warn("no registered path found for control frame", "path", pathID, "type", f.Type)
			}
		}

		m.enqueueFrame(f)
	}
	return nil
}

// recordReceivedPacket records packetSeq receipt per path.
func (m *Multiplexer) recordReceivedPacket(pathID protocol.PathID, packetSeq uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, ok := m.received[pathID]
	if !ok {
		mp = make(map[uint64]struct{})
		m.received[pathID] = mp
	}
	mp[packetSeq] = struct{}{}
	// m.logger.Debug("recorded packet receipt", "path", pathID, "seq", packetSeq)
	// if we've reached the threshold, schedule an immediate ack send
	if len(mp) >= m.ackThreshold {
		go m.sendAckNow(pathID)
	} else if len(mp) > 0 && len(mp)%8 == 0 {
		// Send ACKs more frequently for smaller batches to reduce latency
		go m.sendAckNow(pathID)
	}
}

func (m *Multiplexer) sendAckNow(pid protocol.PathID) {
	ranges := m.GetAckRanges(pid)
	if len(ranges) == 0 {
		return
	}
	payload := protocol.EncodeAckPayload(ranges)
	ackFrame := &protocol.Frame{Type: protocol.FrameTypeAck, StreamID: 0, Seq: 0, Payload: payload}
	if m.packer != nil {
		if path := m.packer.GetRegisteredPath(pid); path != nil {
			_ = m.packer.SubmitFrame(path, ackFrame)
			m.metricsMu.Lock()
			m.ackSentCount++
			m.metricsMu.Unlock()
			// clear receipts
			m.mu.Lock()
			if mp, ok := m.received[pid]; ok {
				for _, r := range ranges {
					for i := uint64(0); i < uint64(r.Count); i++ {
						delete(mp, r.Start+i)
					}
				}
				if len(mp) == 0 {
					delete(m.received, pid)
				} else {
					m.received[pid] = mp
				}
			}
			m.mu.Unlock()
		}
	}
}

// sendNackRanges validates and emits a NACK frame for the requested ranges.
// NACKs are sent on known paths (paths with recorded receipts) and are rate-limited per-stream.
func (m *Multiplexer) sendNackRanges(ranges []protocol.AckRange) {
	// validate ranges
	if len(ranges) == 0 {
		return
	}
	if len(ranges) > m.nackMaxRangeCount {
		m.logger.Warn("nack rejected: too many ranges", "count", len(ranges))
		return
	}
	var total uint64
	for _, r := range ranges {
		if r.Count == 0 {
			m.logger.Warn("nack rejected: zero-length range")
			return
		}
		total += uint64(r.Count)
		if total > m.nackMaxTotalSeqs {
			m.logger.Warn("nack rejected: too many sequences requested", "total", total)
			return
		}
	}
	payload := protocol.EncodeAckPayload(ranges)
	nackFrame := &protocol.Frame{Type: protocol.FrameTypeNack, StreamID: 0, Seq: 0, Payload: payload}
	if m.packer == nil {
		m.logger.Warn("no packer available for NACK")
		return
	}
	// send NACK on each path we have recorded receipts for
	m.mu.RLock()
	paths := make([]protocol.PathID, 0, len(m.received))
	for pid := range m.received {
		paths = append(paths, pid)
	}
	m.mu.RUnlock()
	for _, pid := range paths {
		if path := m.packer.GetRegisteredPath(pid); path != nil {
			// submit via packer
			if err := m.packer.SubmitFrame(path, nackFrame); err != nil {
				m.logger.Warn("failed to submit nack frame", "path", pid, "err", err)
			} else {
				m.metricsMu.Lock()
				m.nackSentCount++
				m.metricsMu.Unlock()
			}
		}
	}
}

// sendNackRangesToPath validates ranges and sends a NACK only on the specified path.
func (m *Multiplexer) sendNackRangesToPath(pid protocol.PathID, ranges []protocol.AckRange) {
	if len(ranges) == 0 {
		return
	}
	if len(ranges) > m.nackMaxRangeCount {
		m.logger.Warn("nack rejected: too many ranges", "count", len(ranges))
		return
	}
	var total uint64
	for _, r := range ranges {
		if r.Count == 0 {
			m.logger.Warn("nack rejected: zero-length range")
			return
		}
		total += uint64(r.Count)
		if total > m.nackMaxTotalSeqs {
			m.logger.Warn("nack rejected: too many sequences requested", "total", total)
			return
		}
	}
	payload := protocol.EncodeAckPayload(ranges)
	nackFrame := &protocol.Frame{Type: protocol.FrameTypeNack, StreamID: 0, Seq: 0, Payload: payload}
	if m.packer == nil {
		m.logger.Warn("no packer available for NACK")
		return
	}
	if path := m.packer.GetRegisteredPath(pid); path != nil {
		if err := m.packer.SubmitFrame(path, nackFrame); err != nil {
			m.logger.Warn("failed to submit nack frame", "path", pid, "err", err)
		} else {
			m.metricsMu.Lock()
			m.nackSentCount++
			m.metricsMu.Unlock()
		}
	}
}

func (m *Multiplexer) GetMetrics() map[string]interface{} {
	m.metricsMu.Lock()
	defer m.metricsMu.Unlock()
	return map[string]interface{}{"acks_sent": m.ackSentCount, "nacks_sent": m.nackSentCount}
}

// GetAckRanges returns a compact set of AckRange for recently received packets on a path.
// Currently this is a stub returning empty; real implementation should track receipts.
func (m *Multiplexer) GetAckRanges(pathID protocol.PathID) []protocol.AckRange {
	m.mu.RLock()
	mp, ok := m.received[pathID]
	m.mu.RUnlock()
	if !ok || len(mp) == 0 {
		return nil
	}
	// collect and sort sequences
	seqs := make([]uint64, 0, len(mp))
	for s := range mp {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	// collapse into ranges
	var ranges []protocol.AckRange
	start := seqs[0]
	prev := seqs[0]
	for i := 1; i < len(seqs); i++ {
		if seqs[i] == prev+1 {
			prev = seqs[i]
			continue
		}
		ranges = append(ranges, protocol.AckRange{Start: start, Count: uint32(prev - start + 1)})
		start = seqs[i]
		prev = seqs[i]
	}
	ranges = append(ranges, protocol.AckRange{Start: start, Count: uint32(prev - start + 1)})
	return ranges
}

// StartAckLoop periodically computes ack ranges per path and sends ACK frames
// back on the same path via the default packer.
func (m *Multiplexer) StartAckLoop(intervalMs int) {
	go func() {
		for {
			// snapshot paths for which we have receipts
			m.mu.RLock()
			paths := make([]protocol.PathID, 0, len(m.received))
			for pid := range m.received {
				paths = append(paths, pid)
			}
			m.mu.RUnlock()

			for _, pid := range paths {
				ranges := m.GetAckRanges(pid)
				if len(ranges) == 0 {
					continue
				}
				// prepare ack frame payload
				payload := protocol.EncodeAckPayload(ranges)
				ackFrame := &protocol.Frame{
					Type:     protocol.FrameTypeAck,
					StreamID: protocol.StreamID(0), // control stream
					Seq:      0,
					Payload:  payload,
				}
				// send via packer on same path
				if m.packer != nil {
					if path := m.packer.GetRegisteredPath(pid); path != nil {
						// submit ack frame
						_ = m.packer.SubmitFrame(path, ackFrame)
						// clear recorded receipts that we acked
						m.mu.Lock()
						if mp, ok := m.received[pid]; ok {
							// remove sequences that are covered by ranges
							for _, r := range ranges {
								for i := uint64(0); i < uint64(r.Count); i++ {
									delete(mp, r.Start+i)
								}
							}
							if len(mp) == 0 {
								delete(m.received, pid)
							} else {
								m.received[pid] = mp
							}
						}
						m.mu.Unlock()
					}
				}
			}
			time.Sleep(time.Duration(intervalMs) * time.Millisecond)
		}
	}()
}

func (m *Multiplexer) RegisterStream(streamID protocol.StreamID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Vérifier si elle existe déjà pour éviter de l'écraser
	if _, ok := m.queues[streamID]; ok {
		return
	}

	q := &streamQueue{
		framesBySeq: make(map[uint64]*protocol.Frame),
		expectedSeq: 1, // Commencer à attendre la frame 1
	}
	q.cond = sync.NewCond(&q.mu)
	m.queues[streamID] = q
	m.logger.Debug("registered stream queue in multiplexer", "streamID", streamID)
}

func (m *Multiplexer) enqueueFrame(f *protocol.Frame) {
	m.mu.Lock()
	q, ok := m.queues[f.StreamID]
	if !ok {
		// Créer la queue à la volée si elle n'existe pas
		q = &streamQueue{framesBySeq: make(map[uint64]*protocol.Frame), expectedSeq: 1}
		q.cond = sync.NewCond(&q.mu)
		m.queues[f.StreamID] = q
		m.logger.Debug("created stream queue on-the-fly in enqueue", "streamID", f.StreamID)
	}
	m.mu.Unlock()

	q.mu.Lock()
	// initialize expectedSeq if zero
	if q.expectedSeq == 0 {
		q.expectedSeq = 1
	}
	// drop duplicates or old frames
	if f.Seq < q.expectedSeq {
		q.mu.Unlock()
		// Silenced: m.logger.Debug("dropping old/duplicate frame", "stream", f.StreamID, "seq", f.Seq, "expected", q.expectedSeq)
		return
	}
	if _, exists := q.framesBySeq[f.Seq]; exists {
		q.mu.Unlock()
		// Silenced: m.logger.Debug("duplicate frame ignored", "stream", f.StreamID, "seq", f.Seq)
		return
	}

	// detect gap: if frame seq > expectedSeq then we have a gap
	if f.Seq > q.expectedSeq {
		q.gapCount++
		q.lastGapAt = time.Now()
		m.logger.Warn("gap detected in stream", "stream", f.StreamID, "expected", q.expectedSeq, "got", f.Seq, "gapCount", q.gapCount)
		// rate-limit NACKs per-stream
		/*  =============================================================
		now := time.Now()
		if now.Sub(q.lastNackAt) > time.Duration(m.nackCooldownMs)*time.Millisecond {
			// create ranges covering the missing seqs up to a modest limit
			// we request from expectedSeq to f.Seq-1
			start := q.expectedSeq
			count := uint32(0)
			if f.Seq > q.expectedSeq {
				gap := f.Seq - q.expectedSeq
				if gap > m.nackMaxTotalSeqs {
					gap = m.nackMaxTotalSeqs
				}
				count = uint32(gap)
			}
			if count > 0 {
				ranges := []protocol.AckRange{{Start: start, Count: count}}
				// prefer targeting NACK to last-seen path for this stream
				m.mu.RLock()
				targetPath, ok := m.lastSeenPathForStream[f.StreamID]
				m.mu.RUnlock()
				if ok {
					go m.sendNackRangesToPath(targetPath, ranges)
				} else {
					go m.sendNackRanges(ranges)
				}
				q.lastNackAt = now
			}
		}
			============================================================= */
	}

	q.framesBySeq[f.Seq] = f
	q.mu.Unlock()
	q.cond.Signal()
}

// PullFrames returns up to `max` concatenated payload bytes for the requested stream
// in STRICT sequence order. It blocks waiting for missing frames until timeout.
func (m *Multiplexer) PullFrames(streamID protocol.StreamID, max int) ([]byte, error) {
	m.mu.RLock()
	q, ok := m.queues[streamID]
	m.mu.RUnlock()

	if !ok {
		m.logger.Error("BUG: PullFrames called for a non-registered stream", "streamID", streamID)
		return nil, fmt.Errorf("internal error: stream %d not registered in multiplexer", streamID)
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.expectedSeq == 0 {
		q.expectedSeq = 1
	}

	// Boucle d'attente : on attend jusqu'à ce que la frame attendue (q.expectedSeq) soit présente.
	for {
		if _, ok := q.framesBySeq[q.expectedSeq]; ok {
			break // La frame est là, on peut continuer.
		}
		// La frame n'est pas là. On attend un signal.
		// Wait() déverrouille atomiquement le mutex et attend, puis le reverrouille au réveil.
		q.cond.Wait()
	}

	var out []byte
	seq := q.expectedSeq

	// On collecte toutes les frames contiguës disponibles.
	for {
		f, ok := q.framesBySeq[seq]
		if !ok {
			// Plus de frames contiguës, on s'arrête.
			break
		}

		if len(out)+len(f.Payload) > max && len(out) > 0 {
			// La frame suivante ne rentre pas dans le buffer de lecture et on a déjà des données.
			break
		}

		out = append(out, f.Payload...)
		delete(q.framesBySeq, seq)
		seq++

		if len(out) >= max {
			break
		}
	}

	// Mettre à jour la séquence attendue pour la prochaine lecture.
	if seq > q.expectedSeq {
		m.logger.Debug("advanced expected sequence",
			"stream", streamID,
			"from", q.expectedSeq,
			"to", seq)
		q.expectedSeq = seq
	}

	return out, nil
}
