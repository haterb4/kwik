package transport

import (
	"context"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/protocol"
)

type Session interface {
	Packer() *Packer
	Multiplexer() *Multiplexer
}

type Path interface {
	OpenStreamSync(ctx context.Context, streamID protocol.StreamID) error
	OpenStream(streamID protocol.StreamID) error
	AcceptStream(ctx context.Context, streamID protocol.StreamID) error
	// ReadStream reads from the underlying QUIC stream associated with streamID.
	ReadStream(streamID protocol.StreamID, p []byte) (int, error)
	// WriteStream writes to the underlying QUIC stream associated with streamID.
	WriteStream(streamID protocol.StreamID, p []byte) (int, error)
	// HasStream checks if a stream with the given ID exists in this path
	HasStream(streamID protocol.StreamID) bool
	// SendControlFrame sends a control frame (handshake, ping, etc.) on the path
	SendControlFrame(f *protocol.Frame) error
	// HandleControlFrame is invoked when an inbound control frame (StreamID 0) is received
	HandleControlFrame(f *protocol.Frame) error
	LocalAddr() string
	RemoteAddr() string
	RemoveStream(streamID protocol.StreamID)
	PathID() protocol.PathID
	// IsClient returns true when the path was created by an active dial (client side)
	IsClient() bool
	// IsSessionReady reports whether the per-path session handshake completed
	IsSessionReady() bool
	CloseStream(streamID protocol.StreamID) error
	Close() error
	Session() Session
	// GetLastAcceptedStreamID returns the ID of the last accepted stream
	GetLastAcceptedStreamID() protocol.StreamID
}

type PathManager interface {
	OpenPath(ctx context.Context, addr string, session Session) (protocol.PathID, error)
	AccpetPath(conn *quic.Conn, session Session) (protocol.PathID, error)
	SetPrimaryPath(id protocol.PathID)
	// ListPaths() []Path
	GetPrimaryPath() Path
	RemovePath(id protocol.PathID)
	CloseAllPaths()
}
