package kwik

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"

	"github.com/s-anzie/kwik/internal/transport"
)

type ClientSession struct {
	id          protocol.SessionID
	localAddr   string
	remoteAddr  string
	mu          sync.Mutex
	pathMgr     transport.PathManager
	streamMgr   streamManager
	logger      logger.Logger
	packer      *transport.Packer
	multiplexer *transport.Multiplexer
}

// Ensure ClientSession implements .Session
var _ Session = (*ClientSession)(nil)

// DialAddr dials the given address with the provided TLS and KWIK configurations using a context.
func DialAddr(ctx context.Context, address string, tls *tls.Config, cfg *config.Config) (*ClientSession, error) {
	sess := NewClientSession(address, tls, cfg)
	err := sess.connect(ctx)
	if err != nil {
		panic(err)
	}
	sess.localAddr = sess.pathMgr.GetPrimaryPath().LocalAddr()
	return sess, nil
}

func NewClientSession(address string, tls *tls.Config, cfg *config.Config) *ClientSession {
	mgr := transport.NewClientPathManager(tls, cfg)
	packer := transport.NewPacker(1200)
	multiplexer := transport.NewMultiplexer(packer)
	sess := &ClientSession{
		id:          0,
		remoteAddr:  address,
		pathMgr:     mgr,
		streamMgr:   NewStreamManager(),
		logger:      logger.NewLogger(logger.LogLevelSilent).WithComponent("CLIENT_SESSION"),
		packer:      packer,
		multiplexer: multiplexer,
	}
	return sess
}

func (s *ClientSession) Packer() *transport.Packer {
	return s.packer
}

func (s *ClientSession) Multiplexer() *transport.Multiplexer {
	return s.multiplexer
}

func (s *ClientSession) PathManager() transport.PathManager {
	return s.pathMgr
}
func (s *ClientSession) StreamManager() streamManager {
	return s.streamMgr
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

func (s *ClientSession) connect(ctx context.Context) error {
	s.logger.Debug("Connecting to server", "address", s.remoteAddr)

	// Create the primary path with the path manager and mark it as main path.
	id, err := s.pathMgr.OpenPath(ctx, s.remoteAddr, s)
	if err != nil {
		s.logger.Error("Failed to open path", "error", err)
		return err
	}

	if id <= 0 {
		err := protocol.NewInvalidPathIDError(id)
		s.logger.Error("Invalid path ID", "pathID", id, "error", err)
		return err
	}

	s.logger.Debug("Setting primary path", "pathID", id)
	s.pathMgr.SetPrimaryPath(id)

	// Verify the primary path was set correctly
	primaryPath := s.pathMgr.GetPrimaryPath()
	if primaryPath == nil {
		err := protocol.NewPathNotExistsError(id)
		s.logger.Error("Failed to set primary path", "pathID", id, "error", err)
		return err
	}

	s.logger.Info("Successfully connected to server", "pathID", id, "localAddr", primaryPath.LocalAddr(), "remoteAddr", primaryPath.RemoteAddr())
	return nil
}

func (s *ClientSession) LocalAddr() string {
	return s.localAddr

}
func (s *ClientSession) RemoteAddr() string {
	return s.remoteAddr
}

func (s *ClientSession) AcceptStream(ctx context.Context) (Stream, error) {
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
	if err := path.AcceptStream(ctx, stream.StreamID()); err != nil {
		s.mu.Unlock()
		s.logger.Error("Failed to accept stream", "error", err, "pathID", path.PathID())
		return nil, fmt.Errorf("failed to accept stream: %w", err)
	}
	s.multiplexer.RegisterStream(stream.StreamID())
	s.logger.Debug("Creating new stream", "streamID", stream.StreamID(), "pathID", path.PathID())
	// Add the path to the stream (this will also register with packer)
	if err := s.streamMgr.AddStreamPath(stream.StreamID(), path); err != nil {
		s.mu.Unlock()
		s.streamMgr.RemoveStream(stream.StreamID()) // Clean up if path addition fails
		path.RemoveStream(stream.StreamID())
		s.logger.Error("Failed to add path to stream",
			"streamID", stream.StreamID(),
			"pathID", path.PathID(),
			"error", err)
		return nil, fmt.Errorf("failed to add path to stream: %w", err)
	}

	s.mu.Unlock()

	s.logger.Info("Successfully accepted stream",
		"streamID", stream.StreamID(),
		"pathID", path.PathID())

	return stream, nil
}

func (s *ClientSession) OpenStreamSync(ctx context.Context) (Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Get the primary path
	primaryPath := s.pathMgr.GetPrimaryPath()
	if primaryPath == nil {
		return nil, protocol.NewPathNotExistsError(0)
	}

	s.logger.Debug("Opening stream synchronously", "pathID", primaryPath.PathID())

	// Create the stream to get an ID
	streamImpl := s.streamMgr.CreateStream()
	streamID := streamImpl.StreamID()
	s.multiplexer.RegisterStream(streamID)
	// Open the stream with the generated ID
	err := primaryPath.OpenStreamSync(ctx, streamID)
	if err != nil {
		s.streamMgr.RemoveStream(streamID)
		return nil, err
	}

	// Add the path to the stream
	if err := s.streamMgr.AddStreamPath(streamID, primaryPath); err != nil {
		s.streamMgr.RemoveStream(streamID)
		return nil, err
	}

	s.logger.Debug("Successfully opened stream", "streamID", streamID, "pathID", primaryPath.PathID())
	return streamImpl, nil
}

func (s *ClientSession) OpenStream() (Stream, error) {
	s.mu.Lock()

	// Get the primary path
	path := s.pathMgr.GetPrimaryPath()
	if path == nil {
		s.mu.Unlock()
		s.logger.Error("No primary path available for opening stream")
		return nil, protocol.NewPathNotExistsError(0)
	}

	// Generate a new stream ID (odd-numbered for client-initiated streams)
	streamImpl := s.streamMgr.CreateStream()
	streamID := streamImpl.StreamID()
	s.multiplexer.RegisterStream(streamID)
	s.logger.Debug("Opening new stream", "streamID", streamID, "pathID", path.PathID())

	// Create the stream first
	streamLogger := logger.NewLogger(logger.LogLevelSilent).WithComponent(fmt.Sprintf("STREAM_%d", streamID))
	stream := &StreamImpl{
		id:            streamID,
		paths:         make(map[protocol.PathID]transport.Path),
		logger:        streamLogger,
		streamManager: s.streamMgr.(*streamManagerImpl), // Type assertion since streamManager is an interface
	}

	// Register the stream with the manager before opening the stream
	if mgr, ok := s.streamMgr.(*streamManagerImpl); ok {
		mgr.addStream(stream)
	} else {
		s.mu.Unlock()
		return nil, fmt.Errorf("invalid stream manager type")
	}

	// Now open the stream on the path
	if err := path.OpenStream(streamID); err != nil {
		s.mu.Unlock()
		s.streamMgr.RemoveStream(streamID) // Clean up if stream opening fails
		s.logger.Error("Failed to open stream on path",
			"streamID", streamID,
			"pathID", path.PathID(),
			"error", err)
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	// Add the path to the stream (this will also register with packer)
	if err := stream.AddPath(path); err != nil {
		s.mu.Unlock()
		s.streamMgr.RemoveStream(streamID) // Clean up if path addition fails
		s.logger.Error("Failed to add path to stream",
			"streamID", streamID,
			"pathID", path.PathID(),
			"error", err)
		return nil, fmt.Errorf("failed to add path to stream: %w", err)
	}

	s.mu.Unlock()

	s.logger.Info("Successfully opened stream",
		"streamID", streamID,
		"pathID", path.PathID())

	return stream, nil
}

func (s *ClientSession) AddRelay(address string) (transport.Relay, error) {
	// Not implemented for client session.
	return nil, fmt.Errorf("AddRelay not implemented for client session")
}

func (s *ClientSession) RemovePath(pathID string) error {
	// Not implemented for client session.
	return nil
}

func (s *ClientSession) SessionID() protocol.SessionID {
	return s.id
}

// Raw packet transmission for custom protocols
func (s *ClientSession) SendRawData(data []byte, pathID protocol.PathID, streamID protocol.StreamID) error {
	// Not yet implemented.
	return nil
}

func (s *ClientSession) CloseWithError(code int, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Log close reason for debugging (who/why closed the session)
	s.logger.Info("CloseWithError called (client)", "code", code, "msg", msg, "local", s.localAddr, "remote", s.remoteAddr)

	if sm, ok := s.streamMgr.(*streamManagerImpl); ok {
		sm.CloseAllStreams()
	}

	if pm, ok := s.pathMgr.(interface{ CloseAllPaths() }); ok {
		pm.CloseAllPaths()
	}

	return nil
}
