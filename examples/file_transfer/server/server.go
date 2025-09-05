package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
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

type FileMetadata struct {
	FileName       string
	FileSize       int64
	TotalWindows   int
	WindowSize     int
	LastWindowSize int
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
	// Ne fermez pas le stream ici pour permettre la communication continue (ACKs)

	// Read file request
	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		log.Println("Error reading file request:", err)
		stream.Close()
		return
	}
	fileName := string(buf[:n])
	log.Println("Received request for file:", fileName)

	// Open the requested file
	file, err := os.Open(fileName)
	if err != nil {
		log.Println("Error opening file:", err)
		stream.Write([]byte("File not found"))
		stream.Close()
		return
	}
	defer file.Close()

	fileInfo, _ := file.Stat()
	fileSize := fileInfo.Size()
	totalChunks := (fileSize + chunkSize - 1) / chunkSize
	totalWindows := (totalChunks + windowSize - 1) / windowSize

	metadata := FileMetadata{
		FileName:       fileName,
		FileSize:       fileSize,
		TotalWindows:   int(totalWindows),
		WindowSize:     windowSize,
		LastWindowSize: int(totalChunks) % windowSize,
	}
	if metadata.LastWindowSize == 0 && totalChunks > 0 {
		metadata.LastWindowSize = windowSize
	}

	// Send metadata to client
	metaBytes, _ := json.Marshal(metadata)
	_, err = stream.Write(metaBytes)
	if err != nil {
		log.Println("Error sending metadata:", err)
		stream.Close()
		return
	}

	// Setup relay
	relay, err := session.AddRelay(relayAddress)
	if err != nil {
		log.Printf("Erreur création relay: %v\n", err)
		stream.Close()
		return
	}
	go func() {
		fmt.Printf("Nouvelle connexion établie vers le relay %d\n", relay.PathID())
		command := fmt.Sprintf("SEND %s %d %d %d %d", fileName, chunkSize, 1, totalChunks, 2) // start at chunk 1, step 2

		// Le stream ID est déjà connu du contexte de la session du relais
		relay.SendRawData([]byte(command), 0) // StreamID 0 car le relais ouvrira son propre stream
	}()

	// Send even chunks
	for i := 0; i < metadata.TotalWindows; i++ {
		startChunk := i * windowSize

		// Calcul de la taille de la fenêtre actuelle
		currentWindowSize := windowSize
		if i == metadata.TotalWindows-1 {
			currentWindowSize = metadata.LastWindowSize
		}

		endChunk := startChunk + currentWindowSize

		for j := startChunk; j < endChunk; j += 2 {
			if int64(j) >= totalChunks {
				continue
			}
			offset := int64(j * chunkSize)
			file.Seek(offset, 0)

			// Déterminer la taille réelle du chunk (surtout pour le dernier)
			bytesToSend := chunkSize
			if offset+int64(chunkSize) > fileSize {
				bytesToSend = int(fileSize - offset)
			}

			chunkData := make([]byte, bytesToSend)
			_, err := io.ReadFull(file, chunkData)
			if err != nil {
				log.Println("Error reading file chunk:", err)
				stream.Close()
				return
			}
			binary.Write(stream, binary.LittleEndian, uint32(bytesToSend))
			stream.Write(chunkData)
		}

		// Wait for ACK
		ackBuf := make([]byte, 3)
		_, err := stream.Read(ackBuf)
		if err != nil {
			log.Println("Error receiving ACK:", err)
			stream.Close()
			return
		}
		if string(ackBuf) != "ACK" {
			log.Println("Invalid ACK received")
			stream.Close()
			return
		}
		log.Printf("Received ACK for window %d\n", i)
	}
	log.Println("File transfer completed for", fileName)
	stream.Close()
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
