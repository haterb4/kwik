package transport

import (
	"context"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type path struct {
	id      protocol.PathID
	conn    *quic.Conn
	streams map[protocol.StreamID]*quic.Stream
	mu      sync.Mutex
	logger  logger.Logger
}

func NewPath(id protocol.PathID, conn *quic.Conn) *path {
	return &path{
		id:      id,
		conn:    conn,
		streams: make(map[protocol.StreamID]*quic.Stream),
		logger:  logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH"),
	}
}
func (p *path) LocalAddr() string {
	return p.conn.LocalAddr().String()
}
func (p *path) RemoteAddr() string {
	return p.conn.RemoteAddr().String()
}

func (p *path) OpenStreamSync(ctx context.Context, streamID protocol.StreamID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Debug("Opening stream synchronously", "pathID", p.id, "streamID", streamID)
	stream, err := p.conn.OpenStreamSync(ctx)
	if err != nil {
		p.logger.Error("Failed to open QUIC stream", "pathID", p.id, "streamID", streamID, "error", err)
		return err
	}

	if _, ok := p.streams[streamID]; ok {
		p.logger.Error("Stream already exists", "pathID", p.id, "streamID", streamID)
		return protocol.NewExistStremError(p.id, streamID)
	}

	p.streams[streamID] = stream
	p.logger.Debug("Successfully opened stream", "pathID", p.id, "streamID", streamID, "quicStreamID", stream.StreamID())
	return nil
}

func (p *path) OpenStream(streamID protocol.StreamID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Debug("Opening stream asynchronously", "pathID", p.id, "streamID", streamID)
	stream, err := p.conn.OpenStream()
	if err != nil {
		p.logger.Error("Failed to open QUIC stream", "pathID", p.id, "streamID", streamID, "error", err)
		return err
	}

	if _, ok := p.streams[streamID]; ok {
		p.logger.Error("Stream already exists", "pathID", p.id, "streamID", streamID)
		return protocol.NewExistStremError(p.id, streamID)
	}

	p.streams[streamID] = stream
	p.logger.Debug("Successfully opened stream", "pathID", p.id, "streamID", streamID, "quicStreamID", stream.StreamID())
	return nil
}
func (p *path) AcceptStream(ctx context.Context, streamID protocol.StreamID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logger.Debug("Waiting for New Quic Stream on path %d", p.id)
	stream, err := p.conn.AcceptStream(ctx)
	if err != nil {
		return err
	}
	p.logger.Debug("Accepted New Quic Stream %d for stream %d on path %d", stream.StreamID(), streamID, p.id)

	if _, ok := p.streams[streamID]; ok {
		return protocol.NewExistStremError(p.id, streamID)
	}
	p.streams[streamID] = stream
	return nil
}
