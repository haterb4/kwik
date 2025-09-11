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
	SetWriteOffset(offset uint64) error
	GetWriteOffset() uint64
	SetReadOffset(offset uint64) error
	GetReadOffset() uint64
	SetRemoteStreamID(remoteStreamID protocol.StreamID) error
	RemoteStreamID() protocol.StreamID
	RemovePath(pathID protocol.PathID)
}

type StreamManager interface {
	GetNextStreamID() protocol.StreamID
	CreateStream() Stream
	GetStream(streamID protocol.StreamID) (Stream, bool)
	AddPathToStream(streamID protocol.StreamID, path Path) error
	RemoveStream(streamID protocol.StreamID)
	// GetStreamFrameHandler returns an optional handler that can accept direct StreamFrame delivery
	GetStreamFrameHandler(streamID protocol.StreamID) (StreamFrameHandler, bool)
	// GetSendStreamProvider returns an optional provider that can supply frames for sending
	GetSendStreamProvider(streamID protocol.StreamID) (SendStreamProvider, bool)
}

// StreamFrameHandler accepts delivered stream frames for direct in-order reassembly.
type StreamFrameHandler interface {
	HandleStreamFrame(f *protocol.StreamFrame)
}

// SendStreamProvider provides frames ready for packetization from a logical send stream.
type SendStreamProvider interface {
	PopFrames(maxBytes int) []*protocol.StreamFrame
	HasBuffered() bool
}

// sessionInternal is an unexported interface that exposes transport internals
// to code inside the transport package only. Other packages cannot reference
// this type, so it prevents external code from accessing internals directly.
type sessionInternal interface {
	Packer() *Packer
	Multiplexer() *Multiplexer
	PathManager() PathManager
	StreamManager() StreamManager
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

	// WriteStream writes to the underlying QUIC stream associated with streamID.
	WriteStream(streamID protocol.StreamID, p []byte) (int, error)
	// HasStream checks if a stream with the given ID exists in this path
	HasStream(streamID protocol.StreamID) bool
	// Control frames are sent/received on the reserved control QUIC stream (streamID 0).
	// Use WriteStream/ReadStream on stream 0 for control traffic; the concrete path
	// implementation still provides helpers but the interface no longer exposes them.
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
	WriteSeq(streamID protocol.StreamID) (uint64, bool)
	WriteOffsetForStream(streamID protocol.StreamID) uint64
	IncrementWriteOffsetForStream(streamID protocol.StreamID, n uint64)
	// Offset synchronization methods for SendStream coordination
	GetWriteOffset(streamID protocol.StreamID) (uint64, bool)
	SyncWriteOffset(streamID protocol.StreamID, offset uint64) bool
	// Internal helpers callable by higher-level code without directly accessing session internals
	SubmitSendStream(streamID protocol.StreamID) error
	CancelFramesForStream(streamID protocol.StreamID)
	CleanupStreamBuffer(streamID protocol.StreamID)
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
