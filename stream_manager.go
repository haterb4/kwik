package kwik

import (
	"fmt"
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
	isClient     bool
	session      Session
}

func NewStreamManager(session Session) *streamManagerImpl {
	return &streamManagerImpl{
		streams:      make(map[protocol.StreamID]*StreamImpl),
		nextStreamID: 1,
		session:      session,
		isClient:     session.IsClient(),
		logger:       logger.NewLogger(logger.LogLevelSilent).WithComponent("STREAM_MANAGER"),
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

func (m *streamManagerImpl) GetStream(streamID protocol.StreamID) (Stream, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stream, ok := m.streams[streamID]
	return stream, ok
}

// GetStreamFrameHandler returns the receiver for direct StreamFrame delivery if available
func (m *streamManagerImpl) GetStreamFrameHandler(streamID protocol.StreamID) (transport.StreamFrameHandler, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stream, ok := m.streams[streamID]
	if !ok || stream.recv == nil {
		return nil, false
	}
	return stream.recv, true
}

func (m *streamManagerImpl) GetSendStreamProvider(streamID protocol.StreamID) (transport.SendStreamProvider, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stream, ok := m.streams[streamID]
	if !ok || stream.send == nil {
		return nil, false
	}
	return stream.send, true
}

func (m *streamManagerImpl) AddPathToStream(streamID protocol.StreamID, path transport.Path) error {
	if path == nil {
		return protocol.NewPathNotExistsError(0)
	}
	m.logger.Debug("Adding path to stream",
		"streamID", streamID,
		"pathID", path.PathID())
	// Get the stream, it should already exist
	stream, exists := m.streams[streamID]
	if !exists {
		m.logger.Error("Stream not found for relay path",
			"streamID", streamID,
			"pathID", path.PathID())
		return protocol.NewNotExistStreamError(path.PathID(), streamID)
	}

	// Check if the path is already added to this stream (sans mutex pour éviter un interblocage)
	// Nous n'avons pas besoin du mutex du stream car nous sommes déjà sous le mutex du manager
	var pathExists bool
	if stream.paths != nil {
		_, pathExists = stream.paths[path.PathID()]
	}

	if pathExists {
		m.logger.Debug("Path already added to stream",
			"streamID", streamID,
			"pathID", path.PathID())
		return nil // On accepte que le path existe déjà pour éviter un blocage
	}

	// Add the path to the stream

	paths := len(stream.paths)
	stream.paths[path.PathID()] = path

	if paths == 0 {
		stream.primaryPathID = path.PathID()
		m.logger.Debug("Set as primary path for stream",
			"streamID", streamID,
			"pathID", path.PathID())
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

func (m *streamManagerImpl) CloseStream(streamID protocol.StreamID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if stream, exists := m.streams[streamID]; exists {
		// Before removing the stream, cancel any pending packets and cleanup reception buffer
		if !m.isClient {
			fmt.Printf("TRACK MGR CLOSE STREAM %d\n", streamID)
		}
		stream.mu.Lock()
		paths := make([]transport.Path, 0, len(stream.paths))
		for _, p := range stream.paths {
			paths = append(paths, p)
		}
		stream.mu.Unlock()

		for _, p := range paths {
			if p != nil {
				err := p.CloseStream(streamID)
				if err != nil {
					m.logger.Error("Failed to close stream path",
						"streamID", streamID,
						"pathID", p.PathID(),
						"error", err)
					return err
				}
			}
		}

		delete(m.streams, streamID)
		m.logger.Debug("Removed stream", "streamID", streamID)
		if !m.isClient {
			fmt.Printf("TRACK MGR STREAM: %d CLOSED\n", streamID)
		}
		return nil
	}
	if !m.isClient {
		fmt.Printf("TRACK MGR CLOSE STREAM, STREAM: %d NOT FOUND\n", streamID)
	}
	return nil
}

func (m *streamManagerImpl) CloseAllStreams() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, stream := range m.streams {
		// Before removing the stream, cancel any pending packets and cleanup reception buffer
		stream.mu.Lock()
		if !m.isClient {
			fmt.Printf("TRACK MGR CLOSE ALL STREAM %d \n", stream.id)
		}
		paths := make([]transport.Path, 0, len(stream.paths))
		for _, p := range stream.paths {
			paths = append(paths, p)
		}
		stream.mu.Unlock()

		for _, p := range paths {
			if p != nil {
				_ = p.CloseStream(stream.id)
			}
		}

		delete(m.streams, stream.id)
		m.logger.Debug("Removed stream", "streamID", stream.id)
		if !m.isClient {
			fmt.Printf("TRACK STREAM: %d CLOSED (all streams)\n", stream.id)
		}
	}
}
