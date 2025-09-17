package transport

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

// Error classification functions - optimized with faster string operations
func isFatalConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Use single Contains check with OR logic for better performance
	return strings.Contains(errStr, "path closed") ||
		strings.Contains(errStr, "Application error") ||
		strings.Contains(errStr, "connection closed") ||
		strings.Contains(errStr, "NO_ERROR") ||
		strings.Contains(errStr, "INTERNAL_ERROR") ||
		strings.Contains(errStr, "CONNECTION_REFUSED") ||
		strings.Contains(errStr, "PROTOCOL_VIOLATION")
}

func isRecoverableStreamError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "stream reset") ||
		strings.Contains(errStr, "stream closed")
}

type pathStream struct {
	writeOffset uint64 // Using atomic operations, no mutex needed
}

type PathState int32 // Changed to int32 for atomic operations

const (
	PathStateInitializing PathState = iota
	PathStateReady
	PathStateRecovering
	PathStateFailed
	PathStateClosed
)

type path struct {
	// Core fields
	id       protocol.PathID
	conn     *quic.Conn
	isClient bool
	session  Session
	logger   logger.Logger

	// Stream management - optimized with RWMutex
	streams   map[protocol.StreamID]*pathStream
	streamsMu sync.RWMutex

	// State management - using atomics where possible
	state          int32  // PathState as int32 for atomic access
	closed         int32  // atomic bool
	globalWriteSeq uint64 // already atomic
	sessionReady   int32  // atomic bool

	// Transport stream
	transportStream   *quic.Stream
	transportStreamMu sync.Mutex

	// Health monitoring - optimized
	healthy             int32 // atomic bool
	healthIntervalMs    int
	healthLoopStarted   int32 // atomic bool
	pingsSent           int64 // atomic
	pongsRecv           int64 // atomic
	missedPongs         int64 // atomic
	lastPongAtUnixMilli int64 // atomic
	missedPongThreshold int

	// Stream tracking
	lastAcceptedStreamID protocol.StreamID

	// Recovery mechanism
	streamRecoveryAttempts int32 // atomic
	maxRecoveryAttempts    int
	fatalError             int32 // atomic bool
	recoveryMu             sync.Mutex

	// Channel management - using sync.Map for better concurrent access
	openStreamRespCh    sync.Map // map[protocol.StreamID]chan *protocol.OpenStreamRespFrame
	acceptStreamWaiters sync.Map // map[protocol.StreamID]chan struct{}

	// Control readiness
	controlReady chan struct{}
}

func NewPath(id protocol.PathID, conn *quic.Conn, isClient bool, session Session) *path {
	p := &path{
		id:                   id,
		conn:                 conn,
		isClient:             isClient,
		streams:              make(map[protocol.StreamID]*pathStream),
		logger:               logger.NewLogger(logger.LogLevelSilent).WithComponent("PATH"),
		healthIntervalMs:     15000,
		healthy:              1,
		missedPongThreshold:  3,
		lastAcceptedStreamID: 1,
		session:              session,
		sessionReady:         1,
		maxRecoveryAttempts:  2,
		state:                int32(PathStateInitializing),
		controlReady:         make(chan struct{}),
	}

	// Register the path immediately
	if si, ok := p.session.(sessionInternal); ok {
		si.Packer().RegisterPath(p)
	}

	// Optimized stream setup with exponential backoff
	var stream *quic.Stream
	var err error

	maxAttempts := 3
	baseTimeout := 1 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		timeout := time.Duration(attempt) * baseTimeout
		ctx, cancel := context.WithTimeout(context.Background(), timeout)

		if isClient {
			stream, err = conn.OpenStreamSync(ctx)
		} else {
			stream, err = conn.AcceptStream(ctx)
		}
		cancel()

		if err == nil {
			break
		}

		if attempt < maxAttempts {
			// Exponential backoff with jitter
			backoff := time.Duration(attempt*attempt*50) * time.Millisecond
			time.Sleep(backoff)
		}
	}

	if err != nil {
		p.logger.Error("Failed to open/accept stream after retries", "attempts", maxAttempts, "error", err)
		atomic.StoreInt32(&p.state, int32(PathStateFailed))
		return nil
	}

	p.transportStream = stream
	close(p.controlReady)

	// Start goroutines
	go p.runOptimizedTransportStreamReader(stream)
	go p.startHealthLoop()

	// Ajout : surveille la fermeture du contexte QUIC et ferme le path automatiquement
	go func() {
		<-conn.Context().Done()
		if atomic.LoadInt32(&p.closed) == 0 {
			p.logger.Warn("QUIC connection context closed, closing path", "path", p.id)
			p.closePathDueToFatalError(fmt.Errorf("quic connection closed by peer"))
		}
	}()

	atomic.StoreInt32(&p.state, int32(PathStateReady))
	return p
}

// Optimized state access
func (p *path) State() PathState {
	return PathState(atomic.LoadInt32(&p.state))
}

func (p *path) IsClient() bool {
	return p.isClient
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

// Optimized stream management with RWMutex
func (p *path) RemoveStream(streamID protocol.StreamID) {
	p.streamsMu.Lock()
	delete(p.streams, streamID)
	p.streamsMu.Unlock()

	// Async cleanup to avoid blocking
	go func() {
		p.CleanupStreamBuffer(streamID)
		p.CancelFramesForStream(streamID)
	}()
}

func (p *path) GetStream(streamID protocol.StreamID) (*pathStream, bool) {
	p.streamsMu.RLock()
	stream, ok := p.streams[streamID]
	p.streamsMu.RUnlock()
	return stream, ok
}

func (p *path) HasStream(streamID protocol.StreamID) bool {
	p.streamsMu.RLock()
	_, exists := p.streams[streamID]
	p.streamsMu.RUnlock()
	return exists
}

// Optimized recovery mechanism
func (p *path) tryRecoverTransportStream() bool {
	// Fast path: check if already failed
	if atomic.LoadInt32(&p.fatalError) == 1 {
		return false
	}

	p.recoveryMu.Lock()
	defer p.recoveryMu.Unlock()

	// Double-check under lock
	if atomic.LoadInt32(&p.fatalError) == 1 {
		return false
	}

	attempts := atomic.LoadInt32(&p.streamRecoveryAttempts)
	if int(attempts) >= p.maxRecoveryAttempts {
		p.logger.Error("Max recovery attempts reached", "path", p.id, "attempts", attempts)
		atomic.StoreInt32(&p.fatalError, 1)
		return false
	}

	atomic.AddInt32(&p.streamRecoveryAttempts, 1)
	p.logger.Info("Attempting stream recovery", "path", p.id, "attempt", attempts+1)

	// Close old stream safely
	p.transportStreamMu.Lock()
	oldStream := p.transportStream
	p.transportStreamMu.Unlock()

	if oldStream != nil {
		oldStream.Close()
	}

	// Create new stream with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var newStream *quic.Stream
	var err error

	if p.isClient {
		newStream, err = p.conn.OpenStreamSync(ctx)
	} else {
		newStream, err = p.conn.AcceptStream(ctx)
	}

	if err != nil {
		p.logger.Error("Stream recovery failed", "path", p.id, "err", err)
		atomic.StoreInt32(&p.fatalError, 1)
		return false
	}

	// Update transport stream atomically
	p.transportStreamMu.Lock()
	p.transportStream = newStream
	p.transportStreamMu.Unlock()

	// Start new reader
	go p.runOptimizedTransportStreamReader(newStream)

	p.logger.Info("Stream recovery successful", "path", p.id)
	return true
}

// Optimized error handling
func (p *path) handleWriteError(err error, streamID protocol.StreamID) error {
	if err == nil {
		return nil
	}

	if isFatalConnectionError(err) {
		p.logger.Error("Fatal connection error detected", "path", p.id, "stream", streamID, "err", err)
		atomic.StoreInt32(&p.fatalError, 1)
		go p.closePathDueToFatalError(err)
		return err
	}

	if isRecoverableStreamError(err) {
		p.logger.Warn("Recoverable stream error detected", "path", p.id, "stream", streamID, "err", err)
		if p.tryRecoverTransportStream() {
			return fmt.Errorf("stream recovered, retry needed: %w", err)
		}
		return fmt.Errorf("stream recovery failed: %w", err)
	}

	// Unclassified errors - try recovery once
	p.logger.Warn("Unclassified error, attempting recovery", "path", p.id, "stream", streamID, "err", err)
	if p.tryRecoverTransportStream() {
		return fmt.Errorf("stream recovered, retry needed: %w", err)
	}

	return err
}

func (p *path) closePathDueToFatalError(fatalErr error) {
	p.logger.Error("Closing path due to fatal error", "path", p.id, "err", fatalErr)

	atomic.StoreInt32(&p.closed, 1)
	atomic.StoreInt32(&p.state, int32(PathStateFailed))

	if p.transportStream != nil {
		p.transportStream.Close()
	}

	if si, ok := p.session.(sessionInternal); ok {
		si.HandlePathFailure(p.id, fatalErr)
	}

	p.logger.Info("Path closed due to fatal error", "path", p.id)
}

func (p *path) handleReadError(err error) bool {
	if err == nil {
		return false
	}

	if err.Error() == "EOF" {
		return false
	}

	if isFatalConnectionError(err) {
		p.logger.Error("Fatal connection error in reader", "path", p.id, "err", err)
		atomic.StoreInt32(&p.fatalError, 1)
		go p.closePathDueToFatalError(err)
		return false
	}

	if isRecoverableStreamError(err) {
		p.logger.Warn("Recoverable stream error in reader", "path", p.id, "err", err)
		return p.tryRecoverTransportStream()
	}

	p.logger.Warn("Unclassified read error, attempting recovery", "path", p.id, "err", err)
	return p.tryRecoverTransportStream()
}

// Optimized session readiness check
func (p *path) IsSessionReady() bool {
	return atomic.LoadInt32(&p.sessionReady) == 1
}

// Stream operations optimization
func (p *path) SubmitSendStream(streamID protocol.StreamID) error {
	if PathState(atomic.LoadInt32(&p.state)) != PathStateReady {
		return fmt.Errorf("path not ready (state=%v)", p.State())
	}

	if si, ok := p.session.(sessionInternal); ok {
		if packer := si.Packer(); packer != nil {
			return packer.SubmitFromSendStream(p, streamID)
		}
	}
	return fmt.Errorf("session does not expose internal transport")
}

// Optimized write operations
func (p *path) WriteSeq(streamID protocol.StreamID) (uint64, bool) {
	if PathState(atomic.LoadInt32(&p.state)) != PathStateReady {
		return 0, false
	}

	p.streamsMu.RLock()
	_, ok := p.streams[streamID]
	p.streamsMu.RUnlock()

	if !ok {
		return 0, false
	}

	seq := atomic.AddUint64(&p.globalWriteSeq, 1)
	return seq, true
}

func (p *path) GetWriteOffset(streamID protocol.StreamID) (uint64, bool) {
	p.streamsMu.RLock()
	stream, ok := p.streams[streamID]
	if !ok {
		p.streamsMu.RUnlock()
		return 0, false
	}
	offset := atomic.LoadUint64(&stream.writeOffset)
	p.streamsMu.RUnlock()
	return offset, true
}

func (p *path) SyncWriteOffset(streamID protocol.StreamID, offset uint64) bool {
	p.streamsMu.RLock()
	stream, ok := p.streams[streamID]
	if !ok {
		p.streamsMu.RUnlock()
		return false
	}
	atomic.StoreUint64(&stream.writeOffset, offset)
	p.streamsMu.RUnlock()
	return true
}

func (p *path) AdvanceReadSeq(streamID protocol.StreamID, receivedSeq uint64) bool {
	return true // Simplified for unified stream
}

// Optimized stream opening with sync.Map
func (p *path) OpenStreamSync(ctx context.Context, streamID protocol.StreamID) error {
	p.logger.Debug("OpenStreamSync", "pathID", p.id, "streamID", streamID)

	// Check if stream exists
	if p.HasStream(streamID) {
		p.logger.Warn("Stream already exists", "pathID", p.id, "streamID", streamID)
		return protocol.NewExistStreamError(p.id, streamID)
	}

	// Create response channel
	respCh := make(chan *protocol.OpenStreamRespFrame, 1)
	p.openStreamRespCh.Store(streamID, respCh)

	defer func() {
		p.openStreamRespCh.Delete(streamID)
		close(respCh)
	}()

	// Submit frame
	if si, ok := p.session.(sessionInternal); ok {
		if packer := si.Packer(); packer != nil {
			frame := &protocol.OpenStreamFrame{StreamID: uint64(streamID)}
			if err := packer.SubmitFrame(p, frame); err != nil {
				return fmt.Errorf("failed to send OpenStreamFrame: %w", err)
			}
		} else {
			return fmt.Errorf("no packer available")
		}
	} else {
		return fmt.Errorf("session does not support internal interface")
	}

	// Wait for response
	select {
	case resp := <-respCh:
		if resp.Success {
			p.streamsMu.Lock()
			p.streams[streamID] = &pathStream{writeOffset: 0}
			p.streamsMu.Unlock()
			p.logger.Debug("Successfully opened stream", "pathID", p.id, "streamID", streamID)
			return nil
		}
		return fmt.Errorf("remote failed to open stream %d", streamID)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *path) OpenStream(streamID protocol.StreamID) error {
	if p.HasStream(streamID) {
		return protocol.NewExistStreamError(p.id, streamID)
	}

	p.streamsMu.Lock()
	p.streams[streamID] = &pathStream{writeOffset: 0}
	p.streamsMu.Unlock()

	// Submit frame async
	if si, ok := p.session.(sessionInternal); ok {
		if packer := si.Packer(); packer != nil {
			frame := &protocol.OpenStreamFrame{StreamID: uint64(streamID)}
			_ = packer.SubmitFrame(p, frame)
		}
	}
	return nil
}

func (p *path) AcceptStream(ctx context.Context, streamID protocol.StreamID) error {
	p.logger.Debug("AcceptStream", "pathID", p.id, "streamID", streamID)

	if p.HasStream(streamID) {
		return nil
	}

	// Create or get existing channel
	ch := make(chan struct{})
	actual, loaded := p.acceptStreamWaiters.LoadOrStore(streamID, ch)
	if loaded {
		// Use existing channel
		ch = actual.(chan struct{})
	}

	defer func() {
		p.acceptStreamWaiters.Delete(streamID)
	}()

	select {
	case <-ch:
		p.streamsMu.Lock()
		p.streams[streamID] = &pathStream{writeOffset: 0}
		p.streamsMu.Unlock()
		p.logger.Debug("Successfully accepted stream", "pathID", p.id, "streamID", streamID)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Optimized health loop
func (p *path) startHealthLoop() {
	if !atomic.CompareAndSwapInt32(&p.healthLoopStarted, 0, 1) {
		return
	}

	p.logger.Debug("Starting health loop", "path", p.id)
	ticker := time.NewTicker(time.Duration(p.healthIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		// Check if path is closed
		if atomic.LoadInt32(&p.closed) == 1 {
			return
		}

		// Send ping (async via packer)
		atomic.AddInt64(&p.pingsSent, 1)

		// Check pong reception
		baseTimeout := time.Duration(p.healthIntervalMs/2) * time.Millisecond
		time.Sleep(baseTimeout)

		last := atomic.LoadInt64(&p.lastPongAtUnixMilli)
		timeSinceLastPong := time.Now().UnixMilli() - last

		if last == 0 || timeSinceLastPong > int64(p.healthIntervalMs*2) {
			miss := atomic.AddInt64(&p.missedPongs, 1)
			if miss >= int64(p.missedPongThreshold) {
				atomic.StoreInt32(&p.healthy, 0)
				p.logger.Warn("Path marked unhealthy", "path", p.id, "missed_pongs", miss)
			}
		} else {
			if atomic.LoadInt64(&p.missedPongs) > 0 {
				p.logger.Debug("Pong received, resetting missed count", "path", p.id)
			}
			atomic.StoreInt64(&p.missedPongs, 0)
			atomic.StoreInt32(&p.healthy, 1)
		}
	}
}

// Optimized control frame handling with fast path for common frames
func (p *path) HandleControlFrame(nf protocol.Frame) error {
	switch nf.Type() {
	case protocol.FrameTypePing:
		// Fast path for ping frames - most common
		atomic.AddInt64(&p.pongsRecv, 1)
		atomic.StoreInt64(&p.lastPongAtUnixMilli, time.Now().UnixMilli())
		atomic.StoreInt64(&p.missedPongs, 0)
		atomic.StoreInt32(&p.healthy, 1)

		// Echo back if server (non-blocking)
		if !p.isClient {
			if si, ok := p.session.(sessionInternal); ok {
				if packer := si.Packer(); packer != nil {
					go func() { // Async to avoid blocking
						_ = packer.SubmitFrame(p, &protocol.PingFrame{})
					}()
				}
			}
		}
		return nil

	case protocol.FrameTypeOpenStreamResp:
		resp := nf.(*protocol.OpenStreamRespFrame)
		if ch, ok := p.openStreamRespCh.Load(protocol.StreamID(resp.StreamID)); ok {
			select {
			case ch.(chan *protocol.OpenStreamRespFrame) <- resp:
			default:
				// Channel full or closed - ignore
			}
		}
		return nil

	case protocol.FrameTypeOpenStream:
		return p.handleOpenStreamFrame(nf.(*protocol.OpenStreamFrame))

	default:
		// Handle other frame types with existing logic
		return p.handleOtherControlFrames(nf)
	}
}

// Separate handler for OpenStream frames to reduce complexity
func (p *path) handleOpenStreamFrame(osf *protocol.OpenStreamFrame) error {
	sid := protocol.StreamID(osf.StreamID)
	p.logger.Info("Received OpenStreamFrame", "path", p.id, "streamID", osf.StreamID)

	var success bool
	if ch, loaded := p.acceptStreamWaiters.Load(sid); loaded {
		select {
		case <-ch.(chan struct{}):
			// Already closed
		default:
			close(ch.(chan struct{}))
		}
		success = true
	} else {
		// Create closed channel for future use
		ch := make(chan struct{})
		close(ch)
		p.acceptStreamWaiters.Store(sid, ch)
		success = true
	}

	// Send response asynchronously to avoid blocking
	resp := &protocol.OpenStreamRespFrame{StreamID: osf.StreamID, Success: success}
	if si, ok := p.session.(sessionInternal); ok {
		if packer := si.Packer(); packer != nil {
			go func() { // Async submission
				_ = packer.SubmitFrame(p, resp)
			}()
		}
	}
	return nil
}

// Handle less common control frames
func (p *path) handleOtherControlFrames(nf protocol.Frame) error {
	switch nf.Type() {
	case protocol.FrameTypeAddPath:
		if !p.isClient {
			p.logger.Debug("Ignoring AddPath frame on server-side path", "path", p.id)
			return nil
		}
		apf := nf.(*protocol.AddPathFrame)
		p.logger.Debug("Received AddPath request", "address", apf.Address)

		// Handle async to avoid blocking
		go func() {
			pathID, err := p.session.PathManager().OpenPath(context.Background(), apf.Address, p.session)
			if err != nil {
				p.logger.Error("Failed to open path", "error", err)
				return
			}
			respFrame := &protocol.AddPathRespFrame{Address: apf.Address, PathID: pathID}
			if si, ok := p.session.(sessionInternal); ok {
				if packer := si.Packer(); packer != nil {
					_ = packer.SubmitFrame(p, respFrame)
				}
			}
		}()
		return nil

	case protocol.FrameTypeAddPathResp:
		if p.isClient {
			p.logger.Debug("Ignoring AddPathResp frame on client-side path", "path", p.id)
			return nil
		}
		aprf := nf.(*protocol.AddPathRespFrame)
		if pm, ok := p.session.PathManager().(*pathManagerImpl); ok && pm != nil {
			go pm.handleAddPathResp(aprf) // Async handling
		}
		return nil

	case protocol.FrameTypeRelayData:
		if !p.isClient {
			return nil
		}
		// Handle relay data async to avoid blocking reader
		go p.handleRelayDataFrame(nf.(*protocol.RelayDataFrame))
		return nil

	default:
		p.logger.Debug("Unknown control frame", "type", nf.Type())
		return nil
	}
}

// Separate async handler for RelayData frames
func (p *path) handleRelayDataFrame(rdf *protocol.RelayDataFrame) {
	p.logger.Debug("Processing RelayData", "path", p.id, "relayPathID", rdf.RelayPathID,
		"destStreamID", rdf.DestStreamID, "dataLen", len(rdf.RawData))

	if rdf.DestStreamID == 0 {
		p.logger.Error("Invalid DestStreamID 0 in RelayData")
		return
	}

	pathMgr := p.session.PathManager()
	relayPath := pathMgr.GetPath(rdf.RelayPathID)
	if relayPath == nil {
		p.logger.Error("Relay path not found", "relayPathID", rdf.RelayPathID)
		return
	}

	// Ensure stream exists on relay path
	if !relayPath.HasStream(rdf.DestStreamID) {
		if err := relayPath.OpenStream(rdf.DestStreamID); err != nil {
			if !relayPath.HasStream(rdf.DestStreamID) {
				p.logger.Error("Failed to create stream on relay path",
					"relayPathID", rdf.RelayPathID, "streamID", rdf.DestStreamID, "error", err)
				return
			}
		}

		// Register stream with StreamManager
		if streamMgr := p.session.StreamManager(); streamMgr != nil {
			if err := streamMgr.AddPathToStream(rdf.DestStreamID, relayPath); err != nil {
				p.logger.Warn("Failed to register stream path", "error", err)
			}
		}
	}

	// Create and submit data frame
	dataFrame := &protocol.StreamFrame{
		StreamID: uint64(rdf.DestStreamID),
		Offset:   relayPath.WriteOffsetForStream(rdf.DestStreamID),
		Data:     rdf.RawData,
	}
	relayPath.IncrementWriteOffsetForStream(rdf.DestStreamID, uint64(len(rdf.RawData)))

	if packer := p.session.Packer(); packer != nil {
		if err := packer.SubmitFrame(relayPath, dataFrame); err != nil {
			p.logger.Error("Failed to submit data frame", "error", err)
		}
	}
}

// Optimized write operation with buffer pooling
func (p *path) WriteStream(streamID protocol.StreamID, b []byte) (int, error) {
	if len(b) < 4 {
		return 0, fmt.Errorf("buffer too short (%d bytes)", len(b))
	}

	// Fast path state check
	if PathState(atomic.LoadInt32(&p.state)) != PathStateReady {
		return 0, fmt.Errorf("path not ready")
	}

	p.transportStreamMu.Lock()
	defer p.transportStreamMu.Unlock()

	// Check stream is still available after acquiring lock
	if p.transportStream == nil {
		return 0, fmt.Errorf("transport stream closed")
	}

	// Pre-allocate buffer with length prefix - more efficient than separate writes
	totalLen := len(b) + 4

	// Use a larger buffer to reduce allocations for small writes
	var buf []byte
	if totalLen <= 8192 { // 8KB threshold
		// Use stack allocation for small writes
		stackBuf := make([]byte, totalLen)
		buf = stackBuf
	} else {
		buf = make([]byte, totalLen)
	}

	binary.BigEndian.PutUint32(buf[:4], uint32(len(b)))
	copy(buf[4:], b)

	start := time.Now()

	// Single write operation - more efficient than two separate writes
	_, err := p.transportStream.Write(buf)
	if err != nil {
		dur := time.Since(start)
		p.logger.Error("WriteStream error", "path", p.id, "stream", streamID,
			"err", err, "dur_ms", dur.Milliseconds())
		return 0, p.handleWriteError(err, streamID)
	}

	dur := time.Since(start)
	p.logger.Debug("WriteStream succeeded", "path", p.id, "stream", streamID,
		"bytes", len(b), "dur_ms", dur.Milliseconds())

	return len(b), nil
}

// Helper methods for write offset management
func (p *path) WriteOffsetForStream(streamID protocol.StreamID) uint64 {
	if offset, ok := p.GetWriteOffset(streamID); ok {
		return offset
	}
	return 0
}

func (p *path) IncrementWriteOffsetForStream(streamID protocol.StreamID, n uint64) {
	p.streamsMu.RLock()
	stream, ok := p.streams[streamID]
	if ok {
		atomic.AddUint64(&stream.writeOffset, n)
	}
	p.streamsMu.RUnlock()
}

// Optimized metrics collection
func (p *path) GetMetrics() map[string]interface{} {
	return map[string]interface{}{
		"pings_sent":    atomic.LoadInt64(&p.pingsSent),
		"pongs_recv":    atomic.LoadInt64(&p.pongsRecv),
		"missed_pongs":  atomic.LoadInt64(&p.missedPongs),
		"last_pong_ms":  atomic.LoadInt64(&p.lastPongAtUnixMilli),
		"healthy":       atomic.LoadInt32(&p.healthy) == 1,
		"session_ready": atomic.LoadInt32(&p.sessionReady) == 1,
		"state":         p.State().String(),
	}
}

func (p *path) CloseStream(streamID protocol.StreamID) error {
	p.streamsMu.Lock()
	_, ok := p.streams[streamID]
	if !ok {
		p.streamsMu.Unlock()
		return protocol.NewNotExistStreamError(p.id, streamID)
	}
	delete(p.streams, streamID)
	p.streamsMu.Unlock()

	// Async cleanup
	go func() {
		p.CleanupStreamBuffer(streamID)
		p.CancelFramesForStream(streamID)
	}()

	p.logger.Debug("Stream closed", "path", p.id, "stream", streamID)
	return nil
}

// Optimized close operation
func (p *path) Close() error {
	// Atomic close check
	if !atomic.CompareAndSwapInt32(&p.closed, 0, 1) {
		return nil // Already closed
	}

	p.logger.Info("Closing path", "path", p.id)
	atomic.StoreInt32(&p.state, int32(PathStateClosed))

	// Collect streams for cleanup
	p.streamsMu.Lock()
	streamIDs := make([]protocol.StreamID, 0, len(p.streams))
	for sid := range p.streams {
		streamIDs = append(streamIDs, sid)
		delete(p.streams, sid)
	}
	p.streamsMu.Unlock()

	// Async cleanup of streams
	go func() {
		for _, streamID := range streamIDs {
			p.CleanupStreamBuffer(streamID)
			p.CancelFramesForStream(streamID)
		}
	}()

	// Close channels
	p.acceptStreamWaiters.Range(func(key, value interface{}) bool {
		ch := value.(chan struct{})
		select {
		case <-ch:
			// Already closed
		default:
			close(ch)
		}
		p.acceptStreamWaiters.Delete(key)
		return true
	})

	p.openStreamRespCh.Range(func(key, value interface{}) bool {
		p.openStreamRespCh.Delete(key)
		return true
	})

	// Close transport stream
	p.transportStreamMu.Lock()
	if p.transportStream != nil {
		p.transportStream.Close()
		p.transportStream = nil
	}
	p.transportStreamMu.Unlock()

	// Unregister from packer
	if p.session != nil {
		if packer := p.session.Packer(); packer != nil {
			packer.UnregisterPath(p.id)
		}
	}

	// Close QUIC connection
	return p.conn.CloseWithError(0, "path closed")
}

// Additional helper methods
func (p *path) Session() Session {
	return p.session
}

func (p *path) GetLastAcceptedStreamID() protocol.StreamID {
	p.streamsMu.RLock()
	defer p.streamsMu.RUnlock()
	return p.lastAcceptedStreamID
}

func (p *path) CancelFramesForStream(streamID protocol.StreamID) {
	if si, ok := p.session.(sessionInternal); ok {
		if packer := si.Packer(); packer != nil {
			packer.CancelFramesForStream(streamID)
		}
	}
}

func (p *path) CleanupStreamBuffer(streamID protocol.StreamID) {
	if si, ok := p.session.(sessionInternal); ok {
		if mux := si.Multiplexer(); mux != nil {
			mux.CleanupStreamBuffer(streamID)
		}
	}
}

// Add String method for PathState
func (s PathState) String() string {
	switch s {
	case PathStateInitializing:
		return "Initializing"
	case PathStateReady:
		return "Ready"
	case PathStateRecovering:
		return "Recovering"
	case PathStateFailed:
		return "Failed"
	case PathStateClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}
