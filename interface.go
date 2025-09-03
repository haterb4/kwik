package kwik

import (
	"context"
	"time"

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
}

type PathStatus int

const (
	PathStatusActive PathStatus = iota
	PathStatusDegraded
	PathStatusDead
	PathStatusConnecting
	PathStatusDisconnecting
)

type PathInfo struct {
	PathID     string
	Address    string
	IsPrimary  bool
	Status     PathStatus
	CreatedAt  time.Time
	LastActive time.Time
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
	AddPath(address string) error
	RemovePath(pathID string) error
	LocalAddr() string
	RemoteAddr() string
	SessionID() protocol.SessionID
	SendRawData(data []byte, pathID protocol.PathID, streamID protocol.StreamID) error
	CloseWithError(code int, msg string) error
}

// Listener represents a KWIK listener
type Listener interface {
	Accept(ctx context.Context) (Session, error)
	Close() error
	Addr() string
	GetActiveSessionCount() int
}
type streamManager interface {
	GetNextStreamID() protocol.StreamID
	CreateStream() Stream
}
