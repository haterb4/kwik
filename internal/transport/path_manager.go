package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type pathManagerImpl struct {
	paths              map[protocol.PathID]*path
	relays             map[protocol.PathID]*relayImpl
	nextPathID         uint64
	tlsCfg             *tls.Config
	config             *config.Config
	primaryPathID      protocol.PathID
	mu                 sync.Mutex
	logger             logger.Logger
	addPathRespChannel chan *protocol.AddPathRespPayload
}

func NewClientPathManager(tls *tls.Config, cfg *config.Config) *pathManagerImpl {
	return &pathManagerImpl{
		paths:         make(map[protocol.PathID]*path),
		nextPathID:    0,
		tlsCfg:        tls,
		config:        cfg,
		primaryPathID: 0, // Initialize to 0 (invalid)
		logger:        logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH_MANAGER"),
	}
}
func NewServerPathManager() *pathManagerImpl {
	return &pathManagerImpl{
		paths:         make(map[protocol.PathID]*path),
		nextPathID:    0,
		primaryPathID: 0, // Initialize to 0 (invalid)
		logger:        logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH_MANAGER"),
	}
}

// OpenPath establishes a new QUIC connection to the specified address and assigns it a unique PathID.
// It returns the newly created PathID or an error if the connection could not be established or if the PathID already exists.
// The function is thread-safe and ensures that each PathID is unique by using atomic operations.
// If a path with the generated PathID already exists, it returns a PathNotExistsError.
func (pm *pathManagerImpl) OpenPath(ctx context.Context, address string, session Session) (protocol.PathID, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	conn, err := quic.DialAddr(ctx, address, pm.tlsCfg, pm.config)
	if err != nil {
		return 0, err
	}
	nextPathID := protocol.PathID(atomic.AddUint64(&pm.nextPathID, 1))
	if _, ok := pm.paths[nextPathID]; ok {
		return 0, protocol.NewPathNotExistsError(nextPathID)
	}
	// mark outbound dialed paths as client-side
	path := NewPath(nextPathID, conn, true, session)

	pm.paths[nextPathID] = path
	pm.logger.Debug("Created client path", "pathID", nextPathID, "address", address)
	return nextPathID, nil
}

func (pm *pathManagerImpl) AccpetPath(conn *quic.Conn, session Session) (protocol.PathID, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	nextPathID := protocol.PathID(atomic.AddUint64(&pm.nextPathID, 1))
	if _, ok := pm.paths[nextPathID]; ok {
		return 0, protocol.NewPathNotExistsError(nextPathID)
	}
	// mark accepted paths as server-side (isClient=false)
	path := NewPath(nextPathID, conn, false, session)
	pm.paths[nextPathID] = path
	pm.logger.Debug("Accepted server path", "pathID", nextPathID, "remoteAddr", conn.RemoteAddr().String())
	return nextPathID, nil
}

func (pm *pathManagerImpl) SetPrimaryPath(id protocol.PathID) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, ok := pm.paths[id]; !ok {
		pm.logger.Error("Cannot set primary path: path does not exist", "pathID", id)
		return
	}
	pm.primaryPathID = id
	pm.logger.Debug("Set primary path", "pathID", id)
}

func (pm *pathManagerImpl) GetPrimaryPath() Path {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if exists, ok := pm.paths[pm.primaryPathID]; ok {
		return exists
	}
	return nil
}

func (pm *pathManagerImpl) GetPath(id protocol.PathID) Path {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if exists, ok := pm.paths[id]; ok {
		return exists
	}
	return nil
}

func (pm *pathManagerImpl) RemovePath(id protocol.PathID) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.paths, id)
}

func (pm *pathManagerImpl) CloseAllPaths() {
	pm.mu.Lock()
	// Close each path and remove it from the map to avoid stale references.
	for id, path := range pm.paths {
		if path != nil {
			if err := path.Close(); err != nil {
				pm.logger.Debug("error closing path", "pathID", id, "err", err)
			}
		}
		delete(pm.paths, id)
	}
	pm.mu.Unlock()
}

func (pm *pathManagerImpl) AddRelay(address string) (Relay, error) {
	pm.mu.Lock()
	primaryPath := pm.paths[pm.primaryPathID]
	pm.mu.Unlock()

	if primaryPath == nil {
		return nil, protocol.NewPathNotExistsError(0)
	}

	// Create a channel to receive the response
	responseChannel := make(chan *protocol.AddPathRespPayload, 1)

	// Register a response handler in the path manager
	pm.registerAddPathRespHandler(responseChannel)

	// 1. Send AddPath frame on primary path
	payload := protocol.EncodeAddPathPayload(address)
	frame := &protocol.Frame{
		Type:     protocol.FrameTypeAddPath,
		StreamID: 0, // Control stream
		Seq:      0,
		Payload:  payload,
	}

	// Send the frame through the primary path
	if err := primaryPath.SendControlFrame(frame); err != nil {
		pm.unregisterAddPathRespHandler()
		return nil, fmt.Errorf("failed to send AddPath frame: %w", err)
	}

	pm.logger.Debug("Sent AddPath request", "address", address)

	// 2. Wait for response synchronously
	select {
	case response := <-responseChannel:
		// 3. Create a new relay with the received path ID
		relay := NewRelay(primaryPath, response.Address, response.PathID)

		// Store the relay in the map
		pm.mu.Lock()
		if pm.relays == nil {
			pm.relays = make(map[protocol.PathID]*relayImpl)
		}
		pm.relays[response.PathID] = relay
		pm.mu.Unlock()

		pm.logger.Debug("Created relay", "address", response.Address, "pathID", response.PathID)
		return relay, nil

	case <-time.After(10 * time.Second):
		pm.unregisterAddPathRespHandler()
		return nil, fmt.Errorf("timeout waiting for AddPathResp")
	}
}

// registerAddPathRespHandler registers a channel to receive AddPathResp responses
func (pm *pathManagerImpl) registerAddPathRespHandler(ch chan *protocol.AddPathRespPayload) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.addPathRespChannel = ch
}

// unregisterAddPathRespHandler removes the registered channel
func (pm *pathManagerImpl) unregisterAddPathRespHandler() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.addPathRespChannel = nil
}

// handleAddPathResp processes an AddPathResp frame and forwards it to the registered channel
func (pm *pathManagerImpl) handleAddPathResp(frame *protocol.Frame) {
	pm.mu.Lock()
	respChannel := pm.addPathRespChannel
	pm.mu.Unlock()

	if respChannel == nil {
		pm.logger.Warn("Received AddPathResp but no handler registered")
		return
	}

	// Decode the response
	resp, err := protocol.DecodeAddPathRespPayload(frame.Payload)
	if err != nil {
		pm.logger.Error("Failed to decode AddPathResp", "error", err)
		return
	}

	// Send to the waiting handler
	select {
	case respChannel <- resp:
		pm.logger.Debug("Forwarded AddPathResp", "address", resp.Address, "pathID", resp.PathID)
	default:
		pm.logger.Warn("AddPathResp handler channel is full")
	}
}
