package transport

import (
	"bytes"
	"container/heap"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

// Item dans le tas du multiplexeur. Contient une trame prête à être lue.
type readyFrame struct {
	frame  *protocol.Frame
	pathID protocol.PathID
}

// Un tas de trames prêtes, triées par Offset.
type readyFrameHeap []readyFrame

func (h readyFrameHeap) Len() int            { return len(h) }
func (h readyFrameHeap) Less(i, j int) bool  { return h[i].frame.Offset < h[j].frame.Offset }
func (h readyFrameHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *readyFrameHeap) Push(x interface{}) { *h = append(*h, x.(readyFrame)) }
func (h *readyFrameHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

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
type pathQueue struct {
	mu              sync.Mutex
	frames          map[uint64]*protocol.Frame // Trames non ordonnées, clé = Seq
	nextReadSeq     uint64                     // Le prochain Seq que nous attendons sur ce path
	readyForPulling *readyFrameHeap            // Un tas partagé pour placer les trames ordonnées
	parentCond      *sync.Cond                 // Le cond du streamQueue parent, pour signaler
}

type streamQueue struct {
	mu             sync.Mutex
	cond           *sync.Cond
	paths          map[protocol.PathID]*pathQueue
	readyFrames    *readyFrameHeap // Tas de trames prêtes à être lues, ordonnées par Offset
	nextReadOffset int64
	buffer         []byte
	isClosed       bool
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
func (m *Multiplexer) SetStreamNextStreamOffset(id protocol.StreamID, offset int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sq, ok := m.queues[id]; ok {
		sq.nextReadOffset = offset
	}
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

		m.enqueueFrame(f, pathID)
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

	if m.packer != nil {
		if path := m.packer.GetRegisteredPath(pid); path != nil {
			payload := protocol.EncodeAckPayload(ranges)
			seq, _ := path.WriteSeq(0)
			ackFrame := &protocol.Frame{
				Type:     protocol.FrameTypeAck,
				StreamID: 0,
				Seq:      seq,
				Payload:  payload,
			}
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
			seq, _ := path.WriteSeq(0)
			nackFrame := &protocol.Frame{Type: protocol.FrameTypeNack, StreamID: 0, Seq: seq, Payload: payload}
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
	if m.packer == nil {
		m.logger.Warn("no packer available for NACK")
		return
	}
	if path := m.packer.GetRegisteredPath(pid); path != nil {
		seq, _ := path.WriteSeq(0)
		nackFrame := &protocol.Frame{Type: protocol.FrameTypeNack, StreamID: 0, Seq: seq, Payload: payload}
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
				// send via packer on same path
				if m.packer != nil {
					if path := m.packer.GetRegisteredPath(pid); path != nil {
						// submit ack frame
						seq, _ := path.WriteSeq(0)
						ackFrame := &protocol.Frame{
							Type:     protocol.FrameTypeAck,
							StreamID: protocol.StreamID(0), // control stream
							Seq:      seq,
							Payload:  payload,
						}
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
	if _, ok := m.queues[streamID]; ok {
		return
	}
	rfh := make(readyFrameHeap, 0)
	heap.Init(&rfh)
	q := &streamQueue{
		paths:          make(map[protocol.PathID]*pathQueue),
		readyFrames:    &rfh,
		nextReadOffset: 0,
	}
	q.cond = sync.NewCond(&q.mu)
	m.queues[streamID] = q
	m.logger.Debug("Registered new streamQueue", "streamID", streamID)
}

// enqueueFrame ajoute une trame à la bonne file d'attente de path.
func (m *Multiplexer) enqueueFrame(frame *protocol.Frame, pathID protocol.PathID) {
	m.mu.RLock()
	sq, ok := m.queues[frame.StreamID]
	m.mu.RUnlock()
	if !ok {
		m.logger.Warn("Frame for unregistered stream", "streamID", frame.StreamID)
		return
	}

	sq.mu.Lock()
	pq, ok := sq.paths[pathID]
	if !ok {
		// Créer la file d'attente pour ce path si elle n'existe pas.
		pq = &pathQueue{
			frames:          make(map[uint64]*protocol.Frame),
			nextReadSeq:     1, // On s'attend à ce que le Seq commence à 1
			readyForPulling: sq.readyFrames,
			parentCond:      sq.cond,
		}
		sq.paths[pathID] = pq
	}
	sq.mu.Unlock()

	// Maintenant, on travaille sur la file d'attente du path.
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Ignorer les vieilles trames.
	if frame.Seq < pq.nextReadSeq {
		m.logger.Debug("Dropping old frame on path", "streamID", frame.StreamID, "pathID", pathID, "seq", frame.Seq)
		return
	}
	m.logger.Debug("Enqueuing frame", "streamID", frame.StreamID, "pathID", pathID, "seq", frame.Seq, "offset", frame.Offset, "len", len(frame.Payload))
	// Stocker la trame.
	pq.frames[frame.Seq] = frame

	// Processus de ré-ordonnancement par Seq.
	// On ajoute au tas 'readyFrames' toutes les trames contiguës qu'on peut.
	for {
		nextFrame, found := pq.frames[pq.nextReadSeq]
		if !found {
			break // On a un trou, on arrête.
		}

		// On a la prochaine trame en séquence.
		sq.mu.Lock()
		heap.Push(sq.readyFrames, readyFrame{frame: nextFrame, pathID: pathID})
		sq.mu.Unlock()

		delete(pq.frames, pq.nextReadSeq)
		pq.nextReadSeq++
	}

	// Signaler à PullFrames qu'il y a potentiellement de nouvelles données prêtes.
	pq.parentCond.Signal()
}

// PullFrames returns up to `max` concatenated payload bytes for the requested stream
// in order of increasing offset, from any available path.
// PullFrames est la logique de lecture finale.
func (m *Multiplexer) PullFrames(streamID protocol.StreamID, p []byte) (int, error) {
	m.mu.RLock()
	sq, ok := m.queues[streamID]
	m.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("stream %d not registered", streamID)
	}

	sq.mu.Lock()
	defer sq.mu.Unlock()

	// Boucle d'attente.
	for !sq.isClosed && len(sq.buffer) == 0 && (sq.readyFrames.Len() == 0 || (*sq.readyFrames)[0].frame.Offset != sq.nextReadOffset) {
		sq.cond.Wait()
	}

	if sq.isClosed && len(sq.buffer) == 0 && sq.readyFrames.Len() == 0 {
		return 0, io.EOF
	}

	totalBytesRead := 0

	// 1. Vider le buffer interne.
	if len(sq.buffer) > 0 {
		n := copy(p, sq.buffer)
		sq.buffer = sq.buffer[n:]
		totalBytesRead += n
	}

	// 2. Lire depuis le tas de trames prêtes.
	for totalBytesRead < len(p) && sq.readyFrames.Len() > 0 && (*sq.readyFrames)[0].frame.Offset == sq.nextReadOffset {
		rf := heap.Pop(sq.readyFrames).(readyFrame)
		frame := rf.frame

		payload := frame.Payload
		spaceLeft := len(p) - totalBytesRead
		bytesToCopy := len(payload)

		if bytesToCopy > spaceLeft {
			bytesToCopy = spaceLeft
			sq.buffer = append(sq.buffer, payload[bytesToCopy:]...)
		}

		copy(p[totalBytesRead:], payload[:bytesToCopy])
		totalBytesRead += bytesToCopy
		sq.nextReadOffset += int64(len(payload))
	}

	return totalBytesRead, nil
}
