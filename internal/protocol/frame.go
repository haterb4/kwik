package protocol

import (
	"bytes"
	"encoding/binary"
	"io"
)

// FrameType indicates the purpose of a frame inside a packet.
type FrameType uint8

const (
	FrameTypeData FrameType = 0x1
	FrameTypeAck  FrameType = 0x2
	FrameTypeNack FrameType = 0x3
	// Control frames
	FrameTypeHandshake     FrameType = 0x4
	FrameTypeHandshakeResp FrameType = 0x5
	FrameTypePing          FrameType = 0x6
	FrameTypePong          FrameType = 0x7
	// future frame types can be added here
)

// Frame is the unit of logical data delivered to a destination StreamID.
type Frame struct {
	Type     FrameType
	StreamID StreamID
	Seq      uint64 // sequence number for ordering within a stream
	Payload  []byte
}

// Packet carries zero or more full Frames. Packets are atomic on the wire:
// we prefix each packet with a 4-byte length (big-endian) and then the packet body.
// Inside the packet we serialize frames one after another.

// encodeFrame writes a frame into w in the following format:
// 1 byte  - frame type
// 8 bytes - streamID (uint64)
// 8 bytes - seq (uint64)
// 4 bytes - payload length (uint32)
// N bytes - payload
func EncodeFrame(w io.Writer, f *Frame) error {
	if err := binary.Write(w, binary.BigEndian, uint8(f.Type)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint64(f.StreamID)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, f.Seq); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(f.Payload))); err != nil {
		return err
	}
	if _, err := w.Write(f.Payload); err != nil {
		return err
	}
	return nil
}

// DecodeFrame reads one frame from r. Caller must ensure stream contains a full frame.
func DecodeFrame(r io.Reader) (*Frame, error) {
	var ft uint8
	if err := binary.Read(r, binary.BigEndian, &ft); err != nil {
		return nil, err
	}
	var sid uint64
	if err := binary.Read(r, binary.BigEndian, &sid); err != nil {
		return nil, err
	}
	var seq uint64
	if err := binary.Read(r, binary.BigEndian, &seq); err != nil {
		return nil, err
	}
	var l uint32
	if err := binary.Read(r, binary.BigEndian, &l); err != nil {
		return nil, err
	}
	payload := make([]byte, l)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return &Frame{
		Type:     FrameType(ft),
		StreamID: StreamID(sid),
		Seq:      seq,
		Payload:  payload,
	}, nil
}

// AckRange represents an inclusive range starting at Start for Count packets.
type AckRange struct {
	Start uint64
	Count uint32
}

// EncodeAckPayload encodes ack ranges into payload bytes: 4-byte count, then each range: 8-byte start, 4-byte count
func EncodeAckPayload(ranges []AckRange) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(len(ranges)))
	for _, r := range ranges {
		binary.Write(&buf, binary.BigEndian, r.Start)
		binary.Write(&buf, binary.BigEndian, r.Count)
	}
	return buf.Bytes()
}

// DecodeAckPayload decodes ack ranges from payload bytes.
func DecodeAckPayload(b []byte) ([]AckRange, error) {
	buf := bytes.NewReader(b)
	var cnt uint32
	if err := binary.Read(buf, binary.BigEndian, &cnt); err != nil {
		return nil, err
	}
	out := make([]AckRange, 0, cnt)
	for i := uint32(0); i < cnt; i++ {
		var start uint64
		var count uint32
		if err := binary.Read(buf, binary.BigEndian, &start); err != nil {
			return nil, err
		}
		if err := binary.Read(buf, binary.BigEndian, &count); err != nil {
			return nil, err
		}
		out = append(out, AckRange{Start: start, Count: count})
	}
	return out, nil
}
