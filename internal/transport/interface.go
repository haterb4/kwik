package transport

import (
	"context"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/protocol"
)

type Path interface {
	OpenStreamSync(ctx context.Context, streamID protocol.StreamID) error
	OpenStream(streamID protocol.StreamID) error
	AcceptStream(ctx context.Context, streamID protocol.StreamID) error
	LocalAddr() string
	RemoteAddr() string
}

type PathManager interface {
	OpenPath(ctx context.Context, addr string) (protocol.PathID, error)
	AccpetPath(conn *quic.Conn) (protocol.PathID, error)
	SetPrimaryPath(id protocol.PathID)
	// ListPaths() []Path
	GetPrimaryPath() Path
}

type ControlManager interface {
	SetConn(conn *quic.Conn)
}
type DataManager interface {
	SetConn(conn *quic.Conn)
}
