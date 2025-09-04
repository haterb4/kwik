package kwik

import (
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
}

// Ensure StreamImpl implements .Stream
var _ Stream = (*StreamImpl)(nil)

func NewStream(id protocol.StreamID) *StreamImpl {
	return &StreamImpl{
		id:     id,
		logger: logger.NewLogger(logger.LogLevelDebug).WithComponent("STREAM_IMPL"),
		paths:  make(map[protocol.PathID]transport.Path),
	}
}

// QUIC-compatible interface
func (s *StreamImpl) Read(p []byte) (int, error) {
	// If a multiplexer is configured, pull ordered frames from it.
	if mx := transport.GetDefaultMultiplexer(); mx != nil {
		// Block until we have data or timeout
		for {
			data, err := mx.PullFrames(s.id, len(p))
			if err != nil {
				return 0, err
			}
			if len(data) > 0 {
				n := copy(p, data)
				return n, nil
			}
			// No data available, wait a bit before trying again
			// This prevents busy waiting while allowing reads to eventually succeed
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Fallback: no multiplexer, do simple per-path read
	s.mu.RLock()
	primary := s.primaryPathID
	s.mu.RUnlock()

	if primary != 0 {
		if path, ok := s.GetPath(primary); ok {
			n, err := path.ReadStream(s.id, p)
			if err == nil || n > 0 {
				return n, err
			}
			// otherwise fallthrough to try other paths
		}
	}

	// Try other paths
	s.mu.RLock()
	for pid, path := range s.paths {
		// skip primary (already tried)
		if pid == primary {
			continue
		}
		s.mu.RUnlock()
		n, err := path.ReadStream(s.id, p)
		s.mu.RLock()
		if err == nil || n > 0 {
			s.mu.RUnlock()
			return n, err
		}
	}
	s.mu.RUnlock()
	return 0, nil
}

func (s *StreamImpl) Write(p []byte) (int, error) {
	// Writes always go through the primary path when set. If no primary path
	// is set, write to the first available path.
	s.mu.RLock()
	primary := s.primaryPathID
	s.mu.RUnlock()

	if primary != 0 {
		if path, ok := s.GetPath(primary); ok {
			// Package the payload into a frame and submit to packer
			seq := atomic.AddUint64(&s.seqCounter, 1)
			f := &protocol.Frame{
				Type:     protocol.FrameTypeData,
				StreamID: s.id,
				Seq:      seq,
				Payload:  append([]byte(nil), p...),
			}
			if pk := transport.GetDefaultPacker(); pk != nil {
				if err := pk.SubmitFrame(path, f); err != nil {
					return 0, err
				}
				return len(p), nil
			}
			// fallback: write raw
			return path.WriteStream(s.id, p)
		}
	}
	return 0, protocol.NewPathNotExistsError(primary)
}

func (s *StreamImpl) Close() error {
	// Not yet implemented
	return nil
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

	// check quickly under lock whether path already exists
	s.mu.RLock()
	if _, ok := s.paths[pid]; ok {
		s.mu.RUnlock()
		return protocol.NewInvalidPathIDError(pid)
	}
	s.mu.RUnlock()

	// register the path first so packer/multiplexer can observe control frames while handshake runs
	if transport.GetDefaultPacker() != nil {
		transport.GetDefaultPacker().RegisterPath(path)
	}

	// now finalize registration under lock
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.paths[pid]; ok {
		// already present
		return protocol.NewInvalidPathIDError(pid)
	}
	s.paths[pid] = path

	// If this is the first path added, make it the primary path
	if s.primaryPathID == 0 {
		s.primaryPathID = pid
		s.logger.Debug("Set primary path for stream", "streamID", s.id, "pathID", pid)
	}

	s.logger.Debug("Added path to stream", "streamID", s.id, "pathID", pid)
	// ensure a default multiplexer exists (lazy init)
	if transport.GetDefaultMultiplexer() == nil {
		transport.SetDefaultMultiplexer(transport.NewMultiplexer())
	}
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
