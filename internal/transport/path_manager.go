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
		paths:      make(map[protocol.PathID]*path),
		nextPathID: 0,
		tlsCfg:     tls,
		config:     cfg,
		logger:     logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH_MANAGER"),
	}
}
func NewServerPathManager() *pathManagerImpl {
	return &pathManagerImpl{
		paths:      make(map[protocol.PathID]*path),
		nextPathID: 0,
		logger:     logger.NewLogger(logger.LogLevelDebug).WithComponent("PATH_MANAGER"),
	}
}

// CreatePath establishes a new QUIC connection to the specified address and assigns it a unique PathID.
// It returns the newly created PathID or an error if the connection could not be established or if the PathID already exists.
// The function is thread-safe and ensures that each PathID is unique by using atomic operations.
// If a path with the generated PathID already exists, it returns a PathNotExistsError.
func (pm *pathManagerImpl) OpenPath(ctx context.Context, address string) (protocol.PathID, error) {
	conn, err := quic.DialAddr(ctx, address, pm.tlsCfg, pm.config)
	if err != nil {
		return 0, err
	}
	nextPathID := protocol.PathID(atomic.AddUint64(&pm.nextPathID, 1))
	if _, ok := pm.paths[nextPathID]; ok {
		return 0, protocol.NewPathNotExistsError(nextPathID)
	}
	path := NewPath(nextPathID, conn)
	// mark outbound dialed paths as client-side
	path.isClient = true
	pm.paths[nextPathID] = path
	pm.nextPathID = uint64(nextPathID)
	return nextPathID, nil
}

func (pm *pathManagerImpl) AccpetPath(conn *quic.Conn) (protocol.PathID, error) {
	nextPathID := protocol.PathID(atomic.AddUint64(&pm.nextPathID, 1))
	if _, ok := pm.paths[nextPathID]; ok {
		return 0, protocol.NewPathNotExistsError(nextPathID)
	}
	path := NewPath(nextPathID, conn)
	// mark accepted paths as server-side (isClient=false)
	path.isClient = false
	pm.paths[nextPathID] = path
	pm.nextPathID = uint64(nextPathID)
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
