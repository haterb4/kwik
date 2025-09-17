package kwik

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

var (
	ErrSessionNotActive  = errors.New("session not active")
	ErrNoPrimaryPath     = errors.New("no primary path available")
	ErrPathNotReady      = errors.New("path not ready")
	ErrConnectionTimeout = errors.New("connection timeout")
)

type ClientSession struct {
	// Basic session info
	id         protocol.SessionID
	localAddr  string
	remoteAddr string

	// Core components
	pathMgr     transport.PathManager
	streamMgr   transport.StreamManager
	packer      *transport.Packer
	multiplexer *transport.Multiplexer
	logger      logger.Logger

	// State management
	state     int32 // atomic SessionState
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeCh   chan struct{}

	// Configuration
	config            *config.Config
	tlsConfig         *tls.Config
	connectionTimeout time.Duration
	reconnectAttempts int
	reconnectInterval time.Duration

	// Metrics
	openStreams    int64
	closedStreams  int64
	pathFailures   int64
	reconnectCount int64
	lastActivity   int64 // Unix timestamp

	// Synchronization
	mu sync.RWMutex // Use RWMutex for better read performance
}

// Ensure ClientSession implements .Session
var _ Session = (*ClientSession)(nil)

type ClientSessionOptions struct {
	ConnectionTimeout time.Duration
	ReconnectAttempts int
	ReconnectInterval time.Duration
	Logger            logger.Logger
}

func DefaultClientSessionOptions() *ClientSessionOptions {
	return &ClientSessionOptions{
		ConnectionTimeout: 30 * time.Second,
		ReconnectAttempts: 3,
		ReconnectInterval: 1 * time.Second,
		Logger:            logger.NewLogger(logger.LogLevelInfo),
	}
}

// DialAddr creates a new client session and connects to the specified address
func DialAddr(ctx context.Context, address string, tls *tls.Config, cfg *config.Config) (*ClientSession, error) {
	return DialAddrWithOptions(ctx, address, tls, cfg, DefaultClientSessionOptions())
}

// DialAddrWithOptions creates a new client session with custom options
func DialAddrWithOptions(ctx context.Context, address string, tls *tls.Config, cfg *config.Config, opts *ClientSessionOptions) (*ClientSession, error) {
	sess := NewClientSession(address, tls, cfg, opts)

	if err := sess.Connect(ctx); err != nil {
		sess.Close()
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	return sess, nil
}

// NewClientSession creates a new client session instance
func NewClientSession(address string, tls *tls.Config, cfg *config.Config, opts *ClientSessionOptions) *ClientSession {
	if opts == nil {
		opts = DefaultClientSessionOptions()
	}

	ctx, cancel := context.WithCancel(context.Background())

	sess := &ClientSession{
		id:                0, // Will be assigned during handshake
		remoteAddr:        address,
		config:            cfg,
		tlsConfig:         tls,
		ctx:               ctx,
		cancel:            cancel,
		closeCh:           make(chan struct{}),
		connectionTimeout: opts.ConnectionTimeout,
		reconnectAttempts: opts.ReconnectAttempts,
		reconnectInterval: opts.ReconnectInterval,
		logger:            opts.Logger.WithComponent("CLIENT_SESSION"),
		lastActivity:      time.Now().Unix(),
	}

	// Initialize components
	sess.pathMgr = transport.NewClientPathManager(tls, cfg)
	sess.packer = transport.NewPacker(1048576) // 1MB buffer
	sess.streamMgr = NewStreamManager(sess)
	sess.multiplexer = transport.NewMultiplexer(sess.packer, sess.streamMgr)

	// Set initial state
	atomic.StoreInt32(&sess.state, int32(transport.SessionStateConnecting))

	return sess
}

/*
=====================================================================================================
* Client session management
*  - connect to remote server
*  - manage paths
*  - manage streams
*  - handle session state
*  - handle authentication
*  - expose session methods
*========================================================================================
=======================================================================================================
*/

// Connect establishes connection to the remote server
func (s *ClientSession) Connect(ctx context.Context) error {
	if !s.compareAndSetState(transport.SessionStateConnecting, transport.SessionStateConnecting) {
		return errors.New("session already connected or closed")
	}

	// Set connection timeout
	connectCtx, cancel := context.WithTimeout(ctx, s.connectionTimeout)
	defer cancel()

	s.logger.Info("Initiating connection", map[string]interface{}{
		"remote_addr": s.remoteAddr,
		"timeout":     s.connectionTimeout.String(),
	})

	// Attempt connection with retries
	var lastErr error
	for attempt := 0; attempt <= s.reconnectAttempts; attempt++ {
		if attempt > 0 {
			s.logger.Debug("Connection retry attempt", map[string]interface{}{
				"attempt": attempt,
				"max":     s.reconnectAttempts,
			})

			// Wait before retry
			select {
			case <-time.After(s.reconnectInterval):
			case <-connectCtx.Done():
				return ErrConnectionTimeout
			}
		}

		if err := s.establishConnection(connectCtx); err != nil {
			lastErr = err
			atomic.AddInt64(&s.reconnectCount, 1)
			s.logger.Warn("Connection attempt failed", map[string]interface{}{
				"attempt": attempt,
				"error":   err.Error(),
			})
			continue
		}

		// Connection successful
		s.setState(transport.SessionStateActive)
		s.updateActivity()

		// Get local address from primary path
		if primaryPath := s.pathMgr.GetPrimaryPath(); primaryPath != nil {
			s.localAddr = primaryPath.LocalAddr()
		}

		s.logger.Info("Connection established successfully", map[string]interface{}{
			"local_addr":  s.localAddr,
			"remote_addr": s.remoteAddr,
			"attempts":    attempt + 1,
		})

		return nil
	}

	s.setState(transport.SessionStateClosed)
	return fmt.Errorf("failed to connect after %d attempts: %w", s.reconnectAttempts+1, lastErr)
}

func (s *ClientSession) establishConnection(ctx context.Context) error {
	// Create primary path
	pathID, err := s.pathMgr.OpenPath(ctx, s.remoteAddr, s)
	if err != nil {
		return fmt.Errorf("failed to open path: %w", err)
	}

	if pathID <= 0 {
		return protocol.NewInvalidPathIDError(pathID)
	}

	// Set as primary path
	s.pathMgr.SetPrimaryPath(pathID)

	// Verify primary path
	primaryPath := s.pathMgr.GetPrimaryPath()
	if primaryPath == nil {
		return protocol.NewPathNotExistsError(pathID)
	}

	s.logger.Debug("Primary path established", map[string]interface{}{
		"path_id":     pathID,
		"local_addr":  primaryPath.LocalAddr(),
		"remote_addr": primaryPath.RemoteAddr(),
	})

	return nil
}

// Session interface implementation
func (s *ClientSession) SessionID() protocol.SessionID {
	return s.id
}

func (s *ClientSession) LocalAddr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.localAddr
}

func (s *ClientSession) RemoteAddr() string {
	return s.remoteAddr
}

func (s *ClientSession) IsClient() bool {
	return true
}

func (s *ClientSession) Context() context.Context {
	return s.ctx
}

// Component accessors
func (s *ClientSession) PathManager() transport.PathManager {
	return s.pathMgr
}

func (s *ClientSession) StreamManager() StreamManager {
	return s.streamMgr
}

func (s *ClientSession) Packer() *transport.Packer {
	return s.packer
}

func (s *ClientSession) Multiplexer() *transport.Multiplexer {
	return s.multiplexer
}

// Stream operations
func (s *ClientSession) AcceptStream(ctx context.Context) (Stream, error) {
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

	// Accept stream on path
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

func (s *ClientSession) OpenStream() (Stream, error) {
	return s.OpenStreamSync(context.Background())
}

func (s *ClientSession) OpenStreamSync(ctx context.Context) (Stream, error) {
	if err := s.checkSessionActive(); err != nil {
		return nil, err
	}

	path, err := s.getPrimaryPath()
	if err != nil {
		return nil, err
	}

	if path.State() != transport.PathStateReady {
		return nil, ErrPathNotReady
	}

	// Create stream
	stream := s.streamMgr.CreateStream()
	streamID := stream.StreamID()

	s.logger.Debug("Opening stream", map[string]interface{}{
		"stream_id": streamID,
		"path_id":   path.PathID(),
	})

	// Open stream on path
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
func (s *ClientSession) HandlePathFailure(pathID protocol.PathID, fatalErr error) {
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
		go s.CloseWithError(1, "all paths failed")
	}
}

// Raw packet transmission
func (s *ClientSession) SendRawData(data []byte, pathID protocol.PathID, streamID protocol.StreamID) error {
	if err := s.checkSessionActive(); err != nil {
		return err
	}

	s.updateActivity()
	// Implementation depends on your transport layer
	return fmt.Errorf("SendRawData not yet implemented")
}

// Session lifecycle
func (s *ClientSession) CloseWithError(code int, msg string) error {
	return s.close(code, msg, false)
}

func (s *ClientSession) Close() error {
	return s.close(0, "client requested close", true)
}

func (s *ClientSession) close(code int, msg string, graceful bool) error {
	var err error

	s.closeOnce.Do(func() {
		s.logger.Info("Closing client session", map[string]interface{}{
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

		// Set final state
		s.setState(transport.SessionStateClosed)

		s.logger.Info("Client session closed", map[string]interface{}{
			"open_streams":   atomic.LoadInt64(&s.openStreams),
			"closed_streams": atomic.LoadInt64(&s.closedStreams),
			"path_failures":  atomic.LoadInt64(&s.pathFailures),
			"reconnects":     atomic.LoadInt64(&s.reconnectCount),
		})
	})

	return err
}

// Not implemented for client
func (s *ClientSession) AddRelay(address string) (transport.Relay, error) {
	return nil, errors.New("AddRelay not supported for client sessions")
}

func (s *ClientSession) RemovePath(pathID string) error {
	return errors.New("RemovePath not supported for client sessions")
}

// Helper methods
func (s *ClientSession) checkSessionActive() error {
	state := transport.SessionState(atomic.LoadInt32(&s.state))
	if state != transport.SessionStateActive {
		return fmt.Errorf("%w: state is %s", ErrSessionNotActive, state.String())
	}
	return nil
}

func (s *ClientSession) getPrimaryPath() (transport.Path, error) {
	path := s.pathMgr.GetPrimaryPath()
	if path == nil {
		return nil, ErrNoPrimaryPath
	}
	return path, nil
}

func (s *ClientSession) setState(state transport.SessionState) {
	atomic.StoreInt32(&s.state, int32(state))
}

func (s *ClientSession) getState() transport.SessionState {
	return transport.SessionState(atomic.LoadInt32(&s.state))
}

func (s *ClientSession) compareAndSetState(expected, new transport.SessionState) bool {
	return atomic.CompareAndSwapInt32(&s.state, int32(expected), int32(new))
}

func (s *ClientSession) updateActivity() {
	atomic.StoreInt64(&s.lastActivity, time.Now().Unix())
}

// Monitoring and metrics
func (s *ClientSession) GetMetrics() map[string]interface{} {
	return map[string]interface{}{
		"state":          s.getState().String(),
		"open_streams":   atomic.LoadInt64(&s.openStreams),
		"closed_streams": atomic.LoadInt64(&s.closedStreams),
		"path_failures":  atomic.LoadInt64(&s.pathFailures),
		"reconnects":     atomic.LoadInt64(&s.reconnectCount),
		"last_activity":  time.Unix(atomic.LoadInt64(&s.lastActivity), 0),
		"active_paths":   len(s.pathMgr.GetActivePathIDs()),
	}
}

func (s *ClientSession) IsActive() bool {
	return s.getState() == transport.SessionStateActive
}

func (s *ClientSession) IsClosed() bool {
	state := s.getState()
	return state == transport.SessionStateClosed || state == transport.SessionStateClosing
}
