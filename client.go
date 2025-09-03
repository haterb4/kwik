package kwik

import (
	"context"
	"crypto/tls"
	"sync"

	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"

	"github.com/s-anzie/kwik/internal/transport"
)

type ClientSession struct {
	id         protocol.SessionID
	localAddr  string
	remoteAddr string
	mu         sync.Mutex
	pathMgr    transport.PathManager
	streamMgr  streamManager
	logger     logger.Logger
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
	return &ClientSession{
		id:         0,
		remoteAddr: address,
		pathMgr:    mgr,
		streamMgr:  NewStreamManager(),
		logger:     logger.NewLogger(logger.LogLevelDebug).WithComponent("CLIENT_SESSION"),
	}
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
	// Create the primary path with the path manager and mark it as main path.
	id, err := s.pathMgr.OpenPath(ctx, s.remoteAddr)
	if err != nil {
		return err
	}
	if id <= 0 {
		return protocol.NewInvalidPathIDError(id)
	}
	s.pathMgr.SetPrimaryPath(id)
	return nil
}

func (s *ClientSession) LocalAddr() string {
	return s.localAddr

}
func (s *ClientSession) RemoteAddr() string {
	return s.remoteAddr
}

func (s *ClientSession) AcceptStream(ctx context.Context) (Stream, error) {
	// Not yet implemented - return a new stream
	streamImpl := NewStream(protocol.StreamID(1))
	return streamImpl, nil
}

func (s *ClientSession) OpenStreamSync(ctx context.Context) (Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.streamMgr.GetNextStreamID()
	err := s.pathMgr.GetPrimaryPath().OpenStreamSync(ctx, id)
	if err != nil {
		return nil, err
	}
	streamImpl := s.streamMgr.CreateStream()
	return streamImpl, nil
}

func (s *ClientSession) OpenStream() (Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.streamMgr.GetNextStreamID()
	err := s.pathMgr.GetPrimaryPath().OpenStream(id)
	if err != nil {
		return nil, err
	}
	streamImpl := s.streamMgr.CreateStream()
	return streamImpl, nil
}

func (s *ClientSession) AddPath(address string) error {
	// Not implemented for client session.
	return nil
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
	// Not yet implemented.
	return nil
}
