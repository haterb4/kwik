package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
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
	sessionMu            sync.Mutex
	sessionReady         bool
	handshakeRespCh      chan *protocol.Frame
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
	controlMu       sync.Mutex
	controlReady    chan struct{}
	controlCreating bool
	session         Session
}

func NewPath(id protocol.PathID, conn *quic.Conn, isClient bool, session Session) *path {
	path := path{
		id:                   id,
		conn:                 conn,
		isClient:             isClient,
		streams:              make(map[protocol.StreamID]*quic.Stream),
		logger:               logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH"),
		healthIntervalMs:     15000, // Augmenter à 15 secondes pour réduire le trafic de contrôle
		healthy:              1,
		missedPongThreshold:  3,
		lastAcceptedStreamID: 1,
		session:              session,
	}
	// Initialize control stream readiness channel
	path.controlReady = make(chan struct{})

	// Register the path immediately so it can receive control frames
	path.session.Packer().RegisterPath(&path)

	// Opening or accepting the control stream here and perform the handshake here before returning the path.
	if isClient {
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			panic(err)
		}
		path.streams[protocol.StreamID(0)] = stream
		// Mark control stream as ready since we just created it
		close(path.controlReady)
		go path.runStreamReader(protocol.StreamID(0), stream)
		err = path.startClientHandshake()
		if err != nil {
			panic(err)
		}
	} else {
		// Server waits for client to open the control stream
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			panic(err)
		}
		path.streams[protocol.StreamID(0)] = stream
		// Mark control stream as ready since we just accepted it
		close(path.controlReady)
		go path.runStreamReader(protocol.StreamID(0), stream)
		err = path.startServerHandshake()
		if err != nil {
			panic(err)
		}
	}
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
}

// IsSessionReady reports whether the handshake/session is established.
func (p *path) IsSessionReady() bool {
	p.sessionMu.Lock()
	ready := p.sessionReady
	p.sessionMu.Unlock()
	return ready
}

func (p *path) OpenStreamSync(ctx context.Context, streamID protocol.StreamID) error {
	p.logger.Debug("Opening stream synchronously", "pathID", p.id, "streamID", streamID)
	stream, err := p.conn.OpenStreamSync(ctx)
	if err != nil {
		p.logger.Error("Failed to open QUIC stream", "pathID", p.id, "streamID", streamID, "error", err)
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.logger.Debug("Opening stream asynchronously", "pathID", p.id, "streamID", streamID)
	stream, err := p.conn.OpenStream()
	if err != nil {
		p.logger.Error("Failed to open QUIC stream", "pathID", p.id, "streamID", streamID, "error", err)
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.logger.Debug("Waiting for new QUIC stream on path", "pathID", p.id)
	stream, err := p.conn.AcceptStream(ctx)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.logger.Debug("Accepted new QUIC stream",
		"pathID", p.id,
		"quicStreamID", stream.StreamID(),
		"logicalStreamID", streamID)

	if _, ok := p.streams[streamID]; ok {
		stream.Close()
		return protocol.NewExistStreamError(p.id, streamID)
	}

	p.streams[streamID] = stream
	p.lastAcceptedStreamID = streamID // Update last accepted stream ID

	// Start reader goroutine to forward length-prefixed packets to multiplexer
	go p.runStreamReader(streamID, stream)
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
		// detailed debug for control stream packets
		if streamID == protocol.StreamID(0) && p.logger != nil {
			p.logger.Debug("control stream packet received", "path", p.id, "stream", streamID, "seq", packetSeq, "len", l)
		}
		if mx := p.session.Multiplexer(); mx != nil {
			_ = mx.PushPacketWithSeq(p.id, packetSeq, buf)
		}
		runtime.Gosched()
	}
}

// SendControlFrame writes a control frame (handshake, ping, pong) on the reserved control stream (streamID 0).
func (p *path) SendControlFrame(f *protocol.Frame) error {
	// Silenced ping/pong logs
	if f.Type != protocol.FrameTypePing && f.Type != protocol.FrameTypePong {
		p.logger.Debug("sending control frame", "path", p.id, "type", f.Type, "isClient", p.isClient)
	}
	// ensure control stream exists (create exactly once)
	if err := p.ensureControlStream(); err != nil {
		p.logger.Error("failed to ensure control stream", "path", p.id, "err", err)
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
	// debug log for outgoing control frame (silenced for ping/pong)
	if p.logger != nil && f.Type != protocol.FrameTypePing && f.Type != protocol.FrameTypePong {
		p.logger.Debug("sending control frame", "path", p.id, "type", f.Type, "len", len(outWithSeq))
	}
	// write with packetSeq 0
	_, err := p.WriteStream(protocol.StreamID(0), outWithSeq)

	// Si l'écriture sur le canal de contrôle échoue et que l'erreur indique une fermeture de connexion
	if err != nil && (strings.Contains(err.Error(), "path closed") ||
		strings.Contains(err.Error(), "Application error")) {
		p.logger.Warn("control stream write failed, closing path", "path", p.id, "err", err)

		// Fermer ce path
		closeErr := p.Close()
		if closeErr != nil {
			p.logger.Error("error closing path after control stream failure",
				"path", p.id, "err", closeErr)
		}

		// Si c'est le path principal (ID 1), fermer également la session
		if p.id == protocol.PathID(1) && p.session != nil {
			p.logger.Warn("primary path control stream failed, closing entire session", "path", p.id)
			// Utiliser la connexion sous-jacente pour fermer avec erreur
			if p.conn != nil {
				p.conn.CloseWithError(0, "primary path control stream failed")
			}
		}
	}

	return err
}

// ensureControlStream creates the reserved control stream exactly once and
// allows multiple callers to wait until it's ready.
func (p *path) ensureControlStream() error {
	// p.logger.Debug("ensuring control stream", "path", p.id, "isClient", p.isClient)
	p.controlMu.Lock()
	if p.controlReady != nil {
		// already created or being created
		ch := p.controlReady
		p.controlMu.Unlock()
		// p.logger.Debug("control stream already ready, waiting", "path", p.id)
		<-ch
		return nil
	}

	// Check if control stream already exists (created in NewPath)
	p.mu.Lock()
	_, exists := p.streams[protocol.StreamID(0)]
	p.mu.Unlock()

	if exists {
		// Control stream already exists, just mark as ready
		p.controlReady = make(chan struct{})
		close(p.controlReady)
		p.controlMu.Unlock()
		p.logger.Debug("control stream already exists", "path", p.id)
		return nil
	}

	// not created yet; mark creating
	p.controlReady = make(chan struct{})
	p.controlCreating = true
	p.controlMu.Unlock()
	p.logger.Debug("creating control stream", "path", p.id, "isClient", p.isClient)

	// attempt to open or accept control stream depending on role
	var err error
	if p.isClient {
		// client actively opens the control stream
		err = p.OpenStream(protocol.StreamID(0))
	} else {
		// For server, control stream should already exist from NewPath
		p.logger.Error("control stream should have been created in NewPath", "path", p.id)
		err = protocol.NewHandshakeFailedError(p.id)
	}

	p.controlMu.Lock()
	// signal waiters
	close(p.controlReady)
	p.controlCreating = false
	p.controlMu.Unlock()
	return err
}
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
	// Prevent multiple health loops
	if !atomic.CompareAndSwapInt32(&p.healthLoopStarted, 0, 1) {
		p.logger.Debug("health loop already started", "path", p.id)
		return
	}

	p.logger.Debug("starting health loop", "path", p.id)
	ticker := time.NewTicker(time.Duration(p.healthIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	// Variables pour suivre les erreurs et les succès
	consecutiveWriteErrors := 0
	consecutiveSuccesses := 0
	maxConsecutiveErrors := 5 // Plus tolérant qu'avant (était 3)
	errorResetThreshold := 3  // Réinitialiser après N succès consécutifs

	// Variables pour la gestion des timeouts adaptatifs
	baseTimeout := time.Duration(p.healthIntervalMs/2) * time.Millisecond
	currentTimeout := baseTimeout
	maxTimeout := time.Duration(p.healthIntervalMs) * time.Millisecond

	// Compteurs pour les statistiques détaillées
	totalPingsSent := int64(0)
	totalErrors := int64(0)
	lastSuccessfulPong := time.Now()

	for range ticker.C {
		// Vérifier si le path est encore valide avant d'envoyer un ping
		if p.conn == nil {
			p.logger.Debug("connection is nil, stopping health loop", "path", p.id)
			return
		}

		// Créer le ping frame avec timestamp
		ts := time.Now().UnixMilli()
		var tb [8]byte
		binary.BigEndian.PutUint64(tb[:], uint64(ts))
		ping := &protocol.Frame{
			Type:     protocol.FrameTypePing,
			StreamID: 0,
			Seq:      0,
			Payload:  tb[:],
		}

		// Envoyer le ping avec gestion d'erreurs améliorée
		pingStartTime := time.Now()
		err := p.SendControlFrame(ping)
		pingDuration := time.Since(pingStartTime)

		totalPingsSent++
		atomic.AddInt64(&p.pingsSent, 1)

		if err != nil {
			totalErrors++

			// Catégoriser les types d'erreurs
			isConnectionError := strings.Contains(err.Error(), "path closed") ||
				strings.Contains(err.Error(), "Application error") ||
				strings.Contains(err.Error(), "connection closed") ||
				strings.Contains(err.Error(), "use of closed") ||
				strings.Contains(err.Error(), "broken pipe")

			isTimeoutError := strings.Contains(err.Error(), "timeout") ||
				strings.Contains(err.Error(), "deadline exceeded")

			if isConnectionError {
				consecutiveWriteErrors++
				consecutiveSuccesses = 0
				p.logger.Warn("ping failed due to connection issues",
					"path", p.id,
					"error", err,
					"consecutive_errors", consecutiveWriteErrors,
					"ping_duration_ms", pingDuration.Milliseconds(),
					"total_pings", totalPingsSent,
					"total_errors", totalErrors)

				// Fermeture progressive : plus d'erreurs consécutives requises
				if consecutiveWriteErrors >= maxConsecutiveErrors {
					p.logger.Error("stopping health loop due to too many consecutive connection errors",
						"path", p.id,
						"consecutive_errors", consecutiveWriteErrors,
						"total_pings", totalPingsSent,
						"total_errors", totalErrors,
						"last_successful_pong", time.Since(lastSuccessfulPong))

					// Fermer ce path
					if err := p.Close(); err != nil {
						p.logger.Error("failed to close path after too many health check errors",
							"path", p.id, "close_err", err)
					}

					// Si c'est le path principal, fermer la session avec plus de contexte
					if p.id == protocol.PathID(1) && p.session != nil {
						p.logger.Warn("primary path health check failed, closing entire session",
							"path", p.id,
							"consecutive_errors", consecutiveWriteErrors,
							"session_duration", time.Since(lastSuccessfulPong))

						if p.conn != nil {
							closeMsg := fmt.Sprintf("primary path health check failed after %d consecutive errors", consecutiveWriteErrors)
							p.conn.CloseWithError(0, closeMsg)
						}
					}

					return
				}

				// Augmenter temporairement le timeout après une erreur de connexion
				currentTimeout = time.Duration(float64(currentTimeout) * 1.5)
				if currentTimeout > maxTimeout {
					currentTimeout = maxTimeout
				}

			} else if isTimeoutError {
				// Les timeouts sont moins graves, ne pas les compter comme erreurs de connexion
				p.logger.Debug("ping timeout (less critical)",
					"path", p.id,
					"error", err,
					"ping_duration_ms", pingDuration.Milliseconds())

				// Réinitialiser partiellement les succès mais pas complètement
				if consecutiveSuccesses > 0 {
					consecutiveSuccesses--
				}

			} else {
				// Autres types d'erreurs (moins critiques)
				p.logger.Debug("ping failed with non-critical error",
					"path", p.id,
					"error", err,
					"ping_duration_ms", pingDuration.Milliseconds())

				// Ne pas incrémenter consecutiveWriteErrors pour les erreurs non-critiques
				// mais réduire les succès consécutifs
				if consecutiveSuccesses > 0 {
					consecutiveSuccesses--
				}
			}

		} else {
			// Ping envoyé avec succès
			consecutiveSuccesses++

			// Réinitialiser les erreurs après plusieurs succès consécutifs
			if consecutiveSuccesses >= errorResetThreshold {
				if consecutiveWriteErrors > 0 {
					p.logger.Debug("resetting error count after consecutive successes",
						"path", p.id,
						"previous_errors", consecutiveWriteErrors,
						"consecutive_successes", consecutiveSuccesses)
				}
				consecutiveWriteErrors = 0
				consecutiveSuccesses = 0 // Reset aussi les succès pour éviter l'accumulation
			}

			// Restaurer progressivement le timeout normal
			if currentTimeout > baseTimeout {
				currentTimeout = time.Duration(float64(currentTimeout) * 0.9)
				if currentTimeout < baseTimeout {
					currentTimeout = baseTimeout
				}
			}

			// Log périodique des statistiques (tous les 10 pings réussis)
			if totalPingsSent%10 == 0 {
				p.logger.Debug("health check statistics",
					"path", p.id,
					"total_pings", totalPingsSent,
					"total_errors", totalErrors,
					"error_rate", float64(totalErrors)/float64(totalPingsSent)*100,
					"consecutive_successes", consecutiveSuccesses,
					"current_timeout_ms", currentTimeout.Milliseconds())
			}
		}

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
			lastSuccessfulPong = time.Now()
		}

		// Vérification de sécurité : si le path est resté malsain trop longtemps
		if atomic.LoadInt32(&p.healthy) == 0 && time.Since(lastSuccessfulPong) > time.Duration(p.healthIntervalMs*10)*time.Millisecond {
			p.logger.Warn("path has been unhealthy for extended period",
				"path", p.id,
				"unhealthy_duration_ms", time.Since(lastSuccessfulPong).Milliseconds(),
				"missed_pongs", atomic.LoadInt64(&p.missedPongs))
		}
	}
}

// HandleControlFrame processes inbound control frames received for this path.
func (p *path) HandleControlFrame(f *protocol.Frame) error {
	// p.logger.Debug("HandleControlFrame called", "path", p.id, "isClient", p.isClient, "frameType", f.Type)
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
		// Silenced: p.logger.Debug("received pong", "path", p.id, "time", now)
		return nil
	case protocol.FrameTypeHandshake:
		p.logger.Debug("received handshake frame", "path", p.id, "isClient", p.isClient)
		// reply with handshake response
		// echo the nonce back as a response
		resp := &protocol.Frame{Type: protocol.FrameTypeHandshakeResp, StreamID: 0, Seq: 0, Payload: f.Payload}
		if err := p.SendControlFrame(resp); err != nil {
			p.logger.Error("failed to send handshake response", "path", p.id, "err", err)
			return err
		}
		p.sessionMu.Lock()
		p.sessionReady = true
		p.sessionMu.Unlock()
		p.logger.Info("handshake completed (server-side)", "path", p.id, "isClient", p.isClient)
		// start health loop now that session is established
		go p.startHealthLoop()
		return nil
	case protocol.FrameTypeHandshakeResp:
		p.logger.Debug("received handshake response", "path", p.id, "isClient", p.isClient)
		// signal waiting StartHandshake if present
		select {
		case p.handshakeRespCh <- f:
			p.logger.Debug("sent handshake response to channel", "path", p.id)
		default:
			p.logger.Debug("handshake response channel full or not ready", "path", p.id)
		}
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
	// Vérifier si le path est en cours de fermeture (évite panic lors de la fermeture)
	var pathClosed bool
	p.mu.Lock()
	s, ok := p.streams[streamID]
	pathClosed = len(p.streams) == 0 // Si aucun stream, le path est probablement en cours de fermeture
	p.mu.Unlock()

	// Si le path est en cours de fermeture, retourner une erreur au lieu de paniquer
	if !ok {
		if pathClosed {
			errMsg := fmt.Sprintf("ReadStream: path %d is closing", p.id)
			p.logger.Warn(errMsg)
			return 0, fmt.Errorf("path closed: %w", protocol.NewNotExistStreamError(p.id, streamID))
		} else {
			// Si ce n'est pas lié à une fermeture du path, déclenchons une panic
			// car cela indique un problème de cohérence qui doit être corrigé
			errMsg := fmt.Sprintf("PANIC: ReadStream: stream %d not found in path %d", streamID, p.id)
			p.logger.Error(errMsg)
			panic(errMsg)
		}
	}
	return s.Read(b)
}

// WriteStream writes to the stored quic.Stream for the provided streamID.
func (p *path) WriteStream(streamID protocol.StreamID, b []byte) (int, error) {
	// Vérifier si le path est en cours de fermeture (évite panic lors de la fermeture)
	var pathClosed bool
	p.mu.Lock()
	s, ok := p.streams[streamID]
	pathClosed = len(p.streams) == 0 // Si aucun stream, le path est probablement en cours de fermeture
	p.mu.Unlock()

	// Si le path est en cours de fermeture, retourner une erreur au lieu de paniquer
	if !ok {
		if pathClosed {
			errMsg := fmt.Sprintf("WriteStream: path %d is closing", p.id)
			p.logger.Warn(errMsg)
			return 0, fmt.Errorf("path closed: %w", protocol.NewNotExistStreamError(p.id, streamID))
		} else {
			// Si ce n'est pas lié à une fermeture du path, déclenchons une panic
			// car cela indique un problème de cohérence qui doit être corrigé
			errMsg := fmt.Sprintf("PANIC: WriteStream: stream %d not found in path %d", streamID, p.id)
			p.logger.Error(errMsg)
			panic(errMsg)
		}
	}

	p.logger.Debug("WriteStream: about to write", "path", p.id, "stream", streamID, "len", len(b))

	// Pour éviter les blocages lors de l'écriture, nous effectuons une écriture garantie
	start := time.Now()

	// Écriture principale
	n, err := s.Write(b)
	dur := time.Since(start)

	// En cas d'erreur lors de l'écriture, journaliser et renvoyer l'erreur
	if err != nil {
		p.logger.Warn("WriteStream: write returned error", "path", p.id, "stream", streamID, "n", n, "err", err, "dur_ms", dur.Milliseconds())
	} else {
		p.logger.Debug("WriteStream: write succeeded", "path", p.id, "stream", streamID, "n", n, "dur_ms", dur.Milliseconds())

		// Pour le serveur, nous voulons nous assurer que les données sont envoyées immédiatement
		// Nous pouvons envoyer un petit message de synchronisation pour forcer le flush du buffer
		if !p.isClient && streamID != 0 { // Éviter de faire ceci sur le stream de contrôle
			// Une écriture vide peut parfois forcer le flush du buffer sous-jacent
			// C'est une technique qui peut aider à garantir que les données précédentes sont envoyées
			emptySync := make([]byte, 0) // Message vide pour forcer le flush
			_, syncErr := s.Write(emptySync)
			if syncErr != nil {
				// Une erreur ici n'est pas critique, nous l'enregistrons simplement
				p.logger.Debug("WriteStream: sync write failed", "path", p.id, "stream", streamID, "err", syncErr)
			}
		}
	}

	return n, err
} // HasStream checks if a stream with the given ID exists in this path
func (p *path) HasStream(streamID protocol.StreamID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.streams[streamID]
	return exists
}

func (p *path) CloseStream(streamID protocol.StreamID) error {
	p.mu.Lock()
	stream, ok := p.streams[streamID]
	if !ok {
		p.mu.Unlock()
		return protocol.NewNotExistStreamError(p.id, streamID)
	}

	// Supprimer le stream du path avant de fermer pour éviter les accès concurrents
	delete(p.streams, streamID)
	p.mu.Unlock()

	// Fermer le stream QUIC sous-jacent
	return stream.Close()
}

func (p *path) Close() error {
	// Log path closure
	p.logger.Info("Path Close called", "path", p.id, "local", p.LocalAddr(), "remote", p.RemoteAddr())

	// Synchronisation pour éviter les appels concurrents à Close
	p.mu.Lock()

	// Vérifier si le path est déjà fermé (streams vides)
	if len(p.streams) == 0 {
		p.logger.Debug("path already closed", "path", p.id)
		p.mu.Unlock()
		return nil
	}

	// Unregister path from packer so no further writes are attempted to this path.
	if p.session != nil {
		if packer := p.session.Packer(); packer != nil {
			packer.UnregisterPath(p.id)
			p.logger.Debug("unregistered path from packer on close", "path", p.id)
		}
	}

	// Stocker une copie locale des streams pour les fermer après avoir libéré le mutex
	streamsCopy := make(map[protocol.StreamID]*quic.Stream)
	for sid, s := range p.streams {
		streamsCopy[sid] = s
		delete(p.streams, sid) // Supprimer immédiatement de la map pour éviter les accès concurrents
	}
	p.mu.Unlock()

	// Fermer tous les streams après avoir libéré le mutex pour éviter les deadlocks
	for sid, s := range streamsCopy {
		if s != nil {
			if err := s.Close(); err != nil {
				p.logger.Debug("error closing stream during path close", "path", p.id, "stream", sid, "err", err)
			}
		}
	}

	// Finally close the underlying QUIC connection
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
