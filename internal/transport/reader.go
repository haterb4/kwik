package transport

import (
	"bufio"
	"encoding/binary"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
)

// PathBufferPool - Pool de buffers per-path pour éviter les allocations
type PathBufferPool struct {
	packetPool *sync.Pool
	tmpPool    *sync.Pool
}

func NewPathBufferPool() *PathBufferPool {
	return &PathBufferPool{
		packetPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 4096)
				return &buf
			},
		},
		tmpPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 2048)
				return &buf
			},
		},
	}
}

func (pbp *PathBufferPool) GetPacketBuffer() *[]byte {
	return pbp.packetPool.Get().(*[]byte)
}

func (pbp *PathBufferPool) PutPacketBuffer(buf *[]byte) {
	*buf = (*buf)[:0]
	pbp.packetPool.Put(buf)
}

func (pbp *PathBufferPool) GetTmpBuffer() *[]byte {
	return pbp.tmpPool.Get().(*[]byte)
}

func (pbp *PathBufferPool) PutTmpBuffer(buf *[]byte) {
	pbp.tmpPool.Put(buf)
}

// runOptimizedTransportStreamReader - Version optimisée avec isolation multipath
func (p *path) runOptimizedTransportStreamReader(s *quic.Stream) {
	const (
		maxPacketSize = 1 * 1024 * 1024 // 1MB
		bufferSize    = 64 * 1024       // 64KB buffer initial
	)

	// Pool de buffers dédié à ce path pour isolation complète
	bufferPool := NewPathBufferPool()

	// Utiliser bufio.Reader pour un buffering plus efficace
	reader := bufio.NewReaderSize(s, bufferSize)

	// Buffer principal réutilisable
	readBuffer := make([]byte, 0, 8192)

	p.logger.Info("Starting optimized transport stream reader", "path", p.id)

	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("Transport stream reader panic", "path", p.id, "panic", r)
		}
		p.logger.Info("Exiting optimized transport stream reader", "path", p.id)
	}()

	for {
		// Vérification rapide si le path est fermé
		if atomic.LoadInt32(&p.closed) == 1 {
			return
		}

		// Lecture optimisée avec buffer pool
		tmpBuf := bufferPool.GetTmpBuffer()

		n, err := reader.Read(*tmpBuf)
		if err != nil {
			bufferPool.PutTmpBuffer(tmpBuf)
			if err.Error() != "EOF" {
				p.logger.Debug("Reader error", "path", p.id, "err", err)
				if p.handleReadError(err) {
					p.logger.Info("Reader stopping for stream recovery", "path", p.id)
				}
			}
			return
		}

		if n == 0 {
			bufferPool.PutTmpBuffer(tmpBuf)
			continue
		}

		// Append to read buffer
		readBuffer = append(readBuffer, (*tmpBuf)[:n]...)
		bufferPool.PutTmpBuffer(tmpBuf)

		// Process packets: [4 bytes length][packet data]
		for {
			if len(readBuffer) < 4 {
				break // Pas assez pour le préfixe de longueur
			}

			// Extraire la longueur du paquet
			packetLength := binary.BigEndian.Uint32(readBuffer[:4])

			if packetLength == 0 {
				readBuffer = readBuffer[4:]
				continue
			}

			if packetLength > maxPacketSize {
				p.logger.Warn("Packet too large, dropping", "path", p.id, "len", packetLength)
				return
			}

			if len(readBuffer) < int(4+packetLength) {
				break // Pas assez pour le paquet complet
			}

			// Extraire le paquet en utilisant le buffer pool
			packetBufPtr := bufferPool.GetPacketBuffer()
			packetBuf := *packetBufPtr

			// Assurer la capacité
			if cap(packetBuf) < int(packetLength) {
				packetBuf = make([]byte, packetLength)
			} else {
				packetBuf = packetBuf[:packetLength]
			}

			// Copier les données du paquet
			copy(packetBuf, readBuffer[4:4+packetLength])

			p.logger.Debug("Received packet", "path", p.id, "len", packetLength)

			// ISOLATION MULTIPATH: Traitement synchrone par path
			// Chaque path traite ses paquets dans son propre thread reader
			// Cela préserve l'ordre et l'isolation entre paths
			if p.session != nil {
				if mux := p.session.Multiplexer(); mux != nil {
					mux.PushPacket(p.id, packetBuf)
				}
			}

			// Remettre le buffer dans le pool
			bufferPool.PutPacketBuffer(packetBufPtr)

			// Avancer le buffer
			readBuffer = readBuffer[4+packetLength:]

			// Yield intelligent basé sur la taille du paquet
			if packetLength < 1024 {
				runtime.Gosched() // Petit paquet = yield plus fréquent
			}
		}

		// Yield périodique pour éviter de monopoliser le CPU
		runtime.Gosched()
	}
}
