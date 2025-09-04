package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"encoding/binary"
	"io"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type path struct {
	id      protocol.PathID
	conn    *quic.Conn
	streams map[protocol.StreamID]*quic.Stream
	mu      sync.Mutex
	logger  logger.Logger
	// role: true if this path was created by a client (active dial), false if accepted by server
	isClient bool
	// handshake / session state
	sessionMu       sync.Mutex
	sessionReady    bool
	handshakeRespCh chan *protocol.Frame
	// health check
	healthIntervalMs int
	// health metrics/state
	pingsSent              int64
	pongsRecv              int64
	missedPongs            int64
	lastPongAtUnixMilli    int64
	healthy                int32 // atomic bool
	missedPongThreshold    int
	expectedHandshakeNonce []byte
	// control stream creation guard
	controlMu       sync.Mutex
	controlReady    chan struct{}
	controlCreating bool
}

func NewPath(id protocol.PathID, conn *quic.Conn) *path {
	return &path{
		id:                  id,
		conn:                conn,
		streams:             make(map[protocol.StreamID]*quic.Stream),
		logger:              logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH"),
		healthIntervalMs:    1000,
		healthy:             1,
		missedPongThreshold: 3,
	}
}
func (p *path) LocalAddr() string {
	return p.conn.LocalAddr().String()
}
func (p *path) RemoteAddr() string {
	return p.conn.RemoteAddr().String()
}
func (p *path) PathID() protocol.PathID {
	return p.id
}

// IsClient returns true if this path was created by a client dialing out.
func (p *path) IsClient() bool {
	return p.isClient
}

// IsSessionReady reports whether the handshake/session is established.
func (p *path) IsSessionReady() bool {
	p.sessionMu.Lock()
	ready := p.sessionReady
	p.sessionMu.Unlock()
	return ready
}

func (p *path) OpenStreamSync(ctx context.Context, streamID protocol.StreamID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Debug("Opening stream synchronously", "pathID", p.id, "streamID", streamID)
	stream, err := p.conn.OpenStreamSync(ctx)
	if err != nil {
		p.logger.Error("Failed to open QUIC stream", "pathID", p.id, "streamID", streamID, "error", err)
		return err
	}

	if _, ok := p.streams[streamID]; ok {
		p.logger.Error("Stream already exists", "pathID", p.id, "streamID", streamID)
		return protocol.NewExistStreamError(p.id, streamID)
	}

	p.streams[streamID] = stream
	p.logger.Debug("Successfully opened stream", "pathID", p.id, "streamID", streamID, "quicStreamID", stream.StreamID())
	// start reader goroutine to forward length-prefixed packets to multiplexer
	go p.runStreamReader(streamID, stream)
	return nil
}

func (p *path) OpenStream(streamID protocol.StreamID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Debug("Opening stream asynchronously", "pathID", p.id, "streamID", streamID)
	stream, err := p.conn.OpenStream()
	if err != nil {
		p.logger.Error("Failed to open QUIC stream", "pathID", p.id, "streamID", streamID, "error", err)
		return err
	}

	if _, ok := p.streams[streamID]; ok {
		p.logger.Error("Stream already exists", "pathID", p.id, "streamID", streamID)
		return protocol.NewExistStreamError(p.id, streamID)
	}

	p.streams[streamID] = stream
	p.logger.Debug("Successfully opened stream", "pathID", p.id, "streamID", streamID, "quicStreamID", stream.StreamID())
	// start reader goroutine to forward length-prefixed packets to multiplexer
	go p.runStreamReader(streamID, stream)
	return nil
}
func (p *path) AcceptStream(ctx context.Context, streamID protocol.StreamID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logger.Debug("Waiting for New Quic Stream on path %d", p.id)
	stream, err := p.conn.AcceptStream(ctx)
	if err != nil {
		return err
	}
	p.logger.Debug("Accepted New Quic Stream %d for stream %d on path %d", stream.StreamID(), streamID, p.id)

	if _, ok := p.streams[streamID]; ok {
		return protocol.NewExistStreamError(p.id, streamID)
	}
	p.streams[streamID] = stream
	return nil
}

func (p *path) runStreamReader(streamID protocol.StreamID, s *quic.Stream) {
	// read loop: 4-byte big-endian length prefix followed by packet body
	for {
		var packetSeq uint64
		if err := binary.Read(s, binary.BigEndian, &packetSeq); err != nil {
			p.logger.Debug("stream reader exiting (seq read)", "path", p.id, "stream", streamID, "err", err)
			return
		}
		var l uint32
		if err := binary.Read(s, binary.BigEndian, &l); err != nil {
			p.logger.Debug("stream reader exiting", "path", p.id, "stream", streamID, "err", err)
			return
		}
		if l == 0 {
			continue
		}
		buf := make([]byte, l)
		if _, err := io.ReadFull(s, buf); err != nil {
			p.logger.Debug("failed to read packet body", "path", p.id, "stream", streamID, "err", err)
			return
		}
		if mx := GetDefaultMultiplexer(); mx != nil {
			_ = mx.PushPacketWithSeq(p.id, packetSeq, buf)
		}
	}
}

// SendControlFrame writes a control frame (handshake, ping, pong) on the reserved control stream (streamID 0).
func (p *path) SendControlFrame(f *protocol.Frame) error {
	// ensure control stream exists (create exactly once)
	if err := p.ensureControlStream(); err != nil {
		return err
	}
	// encode frame into packet with length prefix and seq=0
	var buf bytes.Buffer
	if err := protocol.EncodeFrame(&buf, f); err != nil {
		return err
	}
	body := buf.Bytes()
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
	out := append(lenBuf[:], body...)
	// prepend an 8-byte packetSeq (0) so remote runStreamReader can parse
	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], uint64(0))
	outWithSeq := append(seqBuf[:], out...)
	// debug log for outgoing control frame
	if p.logger != nil {
		p.logger.Debug("sending control frame", "path", p.id, "type", f.Type, "len", len(outWithSeq))
	}
	// write with packetSeq 0
	_, err := p.WriteStream(protocol.StreamID(0), outWithSeq)
	return err
}

// ensureControlStream creates the reserved control stream exactly once and
// allows multiple callers to wait until it's ready.
func (p *path) ensureControlStream() error {
	p.controlMu.Lock()
	if p.controlReady != nil {
		// already created
		ch := p.controlReady
		p.controlMu.Unlock()
		<-ch
		return nil
	}
	// not created yet; mark creating
	p.controlReady = make(chan struct{})
	p.controlCreating = true
	p.controlMu.Unlock()

	// attempt to open or accept control stream depending on role
	var err error
	if p.isClient {
		// client actively opens the control stream
		err = p.OpenStream(protocol.StreamID(0))
	} else {
		// server should accept the incoming control stream
		err = p.AcceptStream(context.Background(), protocol.StreamID(0))
	}

	p.controlMu.Lock()
	// signal waiters
	close(p.controlReady)
	p.controlCreating = false
	p.controlMu.Unlock()
	return err
}

// StartHandshake performs a simple handshake exchange: send Handshake, wait for HandshakeResp.
func (p *path) StartHandshake() error {
	p.sessionMu.Lock()
	if p.sessionReady {
		p.sessionMu.Unlock()
		return nil
	}
	// ensure handshake channel exists
	if p.handshakeRespCh == nil {
		p.handshakeRespCh = make(chan *protocol.Frame, 1)
	}
	p.sessionMu.Unlock()

	const maxAttempts = 4
	backoffMs := 200
	// generate expected nonce
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return protocol.NewHandshakeNonceError(p.id, err)
	}
	p.sessionMu.Lock()
	p.expectedHandshakeNonce = make([]byte, len(nonce))
	copy(p.expectedHandshakeNonce, nonce)
	p.sessionMu.Unlock()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		h := &protocol.Frame{Type: protocol.FrameTypeHandshake, StreamID: 0, Seq: 0, Payload: nonce}
		p.logger.Debug("start handshake attempt", "path", p.id, "isClient", p.isClient, "attempt", attempt)
		if err := p.SendControlFrame(h); err != nil {
			p.logger.Warn("handshake send failed", "path", p.id, "err", err, "attempt", attempt)
			// continue to retry
		}

		// wait for response with timeout
		select {
		case resp := <-p.handshakeRespCh:
			if resp != nil && len(resp.Payload) == len(nonce) && bytes.Equal(resp.Payload, nonce) {
				p.sessionMu.Lock()
				p.sessionReady = true
				// clear expected nonce
				p.expectedHandshakeNonce = nil
				p.sessionMu.Unlock()
				// start health loop now that session is established
				go p.startHealthLoop()
				p.logger.Info("handshake completed", "path", p.id, "nonce", hex.EncodeToString(nonce))
				return nil
			}
			p.logger.Warn("invalid handshake response", "path", p.id, "payload", hex.EncodeToString(resp.Payload))
		case <-time.After(time.Duration(backoffMs) * time.Millisecond):
			p.logger.Debug("handshake timeout, will retry", "path", p.id, "attempt", attempt)
		}

		backoffMs *= 2
	}
	return protocol.NewHandshakeFailedError(p.id)
}

func (p *path) startHealthLoop() {
	ticker := time.NewTicker(time.Duration(p.healthIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		// send ping frame with timestamp payload
		ts := time.Now().UnixMilli()
		var tb [8]byte
		binary.BigEndian.PutUint64(tb[:], uint64(ts))
		ping := &protocol.Frame{Type: protocol.FrameTypePing, StreamID: 0, Seq: 0, Payload: tb[:]}
		if err := p.SendControlFrame(ping); err != nil {
			p.logger.Warn("failed to send ping", "path", p.id, "err", err)
			continue
		}
		atomic.AddInt64(&p.pingsSent, 1)

		// wait half interval, then check if we received a pong recently
		time.Sleep(time.Duration(p.healthIntervalMs/2) * time.Millisecond)
		last := atomic.LoadInt64(&p.lastPongAtUnixMilli)
		if last == 0 || (time.Now().UnixMilli()-last) > int64(p.healthIntervalMs) {
			// missed
			miss := atomic.AddInt64(&p.missedPongs, 1)
			p.logger.Warn("missed pong", "path", p.id, "missed", miss)
			if miss >= int64(p.missedPongThreshold) {
				atomic.StoreInt32(&p.healthy, 0)
				p.logger.Warn("path marked unhealthy", "path", p.id)
			}
		} else {
			// received pong recently
			atomic.StoreInt64(&p.missedPongs, 0)
			atomic.StoreInt32(&p.healthy, 1)
		}
	}
}

// HandleControlFrame processes inbound control frames received for this path.
func (p *path) HandleControlFrame(f *protocol.Frame) error {
	switch f.Type {
	case protocol.FrameTypePing:
		// reply with Pong
		// echo payload back
		pong := &protocol.Frame{Type: protocol.FrameTypePong, StreamID: 0, Seq: 0, Payload: f.Payload}
		return p.SendControlFrame(pong)
	case protocol.FrameTypePong:
		// record reception time and update metrics
		atomic.AddInt64(&p.pongsRecv, 1)
		now := time.Now().UnixMilli()
		atomic.StoreInt64(&p.lastPongAtUnixMilli, now)
		atomic.StoreInt64(&p.missedPongs, 0)
		atomic.StoreInt32(&p.healthy, 1)
		p.logger.Debug("received pong", "path", p.id, "time", now)
		return nil
	case protocol.FrameTypeHandshake:
		// reply with handshake response
		// echo the nonce back as a response
		resp := &protocol.Frame{Type: protocol.FrameTypeHandshakeResp, StreamID: 0, Seq: 0, Payload: f.Payload}
		if err := p.SendControlFrame(resp); err != nil {
			return err
		}
		p.sessionMu.Lock()
		p.sessionReady = true
		p.sessionMu.Unlock()
		return nil
	case protocol.FrameTypeHandshakeResp:
		// signal waiting StartHandshake if present
		select {
		case p.handshakeRespCh <- f:
		default:
		}
		// additionally if we have an expected nonce, validate it
		p.sessionMu.Lock()
		expected := p.expectedHandshakeNonce
		if expected != nil && len(expected) == len(f.Payload) && bytes.Equal(expected, f.Payload) {
			p.sessionReady = true
			p.expectedHandshakeNonce = nil
			p.sessionMu.Unlock()
			p.logger.Debug("path sessionReady set via handshake response", "path", p.id, "isClient", p.isClient)
			p.logger.Info("handshake validated via response", "path", p.id, "nonce", hex.EncodeToString(f.Payload))
			return nil
		}
		p.sessionMu.Unlock()
		p.logger.Warn("received unexpected handshake response", "path", p.id)
		return nil
	default:
		p.logger.Debug("unknown control frame", "type", f.Type)
		return nil
	}
}

// GetMetrics returns basic per-path health metrics.
func (p *path) GetMetrics() map[string]interface{} {
	return map[string]interface{}{
		"pings_sent":    atomic.LoadInt64(&p.pingsSent),
		"pongs_recv":    atomic.LoadInt64(&p.pongsRecv),
		"missed_pongs":  atomic.LoadInt64(&p.missedPongs),
		"last_pong_ms":  atomic.LoadInt64(&p.lastPongAtUnixMilli),
		"healthy":       atomic.LoadInt32(&p.healthy) == 1,
		"session_ready": func() bool { p.sessionMu.Lock(); v := p.sessionReady; p.sessionMu.Unlock(); return v }(),
	}
}

// ReadStream reads from the stored quic.Stream for the provided streamID.
func (p *path) ReadStream(streamID protocol.StreamID, b []byte) (int, error) {
	p.mu.Lock()
	s, ok := p.streams[streamID]
	p.mu.Unlock()
	if !ok {
		return 0, protocol.NewNotExistStreamError(p.id, streamID)
	}
	return s.Read(b)
}

// WriteStream writes to the stored quic.Stream for the provided streamID.
func (p *path) WriteStream(streamID protocol.StreamID, b []byte) (int, error) {
	p.mu.Lock()
	s, ok := p.streams[streamID]
	p.mu.Unlock()
	if !ok {
		return 0, protocol.NewNotExistStreamError(p.id, streamID)
	}
	return s.Write(b)
}
