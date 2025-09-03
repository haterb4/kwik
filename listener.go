package kwik

import (
	"context"
	"crypto/tls"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/protocol"
)

type listener struct {
	quiclistener   *quic.Listener
	address        string
	tls            *tls.Config
	cfg            *config.Config
	activeSessions uint64
	closed         chan struct{}
}

// Ensure listener implements types.Listener
var _ Listener = (*listener)(nil)

func listenAddr(address string, tls *tls.Config, cfg *config.Config) (Listener, error) {
	ql, err := quic.ListenAddr(address, tls, cfg)
	if err != nil {
		return nil, err
	}
	return &listener{
		quiclistener:   ql,
		address:        address,
		tls:            tls,
		cfg:            cfg,
		activeSessions: 0,
		closed:         make(chan struct{}),
	}, nil
}

func (l *listener) Accept(ctx context.Context) (Session, error) {
	conn, err := l.quiclistener.Accept(ctx)

	if err != nil {
		return nil, err
	}
	id := protocol.SessionID(l.activeSessions + 1)
	//create and return a new KWIK session
	sess := NewServerSession(id, conn)
	l.activeSessions++
	return sess, nil
}

func (l *listener) Addr() string {
	return l.address
}

func (l *listener) Close() error {
	close(l.closed)
	return l.quiclistener.Close()
}

func (l *listener) GetActiveSessionCount() int {
	return int(l.activeSessions)
}
