package kwik

import (
	"context"
	"crypto/tls"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
	"github.com/s-anzie/kwik/internal/transport"
)

type ServerSession struct {
	id         protocol.SessionID
	localAddr  string
	remoteAddr string
	mu         sync.Mutex
	pathMgr    transport.PathManager
	streamMgr  streamManager
	logger     logger.Logger
}

// Ensure ServerSession implements Session
var _ Session = (*ServerSession)(nil)

func NewServerSession(id protocol.SessionID, conn *quic.Conn) *ServerSession {
	pathMgr := transport.NewServerPathManager()
	pathid, err := pathMgr.AccpetPath(conn)
	if err != nil {
		panic(err)
	}
	pathMgr.SetPrimaryPath(pathid)
	// ensure default packer/multiplexer exist for this process
	if transport.GetDefaultPacker() == nil {
		pk := transport.NewPacker(1200)
		transport.SetDefaultPacker(pk)
		// start retransmit loop
		pk.StartRetransmitLoop(nil)
	}
	if transport.GetDefaultMultiplexer() == nil {
		mx := transport.NewMultiplexer()
		transport.SetDefaultMultiplexer(mx)
		mx.StartAckLoop(200)
	}
	return &ServerSession{
		id:         id,
		pathMgr:    pathMgr,
		streamMgr:  NewStreamManager(),
		localAddr:  conn.LocalAddr().String(),
		remoteAddr: conn.RemoteAddr().String(),
		logger:     logger.NewLogger(logger.LogLevelDebug).WithComponent("SERVER_SESSION"),
	}
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
	defer s.mu.Unlock()

	id := s.streamMgr.GetNextStreamID()
	err := s.pathMgr.GetPrimaryPath().AcceptStream(ctx, id)
	if err != nil {
		return nil, err
	}
	streamImpl := s.streamMgr.CreateStream()
	s.streamMgr.AddStreamPath(streamImpl.StreamID(), s.pathMgr.GetPrimaryPath())
	return streamImpl, nil
}

func (s *ServerSession) OpenStreamSync(ctx context.Context) (Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.streamMgr.GetNextStreamID()
	err := s.pathMgr.GetPrimaryPath().OpenStreamSync(ctx, id)
	if err != nil {
		return nil, err
	}
	streamImpl := s.streamMgr.CreateStream()
	s.streamMgr.AddStreamPath(streamImpl.StreamID(), s.pathMgr.GetPrimaryPath())
	return streamImpl, nil
}

func (s *ServerSession) OpenStream() (Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.streamMgr.GetNextStreamID()
	err := s.pathMgr.GetPrimaryPath().OpenStream(id)
	if err != nil {
		return nil, err
	}
	streamImpl := s.streamMgr.CreateStream()
	s.streamMgr.AddStreamPath(streamImpl.StreamID(), s.pathMgr.GetPrimaryPath())
	return streamImpl, nil
}

func (s *ServerSession) AddPath(address string) error {
	// Not implemented for server session.
	return nil
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
	// Not yet implemented.
	return nil
}
