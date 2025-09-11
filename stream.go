package kwik

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

// StreamAggregatorRegistry gère les agrégateurs de tous les streams
// StreamAggregatorRegistry removed: using direct SendStream/RecvStream model

// ---------- QUIC-like stream helpers ----------
// RecvStream handles incoming STREAM frames and provides a Read() that blocks like quic.Stream
type RecvStream struct {
	mu             sync.Mutex
	streamID       uint64
	expectedOffset uint64
	buffered       map[uint64][]byte
	readBuf        bytes.Buffer
	finReceived    bool
	notifyCh       chan struct{}
	readActive     bool
}

func NewRecvStream(id protocol.StreamID) *RecvStream {
	return &RecvStream{
		streamID: uint64(id),
		buffered: make(map[uint64][]byte),
		notifyCh: make(chan struct{}, 1),
	}
}

func (s *RecvStream) handleFrame(f *protocol.StreamFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if f.Offset == s.expectedOffset {
		s.readBuf.Write(f.Data)
		s.expectedOffset += uint64(len(f.Data))

		for {
			if buf, ok := s.buffered[s.expectedOffset]; ok {
				s.readBuf.Write(buf)
				s.expectedOffset += uint64(len(buf))
				delete(s.buffered, s.expectedOffset)
			} else {
				break
			}
		}

		if f.Fin {
			s.finReceived = true
		}
	} else if f.Offset > s.expectedOffset {
		s.buffered[f.Offset] = append([]byte(nil), f.Data...)
	}
	// Notify any blocked Read
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

func (s *RecvStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.readActive {
		s.mu.Unlock()
		return 0, fmt.Errorf("concurrent Read not allowed")
	}
	s.readActive = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.readActive = false
		s.mu.Unlock()
	}()

	for {
		s.mu.Lock()
		if s.readBuf.Len() > 0 {
			n, err := s.readBuf.Read(p)
			s.mu.Unlock()
			return n, err
		}
		if s.finReceived {
			s.mu.Unlock()
			return 0, io.EOF
		}
		notifyCh := s.notifyCh
		s.mu.Unlock()
		<-notifyCh
	}
}

func (s *RecvStream) GetReadOffset() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expectedOffset - uint64(s.readBuf.Len())
}

func (s *RecvStream) SetReadOffset(offset uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reset buffer and adjust expectedOffset
	s.readBuf.Reset()
	s.expectedOffset = offset
	s.buffered = make(map[uint64][]byte)
}

// HandleStreamFrame implements transport.StreamFrameHandler
func (s *RecvStream) HandleStreamFrame(f *protocol.StreamFrame) {
	s.handleFrame(f)
}

// SendStream buffers outgoing data and provides frames for packetization
type SendStream struct {
	mu          sync.Mutex
	streamID    uint64
	writeOffset uint64
	sendBuffer  [][]byte
	finSent     bool
	baseOffset  uint64 // Tracks the offset of the first buffered byte
	needsFlush  bool   // Indicates if flush is needed
}

func NewSendStream(id protocol.StreamID) *SendStream {
	return &SendStream{
		streamID:   uint64(id),
		sendBuffer: make([][]byte, 0),
		baseOffset: 0,
	}
}

func (s *SendStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := make([]byte, len(p))
	copy(frame, p)
	s.sendBuffer = append(s.sendBuffer, frame)
	s.writeOffset += uint64(len(frame))
	s.needsFlush = true
	return len(p), nil
}

func (s *SendStream) PopFrames(maxBytes int) []*protocol.StreamFrame {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.sendBuffer) == 0 {
		return nil
	}

	var frames []*protocol.StreamFrame
	currentOffset := s.baseOffset
	bytesConsumed := 0

	for len(s.sendBuffer) > 0 && maxBytes > 0 {
		data := s.sendBuffer[0]
		frameSize := len(data)

		if frameSize > maxBytes {
			// Split the frame
			data = data[:maxBytes]
			s.sendBuffer[0] = s.sendBuffer[0][maxBytes:]
			frameSize = maxBytes
		} else {
			// Consume entire frame
			s.sendBuffer = s.sendBuffer[1:]
		}

		frame := &protocol.StreamFrame{
			StreamID: s.streamID,
			Offset:   currentOffset,
			Data:     data,
			Fin:      s.finSent && len(s.sendBuffer) == 0,
		}
		frames = append(frames, frame)

		currentOffset += uint64(frameSize)
		bytesConsumed += frameSize
		maxBytes -= frameSize
	}

	// Update base offset for remaining data
	s.baseOffset += uint64(bytesConsumed)

	// Clear flush flag if all data consumed
	if len(s.sendBuffer) == 0 {
		s.needsFlush = false
	}

	return frames
}

// HasBuffered reports whether there is any data waiting to be sent
func (s *SendStream) HasBuffered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sendBuffer) > 0
}

// NeedsFlush indicates if immediate flushing is recommended
func (s *SendStream) NeedsFlush() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsFlush
}

// aggregator removed

// Batching constants for intelligent flushing
const (
	// FlushThresholdBytes - flush when this many bytes are buffered
	FlushThresholdBytes = 64 * 1024 // 64KB
	// FlushThresholdFrames - flush when this many frames are buffered
	FlushThresholdFrames = 10
	// LargeWriteThreshold - always flush immediately for large writes
	LargeWriteThreshold = 32 * 1024 // 32KB
)

// StreamImpl adapté pour le nouveau système
type StreamImpl struct {
	id             protocol.StreamID
	remoteStreamID protocol.StreamID
	paths          map[protocol.PathID]transport.Path
	mu             sync.RWMutex
	primaryPathID  protocol.PathID
	writeOffset    uint64
	logger         logger.Logger
	streamManager  *streamManagerImpl
	// QUIC-like send/recv streams
	recv *RecvStream
	send *SendStream
}

// Ensure StreamImpl implements Stream
var _ Stream = (*StreamImpl)(nil)

func NewStream(id protocol.StreamID, sm *streamManagerImpl) *StreamImpl {
	s := &StreamImpl{
		id:            id,
		logger:        logger.NewLogger(logger.LogLevelSilent).WithComponent("STREAM_IMPL"),
		paths:         make(map[protocol.PathID]transport.Path),
		streamManager: sm,
		writeOffset:   0,
	}

	// Initialize QUIC-like send/recv streams
	s.recv = NewRecvStream(id)
	s.send = NewSendStream(id)

	return s
}

// Read implémente io.Reader en déléguant à l'agrégateur
func (s *StreamImpl) Read(p []byte) (int, error) {
	if s.recv == nil {
		return 0, fmt.Errorf("no recv stream available for stream %d", s.id)
	}
	return s.recv.Read(p)
}

// GetReadOffset retourne l'offset actuel de lecture du stream
func (s *StreamImpl) GetReadOffset() uint64 {
	if s.recv != nil {
		return s.recv.GetReadOffset()
	}
	return 0
}

// SetReadOffset configure l'offset de lecture (pour les seeks)
func (s *StreamImpl) SetReadOffset(offset uint64) error {
	if s.recv != nil {
		s.recv.SetReadOffset(offset)
	}
	return nil
}

// Write implémente io.Writer
func (s *StreamImpl) Write(p []byte) (int, error) {

	s.logger.Info("Write",
		"streamID", s.id,
		"size", len(p),
		"primaryPath", s.primaryPathID,
		"pathCount", len(s.paths),
		"first_bytes", fmt.Sprintf("% x", p[:min(8, len(p))]))
	// fmt.Printf("TRACK Write: streamID=%d, size=%d, primaryPath=%d, pathCount=%d, first_bytes=% x\n",
	// s.id, len(p), s.primaryPathID, len(s.paths), p[:min(8, len(p))])
	if len(p) == 0 {
		return 0, nil
	}

	// Read primary path and current offset under read-lock.

	s.mu.RLock()
	primaryPath, ok := s.paths[s.primaryPathID]
	s.mu.RUnlock()

	if !ok {
		s.logger.Error("No primary path set for writing", "streamID", s.id)
		return 0, protocol.NewPathNotExistsError(s.primaryPathID)
	}

	// Buffer the data into the SendStream and let the packer pull frames
	if s.send == nil {
		s.logger.Error("No send stream available for stream", "streamID", s.id)
		return 0, fmt.Errorf("no send stream available for stream %d", s.id)
	}

	n, err := s.send.Write(p)
	if err != nil {
		s.logger.Error("Failed to buffer data into send stream", "streamID", s.id, "err", err)
		return 0, err
	}

	s.logger.Info("SendStream buffered",
		"streamID", s.id,
		"buffered_count", len(s.send.sendBuffer),
		"writeOffset", s.send.writeOffset)

	// fmt.Printf("TRACK SendStream buffered: streamID=%d, buffered_count=%d, writeOffset=%d\n",
	// s.id, len(s.send.sendBuffer), s.send.writeOffset)

	// Intelligent batching: only flush if needed based on conditions
	shouldFlush := s.shouldFlushSendStream(len(p))

	if shouldFlush {
		// Ask the path to submit buffered send-stream frames to the packer.
		if err := primaryPath.SubmitSendStream(s.id); err != nil {
			s.logger.Error("Failed to submit frames from send stream to packer",
				"streamID", s.id,
				"pathID", primaryPath.PathID(),
				"error", err)
			return 0, fmt.Errorf("failed to submit frames from send stream: %w", err)
		}
		s.logger.Debug("Flushed send stream", "streamID", s.id, "reason", "batching_threshold")
	} else {
		s.logger.Debug("Buffered without immediate flush", "streamID", s.id, "buffered_bytes", len(s.send.sendBuffer))
	}

	// Advance the write offset only after successful submission.
	s.mu.Lock()
	s.writeOffset += uint64(n)
	s.mu.Unlock()

	s.logger.Info("Write completed",
		"streamID", s.id,
		"size", n,
		"writeOffset", s.writeOffset)
	// fmt.Printf("TRACK Write completed: streamID=%d, size=%d, writeOffset=%d\n",
	// s.id, n, s.writeOffset)
	return n, nil
}

// Close ferme le stream
func (s *StreamImpl) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var lastErr error
	for _, path := range s.paths {
		if err := path.CloseStream(s.id); err != nil {
			s.logger.Error("failed to close stream on path", "streamID", s.id, "pathID", path.PathID(), "error", err)
			lastErr = err
		}
	}

	if s.streamManager != nil {
		s.streamManager.RemoveStream(s.id)
	}

	return lastErr
}

// StreamID retourne l'ID du stream
func (s *StreamImpl) StreamID() protocol.StreamID {
	return s.id
}

// SetWriteOffset configure l'offset d'écriture
func (s *StreamImpl) SetWriteOffset(offset uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeOffset = offset
	return nil
}

// GetWriteOffset retourne l'offset d'écriture
func (s *StreamImpl) GetWriteOffset() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writeOffset
}

// SetRemoteStreamID configure l'ID du stream distant
func (s *StreamImpl) SetRemoteStreamID(remoteStreamID protocol.StreamID) error {
	s.remoteStreamID = remoteStreamID
	return nil
}

// RemoteStreamID retourne l'ID du stream distant
func (s *StreamImpl) RemoteStreamID() protocol.StreamID {
	return s.remoteStreamID
}

// SetPrimaryPath désigne le chemin principal pour les écritures
func (s *StreamImpl) SetPrimaryPath(pid protocol.PathID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primaryPathID = pid
}

// GetPrimaryPath retourne l'ID du chemin principal
func (s *StreamImpl) GetPrimaryPath() protocol.PathID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primaryPathID
}

// AddPath attache un chemin à ce stream
func (s *StreamImpl) AddPath(path transport.Path) error {
	if path == nil {
		return protocol.NewInvalidPathIDError(0)
	}
	pid := path.PathID()

	// Vérifier si le chemin est déjà enregistré
	s.mu.RLock()
	if _, ok := s.paths[pid]; ok {
		s.mu.RUnlock()
		s.logger.Debug("Path already registered with stream", "streamID", s.id, "pathID", pid)
		return nil
	}
	s.mu.RUnlock()

	// Enregistrer le chemin avec le packer si disponible
	var packer *transport.Packer
	if session := path.Session(); session != nil {
		packer = session.Packer()
	}

	if packer != nil {
		packer.RegisterPath(path)
	}

	// Finaliser l'enregistrement
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double vérification sous verrou d'écriture
	if _, ok := s.paths[pid]; ok {
		s.logger.Debug("Path already registered with stream (race detected)", "streamID", s.id, "pathID", pid)
		return nil
	}

	s.paths[pid] = path

	// Si c'est le premier chemin, en faire le chemin principal
	if s.primaryPathID == 0 {
		s.primaryPathID = pid
		s.logger.Debug("Set primary path for stream", "streamID", s.id, "pathID", pid)
	}

	s.logger.Debug("Added path to stream", "streamID", s.id, "pathID", pid)
	return nil
}

// RemovePath détache un chemin de ce stream
func (s *StreamImpl) RemovePath(pathID protocol.PathID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.paths[pathID]; !ok {
		s.logger.Debug("Path not registered with stream, nothing to remove", "streamID", s.id, "pathID", pathID)
		return
	}

	delete(s.paths, pathID)
	s.logger.Debug("Removed path from stream", "streamID", s.id, "pathID", pathID)

	// Si c'était le chemin principal, en choisir un autre
	if s.primaryPathID == pathID {
		s.primaryPathID = 0
		for pid := range s.paths {
			s.primaryPathID = pid
			s.logger.Debug("Set new primary path for stream after removal", "streamID", s.id, "pathID", pid)
			break
		}
	}
}

// GetPath retourne le chemin s'il est présent
func (s *StreamImpl) GetPath(pid protocol.PathID) (transport.Path, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.paths[pid]
	return p, ok
}

// shouldFlushSendStream determines if the send stream should be flushed immediately
func (s *StreamImpl) shouldFlushSendStream(writeSize int) bool {
	if s.send == nil {
		return false
	}

	// Always flush large writes immediately
	if writeSize >= LargeWriteThreshold {
		return true
	}

	// Check if buffer size threshold exceeded
	bufferedBytes := s.getBufferedBytes()
	if bufferedBytes >= FlushThresholdBytes {
		return true
	}

	// Check if frame count threshold exceeded
	frameCount := s.getBufferedFrameCount()
	if frameCount >= FlushThresholdFrames {
		return true
	}

	// Check if the send stream explicitly needs flushing
	if s.send.NeedsFlush() {
		return true
	}

	return false
}

// getBufferedBytes returns the total bytes waiting in the send buffer
func (s *StreamImpl) getBufferedBytes() int {
	if s.send == nil {
		return 0
	}

	s.send.mu.Lock()
	defer s.send.mu.Unlock()

	totalBytes := 0
	for _, frame := range s.send.sendBuffer {
		totalBytes += len(frame)
	}
	return totalBytes
}

// getBufferedFrameCount returns the number of frames waiting in the send buffer
func (s *StreamImpl) getBufferedFrameCount() int {
	if s.send == nil {
		return 0
	}

	s.send.mu.Lock()
	defer s.send.mu.Unlock()

	return len(s.send.sendBuffer)
}

// Flush forces immediate submission of all buffered data to the packer
func (s *StreamImpl) Flush() error {
	s.mu.RLock()
	primaryPath, ok := s.paths[s.primaryPathID]
	s.mu.RUnlock()

	if !ok {
		return protocol.NewPathNotExistsError(s.primaryPathID)
	}

	if s.send == nil || !s.send.HasBuffered() {
		return nil // Nothing to flush
	}

	err := primaryPath.SubmitSendStream(s.id)
	if err != nil {
		s.logger.Error("Failed to flush send stream", "streamID", s.id, "err", err)
		return fmt.Errorf("failed to flush send stream: %w", err)
	}

	s.logger.Debug("Explicitly flushed send stream", "streamID", s.id)
	return nil
}
