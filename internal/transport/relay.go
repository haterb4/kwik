package transport

import (
	"fmt"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

type relayImpl struct {
	address string
	pathID  protocol.PathID
	active  bool
	path    Path
	logger  logger.Logger
}

func NewRelay(path Path, address string, pathID protocol.PathID) *relayImpl {
	return &relayImpl{
		address: address,
		pathID:  pathID,
		active:  true,
		path:    path,
		logger:  logger.NewLogger(logger.LogLevelSilent).WithComponent("RELAY"),
	}
}

func (r *relayImpl) Address() string {
	return r.address
}

func (r *relayImpl) PathID() protocol.PathID {
	return r.pathID
}

func (r *relayImpl) IsActive() bool {
	return r.active
}

func (r *relayImpl) SendRawData(data []byte, streamID protocol.StreamID) (int, error) {
	r.logger.Debug("Sending raw data via relay",
		"address", r.address,
		"pathID", r.pathID,
		"streamID", streamID,
		"dataLength", len(data))
	// Vérifier que le relay est actif
	if !r.active {
		return 0, fmt.Errorf("relay is not active")
	}

	// Vérifier que le path primaire est disponible
	if r.path == nil {
		return 0, fmt.Errorf("relay path is not available")
	}

	// Vérifier que le path est prêt
	if !r.path.IsSessionReady() {
		r.logger.Warn("Relay path session is not ready, but will try to send anyway",
			"address", r.address,
			"pathID", r.pathID)
	}

	// Create RelayDataFrame using the new fields
	frame := &protocol.RelayDataFrame{
		RelayPathID:  r.pathID,
		DestStreamID: streamID,
		RawData:      data,
	}

	// Send the frame via the primary path control stream using internal helper
	var err error
	maxRetries := 3
	for i := 0; i < maxRetries; i++ {
		// Utiliser le packer pour toutes les trames
		var packer interface {
			SubmitFrame(Path, protocol.Frame) error
		}
		if si, ok := r.path.(interface{ sessionInternal() sessionInternal }); ok {
			packer = si.sessionInternal().Packer()
		} else if pp, ok := r.path.(*path); ok {
			if si, ok := pp.session.(sessionInternal); ok {
				packer = si.Packer()
			}
		}
		if packer != nil {
			err = packer.SubmitFrame(r.path, frame)
		} else {
			err = fmt.Errorf("no packer available for relay path")
		}
		if err == nil {
			break
		}
		r.logger.Warn("Failed to send relay data, retrying",
			"attempt", i+1,
			"error", err)
	}

	if err != nil {
		r.logger.Error("Failed to send relay data after retries",
			"error", err)
		return 0, fmt.Errorf("failed to send relay data: %w", err)
	}

	// Retourner la taille des données envoyées
	return len(data), nil
}
