package kwik

import (
	"container/heap"
	"io"
	"sync"

	"github.com/s-anzie/kwik/internal/logger"
	"github.com/s-anzie/kwik/internal/protocol"
)

// FrameItem est une structure interne pour notre tas (heap).
// Elle contient une trame et les métadonnées nécessaires pour le tri.
type FrameItem struct {
	Frame    *protocol.Frame
	Priority int64 // L'offset global est utilisé comme priorité.
	Index    int   // Requis par l'interface container/heap.
}

// FrameHeap est un min-heap de FrameItem, trié par l'Offset (Priority).
type FrameHeap []*FrameItem

func (h FrameHeap) Len() int           { return len(h) }
func (h FrameHeap) Less(i, j int) bool { return h[i].Priority < h[j].Priority }
func (h FrameHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].Index = i
	h[j].Index = j
}

func (h *FrameHeap) Push(x interface{}) {
	n := len(*h)
	item := x.(*FrameItem)
	item.Index = n
	*h = append(*h, item)
}

func (h *FrameHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // Pour aider le garbage collector.
	item.Index = -1 // Marquer comme invalide.
	*h = old[0 : n-1]
	return item
}

// StreamAggregator réassemble un flux de données ordonné à partir de trames
// potentiellement désordonnées provenant de plusieurs chemins.
type StreamAggregator struct {
	streamID protocol.StreamID
	logger   logger.Logger

	mu             sync.Mutex // Protège tous les champs ci-dessous.
	cond           *sync.Cond // Pour attendre/signaler l'arrivée de données.
	frameHeap      *FrameHeap // Stocke les trames futures (out-of-order).
	nextReadOffset int64      // Le "marqueur" de lecture : quel est le prochain octet que l'application attend.
	buffer         []byte     // Stocke les restes d'une trame si le buffer de lecture de l'application était trop petit.
	isClosed       bool       // Indique si le stream a été fermé.
}

// NewStreamAggregator crée et initialise un nouvel agrégateur.
func NewStreamAggregator(streamID protocol.StreamID) *StreamAggregator {
	h := make(FrameHeap, 0)
	heap.Init(&h)

	agg := &StreamAggregator{
		streamID:       streamID,
		logger:         logger.NewLogger(logger.LogLevelDebug).WithComponent("STREAM_AGGREGATOR"),
		frameHeap:      &h,
		nextReadOffset: 0,
		buffer:         make([]byte, 0),
		isClosed:       false,
	}
	agg.cond = sync.NewCond(&agg.mu)
	return agg
}

// PushFrame est appelée par le Multiplexer pour chaque trame reçue.
// C'est le point d'entrée des données dans l'agrégateur.
func (sa *StreamAggregator) PushFrame(frame *protocol.Frame) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	if sa.isClosed {
		return // Ignorer les trames qui arrivent après la fermeture.
	}

	// Nous ne gardons que les trames futures.
	if frame.Offset+int64(len(frame.Payload)) <= sa.nextReadOffset {
		sa.logger.Debug("Dropping old/duplicate frame", "streamID", sa.streamID, "offset", frame.Offset)
		return
	}

	item := &FrameItem{
		Frame:    frame,
		Priority: frame.Offset,
	}
	heap.Push(sa.frameHeap, item)

	sa.logger.Debug("Pushed frame to aggregator heap",
		"streamID", sa.streamID, "offset", frame.Offset, "size", len(frame.Payload), "heapSize", sa.frameHeap.Len())

	// Réveiller une goroutine qui pourrait être en attente dans Read().
	sa.cond.Signal()
}

// Read implémente l'interface io.Reader.
// C'est le point de sortie des données, fournissant un flux ordonné à l'application.
func (sa *StreamAggregator) Read(p []byte) (int, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	// Boucle d'attente. On attend si toutes les conditions suivantes sont vraies :
	// 1. Le stream n'est pas fermé.
	// 2. Notre buffer de "restes" est vide.
	// 3. Le tas est vide OU la trame en haut du tas n'est pas celle qu'on attend.
	for !sa.isClosed && len(sa.buffer) == 0 && (sa.frameHeap.Len() == 0 || (*sa.frameHeap)[0].Priority != sa.nextReadOffset) {
		sa.logger.Debug("Aggregator is waiting for data", "streamID", sa.streamID, "expectedOffset", sa.nextReadOffset)
		sa.cond.Wait() // Attend un signal de PushFrame ou Close.
	}

	if sa.isClosed && len(sa.buffer) == 0 && sa.frameHeap.Len() == 0 {
		return 0, io.EOF
	}

	totalBytesRead := 0

	// 1. Vider notre buffer interne en premier.
	if len(sa.buffer) > 0 {
		n := copy(p, sa.buffer)
		sa.buffer = sa.buffer[n:]
		totalBytesRead += n
	}

	// 2. Lire depuis le tas tant qu'on a de la place et que les trames sont contiguës.
	for totalBytesRead < len(p) && sa.frameHeap.Len() > 0 && (*sa.frameHeap)[0].Priority == sa.nextReadOffset {
		item := heap.Pop(sa.frameHeap).(*FrameItem)
		frame := item.Frame

		payload := frame.Payload
		spaceLeftInP := len(p) - totalBytesRead
		bytesToCopy := len(payload)

		if bytesToCopy > spaceLeftInP {
			// La trame ne rentre pas entièrement, on en copie une partie...
			bytesToCopy = spaceLeftInP
			// ...et on stocke le reste dans notre buffer.
			sa.buffer = append(sa.buffer, payload[bytesToCopy:]...)
		}

		copy(p[totalBytesRead:], payload[:bytesToCopy])

		totalBytesRead += bytesToCopy
		sa.nextReadOffset += int64(len(payload)) // L'offset avance de la taille totale de la trame.
	}

	return totalBytesRead, nil
}

// Close signale à l'agrégateur que plus aucune trame n'arrivera.
func (sa *StreamAggregator) Close() {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	sa.isClosed = true
	// Réveiller toutes les goroutines bloquées dans Read() pour qu'elles puissent retourner io.EOF.
	sa.cond.Broadcast()
}
