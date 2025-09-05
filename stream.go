package kwik

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

type StreamImpl struct {
	id             protocol.StreamID
	offset         int
	remoteStreamID protocol.StreamID
	paths          map[protocol.PathID]transport.Path
	mu             sync.RWMutex
	primaryPathID  protocol.PathID
	seqCounter     uint64
	logger         logger.Logger
	streamManager  *streamManagerImpl
}

// Ensure StreamImpl implements .Stream
var _ Stream = (*StreamImpl)(nil)

func NewStream(id protocol.StreamID, sm *streamManagerImpl) *StreamImpl {
	return &StreamImpl{
		id:            id,
		logger:        logger.NewLogger(logger.LogLevelDebug).WithComponent("STREAM_IMPL"),
		paths:         make(map[protocol.PathID]transport.Path),
		streamManager: sm,
	}
}

// Read implements the io.Reader interface to read data from the stream.
// It blocks until data is available or an error occurs.
func (s *StreamImpl) Read(p []byte) (int, error) {
	s.logger.Info("Read attempt", "streamID", s.id, "bufferSize", len(p))

	if len(p) == 0 {
		return 0, nil
	}

	s.mu.RLock()
	primaryPathID := s.primaryPathID
	path, pathExists := s.paths[primaryPathID]
	s.mu.RUnlock()

	if !pathExists {
		return 0, fmt.Errorf("no primary path for reading on stream %d", s.id)
	}

	session := path.Session()
	if session == nil {
		return 0, fmt.Errorf("no session for path")
	}

	mx := session.Multiplexer()
	if mx == nil {
		return 0, fmt.Errorf("no multiplexer for path")
	}

	// Appel direct à PullFrames qui est maintenant bloquant.
	data, err := mx.PullFrames(s.id, len(p))
	if err != nil {
		s.logger.Error("Read failed on PullFrames", "streamID", s.id, "error", err)
		return 0, err
	}

	if len(data) == 0 {
		// PullFrames peut retourner 0 octets si le stream est fermé.
		// Pour se conformer à io.Reader, on retourne io.EOF dans ce cas.
		return 0, io.EOF
	}

	n := copy(p, data)
	s.logger.Info("Read succeeded", "streamID", s.id, "read", n)
	return n, nil
}

// Write implements the io.Writer interface to write data to the stream.
// It tries to write to the primary path first, then falls back to other available paths.
func (s *StreamImpl) Write(p []byte) (int, error) {
	s.logger.Info("Write attempt",
		"streamID", s.id,
		"size", len(p),
		"primaryPath", s.primaryPathID,
		"pathCount", len(s.paths))
	if len(p) == 0 {
		return 0, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Try the primary path first if available
	if s.primaryPathID <= 0 {
		return 0, protocol.NewPathNotExistsError(s.primaryPathID)
	}

	if path, ok := s.paths[s.primaryPathID]; ok {
		n, err := s.writeToPath(path, p)

		if err != nil {
			s.logger.Error("Write failed",
				"streamID", s.id,
				"error", err,
				"pathID", s.primaryPathID)
		} else {
			s.logger.Info("Write succeeded",
				"streamID", s.id,
				"written", n)
		}

		return n, err
	}
	return 0, protocol.NewPathNotExistsError(s.primaryPathID)
}

// writeToPath is a helper method that writes data to a specific path
func (s *StreamImpl) writeToPath(path transport.Path, p []byte) (int, error) {
	if path == nil {
		s.logger.Error("Cannot write to nil path")
		return 0, fmt.Errorf("cannot write to nil path")
	}

	pid := path.PathID()
	s.logger.Debug("1- Writing to path",
		"streamID", s.id,
		"pathID", pid,
		"size", len(p))

	// Package the payload into a frame and submit to packer
	seq := atomic.AddUint64(&s.seqCounter, 1)
	f := &protocol.Frame{
		Type:     protocol.FrameTypeData,
		StreamID: s.id,
		Seq:      seq,
		Payload:  append([]byte(nil), p...),
	}

	s.logger.Debug("Using sequence number for frame",
		"streamID", s.id,
		"seq", seq,
		"nextSeq", atomic.LoadUint64(&s.seqCounter)+1)

	// Get the packer from the path's session
	session := path.Session()
	if session == nil {
		s.logger.Error("No session available for path",
			"streamID", s.id,
			"pathID", pid)
		return 0, fmt.Errorf("no session available for path %d", pid)
	}
	s.logger.Debug("2- Using session for path",
		"streamID", s.id,
		"pathID", pid)

	packer := session.Packer()
	if packer == nil {
		s.logger.Error("No packer available for path",
			"streamID", s.id,
			"pathID", pid)
		return 0, fmt.Errorf("no packer available for path %d", pid)
	}
	s.logger.Debug("3- Using packer for path",
		"streamID", s.id,
		"pathID", pid)

	// SOLUTION POUR DÉBLOQUER L'ARCHITECTURE:
	// Toujours utiliser SubmitServerFrame pour envoyer immédiatement,
	// que ce soit côté client ou serveur. SubmitClientFrame cause des problèmes
	// de synchronisation car il diffère l'envoi des frames, ce qui crée des
	// interblocages dans les communications bidirectionnelles.
	var err error

	// Vérifier si le chemin est toujours enregistré dans les chemins du stream
	_, pathExists := s.GetPath(pid)
	if !pathExists {
		s.logger.Error("Path no longer registered with stream",
			"streamID", s.id,
			"pathID", pid)
		return 0, fmt.Errorf("path %d no longer registered with stream %d", pid, s.id)
	}

	if session := path.Session(); session != nil {
		err = packer.SubmitFrame(path, f)
	} else {
		return 0, fmt.Errorf("no session available")
	}

	if err != nil {
		s.logger.Error("Failed to submit frame to packer",
			"streamID", s.id,
			"pathID", pid,
			"seq", seq,
			"size", len(p),
			"error", err)
		return 0, fmt.Errorf("failed to submit frame to packer: %w", err)
	}

	s.logger.Debug("Successfully submitted frame to packer",
		"streamID", s.id,
		"pathID", pid,
		"seq", seq,
		"size", len(p))

	return len(p), nil
}

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

// KWIK-specific metadata
func (s *StreamImpl) StreamID() protocol.StreamID {
	return s.id
}

// Secondary stream isolation methods
func (s *StreamImpl) SetOffset(offset int) error {
	s.offset = offset
	return nil
}

func (s *StreamImpl) GetOffset() int {
	return s.offset
}

func (s *StreamImpl) SetRemoteStreamID(remoteStreamID protocol.StreamID) error {
	s.remoteStreamID = remoteStreamID
	return nil
}

func (s *StreamImpl) RemoteStreamID() protocol.StreamID {
	return s.remoteStreamID
}

// SetPrimaryPath designates which path should be used for writes and preferred reads.
func (s *StreamImpl) SetPrimaryPath(pid protocol.PathID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primaryPathID = pid
}

func (s *StreamImpl) GetPrimaryPath() protocol.PathID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primaryPathID
}

// AddPath attaches a transport.Path to this stream. Thread-safe.
func (s *StreamImpl) AddPath(path transport.Path) error {
	if path == nil {
		return protocol.NewInvalidPathIDError(0)
	}
	pid := path.PathID()

	// First check if the path is already registered
	s.mu.RLock()
	if _, ok := s.paths[pid]; ok {
		s.mu.RUnlock()
		s.logger.Debug("Path already registered with stream", "streamID", s.id, "pathID", pid)
		return nil // Return success if path is already registered
	}
	s.mu.RUnlock()

	// Get the session from the path to access packer
	var packer *transport.Packer
	if session := path.Session(); session != nil {
		packer = session.Packer()
	}

	// Register the path with the packer if available
	if packer != nil {
		// Use a best-effort approach to register the path
		// If it's already registered, the packer should handle it gracefully
		packer.RegisterPath(path)
	}

	// Now finalize registration under write lock
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check under write lock in case of race condition
	if _, ok := s.paths[pid]; ok {
		s.logger.Debug("Path already registered with stream (race detected)", "streamID", s.id, "pathID", pid)
		return nil
	}

	s.paths[pid] = path

	// If this is the first path added, make it the primary path
	if s.primaryPathID == 0 {
		s.primaryPathID = pid
		s.logger.Debug("Set primary path for stream", "streamID", s.id, "pathID", pid)
	}

	s.logger.Debug("Added path to stream", "streamID", s.id, "pathID", pid)
	return nil
}

// RemovePath détache un path de ce stream. Thread-safe.
func (s *StreamImpl) RemovePath(pathID protocol.PathID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Vérifier si le path existe dans ce stream
	if _, ok := s.paths[pathID]; !ok {
		s.logger.Debug("Path not registered with stream, nothing to remove", "streamID", s.id, "pathID", pathID)
		return
	}

	// Supprimer le path
	delete(s.paths, pathID)
	s.logger.Debug("Removed path from stream", "streamID", s.id, "pathID", pathID)

	// Si c'était le path principal, choisir un autre path comme principal s'il en reste
	if s.primaryPathID == pathID {
		// Réinitialiser d'abord
		s.primaryPathID = 0

		// Choisir le premier path disponible comme nouveau path principal
		for pid := range s.paths {
			s.primaryPathID = pid
			s.logger.Debug("Set new primary path for stream after removal", "streamID", s.id, "pathID", pid)
			break
		}
	}
}

// GetPath returns the path if present.
func (s *StreamImpl) GetPath(pid protocol.PathID) (transport.Path, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.paths[pid]
	return p, ok
}

// SetSeqNumber sets the sequence number to use for the next write operation.
// After each write, the sequence number will be automatically incremented.
// This method is thread-safe.
func (s *StreamImpl) SetSeqNumber(seq uint64) error {
	atomic.StoreUint64(&s.seqCounter, seq-1) // -1 because the counter will be incremented before use in writeToPath
	s.logger.Debug("Set sequence number for stream",
		"streamID", s.id,
		"newSeq", seq,
		"nextSeqAfterWrite", seq)
	return nil
}
