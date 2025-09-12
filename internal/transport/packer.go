package transport

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type Packer struct {
	mu sync.Mutex
	// stop channel to signal background goroutines to exit
	stopCh        chan struct{}
	wg            sync.WaitGroup
	queues        map[protocol.PathID][]protocol.Frame
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
	// streams that currently have buffered data waiting to be flushed
	bufferedStreams map[protocol.StreamID]protocol.PathID
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
		queues:           make(map[protocol.PathID][]protocol.Frame),
		maxPacketSize:    maxPacketSize,
		logger:           logger.NewLogger(logger.LogLevelSilent).WithComponent("PACKER"),
		nextPacketSeq:    make(map[protocol.PathID]uint64),
		pending:          make(map[protocol.PathID]map[uint64]*pendingPacket),
		paths:            make(map[protocol.PathID]Path),
		bufferedStreams:  make(map[protocol.StreamID]protocol.PathID),
		flushInterval:    10 * time.Millisecond, // Reduced from 20ms to 10ms
		minFramesForSend: 1,                     // Reduced from 2 to 1 for more aggressive sending
		firstEnqueueTime: make(map[protocol.PathID]time.Time),
		// improved defaults for better reliability and lower latency
		maxAttempts:   12,   // Increased from 8 to 12 for more persistence
		baseBackoffMs: 200,  // Increased from 50ms to 200ms for more reasonable retransmissions
		maxBackoffMs:  3000, // Increased from 2000ms to 3000ms
	}

	// Start retransmit and flush loops
	p.StartRetransmitLoop(nil)

	return p
}

// SubmitFrame queues a frame for the given path and attempts to assemble a packet
// containing as many whole frames as possible (without splitting frames) up to
// maxPacketSize. The assembled packet is written to the quic stream corresponding
// to the first frame's StreamID.
// SubmitFrame dispatches to the client or server submitter depending on path role.

// SubmitNewFrame queues a NewFrame for the given path. It's the preferred API that avoids
// conversions from legacy Frame and lets the packer operate on the new canonical frame model.
func (p *Packer) SubmitFrame(path Path, nf protocol.Frame) error {
	pid := path.PathID()
	if nf.Type() == protocol.FrameTypeStream {
		// fmt.Printf("TRACK SubmitFrame Received Frame: %v pathID=%d\n", nf.String(), pid)
	}
	p.mu.Lock()
	p.queues[pid] = append(p.queues[pid], nf)
	// p.logger.Debug("SubmitFrame queued", "path", pid, "queue_len", len(p.queues[pid]))

	frames := p.queues[pid]
	var assembled []protocol.Frame
	// Build a logical Packet containing Frame entries directly
	var pkt protocol.Packet
	pkt.Payload = make([]protocol.Frame, 0, len(frames))
	size := 0

	for _, fr := range frames {
		// estimate encoded size conservatively
		// For StreamFrame estimate header sizes; for control frames use payload length
		switch v := fr.(type) {
		case *protocol.StreamFrame:
			encSize := 1 + 8 + 8 + 4 + len(v.Data)
			if encSize > p.maxPacketSize {
				p.mu.Unlock()
				return protocol.NewFrameTooLargeError(encSize, p.maxPacketSize)
			}
			if size+encSize > p.maxPacketSize {
				break
			}
			size += encSize
		default:
			// control frames
			// best-effort estimate: 1 + len(payload)
			// (Packet header + length prefix handled separately)
			// try to obtain payload length via reflection-like switch
			switch x := fr.(type) {
			case *protocol.AckFrame:
				// AckFrame will encode to variable size; use a conservative estimate
				size += 32
			case *protocol.StreamFrame:
				size += len(x.Data)
			default:
				size += 16
			}
		}
		pkt.Payload = append(pkt.Payload, fr)
		assembled = append(assembled, fr)
	}

	// TRACK: juste avant le test assembled vide
	// fmt.Printf("TRACK SubmitFrame: pathID=%d, frames=%d\n", pid, len(assembled))
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

	// On calcule le numéro de séquence juste avant la sérialisation
	p.seqMu.Lock()
	nextPacketSeq := p.nextPacketSeq[pid] + 1
	p.nextPacketSeq[pid] = nextPacketSeq
	p.seqMu.Unlock()

	// Définit le numéro de séquence dans le header du paquet AVANT la sérialisation
	pkt.Header.PacketNum = nextPacketSeq

	// build raw packet body from logical Packet
	packetBody, err := pkt.Serialize()
	if err != nil {
		p.logger.Error("failed to build packet payload", "err", err)
		return err
	}
	// Do not send empty or too-short packets (e.g. header-only, no frames)
	minPacketLen := 8 // empirique: header + frame count minimal
	if len(packetBody) < minPacketLen {
		p.logger.Warn("Not sending packet: too short or empty", "len", len(packetBody), "path", pid)
		return nil
	}
	if len(packetBody) > p.maxPacketSize {
		return protocol.NewFrameTooLargeError(len(packetBody), p.maxPacketSize)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(packetBody)))
	out := append(lenBuf[:], packetBody...)

	// Select the QUIC stream to send the packet on (independent of logical streamIDs)
	// Policy: always use stream 0 if pool is disabled, else round-robin on pool
	quicStreamID := protocol.StreamID(0)
	if pool, ok := path.(interface{ NextQUICStreamID() protocol.StreamID }); ok {
		quicStreamID = pool.NextQUICStreamID()
	}

	p.mu.Lock()
	_, pathExists := p.paths[pid]
	p.mu.Unlock()

	if !pathExists {
		p.logger.Warn("Skipping write to unregistered path", "path", pid)
		return protocol.NewPathNotExistsError(pid)
	}

	p.mu.Lock()
	if _, ok := p.pending[pid]; !ok {
		p.pending[pid] = make(map[uint64]*pendingPacket)
	}

	// Collect metadata for ACK logging
	frameInfos := make([]frameMetadata, 0, len(assembled))
	for _, f := range assembled {
		switch v := f.(type) {
		case *protocol.StreamFrame:
			frameInfos = append(frameInfos, frameMetadata{streamID: protocol.StreamID(v.StreamID), offset: v.Offset, size: len(v.Data)})
		default:
			frameInfos = append(frameInfos, frameMetadata{streamID: 0, offset: 0, size: 0})
		}
	}

	p.pending[pid][nextPacketSeq] = &pendingPacket{
		body:      packetBody,
		carrier:   quicStreamID,
		frameInfo: frameInfos,
		attempts:  1,                                                                 // First attempt
		nextRetry: time.Now().Add(time.Duration(p.baseBackoffMs) * time.Millisecond), // Set initial retry time
	}
	pendingCount := len(p.pending[pid])
	p.mu.Unlock()

	p.logger.Debug("submit packet", "path", pid, "frames", len(assembled), "seq", nextPacketSeq, "size", len(out), "quicStreamID", quicStreamID, "pending_count", pendingCount)

	// TRACK: juste avant l'appel à WriteStream
	// fmt.Printf("TRACK PacketBuild: pathID=%d, seq=%d, size=%d, quicStreamID=%d, first_bytes=% x\n", pid, nextPacketSeq, len(out), quicStreamID, out[:min(8, len(out))])
	start := time.Now()
	// p.logger.Debug("WriteStream (SubmitFrame)", "path", pid, "seq", seq, "quicStreamID", quicStreamID, "bytes", len(out), "first_bytes", fmt.Sprintf("% x", out[:min(8, len(out))]))
	_, err = path.WriteStream(quicStreamID, out)
	dur := time.Since(start)

	if err != nil {
		p.logger.Warn("write returned error (submit)", "path", pid, "seq", nextPacketSeq, "dur_ms", dur.Milliseconds(), "err", err)
	} else {
		p.logger.Debug("write returned ok (submit)", "path", pid, "seq", nextPacketSeq, "dur_ms", dur.Milliseconds())
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
		p.logger.Debug("OnAck: no pending packets for path", "pathID", pathID)
		return
	}

	p.logger.Info("Processing ACK", "pathID", pathID, "rangeCount", len(ranges))

	for _, r := range ranges {
		p.logger.Debug("Processing ACK range", "pathID", pathID, "smallest", r.Smallest, "largest", r.Largest)

		for seq := r.Smallest; seq <= r.Largest; seq++ {
			if packet, ok := pending[seq]; ok {
				// Loguer les métadonnées des frames dans ce paquet
				p.logger.Info("ACKed packet", "pathID", pathID, "seq", seq, "frameCount", len(packet.frameInfo))

				for j, frame := range packet.frameInfo {
					p.logger.Info("ACKed frame",
						"pathID", pathID,
						"packetSeq", seq,
						"frameIndex", j,
						"streamID", frame.streamID,
						"offset", frame.offset,
						"size", frame.size)
				}

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
		for seq := r.Smallest; seq <= r.Largest; seq++ {
			if pkt, ok := pending[seq]; ok {
				// Ensure pkt.body exists; if not, try to build from pkt.pkt
				if len(pkt.body) == 0 && pkt.pkt != nil {
					if b, err2 := pkt.pkt.Serialize(); err2 == nil {
						pkt.body = b
					} else {
						p.logger.Warn("failed to rebuild packet body for resend", "seq", seq, "err", err2)
					}
				}
				var lenBuf [4]byte
				binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pkt.body)))
				out := append(lenBuf[:], pkt.body...)
				p.logger.Debug("WriteStream (OnAck resend)", "path", pathID, "seq", seq, "carrier", pkt.carrier, "bytes", len(out), "first_bytes", fmt.Sprintf("% x", out[:min(8, len(out))]))
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
	// Ensure stopCh exists
	if p.stopCh == nil {
		p.stopCh = make(chan struct{})
	}

	p.logger.Info("Starting packet retransmission loop")
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.stopCh:
				p.logger.Debug("Retransmit loop stopping")
				return
			default:
			}
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
					// If packet.body is empty but we have the logical pkt, try to build now
					body := packet.body
					if len(body) == 0 && packet.pkt != nil {
						if b, err := packet.pkt.Serialize(); err == nil {
							body = b
						} else {
							p.logger.Warn("failed to build packet body for retransmit task", "path", pid, "seq", seq, "err", err)
						}
					}
					tasks = append(tasks, task{pathID: pid, seq: seq, body: body, carrier: packet.carrier})
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
					// Utiliser la fonction de retransmission personnalisée
					p.logger.Debug("Using custom resend function", "path", t.pathID, "seq", t.seq)
					err = resend(t.pathID, t.seq, t.body)
				} else if path, ok := p.paths[t.pathID]; ok {
					// Fallback à la méthode par défaut
					p.logger.Debug("Using default resend method", "path", t.pathID, "seq", t.seq, "carrier", t.carrier)
					var lenBuf [4]byte
					binary.BigEndian.PutUint32(lenBuf[:], uint32(len(t.body)))
					out := append(lenBuf[:], t.body...)
					p.logger.Debug("WriteStream (RetransmitLoop)", "path", t.pathID, "seq", t.seq, "carrier", t.carrier, "bytes", len(out), "first_bytes", fmt.Sprintf("% x", out[:min(8, len(out))]))
					_, err = path.WriteStream(t.carrier, out)
				}

				// update retry schedule under lock
				p.mu.Lock()
				if mp, ok := p.pending[t.pathID]; ok {
					if packet, ok2 := mp[t.seq]; ok2 {
						if err != nil {
							// If the error indicates the data stream no longer exists on the path,
							// drop the pending packet instead of scheduling retries. This avoids
							// noisy retry storms when application-level streams are closed.
							if ke, ok := err.(*protocol.KwikError); ok && ke.Code == protocol.ErrDataStreamNotExists {
								p.logger.Warn("Dropping pending packet because data stream no longer exists",
									"path", t.pathID,
									"seq", t.seq,
									"carrier", t.carrier)
								delete(mp, t.seq)
								p.metricsMu.Lock()
								p.droppedCount++
								p.metricsMu.Unlock()
							} else {
								p.logger.Warn("Packet retransmission failed",
									"path", t.pathID,
									"seq", t.seq,
									"carrier", t.carrier,
									"attempt", packet.attempts,
									"err", err)

								// Backoff exponentiel avec jitter pour éviter les tempêtes de retransmission
								backoff := p.baseBackoffMs << (packet.attempts - 1)
								if backoff > p.maxBackoffMs {
									backoff = p.maxBackoffMs
								}
								jitter := backoff / 4
								if jitter > 0 {
									backoff += int(time.Now().UnixNano() % int64(jitter))
								}
								packet.nextRetry = time.Now().Add(time.Duration(backoff) * time.Millisecond)
								p.logger.Debug("Scheduled retry",
									"path", t.pathID,
									"seq", t.seq,
									"nextAttempt", packet.attempts+1,
									"backoffMs", backoff)
							}
						} else {
							p.logger.Debug("Packet retransmitted successfully",
								"path", t.pathID,
								"seq", t.seq,
								"carrier", t.carrier,
								"attempt", packet.attempts)

							// Même après une retransmission réussie, on programme une autre tentative
							// au cas où le ACK ne viendrait pas
							waitTime := p.baseBackoffMs * (1 << packet.attempts)
							if waitTime > p.maxBackoffMs {
								waitTime = p.maxBackoffMs
							}
							packet.nextRetry = time.Now().Add(time.Duration(waitTime) * time.Millisecond)
						}

						// Si on atteint le nombre max de tentatives, on abandonne
						if packet.attempts >= p.maxAttempts {
							p.logger.Warn("Max retransmission attempts reached, dropping packet",
								"path", t.pathID,
								"seq", t.seq,
								"maxAttempts", p.maxAttempts)
							delete(mp, t.seq)
							p.metricsMu.Lock()
							p.droppedCount++
							p.metricsMu.Unlock()
						}
					}
				}
				p.mu.Unlock()
			}

			// Sleep but wake early if stop signal received
			select {
			case <-time.After(100 * time.Millisecond): // Increased from 50ms to 100ms to reduce retransmission frequency
			case <-p.stopCh:
				p.logger.Debug("Retransmit loop stopping (sleep)")
				return
			}
		}
	}()

	// Flush goroutine: periodically try to flush send-stream buffers
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(p.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-p.stopCh:
				p.logger.Debug("Flush goroutine stopping")
				return
			}
			var toFlush []struct {
				sid protocol.StreamID
				pid protocol.PathID
			}

			p.mu.Lock()
			for sid, pid := range p.bufferedStreams {
				toFlush = append(toFlush, struct {
					sid protocol.StreamID
					pid protocol.PathID
				}{sid: sid, pid: pid})
			}
			p.mu.Unlock()

			for _, entry := range toFlush {
				p.mu.Lock()
				path := p.paths[entry.pid]
				p.mu.Unlock()
				if path == nil {
					p.mu.Lock()
					delete(p.bufferedStreams, entry.sid)
					p.mu.Unlock()
					continue
				}
				// Attempt to flush; ignore errors here
				_ = p.SubmitFromSendStream(path, entry.sid)

				// After attempt, check provider state and remove if cleared
				// We need session->stream manager to query provider
				session := path.Session()
				if session == nil {
					p.mu.Lock()
					delete(p.bufferedStreams, entry.sid)
					p.mu.Unlock()
					continue
				}
				if provider, ok := session.StreamManager().GetSendStreamProvider(entry.sid); ok {
					if !provider.HasBuffered() {
						p.mu.Lock()
						delete(p.bufferedStreams, entry.sid)
						p.mu.Unlock()
					} else {
						// leave it for future flush
					}
				} else {
					p.mu.Lock()
					delete(p.bufferedStreams, entry.sid)
					p.mu.Unlock()
				}
			}
		}
	}()

}

// Close stops background goroutines and releases internal resources held by the packer.
// It is safe to call Close multiple times.
func (p *Packer) Close() {
	p.mu.Lock()
	if p.stopCh == nil {
		// nothing started
		p.mu.Unlock()
		return
	}
	// Close stopCh once
	select {
	case <-p.stopCh:
		// already closed
	default:
		close(p.stopCh)
	}
	p.mu.Unlock()

	// Wait for background goroutines to exit
	p.wg.Wait()

	// Release heavy maps to allow GC
	p.mu.Lock()
	p.queues = nil
	p.pending = nil
	p.paths = nil
	p.bufferedStreams = nil
	p.firstEnqueueTime = nil
	p.mu.Unlock()

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

// CancelFramesForStream removes any pending packets that carry frames for the given streamID
// This should be called when the application closes a logical stream to avoid retransmission
// storms for packets that will never be ACKed.
func (p *Packer) CancelFramesForStream(streamID protocol.StreamID) {
	p.mu.Lock()
	defer p.mu.Unlock()

	removed := 0
	for pid, mp := range p.pending {
		for seq, pkt := range mp {
			// inspect frameInfo to see if any frame belongs to streamID
			drop := false
			for _, fi := range pkt.frameInfo {
				if fi.streamID == streamID {
					drop = true
					break
				}
			}
			if drop {
				delete(mp, seq)
				removed++
			}
		}
		if len(mp) == 0 {
			delete(p.pending, pid)
		}
	}
	p.logger.Debug("Cancelled pending packets for stream", "streamID", streamID, "removed", removed)
}

// GetRegisteredPath returns the registered Path for pathID or nil.
func (p *Packer) GetRegisteredPath(pathID protocol.PathID) Path {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paths[pathID]
}

// SubmitFromSendStream pulls frames from a SendStreamProvider and assembles a packet.
func (p *Packer) SubmitFromSendStream(path Path, streamID protocol.StreamID) error {
	pid := path.PathID()

	p.logger.Debug("SubmitFromSendStream enter", "path", pid, "stream", streamID)
	// fmt.Printf("TRACK SubmitFromSendStream: pathID=%d, streamID=%d\n", pid, streamID)
	// Retrieve the provider via session's stream manager
	session := path.Session()
	if session == nil {
		p.logger.Error("no session for path", "path", pid)
		return protocol.NewPathNotExistsError(pid)
	}

	sm := session.StreamManager()
	if sm == nil {
		p.logger.Error("no stream manager for session", "path", pid)
		return protocol.NewPathNotExistsError(pid)
	}

	provider, ok := sm.GetSendStreamProvider(streamID)
	if !ok || provider == nil {
		p.logger.Debug("no send stream provider available", "stream", streamID)
		return nil
	}

	// Attempt to collect up to maxPacketSize worth of frames
	maxBytes := p.maxPacketSize
	frames := provider.PopFrames(maxBytes)
	if len(frames) == 0 {
		p.logger.Debug("no frames returned from provider", "stream", streamID)
		// fmt.Printf("TRACK SubmitFromSendStream: no frames from provider streamID=%d\n", streamID)
		return nil
	}
	// TRACK: juste après le test frames vide
	// fmt.Printf("TRACK SubmitFromSendStream: pathID=%d, streamID=%d, frames=%d\n", pid, streamID, len(frames))

	var pkt protocol.Packet
	pkt.Payload = make([]protocol.Frame, 0, len(frames))
	size := 0
	frameInfos := make([]frameMetadata, 0, len(frames))

	for _, sf := range frames {
		encSize := 1 + 8 + 8 + 4 + len(sf.Data)
		if encSize > p.maxPacketSize {
			p.logger.Error("frame too large for packet", "size", encSize, "max", p.maxPacketSize)
			return protocol.NewFrameTooLargeError(encSize, p.maxPacketSize)
		}
		if size+encSize > p.maxPacketSize {
			break
		}
		pkt.Payload = append(pkt.Payload, sf)
		size += encSize
		frameInfos = append(frameInfos, frameMetadata{streamID: protocol.StreamID(sf.StreamID), offset: sf.Offset, size: len(sf.Data)})
		if sf.Type() == protocol.FrameTypeStream {
			// fmt.Printf("TRACK SubmitFromSendStream Included Frame: %v pathID=%d\n", sf.String(), pid)
		}
	}

	if len(pkt.Payload) == 0 {
		p.logger.Debug("no frames fit in packet", "path", pid)
		return nil
	}

	p.seqMu.Lock()
	seq := p.nextPacketSeq[pid] + 1
	p.nextPacketSeq[pid] = seq
	p.seqMu.Unlock()

	// Définit le numéro de séquence dans le header du paquet
	pkt.Header.PacketNum = seq

	// build packet body
	// fmt.Printf("TRACK SubmitFromSendStream Assembled Packet: id=%d pathID=%d, streamID=%d, frames=%d\n", pkt.Header.PacketNum, pid, streamID, len(pkt.Payload))
	packetBody, err := pkt.Serialize()
	if err != nil {
		p.logger.Error("failed to build packet payload from send stream", "err", err)
		return err
	}
	if len(packetBody) > p.maxPacketSize {
		return protocol.NewFrameTooLargeError(len(packetBody), p.maxPacketSize)
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(packetBody)))
	out := append(lenBuf[:], packetBody...)

	carrier := streamID

	p.mu.Lock()
	_, pathExists := p.paths[pid]
	p.mu.Unlock()

	if !pathExists {
		p.logger.Warn("Skipping write to unregistered path", "path", pid)
		return protocol.NewPathNotExistsError(pid)
	}

	p.mu.Lock()
	if _, ok := p.pending[pid]; !ok {
		p.pending[pid] = make(map[uint64]*pendingPacket)
	}
	p.pending[pid][seq] = &pendingPacket{
		body:      packetBody,
		pkt:       &pkt,
		carrier:   carrier,
		frameInfo: frameInfos,
		attempts:  1,                                                                 // First attempt
		nextRetry: time.Now().Add(time.Duration(p.baseBackoffMs) * time.Millisecond), // Set initial retry time
	}
	p.mu.Unlock()

	// TRACK: juste avant l'appel à WriteStream
	// fmt.Printf("TRACK PacketBuild: pathID=%d, seq=%d, size=%d, carrier=%d, first_bytes=% x\n", pid, seq, len(out), carrier, out[:min(8, len(out))])
	p.logger.Debug("WriteStream (sendBufferedStream)", "path", pid, "seq", seq, "carrier", carrier, "bytes", len(out), "first_bytes", fmt.Sprintf("% x", out[:min(8, len(out))]))
	_, err = path.WriteStream(carrier, out)
	if err != nil {
		p.logger.Error("failed to write packet from send stream", "path", pid, "err", err)
		return err
	}

	// If provider still has buffered data, ensure it's scheduled for flushing
	if provider.HasBuffered() {
		p.mu.Lock()
		p.bufferedStreams[streamID] = pid
		p.mu.Unlock()
	}
	return nil
}

type pendingPacket struct {
	body      []byte
	pkt       *protocol.Packet
	carrier   protocol.StreamID
	attempts  int
	nextRetry time.Time
	frameInfo []frameMetadata // Informations supplémentaires sur les frames dans ce paquet
}

// frameMetadata contient des métadonnées sur une frame incluse dans un paquet
type frameMetadata struct {
	streamID protocol.StreamID
	offset   uint64
	size     int
}
