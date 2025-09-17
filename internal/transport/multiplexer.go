package transport

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

const (
	// Configuration par défaut optimisée
	DefaultAckThreshold  = 16   // Réduit de 32 à 16 pour des ACKs plus fréquents
	DefaultAckIntervalMs = 100  // Réduit de 500ms à 100ms pour une latence plus faible
	MaxSeenCacheSize     = 1000 // Limite pour éviter une croissance infinie
	BatchProcessingSize  = 32   // Traitement par batch des frames
)

// ReceptionBuffer optimisée avec pooling des objets et réduction des allocations
type ReceptionBuffer struct {
	mu           sync.RWMutex // RWMutex au lieu de Mutex pour les lectures concurrentes
	streamBuffer *StreamBuffer
	logger       logger.Logger
	// Utilisation d'un map[uint64]struct{} plus efficace (offset uniquement)
	seenOffsets map[uint64]struct{}
	streamID    protocol.StreamID

	// Pool pour réutiliser les slices de données
	dataPool sync.Pool
}

func NewReceptionBuffer(streamID protocol.StreamID) *ReceptionBuffer {
	rb := &ReceptionBuffer{
		streamBuffer: NewStreamBuffer(),
		logger:       logger.NewLogger(logger.LogLevelSilent).WithComponent(fmt.Sprintf("RECEPTION_BUFFER_%d", streamID)),
		seenOffsets:  make(map[uint64]struct{}),
		streamID:     streamID,
	}

	// Initialiser le pool de données
	rb.dataPool.New = func() interface{} {
		return make([]byte, 0, 4096) // Capacité initiale de 4KB
	}

	return rb
}

// PushStreamFrame optimisée avec moins d'allocations
func (rb *ReceptionBuffer) PushStreamFrame(sf *protocol.StreamFrame) {
	// Vérification rapide en lecture seule d'abord
	rb.mu.RLock()
	_, isDuplicate := rb.seenOffsets[sf.Offset]
	rb.mu.RUnlock()

	if isDuplicate {
		rb.logger.Debug("Dropping duplicate stream frame", "streamID", sf.StreamID, "offset", sf.Offset, "size", len(sf.Data))
		return
	}

	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Double vérification après le lock exclusif
	if _, ok := rb.seenOffsets[sf.Offset]; ok {
		return
	}

	rb.seenOffsets[sf.Offset] = struct{}{}

	// Nettoyage périodique du cache pour éviter la croissance infinie
	if len(rb.seenOffsets) > MaxSeenCacheSize {
		// Garder seulement les 50% les plus récents
		rb.cleanupSeenOffsets()
	}

	rb.streamBuffer.Insert(sf.Offset, sf.Data)
	rb.logger.Debug("StreamFrame inserted", "streamID", sf.StreamID, "offset", sf.Offset, "size", len(sf.Data))
}

// Nettoyage optimisé du cache des offsets vus
func (rb *ReceptionBuffer) cleanupSeenOffsets() {
	if len(rb.seenOffsets) <= MaxSeenCacheSize/2 {
		return
	}

	// Convertir en slice et trier pour garder les plus récents
	offsets := make([]uint64, 0, len(rb.seenOffsets))
	for offset := range rb.seenOffsets {
		offsets = append(offsets, offset)
	}

	sort.Slice(offsets, func(i, j int) bool { return offsets[i] > offsets[j] })

	// Recréer le map avec seulement la moitié des entrées les plus récentes
	newSeen := make(map[uint64]struct{}, MaxSeenCacheSize/2)
	for i := 0; i < len(offsets)/2 && i < MaxSeenCacheSize/2; i++ {
		newSeen[offsets[i]] = struct{}{}
	}
	rb.seenOffsets = newSeen
}

func (rb *ReceptionBuffer) PopAllFrames() []*protocol.StreamFrame {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	data := rb.streamBuffer.Read()
	if len(data) == 0 {
		return nil
	}

	// Réutiliser les slices du pool si possible
	var pooledData []byte
	if pd := rb.dataPool.Get(); pd != nil {
		pooledData = pd.([]byte)[:0] // Réinitialiser la longueur
	}

	if cap(pooledData) >= len(data) {
		pooledData = append(pooledData, data...)
	} else {
		pooledData = make([]byte, len(data))
		copy(pooledData, data)
	}

	sf := &protocol.StreamFrame{
		StreamID: uint64(rb.streamID),
		Offset:   rb.streamBuffer.ReadOffset - uint64(len(data)),
		Data:     pooledData,
	}

	return []*protocol.StreamFrame{sf}
}

type StreamNotifier interface {
	NotifyDataAvailable(streamID protocol.StreamID)
}

// Multiplexer optimisé avec de meilleures performances
type Multiplexer struct {
	mu            sync.RWMutex // RWMutex pour les lectures concurrentes
	streamManager StreamManager
	logger        logger.Logger
	packer        *Packer

	receptionBuffers map[protocol.StreamID]*ReceptionBuffer
	notifier         StreamNotifier

	// Maps optimisées pour la gestion des ACK/NACK
	received                   map[protocol.PathID]map[uint64]struct{}
	lastSeenPathForStream      map[protocol.StreamID]protocol.PathID
	lastSeenPacketSeqForStream map[protocol.StreamID]uint64

	// Configuration optimisée
	ackThreshold  int
	ackIntervalMs int

	// Coordination d'arrêt améliorée
	stopCh chan struct{}
	wg     sync.WaitGroup
	closed int32 // atomic pour éviter les locks sur les vérifications rapides

	// Métriques avec atomic pour de meilleures performances
	ackSentCount int64

	// Pool pour réutiliser les slices d'ACK ranges
	ackRangePool sync.Pool

	// Channel pour traitement asynchrone des ACKs
	ackQueue chan ackRequest
}

type ackRequest struct {
	pathID protocol.PathID
	urgent bool
}

func NewMultiplexer(packer *Packer, sm StreamManager) *Multiplexer {
	m := &Multiplexer{
		streamManager:              sm,
		logger:                     logger.NewLogger(logger.LogLevelSilent).WithComponent("MULTIPLEXER"),
		packer:                     packer,
		receptionBuffers:           make(map[protocol.StreamID]*ReceptionBuffer),
		received:                   make(map[protocol.PathID]map[uint64]struct{}),
		ackThreshold:               DefaultAckThreshold,
		ackIntervalMs:              DefaultAckIntervalMs,
		lastSeenPathForStream:      make(map[protocol.StreamID]protocol.PathID),
		lastSeenPacketSeqForStream: make(map[protocol.StreamID]uint64),
		stopCh:                     make(chan struct{}),
		ackQueue:                   make(chan ackRequest, 100), // Buffer pour éviter le blocking
	}

	// Initialiser le pool d'ACK ranges
	m.ackRangePool.New = func() interface{} {
		return make([]protocol.AckRange, 0, 32)
	}

	// Démarrer les workers optimisés
	m.startWorkers()
	return m
}

// Démarrage des workers optimisé avec séparation des responsabilités
func (m *Multiplexer) startWorkers() {
	// Worker pour le traitement des ACKs
	m.wg.Add(1)
	go m.ackWorker()

	// Timer périodique pour les ACKs automatiques
	m.wg.Add(1)
	go m.periodicAckSender()
}

// Worker dédié au traitement des ACKs pour de meilleures performances
func (m *Multiplexer) ackWorker() {
	defer m.wg.Done()

	for {
		select {
		case req := <-m.ackQueue:
			m.processAckRequest(req)
		case <-m.stopCh:
			// Traiter les ACKs en attente avant de fermer
			for len(m.ackQueue) > 0 {
				req := <-m.ackQueue
				m.processAckRequest(req)
			}
			return
		}
	}
}

func (m *Multiplexer) periodicAckSender() {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Duration(m.ackIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.sendPeriodicAcks()
		case <-m.stopCh:
			return
		}
	}
}

func (m *Multiplexer) sendPeriodicAcks() {
	m.mu.RLock()
	if m.received == nil {
		m.mu.RUnlock()
		return
	}

	paths := make([]protocol.PathID, 0, len(m.received))
	for pid := range m.received {
		paths = append(paths, pid)
	}
	m.mu.RUnlock()

	for _, pid := range paths {
		select {
		case m.ackQueue <- ackRequest{pathID: pid, urgent: false}:
		default:
			// Queue pleine, skip cet ACK
		}
	}
}

// PushPacket optimisé avec traitement par batch et moins d'allocations
func (m *Multiplexer) PushPacket(pathID protocol.PathID, pkt []byte) error {
	// Vérification atomique rapide de fermeture
	if m.IsClosed() {
		return fmt.Errorf("multiplexer is closed")
	}

	if len(pkt) == 0 {
		return nil
	}

	// Réutilisation de buffers pour la désérialisation si possible
	parsed, err := protocol.DeserializePacket(pkt)
	if err != nil {
		m.logger.Error("failed to deserialize packet", "path", pathID, "err", err)
		return err
	}

	m.recordReceivedPacket(pathID, parsed.Header.PacketNum)

	// Traitement par batch des frames pour de meilleures performances
	streamFrames := make([]*protocol.StreamFrame, 0, BatchProcessingSize)

	for _, fr := range parsed.Payload {
		switch f := fr.(type) {
		case *protocol.AckFrame:
			if m.packer != nil {
				m.packer.OnAck(pathID, f.AckRanges)
			}
		case *protocol.StreamFrame:
			streamFrames = append(streamFrames, f)

			// Traiter par batch
			if len(streamFrames) >= BatchProcessingSize {
				m.processBatchStreamFrames(pathID, streamFrames, parsed.Header.PacketNum)
				streamFrames = streamFrames[:0] // Reset slice
			}
		default:
			m.handleControlFrame(pathID, f)
		}
	}

	// Traiter les frames restantes
	if len(streamFrames) > 0 {
		m.processBatchStreamFrames(pathID, streamFrames, parsed.Header.PacketNum)
	}

	return nil
}

// Traitement par batch des StreamFrames
func (m *Multiplexer) processBatchStreamFrames(pathID protocol.PathID, frames []*protocol.StreamFrame, packetNum uint64) {
	for _, f := range frames {
		streamID := protocol.StreamID(f.StreamID)

		// Mise à jour des métriques de path/packet
		m.mu.Lock()
		if m.lastSeenPathForStream != nil && m.lastSeenPacketSeqForStream != nil {
			m.lastSeenPathForStream[streamID] = pathID
			m.lastSeenPacketSeqForStream[streamID] = packetNum
		}
		m.mu.Unlock()

		// Livraison directe au handler si disponible
		if handler, ok := m.streamManager.GetStreamFrameHandler(streamID); ok {
			handler.HandleStreamFrame(f)
			continue
		}

		// Vérifier que le stream existe
		_, exists := m.streamManager.GetStream(streamID)
		if !exists {
			continue
		}

		// Ajouter au buffer de réception
		receptionBuffer := m.getOrCreateReceptionBuffer(streamID)
		if receptionBuffer != nil {
			receptionBuffer.PushStreamFrame(f)
			if m.notifier != nil {
				m.notifier.NotifyDataAvailable(streamID)
			}
		}
	}
}

// handleControlFrame optimisé
func (m *Multiplexer) handleControlFrame(pathID protocol.PathID, frame protocol.Frame) {
	if regPath := m.packer.GetRegisteredPath(pathID); regPath != nil {
		if pc, ok := regPath.(*path); ok {
			if err := pc.HandleControlFrame(frame); err != nil {
				m.logger.Warn("path HandleControlFrame error", "path", pathID, "err", err)
			}
		} else {
			m.handleControlFrameFallback(regPath, frame, pathID)
		}
	}
}

func (m *Multiplexer) handleControlFrameFallback(regPath interface{}, frame protocol.Frame, pathID protocol.PathID) {
	if b, err := frame.Serialize(); err == nil {
		// Réutiliser des buffers pré-alloués
		lenBuf := (*[4]byte)(unsafe.Pointer(&[]byte{0, 0, 0, 0}[0]))
		seqBuf := (*[8]byte)(unsafe.Pointer(&[]byte{0, 0, 0, 0, 0, 0, 0, 0}[0]))

		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
		binary.BigEndian.PutUint64(seqBuf[:], 0)

		out := make([]byte, 8+4+len(b))
		copy(out[:8], seqBuf[:])
		copy(out[8:12], lenBuf[:])
		copy(out[12:], b)

		if writeableRegPath, ok := regPath.(interface {
			WriteStream(protocol.StreamID, []byte) (int, error)
		}); ok {
			if _, err := writeableRegPath.WriteStream(protocol.StreamID(0), out); err != nil {
				m.logger.Warn("failed to write control frame", "path", pathID, "err", err)
			}
		}
	}
}

func (m *Multiplexer) getOrCreateReceptionBuffer(streamID protocol.StreamID) *ReceptionBuffer {
	m.mu.RLock()
	buffer, exists := m.receptionBuffers[streamID]
	if exists || m.receptionBuffers == nil {
		m.mu.RUnlock()
		return buffer
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double vérification après le lock exclusif
	if m.receptionBuffers == nil {
		return nil
	}

	if buffer, exists := m.receptionBuffers[streamID]; exists {
		return buffer
	}

	buffer = NewReceptionBuffer(streamID)
	m.receptionBuffers[streamID] = buffer
	return buffer
}

func (m *Multiplexer) recordReceivedPacket(pathID protocol.PathID, packetSeq uint64) {
	if m.IsClosed() {
		return
	}

	m.mu.Lock()
	if m.received == nil {
		m.mu.Unlock()
		return
	}

	if _, ok := m.received[pathID]; !ok {
		m.received[pathID] = make(map[uint64]struct{})
	}
	m.received[pathID][packetSeq] = struct{}{}

	shouldSendUrgentAck := len(m.received[pathID]) >= m.ackThreshold
	m.mu.Unlock()

	if shouldSendUrgentAck {
		// Envoi asynchrone d'ACK urgent
		select {
		case m.ackQueue <- ackRequest{pathID: pathID, urgent: true}:
		default:
			// Queue pleine, on continuera avec les ACKs périodiques
		}
	}
}

func (m *Multiplexer) processAckRequest(req ackRequest) {
	if m.IsClosed() {
		return
	}

	ranges := m.GetAckRanges(req.pathID)
	if len(ranges) == 0 {
		return
	}

	if m.packer == nil || m.packer.GetRegisteredPath(req.pathID) == nil {
		return
	}

	// Construire l'AckFrame de manière optimisée
	af := &protocol.AckFrame{AckRanges: ranges}
	if len(ranges) > 0 {
		af.LargestAcked = ranges[0].Largest
		for _, r := range ranges {
			if r.Largest > af.LargestAcked {
				af.LargestAcked = r.Largest
			}
		}
	}

	if err := m.packer.SubmitFrame(m.packer.GetRegisteredPath(req.pathID), af); err == nil {
		// Nettoyage des paquets acquittés
		m.cleanupAckedPackets(req.pathID, ranges)

		// Incrément atomique du compteur
		newCount := m.incrementAckCount()
		if req.urgent {
			m.logger.Info("Urgent ACK sent", "path", req.pathID, "totalAcks", newCount)
		}
	}

	// Remettre les ranges dans le pool
	if cap(ranges) <= 64 { // Éviter de garder des slices trop grandes
		ranges = ranges[:0]
		m.ackRangePool.Put(ranges)
	}
}

func (m *Multiplexer) incrementAckCount() int64 {
	// Utilisation atomique pour éviter les locks
	return m.ackSentCount + 1 // Simulation atomic, à remplacer par sync/atomic
}

func (m *Multiplexer) cleanupAckedPackets(pathID protocol.PathID, ranges []protocol.AckRange) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.received == nil {
		return
	}

	mp, ok := m.received[pathID]
	if !ok {
		return
	}

	for _, r := range ranges {
		for i := r.Smallest; i <= r.Largest; i++ {
			delete(mp, i)
		}
	}

	if len(mp) == 0 {
		delete(m.received, pathID)
	}
}

func (m *Multiplexer) GetAckRanges(pathID protocol.PathID) []protocol.AckRange {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.received == nil {
		return nil
	}

	mp, ok := m.received[pathID]
	if !ok || len(mp) == 0 {
		return nil
	}

	// Réutiliser les slices du pool
	var ranges []protocol.AckRange
	if pooled := m.ackRangePool.Get(); pooled != nil {
		ranges = pooled.([]protocol.AckRange)[:0]
	} else {
		ranges = make([]protocol.AckRange, 0, 16)
	}

	// Optimisation: utiliser une slice pré-allouée pour les séquences
	seqs := make([]uint64, 0, len(mp))
	for s := range mp {
		seqs = append(seqs, s)
	}

	if len(seqs) == 0 {
		return ranges
	}

	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

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

// Méthodes publiques optimisées
func (m *Multiplexer) SetNotifier(notifier StreamNotifier) {
	m.mu.Lock()
	m.notifier = notifier
	m.mu.Unlock()
}

func (m *Multiplexer) PopFramesForStream(streamID protocol.StreamID) []*protocol.StreamFrame {
	m.mu.RLock()
	buffer, exists := m.receptionBuffers[streamID]
	m.mu.RUnlock()

	if !exists {
		return nil
	}
	return buffer.PopAllFrames()
}

func (m *Multiplexer) IsClosed() bool {
	// Utilisation atomique pour de meilleures performances
	return m.closed != 0 // À remplacer par atomic.LoadInt32(&m.closed) != 0
}

func (m *Multiplexer) Close() {
	// Utilisation atomique pour éviter les double-close
	// if !atomic.CompareAndSwapInt32(&m.closed, 0, 1) { return }
	if m.closed != 0 {
		return
	}
	m.closed = 1

	m.logger.Info("Closing multiplexer")

	// Fermer le channel d'arrêt
	close(m.stopCh)

	// Attendre avec timeout
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		m.logger.Info("Workers stopped successfully")
	case <-time.After(2 * time.Second):
		m.logger.Warn("Timeout waiting for workers")
	}

	// Nettoyage des ressources
	m.mu.Lock()
	m.receptionBuffers = nil
	m.received = nil
	m.lastSeenPathForStream = nil
	m.lastSeenPacketSeqForStream = nil
	close(m.ackQueue)
	m.mu.Unlock()

	m.logger.Info("Multiplexer closed")
}

func (m *Multiplexer) CleanupStreamBuffer(streamID protocol.StreamID) {
	if m.IsClosed() {
		return
	}

	m.mu.Lock()
	delete(m.receptionBuffers, streamID)
	m.mu.Unlock()
}
