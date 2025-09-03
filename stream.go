package kwik

import (
	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type StreamImpl struct {
	id             protocol.StreamID
	offset         int
	remoteStreamID protocol.StreamID
	quicStream     quic.Stream // Référence vers le stream QUIC réel
	logger         logger.Logger
}

// Ensure StreamImpl implements .Stream
var _ Stream = (*StreamImpl)(nil)

func NewStream(id protocol.StreamID) *StreamImpl {
	return &StreamImpl{
		id:     id,
		logger: logger.NewLogger(logger.LogLevelDebug).WithComponent("STEAM_IMPL"),
	}
}

func NewStreamWithQuicStream(id protocol.StreamID, quicStream quic.Stream) *StreamImpl {
	return &StreamImpl{
		id:         id,
		quicStream: quicStream,
		logger:     logger.NewLogger(logger.LogLevelDebug).WithComponent("STEAM_IMPL"),
	}
}

// QUIC-compatible interface
func (s *StreamImpl) Read(p []byte) (int, error) {
	// Not yet implemented
	return 0, nil
}

func (s *StreamImpl) Write(p []byte) (int, error) {
	// Not yet implemented
	return 0, nil
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
