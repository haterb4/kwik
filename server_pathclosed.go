package kwik

import (
	"github.com/s-anzie/kwik/internal/protocol"
)

// NotifyPathClosed informs all streams that a path has been closed
// This method is called by path.Close() to notify the server session
// that a path is being closed, so the streams using that path can be updated.
func (s *ServerSession) NotifyPathClosed(pathID protocol.PathID, streamIDs []protocol.StreamID) {
	s.logger.Debug("Notifying streams that path was closed", "pathID", pathID, "streamCount", len(streamIDs))

	// Exclude control stream (ID 0) from notifications
	var appStreamIDs []protocol.StreamID
	for _, sid := range streamIDs {
		if sid != 0 { // Skip control stream
			appStreamIDs = append(appStreamIDs, sid)
		}
	}

	if len(appStreamIDs) == 0 {
		s.logger.Debug("No application streams to notify about path closure", "pathID", pathID)
		return
	}

	for _, sid := range appStreamIDs {
		// Get the stream and remove the path
		stream, exists := s.streamMgr.GetStream(sid)
		if !exists {
			s.logger.Debug("Stream not found when notifying of path closure", "streamID", sid, "pathID", pathID)
			continue
		}

		// Type assertion to StreamImpl to call RemovePath
		impl, ok := stream.(*StreamImpl)
		if !ok {
			s.logger.Error("Stream is not a StreamImpl, can't remove path", "streamID", sid, "pathID", pathID)
			continue
		}

		// Remove the path from the stream
		impl.RemovePath(pathID)
		s.logger.Debug("Notified stream of path closure", "streamID", sid, "pathID", pathID)
	}
}
