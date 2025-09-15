package kwik

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

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
	isClosed       bool
	closeOnce      sync.Once
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
	// Check if listener is closed first
	select {
	case <-l.closed:
		return nil, quic.ErrServerClosed
	default:
	}

	// Create a context that cancels when the listener closes
	acceptCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Monitor for listener closure in a separate goroutine
	go func() {
		select {
		case <-l.closed:
			cancel()
		case <-acceptCtx.Done():
		}
	}()

	conn, err := l.quiclistener.Accept(acceptCtx)
	if err != nil {
		select {
		case <-l.closed:
			return nil, quic.ErrServerClosed
		default:
			return nil, err
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Double-check if closed after acquiring lock
	if l.isClosed {
		conn.CloseWithError(0, "listener closed")
		return nil, quic.ErrServerClosed
	}

	id := protocol.SessionID(len(l.activeSessions) + 1)
	//create and return a new KWIK session
	sess := NewServerSession(id, conn, l)
	l.activeSessions[id] = sess
	return sess, nil
}

func (l *listener) Addr() string {
	return l.address
}

func (l *listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.isClosed = true
		// Close the notification channel first
		close(l.closed)

		// Close all active sessions and give them time to cleanup
		sessionIDs := make([]protocol.SessionID, 0, len(l.activeSessions))
		for id, sess := range l.activeSessions {
			if sess != nil {
				go sess.CloseWithError(0, "listener closed")
				sessionIDs = append(sessionIDs, id)
			}
		}
		l.mu.Unlock()

		// Wait for sessions to close gracefully with progress monitoring
		maxWaitTime := 1 * time.Second
		checkInterval := 50 * time.Millisecond
		deadline := time.Now().Add(maxWaitTime)

		for time.Now().Before(deadline) {
			l.mu.Lock()
			remainingCount := len(l.activeSessions)
			l.mu.Unlock()

			if remainingCount == 0 {
				break
			}
			time.Sleep(checkInterval)
		}

		// Force clean up any remaining sessions
		l.mu.Lock()

		for _, id := range sessionIDs {
			delete(l.activeSessions, id)
		}
		l.mu.Unlock()

		// Close the underlying QUIC listener
		qerr := l.quiclistener.Close()
		if qerr != nil {
			err = qerr
		}

		l.waitForPortToBeReleased()
	})
	return err
}

func (l *listener) GetActiveSessionCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.activeSessions)
}

// RemoveSession removes a session from the active sessions map
// This should be called when a session closes
func (l *listener) RemoveSession(sessionID protocol.SessionID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.activeSessions, sessionID)
}

// IsClosed returns true if the listener has been closed
func (l *listener) IsClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isClosed
}

// isPortFree checks if a UDP port is available for binding
func isPortFree(address string) bool {
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		// Port is not free
		return false
	}
	conn.Close()

	// Double-check by trying to bind again immediately
	conn2, err2 := net.ListenPacket("udp", address)
	if err2 != nil {
		return false
	}
	conn2.Close()
	return true
}

// waitForPortToBeReleased waits until the port is actually free
func (l *listener) waitForPortToBeReleased() {
	maxWait := 3 * time.Second             // Reduced from 5s to 3s
	checkInterval := 25 * time.Millisecond // More frequent checks
	timeout := time.After(maxWait)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	checkCount := 0
	for {
		select {
		case <-timeout:
			return
		case <-ticker.C:
			checkCount++
			if isPortFree(l.address) {
				return
			}
			if checkCount%40 == 0 {
				fmt.Printf("KWIK LISTENER: Still waiting for port %s to be released (check %d)\n", l.address, checkCount)
			}
		}
	}
}
