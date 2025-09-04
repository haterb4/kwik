package transport

import (
	"context"
	"crypto/tls"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik/internal/config"
	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type pathManagerImpl struct {
	paths         map[protocol.PathID]*path
	nextPathID    uint64
	tlsCfg        *tls.Config
	config        *config.Config
	primaryPahtID protocol.PathID
	mu            sync.Mutex
	logger        logger.Logger
}

func NewClientPathManager(tls *tls.Config, cfg *config.Config) *pathManagerImpl {
	return &pathManagerImpl{
		paths:         make(map[protocol.PathID]*path),
		nextPathID:    0,
		tlsCfg:        tls,
		config:        cfg,
		primaryPahtID: 0, // Initialize to 0 (invalid)
		logger:        logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH_MANAGER"),
	}
}
func NewServerPathManager() *pathManagerImpl {
	return &pathManagerImpl{
		paths:         make(map[protocol.PathID]*path),
		nextPathID:    0,
		primaryPahtID: 0, // Initialize to 0 (invalid)
		logger:        logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH_MANAGER"),
	}
}

// OpenPath establishes a new QUIC connection to the specified address and assigns it a unique PathID.
// It returns the newly created PathID or an error if the connection could not be established or if the PathID already exists.
// The function is thread-safe and ensures that each PathID is unique by using atomic operations.
// If a path with the generated PathID already exists, it returns a PathNotExistsError.
func (pm *pathManagerImpl) OpenPath(ctx context.Context, address string) (protocol.PathID, error) {
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
	path := NewPath(nextPathID, conn, true)

	pm.paths[nextPathID] = path
	pm.logger.Debug("Created client path", "pathID", nextPathID, "address", address)
	return nextPathID, nil
}

func (pm *pathManagerImpl) AccpetPath(conn *quic.Conn) (protocol.PathID, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	nextPathID := protocol.PathID(atomic.AddUint64(&pm.nextPathID, 1))
	if _, ok := pm.paths[nextPathID]; ok {
		return 0, protocol.NewPathNotExistsError(nextPathID)
	}
	// mark accepted paths as server-side (isClient=false)
	path := NewPath(nextPathID, conn, false)
	pm.paths[nextPathID] = path
	pm.logger.Debug("Accepted server path", "pathID", nextPathID, "remoteAddr", conn.RemoteAddr().String())
	return nextPathID, nil
}

func (pm *pathManagerImpl) SetPrimaryPath(id protocol.PathID) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, ok := pm.paths[id]; !ok {
		panic(protocol.NewPathNotExistsError(id))
	}
	pm.primaryPahtID = id
}

func (pm *pathManagerImpl) GetPrimaryPath() Path {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if exists, ok := pm.paths[pm.primaryPahtID]; ok {
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
