package main

import (
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

	// WaitGroup pour synchroniser la fin de tous les lecteurs de flux
	var wg sync.WaitGroup

	// Canal pour compter les chunks reçus par fenêtre
	chunksInWindowChan := make(chan int, totalChunks)

	// Goroutine pour lire les chunks d'un flux donné
	readChunks := func(s kwik.Stream) {
		defer wg.Done()
		for {
			var chunkIndex uint64
			err := binary.Read(s, binary.BigEndian, &chunkIndex)
			if err != nil {
				if err == io.EOF || err.Error() == "ApplicationError 0x0" {
					break
				}
				log.Printf("Error reading chunk index: %v", err)
				break
			}

			var dataSize uint32
			err = binary.Read(s, binary.BigEndian, &dataSize)
			if err != nil {
				log.Printf("Error reading chunk data size: %v", err)
				break
			}

			data := make([]byte, dataSize)
			_, err = io.ReadFull(s, data)
			if err != nil {
				log.Printf("Error reading chunk data: %v", err)
				break
			}

			mu.Lock()
			chunks[chunkIndex] = data
			mu.Unlock()
			chunksInWindowChan <- 1
		}
	}

	// Lancer le lecteur pour le flux principal (chunks du serveur)
	wg.Add(1)
	go readChunks(stream)

	// Lancer une goroutine pour accepter les flux entrants (du relais)
	go func() {
		for {
			relayStream, err := session.AcceptStream(context.Background())
			if err != nil {
				log.Printf("Finished accepting relay streams: %v", err)
				return
			}
			log.Println("Accepted a new stream from relay.")
			wg.Add(1)
			go readChunks(relayStream)
		}
	}()

	// Boucle principale pour la gestion des fenêtres et des ACKs
	for w := 0; w < metadata.TotalWindows; w++ {
		chunksToExpect := metadata.WindowSizeChunks
		if w == metadata.TotalWindows-1 {
			chunksToExpect = metadata.LastWindowSizeChunks
		}

		log.Printf("Waiting for %d chunks for window %d...", chunksToExpect, w)
		received := 0
		for received < chunksToExpect {
			<-chunksInWindowChan
			received++
		}

		_, err = stream.Write([]byte("ACK"))
		if err != nil {
			log.Fatalf("Failed to send ACK for window %d: %v", w, err)
		}
		log.Printf("Sent ACK for window %d.", w)
	}

	close(chunksInWindowChan)

	// Attendre que tous les lecteurs de flux terminent
	log.Println("Waiting for all stream readers to finish...")
	wg.Wait()
	stream.Close() // Ferme le flux principal après avoir envoyé le dernier ACK

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
			log.Fatalf("Missing chunk %d!", i)
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
