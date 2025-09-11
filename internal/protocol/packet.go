package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Packet carries zero or more full Frames. Packets are atomic on the wire:
// we prefix each packet with a 4-byte length (big-endian) and then the packet body.
// Inside the packet we serialize frames one after another.

// HeaderType represents simplified QUIC header categories (long vs short)
type HeaderType uint8

const (
	HeaderTypeLong HeaderType = iota
	HeaderTypeShort
)

// Header is a simplified representation of a QUIC packet header.
type Header struct {
	Type      HeaderType
	PacketNum uint64
}

// Packet is a logical representation of a QUIC packet: header + payload frames.
type Packet struct {
	Header  Header
	Payload []Frame // list of new-style frames
}

func (h *Header) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Type
	if err := buf.WriteByte(byte(h.Type)); err != nil {
		return nil, err
	}
	// PacketNum
	if err := writeVarInt(buf, h.PacketNum); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func DeserializeHeader(r *bytes.Reader) (*Header, error) {
	ht, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	packetNum, err := readVarInt(r)
	if err != nil {
		return nil, err
	}

	return &Header{
		Type:      HeaderType(ht),
		PacketNum: packetNum,
	}, nil
}
func (p *Packet) Serialize() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Header
	hdrBytes, err := p.Header.Serialize()
	if err != nil {
		return nil, err
	}
	if _, err := buf.Write(hdrBytes); err != nil {
		return nil, err
	}
	// Nombre de frames
	if err := writeVarInt(buf, uint64(len(p.Payload))); err != nil {
		return nil, err
	}
	// Frames
	// Chaque frame
	for _, f := range p.Payload {
		if f.Type() != FrameTypeStream {
			// fmt.Printf("TRACK Packet.Serialize frame: type=%T content=%v\n", f.Type(), f)
		}
		raw, err := f.Serialize()
		if err != nil {
			return nil, err
		}
		if _, err := buf.Write(raw); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func DeserializePacket(data []byte) (*Packet, error) {
	// Debug: afficher les premiers bytes du paquet
	// fmt.Printf("TRACK DeserializePacket: data len=%d, first_bytes=% x\n", len(data), data[:min(16, len(data))])

	// Vérifier si le paquet commence par un préfixe de longueur (4 bytes)
	// Si les 4 premiers bytes correspondent à la longueur des bytes restants, les enlever
	if len(data) >= 4 {
		prefixLen := binary.BigEndian.Uint32(data[:4])
		if int(prefixLen) == len(data)-4 {
			// fmt.Printf("TRACK DeserializePacket: Detected length prefix %d, removing it\n", prefixLen)
			data = data[4:]
		}
	}

	r := bytes.NewReader(data)
	// fmt.Printf("TRACK DeserializePacket: After prefix check, data len=%d, first_bytes=% x\n", len(data), data[:min(16, len(data))]) // Header
	// fmt.Printf("TRACK DeserializePacket: About to read header from position %d\n", len(data)-r.Len())
	hdr, err := DeserializeHeader(r)
	if err != nil {
		// fmt.Printf("TRACK DeserializePacket: Header error: %v\n", err)
		return nil, err
	}
	// fmt.Printf("TRACK DeserializePacket: Header parsed successfully, now at position %d\n", len(data)-r.Len())

	// Nombre de frames
	// fmt.Printf("TRACK DeserializePacket: About to read frame count from position %d\n", len(data)-r.Len())
	count, err := readVarInt(r)
	if err != nil {
		// fmt.Printf("TRACK DeserializePacket: readVarInt error: %v\n", err)
		return nil, err
	}
	// fmt.Printf("TRACK DeserializePacket: Frame count read: %d, now at position %d\n", count, len(data)-r.Len())

	// Protection contre les allocations mémoire énormes
	if count > 10000 { // Limite raisonnable de frames par paquet
		return nil, fmt.Errorf("packet contains unreasonable number of frames: %d", count)
	}

	payload := make([]Frame, 0, count)
	for i := uint64(0); i < count; i++ {
		// Chaque frame commence par son type
		frameStart := r.Len()
		_, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		// On remet le curseur en arrière (car ParseFrame relira le type)
		r.UnreadByte()
		consumed := len(data) - r.Len()

		// Parse
		remaining := data[consumed:]
		frame, err := ParseFrame(remaining)
		if err != nil {
			return nil, err
		}
		payload = append(payload, frame)
		// fmt.Printf("TRACK DeserializePacket frame: %v\n", frame.String())
		// Avancer le Reader en fonction de ce qui a été lu
		after := r.Len()
		skipped := frameStart - after
		if skipped > 0 {
			r.Seek(int64(skipped), io.SeekCurrent)
		}
	}

	return &Packet{
		Header:  *hdr,
		Payload: payload,
	}, nil
}
