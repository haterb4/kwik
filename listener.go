package kwik

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

var (
	ErrListenerClosed = errors.New("listener closed")
	ErrSessionClosed  = errors.New("session closed")
)

type SessionRegistry struct {
	sessions    sync.Map
	connections sync.Map
	counter     int64
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		counter: 0,
	}
}

func (sr *SessionRegistry) GenerateID() protocol.SessionID {
	return protocol.SessionID(atomic.AddInt64(&sr.counter, 1))
}

func (sr *SessionRegistry) Add(id protocol.SessionID, session Session, conn *quic.Conn) {
	sr.sessions.Store(id, session)
	sr.connections.Store(id, conn)
}

func (sr *SessionRegistry) Remove(id protocol.SessionID) (Session, *quic.Conn) {
	var session Session
	var conn *quic.Conn

	if s, ok := sr.sessions.LoadAndDelete(id); ok {
		session = s.(Session)
	}
	if c, ok := sr.connections.LoadAndDelete(id); ok {
		conn = c.(*quic.Conn)
	}

	return session, conn
}

func (sr *SessionRegistry) GetSession(id protocol.SessionID) (Session, bool) {
	if s, ok := sr.sessions.Load(id); ok {
		return s.(Session), true
	}
	return nil, false
}

func (sr *SessionRegistry) Count() int {
	count := 0
	sr.sessions.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}

func (sr *SessionRegistry) ForEach(fn func(id protocol.SessionID, session Session, conn *quic.Conn) bool) {
	sr.sessions.Range(func(key, value interface{}) bool {
		id := key.(protocol.SessionID)
		session := value.(Session)

		var conn *quic.Conn
		if c, ok := sr.connections.Load(id); ok {
			conn = c.(*quic.Conn)
		}

		return fn(id, session, conn)
	})
}

type listener struct {
	quicListener *quic.Listener
	address      string
	tlsConfig    *tls.Config
	config       *config.Config
	registry     *SessionRegistry
	logger       logger.Logger

	// Shutdown coordination
	ctx          context.Context
	cancel       context.CancelFunc
	closeOnce    sync.Once
	shutdownChan chan struct{}

	// Metrics
	acceptedConnections  int64
	rejectedConnections  int64
	gracefulCloseTimeout time.Duration
	portReleaseTimeout   time.Duration
}

// Ensure listener implements Listener interface
var _ Listener = (*listener)(nil)

func listenAddr(address string, tls *tls.Config, cfg *config.Config) (Listener, error) {
	ql, err := quic.ListenAddr(address, tls, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create QUIC listener: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	l := &listener{
		quicListener:         ql,
		address:              address,
		tlsConfig:            tls,
		config:               cfg,
		registry:             NewSessionRegistry(),
		logger:               logger.NewLogger(logger.LogLevelDebug).WithComponent("LISTENER"),
		ctx:                  ctx,
		cancel:               cancel,
		shutdownChan:         make(chan struct{}),
		gracefulCloseTimeout: 2 * time.Second,
		portReleaseTimeout:   5 * time.Second,
	}

	l.logger.Info("QUIC listener created", map[string]interface{}{
		"address": address,
	})

	return l, nil
}

func (l *listener) Accept(ctx context.Context) (Session, error) {
	// Combine contexts for proper cancellation
	acceptCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Cancel accept if listener is shutting down
	go func() {
		select {
		case <-l.ctx.Done():
			cancel()
		case <-acceptCtx.Done():
		}
	}()

	// Accept new QUIC connection
	conn, err := l.quicListener.Accept(acceptCtx)
	if err != nil {
		atomic.AddInt64(&l.rejectedConnections, 1)

		// Check if error is due to listener closure
		select {
		case <-l.ctx.Done():
			return nil, ErrListenerClosed
		default:
			l.logger.Warn("Failed to accept connection", map[string]interface{}{
				"error": err.Error(),
			})
			return nil, fmt.Errorf("accept failed: %w", err)
		}
	}

	// Generate session ID and create session
	sessionID := l.registry.GenerateID()

	session, err := l.createSession(sessionID, conn)
	if err != nil {
		// Clean close the connection on session creation failure
		conn.CloseWithError(quic.ApplicationErrorCode(1), "session creation failed")
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Register the session
	l.registry.Add(sessionID, session, conn)
	atomic.AddInt64(&l.acceptedConnections, 1)

	l.logger.Debug("New session created", map[string]interface{}{
		"session_id":  sessionID,
		"remote_addr": conn.RemoteAddr().String(),
	})

	return session, nil
}

func (l *listener) createSession(id protocol.SessionID, conn *quic.Conn) (Session, error) {
	// This would be your session creation logic
	return NewServerSession(id, conn, l)
}

func (l *listener) Addr() string {
	return l.address
}

func (l *listener) GetActiveSessionCount() int {
	return l.registry.Count()
}

func (l *listener) RemoveSession(sessionID protocol.SessionID) {
	session, conn := l.registry.Remove(sessionID)

	l.logger.Debug("Session removed", map[string]interface{}{
		"session_id": sessionID,
	})

	// Close the underlying QUIC connection if it exists and is still active
	if conn != nil {
		select {
		case <-conn.Context().Done():
			// Connection already closed
		default:
			// Force close with error
			if err := conn.CloseWithError(quic.ApplicationErrorCode(0), "session removed"); err != nil {
				l.logger.Warn("Failed to close QUIC connection", map[string]interface{}{
					"session_id": sessionID,
					"error":      err.Error(),
				})
			}
		}
	}

	// Trigger session cleanup if needed
	if session != nil {
		// Let the session know it should clean up
		go func() {
			defer func() {
				if r := recover(); r != nil {
					l.logger.Error("Panic during session cleanup", map[string]interface{}{
						"session_id": sessionID,
						"panic":      r,
					})
				}
			}()
			session.CloseWithError(0, "removed from listener")
		}()
	}
}

func (l *listener) IsClosed() bool {
	select {
	case <-l.ctx.Done():
		return true
	default:
		return false
	}
}

func (l *listener) Close() error {
	var closeErr error

	l.closeOnce.Do(func() {
		l.logger.Info("Starting listener shutdown", map[string]interface{}{
			"active_sessions": l.registry.Count(),
		})

		// Signal shutdown to all components
		l.cancel()
		close(l.shutdownChan)

		// Close sessions gracefully
		closeErr = l.gracefulShutdown()

		// Close the underlying QUIC listener
		if l.quicListener != nil {
			if err := l.quicListener.Close(); err != nil {
				l.logger.Error("Failed to close QUIC listener", map[string]interface{}{
					"error": err.Error(),
				})
				if closeErr == nil {
					closeErr = err
				}
			}
		}

		// Wait for port to be released
		l.waitForPortRelease()

		l.logger.Info("Listener shutdown completed", map[string]interface{}{
			"total_accepted": atomic.LoadInt64(&l.acceptedConnections),
			"total_rejected": atomic.LoadInt64(&l.rejectedConnections),
		})
	})

	return closeErr
}

func (l *listener) gracefulShutdown() error {
	deadline := time.Now().Add(l.gracefulCloseTimeout)

	// Collect all sessions for graceful shutdown
	var sessionIDs []protocol.SessionID
	var connections []*quic.Conn

	l.registry.ForEach(func(id protocol.SessionID, session Session, conn *quic.Conn) bool {
		sessionIDs = append(sessionIDs, id)
		connections = append(connections, conn)

		// Start graceful close for each session
		go func(s Session) {
			defer func() {
				if r := recover(); r != nil {
					l.logger.Error("Panic during session graceful close", map[string]interface{}{
						"session_id": id,
						"panic":      r,
					})
				}
			}()
			s.CloseWithError(0, "listener shutting down")
		}(session)

		return true
	})

	l.logger.Info("Starting graceful shutdown", map[string]interface{}{
		"sessions_count": len(sessionIDs),
		"timeout":        l.gracefulCloseTimeout.String(),
	})

	// Wait for sessions to close gracefully
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		remaining := l.registry.Count()
		if remaining == 0 {
			l.logger.Info("All sessions closed gracefully")
			return nil
		}

		if time.Now().After(deadline) {
			l.logger.Warn("Graceful shutdown timeout, forcing close", map[string]interface{}{
				"remaining_sessions": remaining,
			})
			l.forceCloseAllConnections()
			return nil
		}
	}
	return nil
}

func (l *listener) forceCloseAllConnections() {
	l.registry.ForEach(func(id protocol.SessionID, session Session, conn *quic.Conn) bool {
		if conn != nil {
			conn.CloseWithError(quic.ApplicationErrorCode(1), "forced shutdown")
		}
		// Remove from registry
		l.registry.Remove(id)
		return true
	})
}

func (l *listener) waitForPortRelease() {
	// Initial delay for OS cleanup
	time.Sleep(200 * time.Millisecond)

	deadline := time.Now().Add(l.portReleaseTimeout)
	checkInterval := 100 * time.Millisecond

	l.logger.Debug("Waiting for port to be released", map[string]interface{}{
		"address": l.address,
		"timeout": l.portReleaseTimeout.String(),
	})

	for time.Now().Before(deadline) {
		if l.isPortFree() {
			l.logger.Debug("Port successfully released")
			return
		}
		time.Sleep(checkInterval)
	}

	l.logger.Warn("Port release timeout reached", map[string]interface{}{
		"address": l.address,
	})
}

func (l *listener) isPortFree() bool {
	conn, err := net.ListenPacket("udp", l.address)
	if err != nil {
		return false
	}
	defer conn.Close()
	return true
}

// GetMetrics returns listener metrics for monitoring
func (l *listener) GetMetrics() map[string]interface{} {
	return map[string]interface{}{
		"active_sessions":      l.registry.Count(),
		"accepted_connections": atomic.LoadInt64(&l.acceptedConnections),
		"rejected_connections": atomic.LoadInt64(&l.rejectedConnections),
		"is_closed":            l.IsClosed(),
	}
}
