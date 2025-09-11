package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/s-anzie/kwik"
)

const (
	chunkSize    = 65536 // 64KB
	windowSize   = 10    // 10 chunks per window
	relayAddress = "localhost:4434"
)

// CommandType définit le type d'action que le relais doit effectuer.
type CommandType string

const (
	CmdSendWindow CommandType = "SEND_WINDOW"
	CmdFinish     CommandType = "FINISH"
)

// Command est la structure utilisée par le serveur pour donner des ordres au relais.
type Command struct {
	Type            CommandType `json:"type"`
	FileName        string      `json:"file_name,omitempty"`
	ChunkSize       int         `json:"chunk_size,omitempty"`
	StartChunkIndex int         `json:"start_chunk_index,omitempty"`
	EndChunkIndex   int         `json:"end_chunk_index,omitempty"`
	Step            int         `json:"step,omitempty"`
	// StartOffset est l'offset absolu où le relais doit commencer à écrire son premier chunk.
	StartOffset uint64 `json:"start_offset,omitempty"`
}

// FileMetadata est la structure envoyée au client.
type FileMetadata struct {
	FileName             string
	FileSize             int64
	ChunkSize            int
	WindowSizeChunks     int
	TotalWindows         int
	LastWindowSizeChunks int
}

func main() {
	log.Println("Starting QUIC server...")
	tlsConfig := generateTLSConfig()

	listener, err := kwik.ListenAddr("localhost:4433", tlsConfig, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	log.Println("Server listening on localhost:4433")

	for {
		session, err := listener.Accept(context.Background())
		if err != nil {
			log.Println("Error accepting session:", err)
			continue
		}
		go handleSession(session)
	}
}

func handleSession(session kwik.Session) {
	log.Println("New session accepted:", session.RemoteAddr())
	stream, err := session.AcceptStream(context.Background())
	if err != nil {
		log.Println("Error accepting stream:", err)
		return
	}
	defer stream.Close()

	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		log.Println("Error reading file request:", err)
		return
	}
	fileName := string(buf[:n])
	log.Println("Received request for file:", fileName)

	file, err := os.Open(fileName)
	if err != nil {
		log.Println("Error opening file:", err)
		stream.Write([]byte("File not found"))
		return
	}
	defer file.Close()

	fileInfo, _ := file.Stat()
	fileSize := fileInfo.Size()
	totalChunks := (fileSize + chunkSize - 1) / chunkSize
	totalWindows := (totalChunks + windowSize - 1) / windowSize
	lastWindowSize := int(totalChunks) % windowSize
	if lastWindowSize == 0 && totalChunks > 0 {
		lastWindowSize = windowSize
	}

	metadata := FileMetadata{
		FileName:             fileName,
		FileSize:             fileSize,
		ChunkSize:            chunkSize,
		WindowSizeChunks:     windowSize,
		TotalWindows:         int(totalWindows),
		LastWindowSizeChunks: lastWindowSize,
	}

	metaBytes, _ := json.Marshal(metadata)
	_, err = stream.Write(metaBytes)
	if err != nil {
		log.Println("Error sending metadata:", err)
		return
	}

	relay, err := session.AddRelay(relayAddress)
	if err != nil {
		log.Printf("Error creating relay: %v\n", err)
		return
	}
	log.Printf("Connection established to relay (PathID: %d)\n", relay.PathID())

	// L'en-tête de chaque "message" de chunk (index + taille)
	const chunkHeaderSize = 8 + 4 // uint64 + uint32

	for i := 0; i < metadata.TotalWindows; i++ {
		startChunkOfWindow := i * windowSize
		chunksInThisWindow := metadata.WindowSizeChunks
		if i == metadata.TotalWindows-1 {
			chunksInThisWindow = metadata.LastWindowSizeChunks
		}
		endChunkOfWindow := startChunkOfWindow + chunksInThisWindow
		log.Printf("[Window %d] Processing chunks from %d to %d", i, startChunkOfWindow, endChunkOfWindow-1)

		// --- Calcul des offsets pour tous les chunks dans l'ordre ---
		// Créer un mapping des offsets pour chaque chunk dans cette fenêtre
		chunkOffsets := make(map[int]uint64)
		currentOffset := stream.(*kwik.StreamImpl).GetWriteOffset()

		log.Printf("[Window %d] Starting with stream offset: %d", i, currentOffset)

		// Calculer les offsets pour tous les chunks dans l'ordre séquentiel
		for j := startChunkOfWindow; j < endChunkOfWindow; j++ {
			chunkOffsets[j] = currentOffset

			// Calculer la taille de ce chunk
			dataSize := chunkSize
			fileOffset := int64(j * chunkSize)
			if fileOffset+int64(chunkSize) > fileSize {
				dataSize = int(fileSize - fileOffset)
			}

			chunkTotalSize := uint64(chunkHeaderSize + dataSize)
			currentOffset += chunkTotalSize

			log.Printf("[Window %d] Chunk %d will be at offset %d, size %d", i, j, chunkOffsets[j], chunkTotalSize)
		}

		// --- Orchestration du Relais ---
		// Trouver le premier chunk impair et calculer son offset
		firstOddChunkIndex := -1
		for j := startChunkOfWindow; j < endChunkOfWindow; j++ {
			if j%2 != 0 {
				firstOddChunkIndex = j
				break
			}
		}

		if firstOddChunkIndex != -1 {
			relayStartOffset := chunkOffsets[firstOddChunkIndex]

			cmd := Command{
				Type:            CmdSendWindow,
				FileName:        fileName,
				ChunkSize:       chunkSize,
				StartChunkIndex: startChunkOfWindow,
				EndChunkIndex:   endChunkOfWindow,
				Step:            2, // Relais gère les chunks impairs
				StartOffset:     relayStartOffset,
			}
			cmdBytes, _ := json.Marshal(cmd)
			log.Printf("[Window %d] Sending command to relay with start offset %d for chunk %d", i, cmd.StartOffset, firstOddChunkIndex)
			_, err := relay.SendRawData(cmdBytes, stream.StreamID())
			if err != nil {
				log.Printf("[Window %d] Failed to send command to relay: %v", i, err)
				return
			}
		}

		// --- Envoi des données du Serveur (chunks pairs uniquement) ---
		for j := startChunkOfWindow; j < endChunkOfWindow; j++ {
			if j%2 != 0 {
				continue // Le relais gère les chunks impairs
			}

			// Positionner le stream à l'offset calculé pour ce chunk
			targetOffset := chunkOffsets[j]
			log.Printf("[Window %d] Setting server stream offset to %d for chunk %d", i, targetOffset, j)
			err = stream.(*kwik.StreamImpl).SetWriteOffset(targetOffset)
			if err != nil {
				log.Printf("Failed to set write offset: %v", err)
				return
			}

			// Lire les données du fichier
			fileOffset := int64(j * chunkSize)
			file.Seek(fileOffset, 0)

			dataSize := chunkSize
			if fileOffset+int64(chunkSize) > fileSize {
				dataSize = int(fileSize - fileOffset)
			}
			chunkData := make([]byte, dataSize)
			_, err := io.ReadFull(file, chunkData)
			if err != nil {
				log.Println("Error reading file chunk:", err)
				return
			}

			// Construire le payload du chunk
			payloadBuf := new(bytes.Buffer)
			binary.Write(payloadBuf, binary.BigEndian, uint64(j))
			binary.Write(payloadBuf, binary.BigEndian, uint32(len(chunkData)))
			payloadBuf.Write(chunkData)

			log.Printf("[Window %d] Server writing chunk %d at stream offset %d", i, j, stream.(*kwik.StreamImpl).GetWriteOffset())
			_, err = stream.Write(payloadBuf.Bytes())
			if err != nil {
				log.Printf("Failed to write chunk %d: %v", j, err)
				return
			}
		}

		// --- Positionner l'offset final pour la prochaine fenêtre ---
		finalOffset := currentOffset // currentOffset contient déjà l'offset final calculé
		log.Printf("[Window %d] Setting final stream offset to %d for next window", i, finalOffset)
		stream.(*kwik.StreamImpl).SetWriteOffset(finalOffset)

		// --- Attente de l'ACK (length-prefixed to avoid boundary issues) ---
		var lenBuf [4]byte
		_, err = io.ReadFull(stream, lenBuf[:])
		if err != nil {
			log.Println("Error receiving ACK length:", err)
			return
		}
		payloadLen := binary.BigEndian.Uint32(lenBuf[:])
		if payloadLen == 0 || payloadLen > 1024 {
			log.Println("Invalid ACK length:", payloadLen)
			return
		}
		ackBuf := make([]byte, payloadLen)
		_, err = io.ReadFull(stream, ackBuf)
		if err != nil {
			log.Println("Error receiving ACK payload:", err)
			return
		}
		if string(ackBuf) != "ACK" {
			log.Printf("Invalid ACK received: %q", string(ackBuf))
			return
		}
		log.Printf("Received ACK for window %d\n", i)
	}

	log.Println("File transfer complete. Sending FINISH to relay.")
	finishCmd := Command{Type: CmdFinish}
	finishCmdBytes, _ := json.Marshal(finishCmd)
	relay.SendRawData(finishCmdBytes, stream.StreamID())

	log.Println("File transfer process finished for", fileName)
}

// generateTLSConfig crée une configuration TLS auto-signée en mémoire.
func generateTLSConfig() *tls.Config {
	// Générer une clé privée RSA
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal("Erreur génération clé privée:", err)
	}

	// Créer un template de certificat
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization:  []string{"Test Company"},
			Country:       []string{"FR"},
			Province:      []string{""},
			Locality:      []string{"Paris"},
			StreetAddress: []string{""},
			PostalCode:    []string{""},
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:    []string{"localhost"},
	}

	// Créer le certificat
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		log.Fatal("Erreur création certificat:", err)
	}

	// Encoder le certificat en PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	// Encoder la clé privée en PEM
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	// Créer le certificat TLS
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Fatal("Erreur création paire de clés:", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"quic-file-transfer"},
	}
}
