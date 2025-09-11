package kwik

import (
	"io"

	"github.com/s-anzie/kwik/internal/protocol"
)

// StreamAggregator removed — compatibility stub. The real implementation was
// replaced by RecvStream/SendStream. This stub provides minimal methods so
// remaining callsites (if any) still compile.

type StreamAggregator struct{}

func NewStreamAggregator(streamID protocol.StreamID, _ interface{}) *StreamAggregator {
	return &StreamAggregator{}
}
func (sa *StreamAggregator) NotifyDataAvailable(streamID protocol.StreamID) {}
func (sa *StreamAggregator) SetRecvStream(r *RecvStream)                    {}
func (sa *StreamAggregator) Read(p []byte) (int, error)                     { return 0, io.EOF }
func (sa *StreamAggregator) GetReadOffset() uint64                          { return 0 }
func (sa *StreamAggregator) SetReadOffset(offset uint64)                    {}
func (sa *StreamAggregator) GetCurrentReadOffset() uint64                   { return 0 }
func (sa *StreamAggregator) GetStats() interface{}                          { return nil }
func (sa *StreamAggregator) HasPendingData() bool                           { return false }
func (sa *StreamAggregator) Close()                                         {}
func (sa *StreamAggregator) GetExpectedNextOffset() uint64                  { return 0 }
