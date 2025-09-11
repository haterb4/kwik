package kwik

import (
	"context"

	"github.com/s-anzie/kwik/internal/transport"
)

type Relay = transport.Relay
type StreamManager = transport.StreamManager
type Session = transport.Session
type Stream = transport.Stream

// Listener represents a KWIK listener
type Listener interface {
	Accept(ctx context.Context) (Session, error)
	Close() error
	Addr() string
	GetActiveSessionCount() int
}
