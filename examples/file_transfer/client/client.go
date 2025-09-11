package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"

	"github.com/s-anzie/kwik"
)

// FileMetadata contient le plan de transfert reçu du serveur.
type FileMetadata struct {
	FileName             string
	FileSize             int64
	ChunkSize            int
	WindowSizeChunks     int
	TotalWindows         int
	LastWindowSizeChunks int
}

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("Usage: %s <file_name_on_server>", os.Args[0])
	}
	fileName := os.Args[1]

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-file-transfer"},
	}

	session, err := kwik.DialAddr(context.Background(), "localhost:4433", tlsConf, nil)
	if err != nil {
		log.Fatalf("Failed to dial server: %v", err)
	}
	defer session.CloseWithError(0, "Connection closed by client")

	// 1. Ouvrir UN SEUL flux logique pour l'ensemble du transfert.
	// Kwik se chargera d'utiliser plusieurs chemins physiques (vers le serveur et le relais)
	// pour alimenter ce flux unique.
	stream, err := session.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatalf("Failed to open stream: %v", err)
	}

	log.Println("Requesting file:", fileName)
	_, err = stream.Write([]byte(fileName))
	if err != nil {
		log.Fatalf("Failed to send file request: %v", err)
	}

	// Lire les métadonnées
	buf := make([]byte, 4096)
	n, err := stream.Read(buf)
	if err != nil {
		log.Fatalf("Failed to read metadata: %v", err)
	}

	var metadata FileMetadata
	err = json.Unmarshal(buf[:n], &metadata)
	if err != nil {
		log.Fatalf("Failed to unmarshal metadata: %v", err)
	}
	log.Printf("Received metadata: %+v\n", metadata)

	totalChunks := (metadata.FileSize + int64(metadata.ChunkSize) - 1) / int64(metadata.ChunkSize)

	// Stockage des chunks (protégé pour l'accès concurrent)
	chunks := make(map[uint64][]byte, totalChunks)
	var mu sync.Mutex

	// WaitGroup pour synchroniser la fin de la goroutine de lecture
	var wg sync.WaitGroup

	// Canal pour compter les chunks reçus par fenêtre
	chunksInWindowChan := make(chan int, totalChunks)

	// Goroutine pour lire les chunks d'un flux donné.
	// Cette fonction lit les messages applicatifs (index + taille + données) de manière robuste.
	readChunks := func(s kwik.Stream) {
		defer wg.Done()
		for {
			// 1. Lire l'en-tête de taille fixe (12 octets) en une seule fois.
			// C'est la manière la plus robuste de consommer depuis un flux réseau.
			header := make([]byte, 12) // 8 octets pour l'index (uint64), 4 pour la taille (uint32)
			_, err := io.ReadFull(s, header)
			if err != nil {
				// io.EOF est une fin de stream normale et attendue.
				if err != io.EOF {
					log.Printf("Error reading chunk header: %v", err)
				}
				break
			}

			// 2. Décoder l'en-tête en utilisant un lecteur en mémoire.
			reader := bytes.NewReader(header)
			var chunkIndex uint64
			var dataSize uint32
			binary.Read(reader, binary.BigEndian, &chunkIndex)
			binary.Read(reader, binary.BigEndian, &dataSize)

			// 3. Lire le payload de taille variable en une seule fois.
			data := make([]byte, dataSize)
			_, err = io.ReadFull(s, data)
			if err != nil {
				log.Printf("Error reading chunk data for index %d: %v", chunkIndex, err)
				break
			}

			// 4. Stocker le chunk et notifier la boucle principale.
			mu.Lock()
			// Vérifier si on n'a pas déjà reçu ce chunk (à cause de retransmissions réseau)
			if _, exists := chunks[chunkIndex]; !exists {
				chunks[chunkIndex] = data
				chunksInWindowChan <- 1
				log.Printf("Received chunk %d (size: %d bytes)", chunkIndex, dataSize)
			} else {
				log.Printf("Received DUPLICATE chunk %d, ignoring.", chunkIndex)
			}
			mu.Unlock()
		}
	}

	// Lancer UNE SEULE goroutine de lecture sur le stream principal.
	// Le StreamAggregator de Kwik s'assurera que cette goroutine reçoit
	// les données du serveur ET du relais, dans le bon ordre.
	wg.Add(1)
	go readChunks(stream)

	// Le bloc 'session.AcceptStream()' a été supprimé car il était incorrect.
	// Le protocole utilise le multipath sur un seul flux logique, il n'y a pas
	// de nouveau flux à accepter.

	// Boucle principale pour la gestion des fenêtres et des ACKs
	for w := 0; w < metadata.TotalWindows; w++ {
		chunksToExpect := metadata.WindowSizeChunks
		if w == metadata.TotalWindows-1 && metadata.LastWindowSizeChunks > 0 {
			chunksToExpect = metadata.LastWindowSizeChunks
		}

		log.Printf("Waiting for %d chunks for window %d...", chunksToExpect, w)
		received := 0
		for received < chunksToExpect {
			<-chunksInWindowChan
			received++
		}

		// Ne pas envoyer d'ACK pour la toute dernière fenêtre pour éviter un blocage
		if w < metadata.TotalWindows-1 {
			// Send length-prefixed ACK (4-byte big-endian length + payload)
			ackPayload := []byte("ACK")
			var lenBuf [4]byte
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ackPayload)))
			_, err = stream.Write(append(lenBuf[:], ackPayload...))
			if err != nil {
				log.Fatalf("Failed to send ACK for window %d: %v", w, err)
			}
			log.Printf("Sent ACK for window %d.", w)
		}
	}

	close(chunksInWindowChan)
	stream.Close() // Fermer le stream après avoir reçu tous les chunks

	// Attendre que la goroutine de lecture termine proprement
	log.Println("Waiting for stream reader to finish...")
	wg.Wait()

	log.Println("All chunks received. Assembling file...")

	// Créer le fichier de destination et assembler les chunks
	destFileName := "downloaded_" + metadata.FileName
	destFile, err := os.Create(destFileName)
	if err != nil {
		log.Fatalf("Failed to create destination file: %v", err)
	}
	defer destFile.Close()

	for i := uint64(0); i < uint64(totalChunks); i++ {
		mu.Lock()
		chunkData, ok := chunks[i]
		mu.Unlock()
		if !ok {
			log.Fatalf("FATAL: Missing chunk %d! Assembly failed.", i)
		}
		_, err := destFile.Write(chunkData)
		if err != nil {
			log.Fatalf("Failed to write chunk %d to file: %v", i, err)
		}
	}

	finalStat, _ := destFile.Stat()
	log.Printf("File download complete: %s (Size: %d bytes)", destFileName, finalStat.Size())
	if finalStat.Size() != metadata.FileSize {
		log.Printf("WARNING: Final file size (%d) does not match expected size (%d)!", finalStat.Size(), metadata.FileSize)
	}
}
