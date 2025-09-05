package kwik

import (
	"sync"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

type streamManagerImpl struct {
	mu           sync.RWMutex
	streams      map[protocol.StreamID]*StreamImpl
	nextStreamID protocol.StreamID
	logger       logger.Logger
}

func NewStreamManager() *streamManagerImpl {
	return &streamManagerImpl{
		streams:      make(map[protocol.StreamID]*StreamImpl),
		nextStreamID: 1,
		logger:       logger.NewLogger(logger.LogLevelDebug).WithComponent("STREAM_MANAGER"),
	}
}

func (m *streamManagerImpl) CreateStream() Stream {
	m.mu.Lock()
	defer m.mu.Unlock()

	streamID := m.nextStreamID
	m.nextStreamID++

	stream := NewStream(streamID, m)

	m.streams[streamID] = stream
	m.logger.Debug("Created new stream", "streamID", streamID)
	return stream
}

func (m *streamManagerImpl) GetNextStreamID() protocol.StreamID {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nextStreamID
}

func (m *streamManagerImpl) AddStreamPath(streamID protocol.StreamID, path transport.Path) error {
	if path == nil {
		return protocol.NewPathNotExistsError(0)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Get or create the stream
	stream, exists := m.streams[streamID]
	if !exists {
		return protocol.NewNotExistStreamError(path.PathID(), streamID)
	} else {
		// Check if the path is already added to this stream
		if _, exists := stream.paths[path.PathID()]; exists {
			return protocol.NewExistStreamError(path.PathID(), streamID)
		}
		// Add the path to the existing stream
		paths := len(stream.paths)
		stream.paths[path.PathID()] = path
		if paths == 0 {
			stream.primaryPathID = path.PathID()
		}
	}

	// Update the next stream ID if needed
	if streamID >= m.nextStreamID {
		m.nextStreamID = streamID + 1
	}

	m.logger.Debug("Added path to stream",
		"streamID", streamID,
		"pathID", path.PathID(),
		"totalPaths", len(stream.paths))

	return nil
}

// addStream adds a stream to the manager's internal map without locking.
// Caller must hold the mutex.
func (m *streamManagerImpl) addStream(stream *StreamImpl) {
	m.streams[stream.id] = stream
	if stream.id >= m.nextStreamID {
		m.nextStreamID = stream.id + 1
	}
	m.logger.Debug("Added stream", "streamID", stream.id)
}

func (m *streamManagerImpl) RemoveStream(streamID protocol.StreamID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.streams[streamID]; exists {
		delete(m.streams, streamID)
		m.logger.Debug("Removed stream", "streamID", streamID)
	}
}

func (m *streamManagerImpl) GetStream(streamID protocol.StreamID) (Stream, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stream, ok := m.streams[streamID]
	return stream, ok
}

func (mg *streamManagerImpl) CloseAllStreams() {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	for _, stream := range mg.streams {
		stream.Close()
	}
}
