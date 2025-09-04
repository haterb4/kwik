package kwik

import (
	"context"
	"crypto/tls"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/protocol"
)

type listener struct {
	quiclistener   *quic.Listener
	address        string
	tls            *tls.Config
	cfg            *config.Config
	activeSessions map[protocol.SessionID]Session
	mu             sync.Mutex
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
		activeSessions: make(map[protocol.SessionID]Session),
		closed:         make(chan struct{}),
	}, nil
}

func (l *listener) Accept(ctx context.Context) (Session, error) {
	conn, err := l.quiclistener.Accept(ctx)

	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id := protocol.SessionID(len(l.activeSessions) + 1)
	//create and return a new KWIK session
	sess := NewServerSession(id, conn)
	l.activeSessions[id] = sess
	return sess, nil
}

func (l *listener) Addr() string {
	return l.address
}

func (l *listener) Close() error {
	close(l.closed)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, sess := range l.activeSessions {
		sess.CloseWithError(0, "listener closed")
	}
	return l.quiclistener.Close()
}

func (l *listener) GetActiveSessionCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.activeSessions)
}
