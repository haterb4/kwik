package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// encode VarInt simplifié (toujours 8 octets big-endian pour l’instant)
func writeVarInt(w io.Writer, v uint64) error {
	// Simple 8-byte big-endian encoding for reliability
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	_, err := w.Write(buf[:])
	return err
}

func readVarInt(r io.Reader) (uint64, error) {
	// Simple 8-byte big-endian decoding for reliability
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(buf[:]), nil
}

// FrameType indicates the purpose of a frame inside a packet.
type Frame interface {
	Type() FrameType
	// String provides a human-readable representation of the frame for logging/debugging.
	String() string
	Serialize() ([]byte, error)
}

type FrameType uint8

const (
	FrameTypeStream FrameType = 0x1
	FrameTypeAck    FrameType = 0x2
	FrameTypeNack   FrameType = 0x3
	// Control frames
	FrameTypePing      FrameType = 0x5
	// Path management frames
	FrameTypeAddPath     FrameType = 0x6
	FrameTypeAddPathResp FrameType = 0x7
	// Relay data frame
	FrameTypeRelayData FrameType = 0x8
	// Logical stream open frame
	FrameTypeOpenStream FrameType = 0x9
	// Logical stream open response frame
	FrameTypeOpenStreamResp FrameType = 0xA
	// future frame types can be added here
)

// OpenStreamRespFrame accuse réception de l'ouverture d'un flux logique (StreamID)
type OpenStreamRespFrame struct {
	StreamID uint64
	Success  bool
}

func (f *OpenStreamRespFrame) Type() FrameType { return FrameTypeOpenStreamResp }
func (f *OpenStreamRespFrame) String() string {
	return fmt.Sprintf("OpenStreamRespFrame{StreamID: %d, Success: %t}", f.StreamID, f.Success)
}
func (f *OpenStreamRespFrame) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := buf.WriteByte(byte(f.Type())); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, f.StreamID); err != nil {
		return nil, err
	}
	var okByte byte
	if f.Success {
		okByte = 1
	} else {
		okByte = 0
	}
	if err := buf.WriteByte(okByte); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseOpenStreamRespFrame(r *bytes.Reader) (*OpenStreamRespFrame, error) {
	streamID, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	okByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	return &OpenStreamRespFrame{StreamID: streamID, Success: okByte == 1}, nil
}

// OpenStreamFrame signale l'ouverture d'un flux logique (StreamID)
type OpenStreamFrame struct {
	StreamID uint64
}

func (f *OpenStreamFrame) Type() FrameType { return FrameTypeOpenStream }
func (f *OpenStreamFrame) String() string {
	return fmt.Sprintf("OpenStreamFrame{StreamID: %d}", f.StreamID)
}
func (f *OpenStreamFrame) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := buf.WriteByte(byte(f.Type())); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, f.StreamID); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseOpenStreamFrame(r *bytes.Reader) (*OpenStreamFrame, error) {
	streamID, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	return &OpenStreamFrame{StreamID: streamID}, nil
}

// StreamFrame represents a QUIC STREAM frame in the new formalism.
type StreamFrame struct {
	StreamID uint64
	Offset   uint64
	Fin      bool
	Data     []byte
}

func (f *StreamFrame) Type() FrameType { return FrameTypeStream }
func (f *StreamFrame) String() string {
	return fmt.Sprintf("StreamFrame{StreamID: %d, Offset: %d, Fin: %t, DataLen: %d}", f.StreamID, f.Offset, f.Fin, len(f.Data))
}
func (f *StreamFrame) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Frame type
	if err := buf.WriteByte(byte(f.Type())); err != nil {
		return nil, err
	}

	if err := writeVarInt(buf, f.StreamID); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, f.Offset); err != nil {
		return nil, err
	}

	// Fin flag
	if f.Fin {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}

	// Data
	if err := writeVarInt(buf, uint64(len(f.Data))); err != nil {
		return nil, err
	}
	if _, err := buf.Write(f.Data); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func parseStreamFrame(r *bytes.Reader) (*StreamFrame, error) {
	streamID, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	offset, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	finByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	fin := finByte == 1

	dataLen, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, dataLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}

	return &StreamFrame{
		StreamID: streamID,
		Offset:   offset,
		Fin:      fin,
		Data:     payload,
	}, nil
}

// Control frame types in the new model

type PingFrame struct{}

func (f *PingFrame) Type() FrameType { return FrameTypePing }
func (f *PingFrame) String() string {
	return "PingFrame{}"
}
func (f *PingFrame) Serialize() ([]byte, error) {
	return []byte{byte(f.Type())}, nil
}

type AddPathFrame struct {
	Address string
}

func (f *AddPathFrame) Type() FrameType { return FrameTypeAddPath }
func (f *AddPathFrame) String() string {
	return fmt.Sprintf("AddPathFrame{Address: %s}", f.Address)
}
func (f *AddPathFrame) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := buf.WriteByte(byte(f.Type())); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, uint64(len(f.Address))); err != nil {
		return nil, err
	}
	if _, err := buf.WriteString(f.Address); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseAddPathFrame(r *bytes.Reader) (*AddPathFrame, error) {
	addrLen, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	addrBytes := make([]byte, addrLen)
	if _, err := io.ReadFull(r, addrBytes); err != nil {
		return nil, err
	}
	return &AddPathFrame{
		Address: string(addrBytes),
	}, nil
}

type AddPathRespFrame struct {
	Address string
	PathID  PathID
}

func (f *AddPathRespFrame) Type() FrameType { return FrameTypeAddPathResp }
func (f *AddPathRespFrame) String() string {
	return fmt.Sprintf("AddPathRespFrame{PathID: %d, Address: %s}", f.PathID, f.Address)
}
func (f *AddPathRespFrame) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := buf.WriteByte(byte(f.Type())); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, uint64(f.PathID)); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, uint64(len(f.Address))); err != nil {
		return nil, err
	}
	if _, err := buf.WriteString(f.Address); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
func parseAddPathRespFrame(r *bytes.Reader) (*AddPathRespFrame, error) {
	pathID, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	addrLen, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	addrBytes := make([]byte, addrLen)
	if _, err := io.ReadFull(r, addrBytes); err != nil {
		return nil, err
	}
	return &AddPathRespFrame{
		PathID:  PathID(pathID),
		Address: string(addrBytes),
	}, nil
}

// RelayDataFrame represents a relay data frame in the new formalism.
type RelayDataFrame struct {
	RelayPathID  PathID
	DestStreamID StreamID
	RawData      []byte
}

func (f *RelayDataFrame) Type() FrameType { return FrameTypeRelayData }
func (f *RelayDataFrame) String() string {
	return fmt.Sprintf("RelayDataFrame{RelayPathID: %d, DestStreamID: %d, RawDataLen: %d}", f.RelayPathID, f.DestStreamID, len(f.RawData))
}
func (f *RelayDataFrame) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := buf.WriteByte(byte(f.Type())); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, uint64(f.RelayPathID)); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, uint64(f.DestStreamID)); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, uint64(len(f.RawData))); err != nil {
		return nil, err
	}
	if _, err := buf.Write(f.RawData); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
func parseRelayDataFrame(r *bytes.Reader) (*RelayDataFrame, error) {
	pathID, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	streamID, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	dataLen, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return &RelayDataFrame{
		RelayPathID:  PathID(pathID),
		DestStreamID: StreamID(streamID),
		RawData:      data,
	}, nil
}

// NewAckRange for new ACK model (to avoid clash with legacy AckRange)
type AckRange struct {
	Smallest uint64
	Largest  uint64
}

// AckFrame represents a QUIC-like ACK frame in the new formalism.
type AckFrame struct {
	LargestAcked uint64
	AckDelay     uint64
	AckRanges    []AckRange
}

func (f *AckFrame) Type() FrameType { return FrameTypeAck }
func (f *AckFrame) String() string {
	return fmt.Sprintf("AckFrame{LargestAcked: %d, AckDelay: %d, AckRanges: %v}", f.LargestAcked, f.AckDelay, f.AckRanges)
}
func (f *AckFrame) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Frame type
	if err := buf.WriteByte(byte(f.Type())); err != nil {
		return nil, err
	}

	if err := writeVarInt(buf, f.LargestAcked); err != nil {
		return nil, err
	}
	if err := writeVarInt(buf, f.AckDelay); err != nil {
		return nil, err
	}

	// Ranges
	if err := writeVarInt(buf, uint64(len(f.AckRanges))); err != nil {
		return nil, err
	}
	for _, r := range f.AckRanges {
		if err := writeVarInt(buf, r.Smallest); err != nil {
			return nil, err
		}
		if err := writeVarInt(buf, r.Largest); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func parseAckFrame(r *bytes.Reader) (*AckFrame, error) {
	largest, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	delay, err := readVarInt(r)
	if err != nil {
		return nil, err
	}

	count, err := readVarInt(r)
	if err != nil {
		return nil, err
	}

	ranges := make([]AckRange, 0, count)
	for i := uint64(0); i < count; i++ {
		small, err := readVarInt(r)
		if err != nil {
			return nil, err
		}
		large, err := readVarInt(r)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, AckRange{Smallest: small, Largest: large})
	}

	return &AckFrame{
		LargestAcked: largest,
		AckDelay:     delay,
		AckRanges:    ranges,
	}, nil
}

func ParseFrame(data []byte) (Frame, error) {
	r := bytes.NewReader(data)

	// Lire le type
	t, err := r.ReadByte()
	if err != nil {
		return nil, err
	}

	switch FrameType(t) {
	case FrameTypeStream:
		return parseStreamFrame(r)
	case FrameTypeAck:
		return parseAckFrame(r)
	case FrameTypePing:
		// un Ping n'a pas de contenu
		return &PingFrame{}, nil
	case FrameTypeAddPath:
		return parseAddPathFrame(r)
	case FrameTypeAddPathResp:
		return parseAddPathRespFrame(r)
	case FrameTypeRelayData:
		return parseRelayDataFrame(r)
	case FrameTypeOpenStream:
		return parseOpenStreamFrame(r)
	case FrameTypeOpenStreamResp:
		return parseOpenStreamRespFrame(r)
	default:
		return nil, fmt.Errorf("unknown frame type: %d", t)
	}
}
