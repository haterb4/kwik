package kwik

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

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
}

// Ensure ServerSession implements Session
var _ Session = (*ServerSession)(nil)

func NewServerSession(id protocol.SessionID, conn *quic.Conn) *ServerSession {
	lg := logger.NewLogger(logger.LogLevelSilent).WithComponent("SERVER_SESSION_FACTORY")
	lg.Debug("Creating new ServerSession...")
	pathMgr := transport.NewServerPathManager()
	packer := transport.NewPacker(1048576)

	// La retransmission sera gérée par le Path, pas à ce niveau

	sess := &ServerSession{
		id:      id,
		pathMgr: pathMgr, localAddr: conn.LocalAddr().String(),
		remoteAddr: conn.RemoteAddr().String(),
		logger:     logger.NewLogger(logger.LogLevelSilent).WithComponent("SERVER_SESSION"),
		packer:     packer,
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
	if err := path.AcceptStream(ctx, stream.StreamID()); err != nil {
		s.mu.Unlock()
		s.logger.Error("Failed to accept stream", "error", err, "pathID", path.PathID())
		return nil, fmt.Errorf("failed to accept stream: %w", err)
	}

	s.logger.Debug("Creating new stream", "streamID", stream.StreamID(), "pathID", path.PathID())
	// Add the path to the stream (this will also register with packer)
	if err := s.streamMgr.AddPathToStream(stream.StreamID(), path); err != nil {
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

func (s *ServerSession) OpenStreamSync(ctx context.Context) (Stream, error) {
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
	// Open the stream with the generated ID
	err := primaryPath.OpenStreamSync(ctx, streamID)
	if err != nil {
		s.streamMgr.RemoveStream(streamID)
		return nil, err
	}

	// Add the path to the stream
	if err := s.streamMgr.AddPathToStream(streamID, primaryPath); err != nil {
		s.streamMgr.RemoveStream(streamID)
		return nil, err
	}

	s.logger.Debug("Successfully opened stream", "streamID", streamID, "pathID", primaryPath.PathID())
	return streamImpl, nil
}

func (s *ServerSession) OpenStream() (Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Get the primary path
	primaryPath := s.pathMgr.GetPrimaryPath()
	if primaryPath == nil {
		return nil, protocol.NewPathNotExistsError(0)
	}

	// Create the stream to get an ID
	streamImpl := s.streamMgr.CreateStream()
	streamID := streamImpl.StreamID()
	// Open the stream with the generated ID
	err := primaryPath.OpenStream(streamID)
	if err != nil {
		s.streamMgr.RemoveStream(streamID)
		return nil, err
	}

	// Add the path to the stream
	if err := s.streamMgr.AddPathToStream(streamID, primaryPath); err != nil {
		s.streamMgr.RemoveStream(streamID)
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

// NotifyPathClosed informs all streams that a path has been closed
// This method is called by path.Close() to notify the server session
// that a path is being closed, so the streams using that path can be updated.
func (s *ServerSession) NotifyPathClosed(pathID protocol.PathID, streamIDs []protocol.StreamID) {
	s.logger.Debug("Notifying streams that path was closed", "pathID", pathID, "streamCount", len(streamIDs))

	// Exclude control stream (ID 0) from notifications
	var appStreamIDs []protocol.StreamID
	for _, sid := range streamIDs {
		if sid != 0 { // Skip control stream
			appStreamIDs = append(appStreamIDs, sid)
		}
	}

	if len(appStreamIDs) == 0 {
		s.logger.Debug("No application streams to notify about path closure", "pathID", pathID)
		return
	}

	for _, sid := range appStreamIDs {
		// Get the stream and remove the path
		stream, exists := s.streamMgr.GetStream(sid)
		if !exists {
			s.logger.Debug("Stream not found when notifying of path closure", "streamID", sid, "pathID", pathID)
			continue
		}

		// Remove the path from the stream
		stream.RemovePath(pathID)
		s.logger.Debug("Notified stream of path closure", "streamID", sid, "pathID", pathID)
	}
}

func (s *ServerSession) CloseWithError(code int, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Log close reason for debugging (who/why closed the session)
	s.logger.Info("CloseWithError called (server)", "code", code, "msg", msg, "session", s.id, "local", s.localAddr, "remote", s.remoteAddr)

	if s.streamMgr != nil {
		s.streamMgr.CloseAllStreams()
	}

	if pm, ok := s.pathMgr.(interface{ CloseAllPaths() }); ok {
		pm.CloseAllPaths()
	}

	// Stop multiplexer and packer background goroutines
	if s.multiplexer != nil {
		s.multiplexer.Close()
	}
	if s.packer != nil {
		s.packer.Close()
	}

	return nil
}
