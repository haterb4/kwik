package kwik

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
	if len(p) == 0 {
		return 0, nil
	}

	s.mu.RLock()
	primary := s.primaryPathID
	s.mu.RUnlock()

	// Try the primary path first if available
	if primary != 0 {
		n, err := s.tryReadFromPath(primary, p)
		if err == nil {
			return n, nil
		}
		s.logger.Debug("Failed to read from primary path, trying others",
			"pathID", primary, "error", err)
	}

	// Try all available paths
	s.mu.RLock()
	defer s.mu.RUnlock()

	for pid := range s.paths {
		if pid == primary {
			continue // Already tried primary
		}
		n, err := s.tryReadFromPath(pid, p)
		if err == nil {
			s.primaryPathID = pid // Promote this path
			s.logger.Debug("Promoted path for reading", "pathID", pid)
			return n, nil
		}
		s.logger.Debug("Failed to read from path", "pathID", pid, "error", err)
	}

	return 0, fmt.Errorf("no available paths for reading")
}

// tryReadFromPath attempts to read data from a specific path
func (s *StreamImpl) tryReadFromPath(pid protocol.PathID, p []byte) (int, error) {
	path, ok := s.GetPath(pid)
	if !ok {
		return 0, fmt.Errorf("path not found")
	}

	session := path.Session()
	if session == nil {
		return 0, fmt.Errorf("no session for path")
	}

	mx := session.Multiplexer()
	if mx == nil {
		return 0, fmt.Errorf("no multiplexer for path")
	}

	// Set a reasonable timeout to prevent hanging indefinitely
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			data, err := mx.PullFrames(s.id, len(p))
			if err != nil {
				return 0, fmt.Errorf("failed to pull frames: %w", err)
			}
			if len(data) > 0 {
				return copy(p, data), nil
			}
			time.Sleep(10 * time.Millisecond) // Yield to other goroutines
		}
	}
}

// Write implements the io.Writer interface to write data to the stream.
// It tries to write to the primary path first, then falls back to other available paths.
func (s *StreamImpl) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Try the primary path first if available
	if s.primaryPathID != 0 {
		if path, ok := s.paths[s.primaryPathID]; ok {
			n, err := s.writeToPath(path, p)
			if err == nil {
				return n, nil
			}
			s.logger.Debug("Failed to write to primary path, trying others",
				"pathID", s.primaryPathID, "error", err)
		}
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
	s.logger.Debug("Writing to path",
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

	// Get the packer from the path's session
	session := path.Session()
	if session == nil {
		s.logger.Error("No session available for path",
			"streamID", s.id,
			"pathID", pid)
		return 0, fmt.Errorf("no session available for path %d", pid)
	}

	packer := session.Packer()
	if packer == nil {
		s.logger.Error("No packer available for path",
			"streamID", s.id,
			"pathID", pid)
		return 0, fmt.Errorf("no packer available for path %d", pid)
	}

	// Submit the frame to the packer
	if err := packer.SubmitFrame(path, f); err != nil {
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

// RemovePath detaches a path from this stream. Thread-safe.
func (s *StreamImpl) RemovePath(pid protocol.PathID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.paths, pid)
}

// GetPath returns the path if present.
func (s *StreamImpl) GetPath(pid protocol.PathID) (transport.Path, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.paths[pid]
	return p, ok
}
