package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"io"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type pathStream struct {
	writeOffset uint64 // Logical write offset for this stream
}
type path struct {
	id      protocol.PathID
	conn    *quic.Conn
	streams map[protocol.StreamID]*pathStream
	mu      sync.Mutex
	logger  logger.Logger
	// Global sequence counter for this path (since we use single QUIC stream)
	globalWriteSeq uint64 // Atomic counter for packet sequence numbers
	// Single QUIC stream for ALL traffic (control + data)
	transportStream   *quic.Stream
	transportStreamMu sync.Mutex
	// role: true if this path was created by a client (active dial), false if accepted by server
	isClient bool
	// handshake / session state
	sessionMu    sync.Mutex
	sessionReady bool
	// handshakeRespCh now carries Frame values (HandshakeRespFrame) from HandleControlFrame
	handshakeRespCh      chan protocol.Frame
	lastAcceptedStreamID protocol.StreamID // tracks the last accepted stream ID
	// health check
	healthIntervalMs  int
	healthLoopStarted int32 // atomic bool to prevent multiple health loops
	// health metrics/state
	pingsSent              int64
	pongsRecv              int64
	missedPongs            int64
	lastPongAtUnixMilli    int64
	healthy                int32 // atomic bool
	missedPongThreshold    int
	expectedHandshakeNonce []byte
	// control stream creation guard
	controlReady chan struct{}
	session      Session
	// open stream response channel (per streamID)
	openStreamRespChMu sync.Mutex
	openStreamRespCh   map[protocol.StreamID]chan *protocol.OpenStreamRespFrame
	// ...existing code...
	// Accept stream waiters: map logical streamID to channel to signal AcceptStream
	acceptStreamWaitersMu sync.Mutex
	acceptStreamWaiters   map[protocol.StreamID]chan struct{}
}

func NewPath(id protocol.PathID, conn *quic.Conn, isClient bool, session Session) *path {
	path := path{
		id:                   id,
		conn:                 conn,
		isClient:             isClient,
		streams:              make(map[protocol.StreamID]*pathStream),
		logger:               logger.NewLogger(logger.LogLevelSilent).WithComponent("PATH"),
		healthIntervalMs:     15000, // Augmenter à 15 secondes pour réduire le trafic de contrôle
		healthy:              1,
		missedPongThreshold:  3,
		lastAcceptedStreamID: 1,
		session:              session,
	}
	// Initialize control stream readiness channel
	path.controlReady = make(chan struct{})

	// Register the path immediately so it can receive control frames
	if si, ok := path.session.(sessionInternal); ok {
		si.Packer().RegisterPath(&path)
	}
	if isClient {
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			panic(err)
		}
		path.transportStream = stream
		close(path.controlReady)
		go path.runTransportStreamReader(stream)
		err = path.startClientHandshake()
		if err != nil {
			panic(err)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			panic(err)
		}
		path.transportStream = stream
		close(path.controlReady)
		go path.runTransportStreamReader(stream)
		err = path.startServerHandshake()
		if err != nil {
			panic(err)
		}
	}
	path.acceptStreamWaiters = make(map[protocol.StreamID]chan struct{})
	return &path
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
func (p *path) RemoveStream(streamID protocol.StreamID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.streams, streamID)

	// Nettoyer le buffer de réception pour éviter les fuites mémoire
	go p.CleanupStreamBuffer(streamID)

	// Annuler les paquets en attente de retransmission pour ce stream
	go p.CancelFramesForStream(streamID)
}
func (p *path) GetStream(streamID protocol.StreamID) (*pathStream, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stream, ok := p.streams[streamID]
	return stream, ok
}

// SubmitSendStream attempts to flush buffered frames for a given stream via the packer.
func (p *path) SubmitSendStream(streamID protocol.StreamID) error {
	p.sessionMu.Lock()
	sess := p.session
	p.sessionMu.Unlock()
	if sess == nil {
		return fmt.Errorf("no session")
	}
	// fmt.Printf("TRACK SubmitSendStream: streamID=%d\n", streamID)
	if si, ok := sess.(sessionInternal); ok {
		if packer := si.Packer(); packer != nil {
			return packer.SubmitFromSendStream(p, streamID)
		}
	}
	return fmt.Errorf("session does not expose internal transport")
}

// CancelFramesForStream cancels pending packets associated with a stream.
func (p *path) CancelFramesForStream(streamID protocol.StreamID) {
	p.sessionMu.Lock()
	sess := p.session
	p.sessionMu.Unlock()
	if sess == nil {
		return
	}
	if si, ok := sess.(sessionInternal); ok {
		if packer := si.Packer(); packer != nil {
			packer.CancelFramesForStream(streamID)
		}
	}
}

// CleanupStreamBuffer asks the multiplexer to clean up any per-stream reception state.
func (p *path) CleanupStreamBuffer(streamID protocol.StreamID) {
	p.sessionMu.Lock()
	sess := p.session
	p.sessionMu.Unlock()
	if sess == nil {
		return
	}
	if si, ok := sess.(sessionInternal); ok {
		if mux := si.Multiplexer(); mux != nil {
			mux.CleanupStreamBuffer(streamID)
		}
	}
}

// WriteSeq generates a packet sequence number for the unified transport stream
// This is different from stream logical offsets (handled by GetWriteOffset/SyncWriteOffset)
func (p *path) WriteSeq(streamID protocol.StreamID) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.streams[streamID]
	if !ok {
		return 0, false
	}
	// Use global path sequence counter for packet numbering in unified transport
	seq := atomic.AddUint64(&p.globalWriteSeq, 1)
	return seq, true
}

// GetWriteOffset returns the current write offset for a stream (used by SendStream sync)
func (p *path) GetWriteOffset(streamID protocol.StreamID) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stream, ok := p.streams[streamID]
	if !ok {
		return 0, false
	}
	// Return the logical stream's write offset, not the global sequence
	return stream.writeOffset, true
}

// SyncWriteOffset synchronizes the path's stream offset with the SendStream offset
func (p *path) SyncWriteOffset(streamID protocol.StreamID, offset uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	stream, ok := p.streams[streamID]
	if !ok {
		return false
	}
	// Update the logical stream's write offset
	stream.writeOffset = offset
	return true
}

// AdvanceReadSeq is simplified in unified stream architecture
// Packet sequence verification is handled at transport level, not per logical stream
func (p *path) AdvanceReadSeq(streamID protocol.StreamID, receivedSeq uint64) (isExpected bool) {
	// With single stream multiplexing, packet sequence tracking is handled centrally
	// This method kept for API compatibility - logical stream sequencing via frame order
	return true
}

// IsSessionReady reports whether the handshake/session is established.
func (p *path) IsSessionReady() bool {
	p.sessionMu.Lock()
	ready := p.sessionReady
	p.sessionMu.Unlock()
	return ready
}

func (p *path) OpenStreamSync(ctx context.Context, streamID protocol.StreamID) error {
	p.logger.Debug("OpenStreamSync: Opening stream synchronously", "pathID", p.id, "streamID", streamID)

	// Check if stream already exists
	p.mu.Lock()
	if _, exists := p.streams[streamID]; exists {
		p.mu.Unlock()
		p.logger.Warn("OpenStreamSync: stream already exists", "pathID", p.id, "streamID", streamID)
		return protocol.NewExistStreamError(p.id, streamID)
	}
	p.mu.Unlock()

	// Prépare le canal de réponse pour ce streamID AVANT d'envoyer la frame
	p.openStreamRespChMu.Lock()
	if p.openStreamRespCh == nil {
		p.openStreamRespCh = make(map[protocol.StreamID]chan *protocol.OpenStreamRespFrame)
	}
	respCh := make(chan *protocol.OpenStreamRespFrame, 1)
	p.openStreamRespCh[streamID] = respCh
	p.openStreamRespChMu.Unlock()

	// Ensure cleanup on all exit paths
	defer func() {
		p.openStreamRespChMu.Lock()
		delete(p.openStreamRespCh, streamID)
		p.openStreamRespChMu.Unlock()
	}()

	// Ajoute une OpenStreamFrame au buffer d'envoi (batching)
	if si, ok := p.session.(sessionInternal); ok {
		packer := si.Packer()
		if packer != nil {
			frame := &protocol.OpenStreamFrame{StreamID: uint64(streamID)}
			err := packer.SubmitFrame(p, frame)
			if err != nil {
				p.logger.Error("OpenStreamSync: failed to submit OpenStreamFrame", "streamID", streamID, "err", err)
				return fmt.Errorf("failed to send OpenStreamFrame: %w", err)
			}
		} else {
			return fmt.Errorf("OpenStreamSync: no packer available")
		}
	} else {
		return fmt.Errorf("OpenStreamSync: session does not support internal interface")
	}

	// Attend la réponse du pair avec timeout approprié
	p.logger.Debug("OpenStreamSync: waiting for OpenStreamRespFrame", "streamID", streamID)
	select {
	case resp := <-respCh:
		if resp.Success {
			p.logger.Debug("OpenStreamSync: received OpenStreamRespFrame success", "streamID", streamID)
			// Add stream to path under lock
			p.mu.Lock()
			p.streams[streamID] = &pathStream{writeOffset: 0}
			p.mu.Unlock()
			p.logger.Debug("OpenStreamSync: Successfully opened stream", "pathID", p.id, "streamID", streamID)
			return nil
		} else {
			p.logger.Error("OpenStreamSync: received OpenStreamRespFrame failure", "streamID", streamID)
			return fmt.Errorf("OpenStreamSync: remote failed to open stream %d", streamID)
		}
	case <-ctx.Done():
		p.logger.Error("OpenStreamSync: timeout waiting for OpenStreamRespFrame", "streamID", streamID)
		return ctx.Err()
	}
}

func (p *path) OpenStream(streamID protocol.StreamID) error {
	// Aligner avec OpenStreamSync : n'ouvre pas de nouveau QUIC stream, prépare juste la trame et l'entrée logique
	p.logger.Debug("OpenStream: sending OpenStreamFrame only (no new QUIC stream)", "pathID", p.id, "streamID", streamID)
	p.mu.Lock()
	if _, ok := p.streams[streamID]; ok {
		p.logger.Warn("OpenStream: stream already exists", "pathID", p.id, "streamID", streamID)
		p.mu.Unlock()
		return protocol.NewExistStreamError(p.id, streamID)
	}
	p.streams[streamID] = &pathStream{writeOffset: 0}
	p.mu.Unlock()
	// Ajoute une OpenStreamFrame au buffer d'envoi (batching)
	if si, ok := p.session.(sessionInternal); ok {
		packer := si.Packer()
		if packer != nil {
			frame := &protocol.OpenStreamFrame{StreamID: uint64(streamID)}
			_ = packer.SubmitFrame(p, frame)
		}
	}
	return nil
}
func (p *path) AcceptStream(ctx context.Context, streamID protocol.StreamID) error {
	p.logger.Debug("AcceptStream: waiting for OpenStreamFrame signal", "pathID", p.id, "streamID", streamID)

	// Check if stream already exists under lock
	p.mu.Lock()
	if _, exists := p.streams[streamID]; exists {
		p.mu.Unlock()
		p.logger.Debug("AcceptStream: stream already exists", "pathID", p.id, "streamID", streamID)
		return nil
	}
	p.mu.Unlock()

	// Set up waiter channel under lock
	p.acceptStreamWaitersMu.Lock()
	if p.acceptStreamWaiters == nil {
		p.acceptStreamWaiters = make(map[protocol.StreamID]chan struct{})
	}
	ch, exists := p.acceptStreamWaiters[streamID]
	if !exists {
		// No existing channel, create a new one
		ch = make(chan struct{})
		p.acceptStreamWaiters[streamID] = ch
		p.acceptStreamWaitersMu.Unlock()
	} else {
		// Channel exists - might be closed already (OpenStreamFrame arrived first)
		p.acceptStreamWaitersMu.Unlock()
	}

	// Ensure cleanup on all exit paths (only if we created the channel)
	defer func() {
		p.acceptStreamWaitersMu.Lock()
		if currentCh, exists := p.acceptStreamWaiters[streamID]; exists && currentCh == ch {
			delete(p.acceptStreamWaiters, streamID)
		}
		p.acceptStreamWaitersMu.Unlock()
	}()

	select {
	case <-ch:
		// Signal received: OpenStreamFrame was received and logical stream is ready
		p.logger.Debug("AcceptStream: OpenStreamFrame signal received", "pathID", p.id, "streamID", streamID)
		// Add stream to path under lock
		p.mu.Lock()
		p.streams[streamID] = &pathStream{writeOffset: 0}
		p.mu.Unlock()
		p.logger.Debug("AcceptStream: Successfully accepted stream", "pathID", p.id, "streamID", streamID)
		return nil
	case <-ctx.Done():
		p.logger.Error("AcceptStream: context done while waiting for OpenStreamFrame", "pathID", p.id, "streamID", streamID)
		return ctx.Err()
	}
}

// runUnifiedStreamReader reads from the single unified stream
// All packets go through here, containing different frame types:
// - StreamFrame: routed to specific logical streamID
// - Control frames: handled by HandleControlFrame
func (p *path) runTransportStreamReader(s *quic.Stream) {
	const maxPacketSize = 1 * 1024 * 1024 // 1MB QUIC recommended
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 2048)

	p.logger.Info("runUnifiedStreamReader: starting unified stream reader", "path", p.id)

	for {
		n, err := s.Read(tmp)
		if err != nil {
			if err == io.EOF {
				p.logger.Debug("unified stream reader exiting (EOF)", "path", p.id)
			} else {
				p.logger.Debug("unified stream reader exiting (read error)", "path", p.id, "err", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		buf = append(buf, tmp[:n]...)

		// Process packets: [4 bytes length][packet data]
		// No streamID prefix needed - frame types handle routing
		for {
			if len(buf) < 4 {
				break // not enough for length prefix
			}

			// Extract packet length
			packetLength := binary.BigEndian.Uint32(buf[:4])

			if packetLength == 0 {
				buf = buf[4:]
				continue
			}
			if packetLength > maxPacketSize {
				p.logger.Warn("unified stream reader: packet too large, dropping",
					"path", p.id, "len", packetLength)
				return
			}
			if len(buf) < int(4+packetLength) {
				break // not enough for full packet
			}

			// Extract the actual packet
			packet := buf[4 : 4+packetLength]

			p.logger.Debug("unified stream reader received packet",
				"path", p.id,
				"len", packetLength,
				"bytes", fmt.Sprintf("% x", packet[:min(32, int(packetLength))]))

			// fmt.Printf("TRACK UnifiedStreamReader received packet pathID=%d len=%d\n",
			// p.id, packetLength)

			// Forward packet to multiplexer - frame type routing handled there
			if p.session != nil {
				go p.session.Multiplexer().PushPacket(p.id, packet)
			}

			buf = buf[4+packetLength:]
			runtime.Gosched()
		}
	}
}

// SendControlFrame writes a control Frame (handshake, ping, pong) on the reserved control stream (streamID 0).
// writeControlFrame encodes a Frame into the legacy frame bytes and writes it
// to the reserved control QUIC stream (streamID 0) with packetSeq 0. This is
// internal to the path implementation; higher-level code should call WriteStream
// directly when possible.

func (p *path) startServerHandshake() error {
	p.sessionMu.Lock()
	if p.sessionReady {
		p.sessionMu.Unlock()
		return nil
	}
	p.sessionMu.Unlock()

	// Server waits for handshake from client
	// The handshake will be processed in HandleControlFrame when received
	// We just need to wait for the session to become ready
	const maxWaitTime = 10 * time.Second
	const checkInterval = 100 * time.Millisecond

	start := time.Now()
	for {
		p.sessionMu.Lock()
		ready := p.sessionReady
		p.sessionMu.Unlock()

		if ready {
			p.logger.Info("server handshake completed", "path", p.id)
			return nil
		}

		if time.Since(start) > maxWaitTime {
			p.logger.Error("server handshake timeout", "path", p.id)
			return protocol.NewHandshakeFailedError(p.id)
		}

		time.Sleep(checkInterval)
	}
}

// StartHandshake performs a simple handshake exchange: send Handshake, wait for HandshakeResp.
func (p *path) startClientHandshake() error {
	p.sessionMu.Lock()
	if p.sessionReady {
		p.sessionMu.Unlock()
		return nil
	}
	// ensure handshake channel exists (carries Frame HandshakeRespFrame)
	if p.handshakeRespCh == nil {
		p.handshakeRespCh = make(chan protocol.Frame, 1)
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
		h := &protocol.HandshakeFrame{}
		p.logger.Debug("start handshake attempt", "path", p.id, "isClient", p.isClient, "attempt", attempt)
		if si, ok := p.session.(sessionInternal); ok {
			packer := si.Packer()
			if packer != nil {
				_ = packer.SubmitFrame(p, h)
			}
		}

		// wait for response with timeout
		select {
		case resp := <-p.handshakeRespCh:
			if resp != nil && resp.Type() == protocol.FrameTypeHandshake {
				p.sessionMu.Lock()
				p.sessionReady = true
				p.expectedHandshakeNonce = nil
				p.sessionMu.Unlock()
				go p.startHealthLoop()
				p.logger.Info("handshake completed", "path", p.id)
				return nil
			}
		case <-time.After(time.Duration(backoffMs) * time.Millisecond):
			p.logger.Debug("handshake timeout, will retry", "path", p.id, "attempt", attempt)
		}

		backoffMs *= 2
	}
	return protocol.NewHandshakeFailedError(p.id)
}

func (p *path) startHealthLoop() {
	// Prevent multiple health loops
	if !atomic.CompareAndSwapInt32(&p.healthLoopStarted, 0, 1) {
		p.logger.Debug("health loop already started", "path", p.id)
		return
	}

	p.logger.Debug("starting health loop", "path", p.id)
	ticker := time.NewTicker(time.Duration(p.healthIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	// Variables pour suivre les erreurs et les succès

	// Timeout de base pour la boucle de santé
	baseTimeout := time.Duration(p.healthIntervalMs/2) * time.Millisecond
	currentTimeout := baseTimeout

	// Erreur d'envoi ignorée (asynchrone via packer)
	atomic.AddInt64(&p.pingsSent, 1)

	// Attendre avant de vérifier les pongs (timeout adaptatif)
	time.Sleep(currentTimeout)

	// Vérifier la réception des pongs
	last := atomic.LoadInt64(&p.lastPongAtUnixMilli)
	timeSinceLastPong := time.Now().UnixMilli() - last

	if last == 0 || timeSinceLastPong > int64(p.healthIntervalMs*2) { // Double du délai normal
		// Pong manqué
		miss := atomic.AddInt64(&p.missedPongs, 1)

		if miss == 1 {
			// Premier pong manqué, juste un warning
			p.logger.Debug("first missed pong", "path", p.id, "time_since_last_ms", timeSinceLastPong)
		} else if miss >= int64(p.missedPongThreshold) {
			// Trop de pongs manqués
			atomic.StoreInt32(&p.healthy, 0)
			p.logger.Warn("path marked unhealthy due to missed pongs",
				"path", p.id,
				"missed_pongs", miss,
				"threshold", p.missedPongThreshold,
				"time_since_last_pong_ms", timeSinceLastPong)
		}
	} else {
		// Pong reçu récemment
		if atomic.LoadInt64(&p.missedPongs) > 0 {
			p.logger.Debug("pong received, resetting missed count", "path", p.id)
		}
		atomic.StoreInt64(&p.missedPongs, 0)
		atomic.StoreInt32(&p.healthy, 1)
	}

	// Vérification de sécurité : si le path est resté malsain trop longtemps
	if atomic.LoadInt32(&p.healthy) == 0 {
		p.logger.Warn("path has been unhealthy for extended period",
			"path", p.id,
			"missed_pongs", atomic.LoadInt64(&p.missedPongs))
	}

}

// HandleControlFrame processes inbound control frames received for this path.
func (p *path) HandleControlFrame(nf protocol.Frame) error {
	p.logger.Debug("HandleControlFrame called (new)", "path", p.id, "isClient", p.isClient, "frameType", nf.Type())
	switch nf.Type() {
	case protocol.FrameTypeOpenStreamResp:
		resp, ok := nf.(*protocol.OpenStreamRespFrame)
		if !ok {
			p.logger.Error("received OpenStreamRespFrame with wrong type", "path", p.id)
			return nil
		}
		// Délivre la réponse au bon canal
		p.openStreamRespChMu.Lock()
		ch, ok := p.openStreamRespCh[protocol.StreamID(resp.StreamID)]
		p.openStreamRespChMu.Unlock()
		if ok && ch != nil {
			select {
			case ch <- resp:
			default:
			}
		} else {
			p.logger.Warn("received OpenStreamRespFrame but no waiter", "streamID", resp.StreamID)
		}
		return nil
	case protocol.FrameTypeOpenStream:
		osf, ok := nf.(*protocol.OpenStreamFrame)
		if !ok {
			p.logger.Error("received OpenStreamFrame with wrong type", "path", p.id)
			return nil
		}
		p.logger.Info("received OpenStreamFrame, opening logical stream", "path", p.id, "streamID", osf.StreamID)
		var success bool
		sid := protocol.StreamID(osf.StreamID)

		// Signal any waiting AcceptStream call
		p.acceptStreamWaitersMu.Lock()
		if p.acceptStreamWaiters == nil {
			p.acceptStreamWaiters = make(map[protocol.StreamID]chan struct{})
		}
		ch, exists := p.acceptStreamWaiters[sid]
		if exists {
			// Check if channel is already closed to avoid panic
			select {
			case <-ch:
				// Channel already closed, do nothing
				p.logger.Debug("OpenStreamFrame: channel already closed", "streamID", sid)
			default:
				// Channel not closed, safe to close
				close(ch)
				p.logger.Debug("OpenStreamFrame: signaled AcceptStream waiter", "streamID", sid)
			}
			delete(p.acceptStreamWaiters, sid)
			success = true
		} else {
			// No waiter present - stream can be accepted later
			// Create a closed channel to signal immediate availability
			ch = make(chan struct{})
			close(ch)
			p.acceptStreamWaiters[sid] = ch
			success = true
			p.logger.Debug("OpenStreamFrame: created closed channel for future AcceptStream", "streamID", sid)
		}
		p.acceptStreamWaitersMu.Unlock()

		// Envoie la réponse d'ouverture
		resp := &protocol.OpenStreamRespFrame{StreamID: osf.StreamID, Success: success}
		if si, ok := p.session.(sessionInternal); ok {
			packer := si.Packer()
			if packer != nil {
				_ = packer.SubmitFrame(p, resp)
			}
		}
		return nil
	case protocol.FrameTypePing:
		// Echo the ping back as a simple keepalive response
		if !p.isClient {
			if si, ok := p.session.(sessionInternal); ok {
				packer := si.Packer()
				if packer != nil {
					_ = packer.SubmitFrame(p, &protocol.PingFrame{})
				}
			}
		}

		// update metrics
		atomic.AddInt64(&p.pongsRecv, 1)
		now := time.Now().UnixMilli()
		atomic.StoreInt64(&p.lastPongAtUnixMilli, now)
		atomic.StoreInt64(&p.missedPongs, 0)
		atomic.StoreInt32(&p.healthy, 1)
		return nil
	case protocol.FrameTypeHandshake:
		p.logger.Debug("received handshake frame", "path", p.id, "isClient", p.isClient)
		if p.isClient {
			p.sessionMu.Lock()
			ready := p.sessionReady
			ch := p.handshakeRespCh
			p.sessionMu.Unlock()
			if !ready && ch != nil {
				select {
				case ch <- nf:
					p.logger.Debug("handshake response delivered to client handshake channel", "path", p.id)
				default:
					p.logger.Warn("handshakeRespCh full or not ready", "path", p.id)
				}
			}
			return nil
		} else {
			if si, ok := p.session.(sessionInternal); ok {
				packer := si.Packer()
				if packer != nil {
					_ = packer.SubmitFrame(p, &protocol.HandshakeFrame{})
				}
			}
			p.sessionMu.Lock()
			p.sessionReady = true
			p.sessionMu.Unlock()
			p.logger.Info("handshake completed (server-side)", "path", p.id, "isClient", p.isClient)
			go p.startHealthLoop()
			return nil
		}
	case protocol.FrameTypeAddPath:
		if !p.isClient {
			p.logger.Debug("ignoring AddPath frame on server-side path", "path", p.id)
			return nil
		}
		// fmt.Printf("TRACK CLIENT received AddPath frame - processing pathID=%d\n", p.id)
		apf, ok := nf.(*protocol.AddPathFrame)
		if !ok {
			return nil
		}
		p.logger.Debug("received AddPath request", "address", apf.Address)
		// fmt.Printf("TRACK AddPath request: address=%s\n", apf.Address)
		pathID, err := p.session.PathManager().OpenPath(context.Background(), apf.Address, p.session)
		if err != nil {
			p.logger.Error("failed to open path", "error", err)
			return err
		}
		respFrame := &protocol.AddPathRespFrame{Address: apf.Address, PathID: pathID}
		p.logger.Debug("sending AddPathResp", "address", apf.Address, "pathID", pathID)
		// fmt.Printf("TRACK AddPathResp: address=%s pathID=%d\n", apf.Address, pathID)
		if si, ok := p.session.(sessionInternal); ok {
			packer := si.Packer()
			if packer != nil {
				_ = packer.SubmitFrame(p, respFrame)
			}
		}
		return nil
	case protocol.FrameTypeAddPathResp:
		if p.isClient {
			p.logger.Debug("ignoring AddPathResp frame on client-side path", "path", p.id)
			return nil
		}
		aprf, ok := nf.(*protocol.AddPathRespFrame)
		if !ok {
			p.logger.Error("AddPathResp frame type mismatch")
			return nil
		}
		pm, ok := p.session.PathManager().(*pathManagerImpl)
		if ok && pm != nil {
			pm.handleAddPathResp(aprf)
		} else {
			p.logger.Error("path manager not available or not of type *pathManagerImpl")
		}
		return nil
	case protocol.FrameTypeRelayData:
		if !p.isClient {
			p.logger.Debug("ignoring RelayData frame on server-side path", "path", p.id)
			return nil
		}
		rdf, ok := nf.(*protocol.RelayDataFrame)
		if !ok {
			return nil
		}
		p.logger.Debug("received RelayData", "path", p.id, "relayPathID", rdf.RelayPathID, "destStreamID", rdf.DestStreamID, "dataLen", len(rdf.RawData))
		if rdf.DestStreamID == 0 {
			p.logger.Error("invalid DestStreamID 0 in RelayData")
			return fmt.Errorf("invalid DestStreamID 0 in RelayData")
		}
		pathMgr := p.session.PathManager()
		relayPath := pathMgr.GetPath(rdf.RelayPathID)
		if relayPath == nil {
			p.logger.Error("relay path not found", "relayPathID", rdf.RelayPathID)
			return fmt.Errorf("relay path not found: %d", rdf.RelayPathID)
		}
		if !relayPath.IsSessionReady() {
			p.logger.Warn("relay path session is not ready, will attempt to proceed anyway", "relayPathID", rdf.RelayPathID)
		}
		if relayPath.Session() == nil {
			p.logger.Error("relay path has no session", "relayPathID", rdf.RelayPathID)
			return fmt.Errorf("relay path has no session: %d", rdf.RelayPathID)
		}
		hasStream := relayPath.HasStream(rdf.DestStreamID)
		if !hasStream {
			p.logger.Debug("creating new stream on relay path", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
			err := relayPath.OpenStream(rdf.DestStreamID)
			if err != nil {
				if relayPath.HasStream(rdf.DestStreamID) {
					p.logger.Debug("stream already exists on relay path (concurrent creation)", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
				} else {
					p.logger.Error("failed to open stream on relay path", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "error", err)
					return fmt.Errorf("failed to open stream on relay path: %w", err)
				}
			}
			if p.session == nil {
				p.logger.Error("session is nil when trying to register stream path", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
				return fmt.Errorf("session is nil when trying to register stream path")
			}
			streamMgr := p.session.StreamManager()
			if streamMgr == nil {
				p.logger.Error("stream manager is nil", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
				return fmt.Errorf("stream manager is nil")
			}
			addErr := streamMgr.AddPathToStream(rdf.DestStreamID, relayPath)
			if addErr != nil {
				p.logger.Warn("Failed to register stream path, but continuing anyway", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "error", addErr)
				if !relayPath.HasStream(rdf.DestStreamID) {
					p.logger.Error("Stream still doesn't exist and couldn't be registered", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
					return fmt.Errorf("stream still doesn't exist and couldn't be registered: %w", addErr)
				}
			} else {
				p.logger.Debug("registered stream path in StreamManager for relay stream", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
			}
		} else {
			p.logger.Debug("stream already exists on relay path", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
		}
		if !relayPath.HasStream(rdf.DestStreamID) {
			p.logger.Error("stream still doesn't exist after creation attempt", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
			return fmt.Errorf("stream %d still doesn't exist on path %d after creation attempt", rdf.DestStreamID, rdf.RelayPathID)
		}
		if !relayPath.HasStream(rdf.DestStreamID) {
			p.logger.Error("stream doesn't exist right before getting sequence", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
			return fmt.Errorf("stream %d doesn't exist on path %d right before getting sequence", rdf.DestStreamID, rdf.RelayPathID)
		}
		dataFrame := &protocol.StreamFrame{StreamID: uint64(rdf.DestStreamID), Offset: relayPath.WriteOffsetForStream(rdf.DestStreamID), Data: rdf.RawData}
		relayPath.IncrementWriteOffsetForStream(rdf.DestStreamID, uint64(len(rdf.RawData)))
		p.logger.Debug("forwarding data to relay stream via packer", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "dataLen", len(rdf.RawData))
		packer := p.session.Packer()
		if packer == nil {
			p.logger.Error("packer is nil", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID)
			return fmt.Errorf("packer is nil")
		}
		maxRetries := 3
		var submitErr error
		for i := 0; i < maxRetries; i++ {
			// SubmitFrame expects protocol.Frame
			submitErr = packer.SubmitFrame(relayPath, dataFrame)
			if submitErr == nil {
				p.logger.Debug("successfully submitted data frame on attempt", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "attempt", i+1)
				break
			}
			p.logger.Warn("failed to submit data frame, retrying", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "attempt", i+1, "error", submitErr)
		}
		if submitErr != nil {
			p.logger.Error("all attempts to submit data frame failed", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "error", submitErr)
			return fmt.Errorf("failed to submit data frame after %d attempts: %w", maxRetries, submitErr)
		}
		p.logger.Debug("successfully forwarded data to relay stream", "relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "dataLen", len(rdf.RawData))
		return nil
	default:
		p.logger.Debug("unknown control frame (new)", "type", nf.Type())
		return nil
	}
}

// WriteOffsetForStream returns the current write offset for the given streamID.
func (p *path) WriteOffsetForStream(streamID protocol.StreamID) uint64 {
	p.mu.Lock()
	s, ok := p.streams[streamID]
	if !ok {
		p.mu.Unlock()
		p.logger.Error("WriteOffsetForStream: stream not found", "path", p.id, "streamID", streamID)
		return 0
	}
	offset := s.writeOffset
	p.mu.Unlock()
	return offset
}

func (p *path) IncrementWriteOffsetForStream(streamID protocol.StreamID, n uint64) {
	p.mu.Lock()
	s, ok := p.streams[streamID]
	if !ok {
		p.mu.Unlock()
		p.logger.Error("IncrementWriteOffsetForStream: stream not found", "path", p.id, "streamID", streamID)
		return
	}
	s.writeOffset += n
	p.mu.Unlock()
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

// WriteStream writes to the unified QUIC stream for the provided streamID.
// All frames (control and data) go through the same stream with packet framing
func (p *path) WriteStream(streamID protocol.StreamID, b []byte) (int, error) {
	if len(b) < 4 {
		p.logger.Error("WriteStream: refuse d'écrire un buffer trop court", "path", p.id, "stream", streamID, "len", len(b), "bytes", fmt.Sprintf("% x", b))
		return 0, fmt.Errorf("WriteStream: buffer trop court (%d octets)", len(b))
	}

	p.logger.Debug("WriteStream: about to write", "path", p.id, "stream", streamID, "len", len(b))
	if len(b) <= 8 {
		p.logger.Warn("WriteStream: suspiciously short write", "path", p.id, "stream", streamID, "len", len(b), "bytes", fmt.Sprintf("% x", b))
	}

	// Add length prefix for proper packet framing
	// Format: [4 bytes length][packet data]
	var lengthBuf [4]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(b)))

	// Write length prefix first
	p.logger.Debug("WriteStream: writing packet with length prefix to unified stream",
		"path", p.id,
		"stream", streamID,
		"packetLen", len(b),
		"totalLen", len(b)+4,
		"first_bytes", fmt.Sprintf("% x", b[:min(32, len(b))]))

	// Write to transport stream
	p.transportStreamMu.Lock()
	defer p.transportStreamMu.Unlock()

	// Write length prefix
	total := 0
	start := time.Now()
	for total < 4 {
		n, err := p.transportStream.Write(lengthBuf[total:])
		if n > 0 {
			total += n
		}
		if err != nil {
			dur := time.Since(start)
			p.logger.Error("WriteStream: write error (length prefix)", "path", p.id, "stream", streamID, "err", err, "dur_ms", dur.Milliseconds())
			return total, err
		}
	}

	// Write packet data
	total = 0
	for total < len(b) {
		n, err := p.transportStream.Write(b[total:])
		if n > 0 {
			total += n
		}
		if err != nil {
			dur := time.Since(start)
			p.logger.Warn("WriteStream: write returned error", "path", p.id, "stream", streamID, "written", total, "err", err, "dur_ms", dur.Milliseconds())
			return 0, err // Return 0 as original data wasn't sent successfully
		}
	}
	dur := time.Since(start)
	p.logger.Debug("WriteStream: write succeeded", "path", p.id, "stream", streamID, "n", len(b), "dur_ms", dur.Milliseconds())

	// Return length of original data, not packet data
	return len(b), nil
}
func (p *path) HasStream(streamID protocol.StreamID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.streams[streamID]
	return exists
}

func (p *path) CloseStream(streamID protocol.StreamID) error {
	p.mu.Lock()
	_, ok := p.streams[streamID]
	if !ok {
		p.mu.Unlock()
		return protocol.NewNotExistStreamError(p.id, streamID)
	}
	// Supprimer le stream du path avant de fermer pour éviter les accès concurrents
	delete(p.streams, streamID)
	p.mu.Unlock()

	// Nettoyer le buffer de réception pour éviter les fuites mémoire
	p.CleanupStreamBuffer(streamID)

	// Annuler les paquets en attente de retransmission pour ce stream
	p.CancelFramesForStream(streamID)

	// For unified stream architecture, we just remove from the logical streams map
	// The actual QUIC stream is shared and managed at path level
	p.logger.Debug("CloseStream: removed logical stream", "path", p.id, "stream", streamID)
	return nil
}

func (p *path) Close() error {
	p.logger.Info("Path Close called", "path", p.id, "local", p.LocalAddr(), "remote", p.RemoteAddr())

	p.mu.Lock()
	// Idempotence: si déjà fermé (streams vide)
	alreadyClosed := len(p.streams) == 0
	// Collecter les streamIDs pour nettoyer les buffers de réception
	var streamIDs []protocol.StreamID
	for sid := range p.streams {
		streamIDs = append(streamIDs, sid)
		delete(p.streams, sid)
	}
	p.mu.Unlock()

	if alreadyClosed {
		p.logger.Debug("path already closed", "path", p.id)
		return nil
	}

	// Nettoyer tous les buffers de réception pour éviter les fuites mémoire
	for _, streamID := range streamIDs {
		p.CleanupStreamBuffer(streamID)
		p.CancelFramesForStream(streamID) // Annuler retransmissions
	}

	// Close acceptStreamWaiters to prevent goroutine leaks and signal waiting operations
	p.acceptStreamWaitersMu.Lock()
	for streamID, ch := range p.acceptStreamWaiters {
		select {
		case <-ch:
			// Channel already closed
		default:
			close(ch)
		}
		delete(p.acceptStreamWaiters, streamID)
	}
	p.acceptStreamWaitersMu.Unlock()

	// Close openStreamRespCh to prevent goroutine leaks
	p.openStreamRespChMu.Lock()
	for streamID := range p.openStreamRespCh {
		delete(p.openStreamRespCh, streamID)
	}
	p.openStreamRespChMu.Unlock()

	// Close unified stream
	p.transportStreamMu.Lock()
	if p.transportStream != nil {
		if err := p.transportStream.Close(); err != nil {
			p.logger.Debug("error closing unified stream", "path", p.id, "err", err)
		}
		p.transportStream = nil
	}
	p.transportStreamMu.Unlock()

	// Unregister path from packer so no further writes are attempted to this path.
	if p.session != nil {
		if packer := p.session.Packer(); packer != nil {
			packer.UnregisterPath(p.id)
			p.logger.Debug("unregistered path from packer on close", "path", p.id)
		}
	}

	// Fermer la connexion QUIC sous-jacente
	if err := p.conn.CloseWithError(0, "path closed"); err != nil {
		p.logger.Debug("error closing underlying quic connection", "path", p.id, "err", err)
		return err
	}
	return nil
}

func (p *path) Session() Session {
	return p.session
}

// GetLastAcceptedStreamID returns the ID of the last accepted stream
func (p *path) GetLastAcceptedStreamID() protocol.StreamID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastAcceptedStreamID
}
