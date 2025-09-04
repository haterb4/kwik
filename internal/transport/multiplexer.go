package transport

import (
	"bytes"
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
}

func NewMultiplexer() *Multiplexer {
	return &Multiplexer{
		queues:                     make(map[protocol.StreamID]*streamQueue),
		logger:                     logger.NewLogger(logger.LogLevelDebug).WithComponent("MULTIPLEXER"),
		received:                   make(map[protocol.PathID]map[uint64]struct{}),
		ackThreshold:               16,
		ackIntervalMs:              200,
		nackCooldownMs:             200,
		nackMaxRangeCount:          64,
		nackMaxTotalSeqs:           4096,
		lastSeenPathForStream:      make(map[protocol.StreamID]protocol.PathID),
		lastSeenPacketSeqForStream: make(map[protocol.StreamID]uint64),
	}
}

var defaultMux *Multiplexer

func SetDefaultMultiplexer(m *Multiplexer) {
	defaultMux = m
}

func GetDefaultMultiplexer() *Multiplexer {
	return defaultMux
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
					if pk := GetDefaultPacker(); pk != nil {
						pk.OnAck(pathID, ranges)
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
					if pk := GetDefaultPacker(); pk != nil {
						// instruct packer to resend those ranges on this path
						pk.ResendRanges(pathID, ranges)
					}
				}
				continue
			}

			// other control frames: try to deliver to registered path's control handler
			if path := GetDefaultPacker().GetRegisteredPath(pathID); path != nil {
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
				if pk := GetDefaultPacker(); pk != nil {
					pk.OnAck(pathID, ranges)
				}
			}
			continue
		}
		if f.Type == protocol.FrameTypeNack {
			ranges, err := protocol.DecodeAckPayload(f.Payload)
			if err != nil {
				m.logger.Error("failed to decode nack payload", "err", err)
			} else {
				if pk := GetDefaultPacker(); pk != nil {
					pk.ResendRanges(pathID, ranges)
				}
			}
			continue
		}

		// If this is a control frame on the control stream (0), deliver it to the
		// registered path's control handler (handshake/ping/pong) so it can reply
		// immediately. This mirrors the behavior in PushPacket (no-seq path).
		if f.StreamID == 0 {
			if path := GetDefaultPacker().GetRegisteredPath(pathID); path != nil {
				// best effort; ignore errors
				_ = path.HandleControlFrame(f)
				continue
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
	m.logger.Debug("recorded packet receipt", "path", pathID, "seq", packetSeq)
	// if we've reached the threshold, schedule an immediate ack send
	if len(mp) >= m.ackThreshold {
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
	if pk := GetDefaultPacker(); pk != nil {
		if path := pk.GetRegisteredPath(pid); path != nil {
			_ = pk.SubmitFrame(path, ackFrame)
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
	pk := GetDefaultPacker()
	if pk == nil {
		m.logger.Debug("no packer available to send nack")
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
		if path := pk.GetRegisteredPath(pid); path != nil {
			// submit via packer
			if err := pk.SubmitFrame(path, nackFrame); err != nil {
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
	pk := GetDefaultPacker()
	if pk == nil {
		m.logger.Debug("no packer available to send nack")
		return
	}
	if path := pk.GetRegisteredPath(pid); path != nil {
		if err := pk.SubmitFrame(path, nackFrame); err != nil {
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
				if pk := GetDefaultPacker(); pk != nil {
					if path := pk.GetRegisteredPath(pid); path != nil {
						// submit ack frame
						_ = pk.SubmitFrame(path, ackFrame)
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

func (m *Multiplexer) enqueueFrame(f *protocol.Frame) {
	m.mu.Lock()
	q, ok := m.queues[f.StreamID]
	if !ok {
		q = &streamQueue{framesBySeq: make(map[uint64]*protocol.Frame), expectedSeq: 1}
		m.queues[f.StreamID] = q
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
		m.logger.Debug("dropping old/duplicate frame", "stream", f.StreamID, "seq", f.Seq, "expected", q.expectedSeq)
		return
	}
	if _, exists := q.framesBySeq[f.Seq]; exists {
		q.mu.Unlock()
		m.logger.Debug("duplicate frame ignored", "stream", f.StreamID, "seq", f.Seq)
		return
	}

	// detect gap: if frame seq > expectedSeq then we have a gap
	if f.Seq > q.expectedSeq {
		q.gapCount++
		q.lastGapAt = time.Now()
		m.logger.Warn("gap detected in stream", "stream", f.StreamID, "expected", q.expectedSeq, "got", f.Seq, "gapCount", q.gapCount)
		// rate-limit NACKs per-stream
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
	}

	q.framesBySeq[f.Seq] = f
	q.mu.Unlock()
}

// PullFrames returns up to `max` concatenated payload bytes for the requested stream
// in sequence order starting at the lowest available Seq. It returns zero bytes if
// no frames are available.
func (m *Multiplexer) PullFrames(streamID protocol.StreamID, max int) ([]byte, error) {
	m.mu.RLock()
	q, ok := m.queues[streamID]
	m.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.framesBySeq) == 0 {
		return nil, nil
	}
	var out []byte
	seq := q.expectedSeq
	// collect contiguous frames starting at expectedSeq
	for {
		f, ok := q.framesBySeq[seq]
		if !ok {
			break // gap
		}
		if len(out)+len(f.Payload) > max {
			break
		}
		out = append(out, f.Payload...)
		delete(q.framesBySeq, seq)
		seq++
	}
	// advance expectedSeq if we consumed frames
	if seq != q.expectedSeq {
		q.expectedSeq = seq
	}
	return out, nil
}
