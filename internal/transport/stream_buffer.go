package transport

import "sync"

// StreamBuffer stores fragmented data by offset and allows reading contiguous data from ReadOffset.
type StreamBuffer struct {
	mu         sync.Mutex
	Data       map[uint64][]byte // offset -> data chunk
	ReadOffset uint64
}

func NewStreamBuffer() *StreamBuffer {
	return &StreamBuffer{Data: make(map[uint64][]byte), ReadOffset: 0}
}

// Insert stores data at the specified offset (overwrites if present).
func (sb *StreamBuffer) Insert(offset uint64, data []byte) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	// store a copy
	cp := make([]byte, len(data))
	copy(cp, data)
	sb.Data[offset] = cp
}

// Read returns contiguous data starting at sb.ReadOffset and advances ReadOffset.
// It stops when a gap is encountered.
func (sb *StreamBuffer) Read() []byte {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	result := make([]byte, 0)
	for {
		chunk, ok := sb.Data[sb.ReadOffset]
		if !ok {
			break
		}
		result = append(result, chunk...)
		// advance and delete the consumed chunk
		sb.ReadOffset += uint64(len(chunk))
		delete(sb.Data, sb.ReadOffset-uint64(len(chunk)))
	}
	return result
}
