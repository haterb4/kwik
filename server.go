package kwik

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

var (
	ErrServerSessionNotActive = errors.New("server session not active")
	ErrServerNoPrimaryPath    = errors.New("no primary path available")
	ErrServerSessionClosed    = errors.New("server session closed")
	ErrInvalidConnection      = errors.New("invalid QUIC connection")
)

type ServerSession struct {
	// Basic session info
	id         protocol.SessionID
	localAddr  string
	remoteAddr string

	// Core components
	pathMgr     transport.PathManager
	streamMgr   StreamManager
	packer      *transport.Packer
	multiplexer *transport.Multiplexer
	logger      logger.Logger

	// State management
	state     int32 // atomic ServerSessionState
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeCh   chan struct{}

	// Parent listener reference
	listener Listener

	// Configuration
	config    *config.Config
	tlsConfig *tls.Config

	// Metrics
	openStreams   int64
	closedStreams int64
	pathFailures  int64
	lastActivity  int64 // Unix timestamp
	createdAt     time.Time

	// Synchronization
	mu sync.RWMutex // Use RWMutex for better read performance

	// Connection tracking
	quicConn *quic.Conn
}

// Ensure ServerSession implements Session
var _ Session = (*ServerSession)(nil)

// NewServerSession creates a new server session from an incoming QUIC connection
func NewServerSession(id protocol.SessionID, conn *quic.Conn, l Listener) (*ServerSession, error) {
	if conn == nil {
		return nil, ErrInvalidConnection
	}

	if id <= 0 {
		return nil, protocol.NewInvalidSessionIDError(id)
	}

	ctx, cancel := context.WithCancel(context.Background())

	sess := &ServerSession{
		id:           id,
		localAddr:    conn.LocalAddr().String(),
		remoteAddr:   conn.RemoteAddr().String(),
		listener:     l,
		quicConn:     conn,
		ctx:          ctx,
		cancel:       cancel,
		closeCh:      make(chan struct{}),
		createdAt:    time.Now(),
		logger:       logger.NewLogger(logger.LogLevelDebug).WithComponent("SERVER_SESSION"),
		lastActivity: time.Now().Unix(),
	}

	// Initialize components
	sess.pathMgr = transport.NewServerPathManager()
	sess.packer = transport.NewPacker(1048576) // 1MB buffer
	sess.streamMgr = NewStreamManager(sess)
	sess.multiplexer = transport.NewMultiplexer(sess.packer, sess.streamMgr)

	// Set initial state
	atomic.StoreInt32(&sess.state, int32(transport.SessionStateInitializing))

	sess.logger.Debug("Creating server session", map[string]interface{}{
		"session_id":  id,
		"local_addr":  sess.localAddr,
		"remote_addr": sess.remoteAddr,
	})

	// Accept the incoming QUIC connection as primary path
	if err := sess.initializePrimaryPath(conn); err != nil {
		sess.close(1, "failed to initialize primary path", false)
		return nil, fmt.Errorf("failed to initialize primary path: %w", err)
	}

	// Activate the session
	sess.setState(transport.SessionStateActive)
	sess.logger.Info("Server session created and activated", map[string]interface{}{
		"session_id":  id,
		"local_addr":  sess.localAddr,
		"remote_addr": sess.remoteAddr,
	})

	return sess, nil
}

/*
=====================================================================================================
* Server session management
*  - connect to remote server
*  - manage paths
*  - manage streams
*  - handle session state
*  - handle authentication
*  - expose session methods
*========================================================================================
=======================================================================================================
*/

// initializePrimaryPath sets up the primary path from the QUIC connection
func (s *ServerSession) initializePrimaryPath(conn *quic.Conn) error {
	// Accept the connection as a path
	pathID, err := s.pathMgr.AcceptPath(conn, s)
	if err != nil {
		return fmt.Errorf("failed to accept path: %w", err)
	}

	// Set as primary path
	s.pathMgr.SetPrimaryPath(pathID)

	// Verify primary path was set
	primaryPath := s.pathMgr.GetPrimaryPath()
	if primaryPath == nil {
		return protocol.NewPathNotExistsError(pathID)
	}

	s.logger.Debug("Primary path initialized", map[string]interface{}{
		"path_id":     pathID,
		"local_addr":  primaryPath.LocalAddr(),
		"remote_addr": primaryPath.RemoteAddr(),
	})

	return nil
}

// Session interface implementation
func (s *ServerSession) SessionID() protocol.SessionID {
	return s.id
}

func (s *ServerSession) LocalAddr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.localAddr
}

func (s *ServerSession) RemoteAddr() string {
	return s.remoteAddr
}

func (s *ServerSession) IsClient() bool {
	return false
}

func (s *ServerSession) Context() context.Context {
	return s.ctx
}

// Component accessors
func (s *ServerSession) PathManager() transport.PathManager {
	return s.pathMgr
}

func (s *ServerSession) StreamManager() StreamManager {
	return s.streamMgr
}

func (s *ServerSession) Packer() *transport.Packer {
	return s.packer
}

func (s *ServerSession) Multiplexer() *transport.Multiplexer {
	return s.multiplexer
}

// Stream operations
func (s *ServerSession) AcceptStream(ctx context.Context) (Stream, error) {
	if err := s.checkSessionActive(); err != nil {
		return nil, err
	}

	path, err := s.getPrimaryPath()
	if err != nil {
		return nil, err
	}

	// Create stream
	stream := s.streamMgr.CreateStream()
	streamID := stream.StreamID()

	s.logger.Debug("Accepting stream", map[string]interface{}{
		"stream_id": streamID,
		"path_id":   path.PathID(),
	})

	// Accept stream on path (this may block, so don't hold mutex)
	if err := path.AcceptStream(ctx, streamID); err != nil {
		s.streamMgr.CloseStream(streamID)
		return nil, fmt.Errorf("failed to accept stream: %w", err)
	}

	// Add path to stream
	if err := s.streamMgr.AddPathToStream(streamID, path); err != nil {
		s.streamMgr.CloseStream(streamID)
		path.RemoveStream(streamID)
		return nil, fmt.Errorf("failed to add path to stream: %w", err)
	}

	atomic.AddInt64(&s.openStreams, 1)
	s.updateActivity()

	s.logger.Info("Stream accepted successfully", map[string]interface{}{
		"stream_id": streamID,
		"path_id":   path.PathID(),
	})

	return stream, nil
}

func (s *ServerSession) OpenStream() (Stream, error) {
	return s.OpenStreamSync(context.Background())
}

func (s *ServerSession) OpenStreamSync(ctx context.Context) (Stream, error) {
	if err := s.checkSessionActive(); err != nil {
		return nil, err
	}

	path, err := s.getPrimaryPath()
	if err != nil {
		return nil, err
	}

	// Create stream
	stream := s.streamMgr.CreateStream()
	streamID := stream.StreamID()

	s.logger.Debug("Opening stream", map[string]interface{}{
		"stream_id": streamID,
		"path_id":   path.PathID(),
	})

	// Open stream on path (this may block, so don't hold mutex)
	if err := path.OpenStreamSync(ctx, streamID); err != nil {
		s.streamMgr.CloseStream(streamID)
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	// Add path to stream
	if err := s.streamMgr.AddPathToStream(streamID, path); err != nil {
		s.streamMgr.CloseStream(streamID)
		path.RemoveStream(streamID)
		return nil, fmt.Errorf("failed to add path to stream: %w", err)
	}

	atomic.AddInt64(&s.openStreams, 1)
	s.updateActivity()

	s.logger.Info("Stream opened successfully", map[string]interface{}{
		"stream_id": streamID,
		"path_id":   path.PathID(),
	})

	return stream, nil
}

// Path failure handling
func (s *ServerSession) HandlePathFailure(pathID protocol.PathID, fatalErr error) {
	atomic.AddInt64(&s.pathFailures, 1)

	s.logger.Error("Handling path failure", map[string]interface{}{
		"path_id":    pathID,
		"error":      fatalErr.Error(),
		"session_id": s.SessionID(),
	})

	// Remove failed path
	s.pathMgr.RemovePath(pathID)

	// Check remaining active paths
	activePathIDs := s.pathMgr.GetActivePathIDs()
	s.logger.Info("Active paths after failure", map[string]interface{}{
		"remaining_count": len(activePathIDs),
		"path_ids":        activePathIDs,
	})

	// If no active paths remain, close the session
	if len(activePathIDs) == 0 {
		s.logger.Warn("No active paths remaining, closing session", map[string]interface{}{
			"session_id": s.SessionID(),
		})
		go s.closeSessionAndNotifyListener()
	}
}

// closeSessionAndNotifyListener handles session closure and listener notification
func (s *ServerSession) closeSessionAndNotifyListener() {
	// Close the session
	if err := s.CloseWithError(1, "all paths failed"); err != nil {
		s.logger.Error("Failed to close session after path failure", map[string]interface{}{
			"session_id": s.SessionID(),
			"error":      err.Error(),
		})
	}

	// Notify listener to remove this session
	if listener, ok := s.listener.(interface{ RemoveSession(protocol.SessionID) }); ok {
		listener.RemoveSession(s.SessionID())
		s.logger.Debug("Session removed from listener", map[string]interface{}{
			"session_id": s.SessionID(),
		})
	}
}

// Relay management
func (s *ServerSession) AddRelay(address string) (transport.Relay, error) {
	if err := s.checkSessionActive(); err != nil {
		return nil, err
	}

	s.updateActivity()
	return s.pathMgr.AddRelay(address)
}

func (s *ServerSession) RemovePath(pathID string) error {
	// Not typically used for server sessions - paths are managed automatically
	return errors.New("RemovePath not supported for server sessions")
}

// Raw packet transmission
func (s *ServerSession) SendRawData(data []byte, pathID protocol.PathID, streamID protocol.StreamID) error {
	if err := s.checkSessionActive(); err != nil {
		return err
	}

	s.updateActivity()
	// Implementation depends on your transport layer
	return fmt.Errorf("SendRawData not yet implemented")
}

// Session lifecycle
func (s *ServerSession) CloseWithError(code int, msg string) error {
	return s.close(code, msg, false)
}

func (s *ServerSession) Close() error {
	return s.close(0, "server requested close", true)
}

func (s *ServerSession) close(code int, msg string, graceful bool) error {
	var err error

	s.closeOnce.Do(func() {
		s.logger.Info("Closing server session", map[string]interface{}{
			"code":     code,
			"message":  msg,
			"graceful": graceful,
		})

		// Set closing state
		s.setState(transport.SessionStateClosing)

		// Cancel context to signal shutdown
		if s.cancel != nil {
			s.cancel()
		}

		// Close notification channel
		close(s.closeCh)

		// Small delay to allow ongoing operations to complete
		if graceful {
			time.Sleep(50 * time.Millisecond)
		}

		// Close all streams
		if s.streamMgr != nil {
			s.streamMgr.CloseAllStreams()
		}

		// Close all paths
		if pm, ok := s.pathMgr.(interface{ CloseAllPaths() error }); ok {
			if pathErr := pm.CloseAllPaths(); pathErr != nil {
				s.logger.Error("Failed to close paths", map[string]interface{}{
					"error": pathErr.Error(),
				})
				if err == nil {
					err = pathErr
				}
			}
		}

		// Stop background components
		if s.multiplexer != nil {
			s.multiplexer.Close()
		}

		if s.packer != nil {
			s.packer.Close()
		}

		// Close underlying QUIC connection if we still have access
		if s.quicConn != nil {
			select {
			case <-s.quicConn.Context().Done():
				// Already closed
			default:
				if closeErr := s.quicConn.CloseWithError(quic.ApplicationErrorCode(code), msg); closeErr != nil {
					s.logger.Warn("Failed to close QUIC connection", map[string]interface{}{
						"error": closeErr.Error(),
					})
				}
			}
		}

		// Notify listener to remove this session
		if listener, ok := s.listener.(interface{ RemoveSession(protocol.SessionID) }); ok {
			listener.RemoveSession(s.SessionID())
		}

		// Set final state
		s.setState(transport.SessionStateClosed)

		uptime := time.Since(s.createdAt)
		s.logger.Info("Server session closed", map[string]interface{}{
			"session_id":     s.SessionID(),
			"uptime":         uptime.String(),
			"open_streams":   atomic.LoadInt64(&s.openStreams),
			"closed_streams": atomic.LoadInt64(&s.closedStreams),
			"path_failures":  atomic.LoadInt64(&s.pathFailures),
		})
	})

	return err
}

// Helper methods
func (s *ServerSession) checkSessionActive() error {
	state := transport.SessionState(atomic.LoadInt32(&s.state))
	if state != transport.SessionStateActive {
		return fmt.Errorf("%w: state is %s", ErrServerSessionNotActive, state.String())
	}
	return nil
}

func (s *ServerSession) getPrimaryPath() (transport.Path, error) {
	path := s.pathMgr.GetPrimaryPath()
	if path == nil {
		return nil, ErrServerNoPrimaryPath
	}
	return path, nil
}

func (s *ServerSession) setState(state transport.SessionState) {
	atomic.StoreInt32(&s.state, int32(state))
}

func (s *ServerSession) getState() transport.SessionState {
	return transport.SessionState(atomic.LoadInt32(&s.state))
}

func (s *ServerSession) compareAndSetState(expected, new transport.SessionState) bool {
	return atomic.CompareAndSwapInt32(&s.state, int32(expected), int32(new))
}

func (s *ServerSession) updateActivity() {
	atomic.StoreInt64(&s.lastActivity, time.Now().Unix())
}

// Monitoring and metrics
func (s *ServerSession) GetMetrics() map[string]interface{} {
	uptime := time.Since(s.createdAt)
	return map[string]interface{}{
		"session_id":     s.SessionID(),
		"state":          s.getState().String(),
		"uptime":         uptime.String(),
		"open_streams":   atomic.LoadInt64(&s.openStreams),
		"closed_streams": atomic.LoadInt64(&s.closedStreams),
		"path_failures":  atomic.LoadInt64(&s.pathFailures),
		"last_activity":  time.Unix(atomic.LoadInt64(&s.lastActivity), 0),
		"active_paths":   len(s.pathMgr.GetActivePathIDs()),
		"local_addr":     s.localAddr,
		"remote_addr":    s.remoteAddr,
	}
}

func (s *ServerSession) IsActive() bool {
	return s.getState() == transport.SessionStateActive
}

func (s *ServerSession) IsClosed() bool {
	state := s.getState()
	return state == transport.SessionStateClosed || state == transport.SessionStateClosing
}

func (s *ServerSession) GetUptime() time.Duration {
	return time.Since(s.createdAt)
}

// ListenAddr creates a new QUIC listener (moved from session to package level for consistency)
func ListenAddr(address string, tls *tls.Config, cfg *config.Config) (Listener, error) {
	return listenAddr(address, tls, cfg)
}
