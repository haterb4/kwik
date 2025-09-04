package kwik

import (
	"sync"
	"sync/atomic"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

type streamManagerImpl struct {
	streams      map[protocol.StreamID]*StreamImpl
	nextStreamID uint64
	mu           sync.Mutex
	logger       logger.Logger
}

func NewStreamManager() *streamManagerImpl {
	return &streamManagerImpl{
		streams:      make(map[protocol.StreamID]*StreamImpl),
		nextStreamID: 0,
		logger:       logger.NewLogger(logger.LogLevelDebug).WithComponent("STEAM_MANAGER_IMPL"),
	}
}

func (mg *streamManagerImpl) CreateStream() Stream {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	id := atomic.AddUint64(&mg.nextStreamID, 1)
	stream := NewStream(protocol.StreamID(id))
	mg.streams[protocol.StreamID(id)] = stream
	return stream
}
func (mg *streamManagerImpl) GetNextStreamID() protocol.StreamID {
	return protocol.StreamID(atomic.AddUint64(&mg.nextStreamID, 1))
}
func (mg *streamManagerImpl) AddStreamPath(streamID protocol.StreamID, path transport.Path) error {
	mg.mu.Lock()
	stream, ok := mg.streams[streamID]
	mg.mu.Unlock()
	if !ok {
		return protocol.NewNotExistStreamError(path.PathID(), streamID)
	}
	// Delegate path management to StreamImpl
	return stream.AddPath(path)
}
