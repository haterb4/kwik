package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/s-anzie/kwik"
)

func main() {
	log.Println("Starting QUIC relay...")
	tlsConfig := generateTLSConfig()

	listener, err := kwik.ListenAddr("localhost:4434", tlsConfig, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	log.Println("Relay listening on localhost:4434")

	for {
		session, err := listener.Accept(context.Background())
		if err != nil {
			log.Println("Error accepting session:", err)
			continue
		}
		go handleRelaySession(session)
	}
}

func handleRelaySession(session kwik.Session) {
	log.Println("New relay session accepted:", session.RemoteAddr())

	// Le relais doit accepter un stream unidirectionnel du serveur pour les commandes
	stream, err := session.AcceptStream(context.Background())
	if err != nil {
		log.Println("Error accepting command stream:", err)
		return
	}

	// Read command from server
	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		log.Println("Error reading command:", err)
		return
	}
	command := string(buf[:n])
	log.Println("Received command:", command)

	parts := strings.Split(command, " ")
	if len(parts) != 6 || parts[0] != "SEND" {
		log.Println("Invalid command")
		return
	}

	fileName := parts[1]
	chunkSize, _ := strconv.Atoi(parts[2])
	startChunk, _ := strconv.Atoi(parts[3])
	endChunk, _ := strconv.ParseInt(parts[4], 10, 64)
	step, _ := strconv.Atoi(parts[5])

	file, err := os.Open(fileName)
	if err != nil {
		log.Println("Error opening file:", err)
		return
	}
	defer file.Close()

	fileInfo, _ := file.Stat()
	fileSize := fileInfo.Size()

	for i := startChunk; int64(i) < endChunk; i += step {
		offset := int64(i * chunkSize)
		if offset >= fileSize {
			break
		}
		file.Seek(offset, 0)

		// Déterminer la taille réelle du chunk
		bytesToSend := chunkSize
		if offset+int64(chunkSize) > fileSize {
			bytesToSend = int(fileSize - offset)
		}

		chunkData := make([]byte, bytesToSend)
		_, err := io.ReadFull(file, chunkData)
		if err != nil {
			log.Println("Error reading file chunk:", err)
			return
		}

		binary.Write(stream, binary.LittleEndian, uint32(bytesToSend))
		stream.Write(chunkData)
	}
	log.Println("Relay finished sending chunks for", fileName)
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
