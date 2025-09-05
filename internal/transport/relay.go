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
		logger:  logger.NewLogger(logger.LogLevelDebug).WithComponent("RELAY"),
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

	// Créer la charge utile pour la trame RelayData
	payload := protocol.EncodeRelayDataPayload(r.pathID, streamID, data)

	// Créer la trame avec le type RelayData
	frame := &protocol.Frame{
		Type:     protocol.FrameTypeRelayData,
		StreamID: 0, // On utilise le stream de contrôle (0)
		Seq:      0, // La séquence sera gérée par le packer
		Payload:  payload,
	}

	// Envoyer la trame via le chemin primaire
	if err := r.path.SendControlFrame(frame); err != nil {
		return 0, fmt.Errorf("failed to send relay data: %w", err)
	}

	// Retourner la taille des données envoyées
	return len(data), nil
}
