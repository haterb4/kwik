package transport

import (
	"context"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/protocol"
)

// Stream represents a KWIK stream
type Stream interface {
	// QUIC-compatible interface
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error

	// KWIK-specific metadata
	StreamID() protocol.StreamID

	// Secondary stream isolation methods
	SetOffset(offset int) error
	GetOffset() int
	SetRemoteStreamID(remoteStreamID protocol.StreamID) error
	RemoteStreamID() protocol.StreamID

	// Sequence control
	SetSeqNumber(seq uint64) error
}

type StreamManager interface {
	GetNextStreamID() protocol.StreamID
	CreateStream() Stream
	AddStreamPath(streamID protocol.StreamID, path Path) error
	RemoveStream(streamID protocol.StreamID)
}

// SessionState represents the current state of a session
type SessionState int

const (
	SessionStateConnecting SessionState = iota
	SessionStateAuthenticating
	SessionStateAuthenticated
	SessionStateActive
	SessionStateClosed
)

// Session represents a KWIK session
type Session interface {
	OpenStreamSync(ctx context.Context) (Stream, error)
	OpenStream() (Stream, error)
	AcceptStream(ctx context.Context) (Stream, error)
	AddRelay(address string) (relay Relay, err error)
	RemovePath(pathID string) error
	LocalAddr() string
	RemoteAddr() string
	SessionID() protocol.SessionID
	SendRawData(data []byte, pathID protocol.PathID, streamID protocol.StreamID) error
	CloseWithError(code int, msg string) error
	// Transport layer access
	Packer() *Packer
	Multiplexer() *Multiplexer
	// Path manager access (internal use)
	PathManager() PathManager
	StreamManager() StreamManager
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
type Relay interface {
	Address() string
	PathID() protocol.PathID
	IsActive() bool
	SendRawData(data []byte, streamID protocol.StreamID) (int, error)
}

type PathManager interface {
	OpenPath(ctx context.Context, addr string, session Session) (protocol.PathID, error)
	AccpetPath(conn *quic.Conn, session Session) (protocol.PathID, error)
	SetPrimaryPath(id protocol.PathID)
	AddRelay(address string) (Relay, error)
	// ListPaths() []Path
	GetPrimaryPath() Path
	GetPath(id protocol.PathID) Path
	RemovePath(id protocol.PathID)
	CloseAllPaths()
}
