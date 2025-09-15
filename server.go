package kwik

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

type ServerSession struct {
	id          protocol.SessionID
	localAddr   string
	remoteAddr  string
	mu          sync.Mutex
	pathMgr     transport.PathManager
	streamMgr   *streamManagerImpl
	logger      logger.Logger
	packer      *transport.Packer
	multiplexer *transport.Multiplexer
	ctx         context.Context
	cancel      context.CancelFunc
	listener    *listener // Reference to parent listener for cleanup
}

// Ensure ServerSession implements Session
var _ Session = (*ServerSession)(nil)

func NewServerSession(id protocol.SessionID, conn *quic.Conn, l *listener) *ServerSession {
	lg := logger.NewLogger(logger.LogLevelSilent).WithComponent("SERVER_SESSION_FACTORY")
	lg.Debug("Creating new ServerSession...")
	pathMgr := transport.NewServerPathManager()
	packer := transport.NewPacker(1048576)

	// Créer un context avec cancel pour la session
	ctx, cancel := context.WithCancel(context.Background())

	// La retransmission sera gérée par le Path, pas à ce niveau

	sess := &ServerSession{
		id:      id,
		pathMgr: pathMgr, localAddr: conn.LocalAddr().String(),
		remoteAddr: conn.RemoteAddr().String(),
		logger:     logger.NewLogger(logger.LogLevelSilent).WithComponent("SERVER_SESSION"),
		packer:     packer,
		ctx:        ctx,
		cancel:     cancel,
		listener:   l,
	}
	sess.streamMgr = NewStreamManager(sess)
	sess.multiplexer = transport.NewMultiplexer(packer, sess.streamMgr)
	pathid, err := pathMgr.AccpetPath(conn, sess)
	if err != nil {
		panic(err)
	}
	pathMgr.SetPrimaryPath(pathid)

	return sess
}

func (s *ServerSession) Packer() *transport.Packer {
	return s.packer
}

func (s *ServerSession) Multiplexer() *transport.Multiplexer {
	return s.multiplexer
}

func (s *ServerSession) PathManager() transport.PathManager {
	return s.pathMgr
}

func (s *ServerSession) StreamManager() StreamManager {
	return s.streamMgr
}

func (s *ServerSession) Context() context.Context {
	return s.ctx
}

// listen on the given address with the given TLS and KWIK configurations
func ListenAddr(address string, tls *tls.Config, cfg *config.Config) (Listener, error) {
	l, err := listenAddr(address, tls, cfg)
	if err != nil {
		panic(err)
	}
	return l, nil
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
func (s *ServerSession) LocalAddr() string {
	return s.localAddr

}

func (s *ServerSession) IsClient() bool {
	return false
}

func (s *ServerSession) RemoteAddr() string {
	return s.remoteAddr

}
func (s *ServerSession) AcceptStream(ctx context.Context) (Stream, error) {
	s.mu.Lock()

	// Get the primary path
	path := s.pathMgr.GetPrimaryPath()
	if path == nil {
		s.mu.Unlock()
		return nil, protocol.NewPathNotExistsError(0)
	}

	s.logger.Debug("Accepting new stream on path", "pathID", path.PathID())

	// Accept a new stream from the client
	stream := s.streamMgr.CreateStream()
	streamID := stream.StreamID()

	// Release mutex before blocking call to allow CloseWithError to proceed
	s.mu.Unlock()

	if err := path.AcceptStream(ctx, streamID); err != nil {
		s.logger.Error("Failed to accept stream", "error", err, "pathID", path.PathID())
		return nil, fmt.Errorf("failed to accept stream: %w", err)
	}

	// Re-acquire mutex for the remaining operations
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Debug("Creating new stream", "streamID", streamID, "pathID", path.PathID())
	// Add the path to the stream (this will also register with packer)
	if err := s.streamMgr.AddPathToStream(streamID, path); err != nil {
		_ = s.streamMgr.CloseStream(streamID) // Clean up if path addition fails
		path.RemoveStream(streamID)
		s.logger.Error("Failed to add path to stream",
			"streamID", streamID,
			"pathID", path.PathID(),
			"error", err)
		return nil, fmt.Errorf("failed to add path to stream: %w", err)
	}

	s.logger.Info("Successfully accepted stream",
		"streamID", stream.StreamID(),
		"pathID", path.PathID())

	return stream, nil
}

func (s *ServerSession) OpenStreamSync(ctx context.Context) (Stream, error) {
	s.mu.Lock()

	// Get the primary path
	primaryPath := s.pathMgr.GetPrimaryPath()
	if primaryPath == nil {
		s.mu.Unlock()
		return nil, protocol.NewPathNotExistsError(0)
	}

	s.logger.Debug("Opening stream synchronously", "pathID", primaryPath.PathID())

	// Create the stream to get an ID
	streamImpl := s.streamMgr.CreateStream()
	streamID := streamImpl.StreamID()

	// Release mutex before blocking call
	s.mu.Unlock()

	// Open the stream with the generated ID
	err := primaryPath.OpenStreamSync(ctx, streamID)
	if err != nil {
		_ = s.streamMgr.CloseStream(streamID)
		return nil, err
	}

	// Re-acquire mutex for the remaining operations
	s.mu.Lock()
	defer s.mu.Unlock()

	// Add the path to the stream
	if err := s.streamMgr.AddPathToStream(streamID, primaryPath); err != nil {
		_ = s.streamMgr.CloseStream(streamID)
		return nil, err
	}

	s.logger.Debug("Successfully opened stream", "streamID", streamID, "pathID", primaryPath.PathID())
	return streamImpl, nil
}

func (s *ServerSession) OpenStream() (Stream, error) {
	s.mu.Lock()

	// Get the primary path
	primaryPath := s.pathMgr.GetPrimaryPath()
	if primaryPath == nil {
		s.mu.Unlock()
		return nil, protocol.NewPathNotExistsError(0)
	}

	// Create the stream to get an ID
	streamImpl := s.streamMgr.CreateStream()
	streamID := streamImpl.StreamID()

	// Release mutex before potentially blocking call
	s.mu.Unlock()

	// Open the stream with the generated ID
	err := primaryPath.OpenStream(streamID)
	if err != nil {
		_ = s.streamMgr.CloseStream(streamID)
		return nil, err
	}

	// Re-acquire mutex for the remaining operations
	s.mu.Lock()
	defer s.mu.Unlock()

	// Add the path to the stream
	if err := s.streamMgr.AddPathToStream(streamID, primaryPath); err != nil {
		_ = s.streamMgr.CloseStream(streamID)
		return nil, err
	}

	s.logger.Debug("Opened new stream", "streamID", streamID, "pathID", primaryPath.PathID())
	return streamImpl, nil
}

func (s *ServerSession) AddRelay(address string) (Relay, error) {
	// Pour le serveur, nous déléguons directement à l'implémentation du path manager
	return s.pathMgr.AddRelay(address)
}

func (s *ServerSession) RemovePath(pathID string) error {
	// Not implemented for server session.
	return nil
}

func (s *ServerSession) SessionID() protocol.SessionID {
	return s.id
}

// Raw packet transmission for custom protocols
func (s *ServerSession) SendRawData(data []byte, pathID protocol.PathID, streamID protocol.StreamID) error {
	// Not yet implemented.
	return nil
}

func (s *ServerSession) CloseWithError(code int, msg string) error {
	// Cancel context FIRST to unblock any ongoing I/O operations
	if s.cancel != nil {
		s.cancel()
	}

	// Small delay to let cancelled operations finish
	time.Sleep(10 * time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Log close reason for debugging (who/why closed the session)
	s.logger.Info("CloseWithError called (server)", "code", code, "msg", msg, "session", s.id, "local", s.localAddr, "remote", s.remoteAddr)

	// Annuler le context pour signaler la fermeture
	if s.cancel != nil {
		s.cancel()
	}

	if s.streamMgr != nil {
		s.streamMgr.CloseAllStreams()
	}

	if pm, ok := s.pathMgr.(interface{ CloseAllPaths() error }); ok {
		if err := pm.CloseAllPaths(); err != nil {
			s.logger.Error("Failed to close all paths", "error", err)
			return err
		}
	}

	// Stop multiplexer and packer background goroutines
	if s.multiplexer != nil {
		s.multiplexer.Close()
	}
	if s.packer != nil {
		s.packer.Close()
	}

	// Notify the listener to remove this session from active sessions
	if s.listener != nil {
		s.listener.RemoveSession(s.id)
	}
	return nil
}
