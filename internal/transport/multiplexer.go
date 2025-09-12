package transport

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

// ReceptionBuffer - Tampon de réception simple pour un stream
type ReceptionBuffer struct {
	mu sync.Mutex
	// streamBuffer holds data chunks indexed by offset for contiguous reassembly
	streamBuffer *StreamBuffer
	logger       logger.Logger
	// seen holds keys of frames already stored to avoid duplicates (key: "streamID:offset")
	seen     map[string]struct{}
	streamID protocol.StreamID
}

func NewReceptionBuffer(streamID protocol.StreamID) *ReceptionBuffer {
	return &ReceptionBuffer{
		streamBuffer: NewStreamBuffer(),
		logger:       logger.NewLogger(logger.LogLevelSilent).WithComponent(fmt.Sprintf("RECEPTION_BUFFER_%d", streamID)),
		seen:         make(map[string]struct{}),
		streamID:     streamID,
	}
}

// (legacy PushFrame removed) Use PushStreamFrame for new StreamFrame objects

// PushStreamFrame accepts the new-style StreamFrame directly to avoid conversions.
func (rb *ReceptionBuffer) PushStreamFrame(sf *protocol.StreamFrame) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	key := fmt.Sprintf("%d:%d", sf.StreamID, sf.Offset)
	if _, ok := rb.seen[key]; ok {
		rb.logger.Debug("Dropping duplicate stream frame (seen key)", "streamID", sf.StreamID, "offset", sf.Offset, "size", len(sf.Data))
		return
	}
	rb.seen[key] = struct{}{}

	rb.streamBuffer.Insert(sf.Offset, sf.Data)
	rb.logger.Debug("StreamFrame inserted into stream buffer", "streamID", sf.StreamID, "offset", sf.Offset, "size", len(sf.Data))
}

// PopAllFrames returns all assembled StreamFrames (as a single STREAM frame containing contiguous data)
func (rb *ReceptionBuffer) PopAllFrames() []*protocol.StreamFrame {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Read all contiguous data starting at ReadOffset
	data := rb.streamBuffer.Read()
	// Reset seen map to avoid unbounded growth
	rb.seen = make(map[string]struct{})

	if len(data) == 0 {
		return nil
	}
	// Wrap contiguous data into a single StreamFrame
	sf := &protocol.StreamFrame{StreamID: uint64(rb.streamID), Offset: rb.streamBuffer.ReadOffset - uint64(len(data)), Data: data}
	return []*protocol.StreamFrame{sf}
}

// StreamNotifier interface pour notifier l'agrégateur
type StreamNotifier interface {
	NotifyDataAvailable(streamID protocol.StreamID)
}

// Multiplexer simplifié - ne fait que de la réception et notification
type Multiplexer struct {
	mu            sync.RWMutex
	streamManager StreamManager
	logger        logger.Logger
	packer        *Packer

	// Tampons de réception par stream
	receptionBuffers map[protocol.StreamID]*ReceptionBuffer

	// Interface de notification vers les agrégateurs
	notifier StreamNotifier

	// Champs conservés pour la gestion des ACK/NACK
	received                   map[protocol.PathID]map[uint64]struct{}
	lastSeenPathForStream      map[protocol.StreamID]protocol.PathID
	lastSeenPacketSeqForStream map[protocol.StreamID]uint64
	ackThreshold               int
	ackIntervalMs              int
	// stop channel to terminate background goroutines
	stopCh chan struct{}
	wg     sync.WaitGroup

	// Métriques
	metricsMu    sync.Mutex
	ackSentCount int64
}

func NewMultiplexer(packer *Packer, sm StreamManager) *Multiplexer {
	m := &Multiplexer{
		streamManager:              sm,
		logger:                     logger.NewLogger(logger.LogLevelSilent).WithComponent("MULTIPLEXER"),
		packer:                     packer,
		receptionBuffers:           make(map[protocol.StreamID]*ReceptionBuffer),
		received:                   make(map[protocol.PathID]map[uint64]struct{}),
		ackThreshold:               32,
		ackIntervalMs:              500,
		lastSeenPathForStream:      make(map[protocol.StreamID]protocol.PathID),
		lastSeenPacketSeqForStream: make(map[protocol.StreamID]uint64),
		stopCh:                     make(chan struct{}),
	}

	// Démarrer la boucle d'envoi d'ACK immédiatement pour garantir que les ACKs sont envoyés
	// périodiquement dès que des paquets sont reçus
	m.logger.Info("Starting ACK loop with interval", "intervalMs", m.ackIntervalMs)
	m.StartAckLoop(m.ackIntervalMs)

	return m
}

// SetNotifier configure l'interface de notification
func (m *Multiplexer) SetNotifier(notifier StreamNotifier) {
	m.mu.Lock()
	m.notifier = notifier
	m.mu.Unlock()
}

func (m *Multiplexer) PushPacket(pathID protocol.PathID, pkt []byte) error {
	if len(pkt) == 0 {
		return nil
	}

	parsed, err := protocol.DeserializePacket(pkt)
	if err != nil {
		m.logger.Error("failed to deserialize packet", "path", pathID, "err", err)
		return err
	}
	// fmt.Printf("TRACK PushPacket: Deserialized packet pathID=%d packetNum=%d frameCount=%d\n", pathID, parsed.Header.PacketNum, len(parsed.Payload))
	m.recordReceivedPacket(pathID, parsed.Header.PacketNum)

	for _, fr := range parsed.Payload {
		switch f := fr.(type) {
		case *protocol.AckFrame:
			// fmt.Printf("TRACK PushPacket: Received Ack frame: %v\n", fr)
			if m.packer != nil {
				m.packer.OnAck(pathID, f.AckRanges)
			}
		case *protocol.StreamFrame:
			// fmt.Printf("TRACK PushPacket: Received Stream frame: %v\n", fr)
			streamID := protocol.StreamID(f.StreamID)
			// Record last seen path/packet for this stream
			m.mu.Lock()
			m.lastSeenPathForStream[streamID] = pathID
			m.lastSeenPacketSeqForStream[streamID] = parsed.Header.PacketNum
			m.mu.Unlock()

			// Log whether this is from primary or relay path
			// fmt.Printf("TRACK StreamFrame: streamID=%d pathID=%d offset=%d dataLen=%d\n", streamID, pathID, f.Offset, len(f.Data))

			// Direct delivery to stream handler if present
			if handler, ok := m.streamManager.GetStreamFrameHandler(streamID); ok {
				// fmt.Printf("TRACK StreamFrame: Delivering to handler streamID=%d pathID=%d\n", streamID, pathID)
				handler.HandleStreamFrame(f)
				m.logger.Debug("Delivered frame directly to stream handler", "streamID", streamID, "offset", f.Offset, "size", len(f.Data))
				continue
			}

			// Ensure stream exists
			_, exists := m.streamManager.GetStream(streamID)
			if !exists {
				// fmt.Printf("TRACK StreamFrame: Unregistered stream streamID=%d pathID=%d - IGNORING\n", streamID, pathID)
				m.logger.Warn("Frame for unregistered stream received, ignoring", "streamID", streamID)
				continue
			}

			// fmt.Printf("TRACK StreamFrame: Adding to buffer streamID=%d pathID=%d offset=%d\n", streamID, pathID, f.Offset)
			receptionBuffer := m.getOrCreateReceptionBuffer(streamID)
			receptionBuffer.PushStreamFrame(f)
			if m.notifier != nil {
				m.notifier.NotifyDataAvailable(streamID)
			}
			m.logger.Debug("Frame added to reception buffer and notifier called", "streamID", streamID, "offset", f.Offset, "size", len(f.Data))
		default:
			// Control frames (Handshake, Ping, AddPath, RelayData, etc.)
			// fmt.Printf("TRACK PushPacket: Received Control frame: %v\n", fr)
			if regPath := m.packer.GetRegisteredPath(pathID); regPath != nil {
				m.logger.Debug("dispatching control frame to path handler", "path", pathID)
				if pc, ok := regPath.(*path); ok {
					if err := pc.HandleControlFrame(f); err != nil {
						m.logger.Warn("path HandleControlFrame returned error", "path", pathID, "err", err)
					}
				} else {
					// Fallback: serialize frame bytes and write to control stream
					if b, serr := f.Serialize(); serr == nil {
						var lenBuf [4]byte
						binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
						var seqBuf [8]byte
						binary.BigEndian.PutUint64(seqBuf[:], uint64(0))
						out := append(seqBuf[:], append(lenBuf[:], b...)...)
						if _, err := regPath.WriteStream(protocol.StreamID(0), out); err != nil {
							m.logger.Warn("failed to write control frame to stream 0", "path", pathID, "err", err)
						}
					} else {
						m.logger.Warn("failed to serialize control frame for fallback write", "err", serr)
					}
				}
			} else {
				m.logger.Warn("no registered path found for control frame", "path", pathID)
			}
		}
	}
	return nil
}

// getOrCreateReceptionBuffer obtient ou crée un tampon de réception
func (m *Multiplexer) getOrCreateReceptionBuffer(streamID protocol.StreamID) *ReceptionBuffer {
	m.mu.Lock()
	defer m.mu.Unlock()

	buffer, exists := m.receptionBuffers[streamID]
	if !exists {
		buffer = NewReceptionBuffer(streamID)
		m.receptionBuffers[streamID] = buffer
		m.logger.Debug("Created new reception buffer", "streamID", streamID)
	}
	return buffer
}

// PopFramesForStream retourne toutes les frames en attente pour un stream et vide son tampon
func (m *Multiplexer) PopFramesForStream(streamID protocol.StreamID) []*protocol.StreamFrame {
	m.mu.RLock()
	buffer, exists := m.receptionBuffers[streamID]
	m.mu.RUnlock()

	if !exists {
		return nil
	}

	return buffer.PopAllFrames()
}

// recordReceivedPacket enregistre la réception d'un paquet pour un chemin donné
func (m *Multiplexer) recordReceivedPacket(pathID protocol.PathID, packetSeq uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.received[pathID]; !ok {
		m.received[pathID] = make(map[uint64]struct{})
	}
	m.received[pathID][packetSeq] = struct{}{}

	if len(m.received[pathID]) >= m.ackThreshold {
		go m.sendAckNow(pathID)
	}
}

// sendAckNow envoie immédiatement un ACK pour un chemin donné
func (m *Multiplexer) sendAckNow(pid protocol.PathID) {
	ranges := m.GetAckRanges(pid)
	if len(ranges) == 0 {
		m.logger.Debug("No ACK ranges to send", "path", pid)
		return
	}

	m.logger.Debug("Preparing to send ACK", "path", pid, "rangeCount", len(ranges))

	// Log detailed ranges for debugging
	for i, r := range ranges {
		m.logger.Debug("ACK range detail", "path", pid, "rangeIndex", i, "smallest", r.Smallest, "largest", r.Largest)
	}

	if m.packer == nil {
		m.logger.Warn("Cannot send ACK: packer not initialized")
		return
	}

	regPath := m.packer.GetRegisteredPath(pid)
	if regPath == nil {
		m.logger.Warn("Cannot send ACK: path not found", "path", pid)
		return
	}

	// Build AckFrame using new AckRange fields (Smallest/Largest)
	af := &protocol.AckFrame{AckRanges: make([]protocol.AckRange, 0, len(ranges))}
	var largestAcked uint64
	for _, r := range ranges {
		af.AckRanges = append(af.AckRanges, protocol.AckRange{Smallest: r.Smallest, Largest: r.Largest})
		if r.Largest > largestAcked {
			largestAcked = r.Largest
		}
	}
	af.LargestAcked = largestAcked
	af.AckDelay = 0 // Optionnel: à calculer si tu veux mesurer le délai d'ACK

	m.logger.Info("Sending ACK frame (new)", "path", pid, "rangeCount", len(ranges))
	if err := m.packer.SubmitFrame(regPath, af); err == nil {
		m.metricsMu.Lock()
		m.ackSentCount++
		currentCount := m.ackSentCount
		m.metricsMu.Unlock()
		m.logger.Info("ACK frame sent successfully", "path", pid, "totalAcksSent", currentCount)

		// Cleanup acknowledged packets
		m.mu.Lock()
		if mp, ok := m.received[pid]; ok {
			for _, r := range ranges {
				for i := r.Smallest; i <= r.Largest; i++ {
					delete(mp, i)
				}
			}
			if len(mp) == 0 {
				delete(m.received, pid)
				m.logger.Debug("Cleared all ACKed packets for path", "path", pid)
			} else {
				m.logger.Debug("Remaining unACKed packets after cleanup", "path", pid, "count", len(mp))
			}
		}
		m.mu.Unlock()
	} else {
		m.logger.Error("Failed to send ACK frame", "path", pid, "err", err)
	}
}

// GetAckRanges convertit les numéros de séquence reçus en plages compactes pour un ACK
func (m *Multiplexer) GetAckRanges(pathID protocol.PathID) []protocol.AckRange {
	m.mu.RLock()
	mp, ok := m.received[pathID]
	if !ok || len(mp) == 0 {
		m.mu.RUnlock()
		return nil
	}
	seqs := make([]uint64, 0, len(mp))
	for s := range mp {
		seqs = append(seqs, s)
	}
	m.mu.RUnlock()

	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	if len(seqs) == 0 {
		return nil
	}

	var ranges []protocol.AckRange
	start, prev := seqs[0], seqs[0]
	for i := 1; i < len(seqs); i++ {
		if seqs[i] == prev+1 {
			prev = seqs[i]
			continue
		}
		ranges = append(ranges, protocol.AckRange{Smallest: start, Largest: prev})
		start, prev = seqs[i], seqs[i]
	}
	ranges = append(ranges, protocol.AckRange{Smallest: start, Largest: prev})
	return ranges
}

// StartAckLoop démarre la boucle d'ACK périodique
func (m *Multiplexer) StartAckLoop(intervalMs int) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
		defer ticker.Stop()

		loopCount := 0
		for {
			select {
			case <-ticker.C:
			case <-m.stopCh:
				m.logger.Debug("ACK loop stopping")
				return
			}
			loopCount++

			m.mu.RLock()
			paths := make([]protocol.PathID, 0, len(m.received))
			for pid := range m.received {
				paths = append(paths, pid)
			}
			m.mu.RUnlock()

			for _, pid := range paths {
				m.sendAckNow(pid)
			}
		}
	}()
}

// Close stops the ack loop and clears reception buffers. Safe to call multiple times.
func (m *Multiplexer) Close() {
	m.mu.Lock()
	if m.stopCh == nil {
		m.mu.Unlock()
		return
	}
	select {
	case <-m.stopCh:
		// already closed
	default:
		close(m.stopCh)
	}
	m.mu.Unlock()

	m.wg.Wait()

	// Release maps to allow GC
	m.mu.Lock()
	m.receptionBuffers = nil
	m.received = nil
	m.lastSeenPathForStream = nil
	m.lastSeenPacketSeqForStream = nil
	m.mu.Unlock()
}

// CleanupStreamBuffer nettoie le buffer d'un stream fermé
func (m *Multiplexer) CleanupStreamBuffer(streamID protocol.StreamID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.receptionBuffers, streamID)
	m.logger.Debug("Cleaned up reception buffer", "streamID", streamID)
}
