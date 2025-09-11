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

// CommandType définit le type d'action que le relais doit effectuer.
type CommandType string

const (
	CmdSendWindow CommandType = "SEND_WINDOW"
	CmdFinish     CommandType = "FINISH"
)

// Command est la structure utilisée par le serveur pour donner des ordres au relais.
// Cette structure doit être identique à celle du serveur.
type Command struct {
	Type            CommandType `json:"type"`
	FileName        string      `json:"file_name,omitempty"`
	ChunkSize       int         `json:"chunk_size,omitempty"`
	StartChunkIndex int         `json:"start_chunk_index,omitempty"`
	EndChunkIndex   int         `json:"end_chunk_index,omitempty"`
	Step            int         `json:"step,omitempty"`
	StartOffset     uint64      `json:"start_offset,omitempty"`
}

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
	log.Println("New relay session accepted from:", session.RemoteAddr())
	defer log.Println("Relay session closed for:", session.RemoteAddr())

	// Le relais attend un flux unique du serveur pour recevoir toutes les commandes.
	stream, err := session.AcceptStream(context.Background())
	if err != nil {
		log.Println("Error accepting command stream:", err)
		return
	}
	defer stream.Close()

	for {
		cmdBuf := make([]byte, 2048)
		n, err := stream.Read(cmdBuf)
		if err != nil {
			if err != io.EOF {
				log.Println("Error reading command:", err)
			}
			return
		}

		var cmd Command
		err = json.Unmarshal(cmdBuf[:n], &cmd)
		if err != nil {
			log.Printf("Failed to unmarshal command: %v", err)
			continue
		}

		log.Printf("Received command: %+v", cmd)

		switch cmd.Type {
		case CmdFinish:
			log.Println("FINISH command received. Terminating.")
			return

		case CmdSendWindow:
			err := processSendWindowCommand(stream, cmd)
			if err != nil {
				log.Printf("Error processing window: %v", err)
				return // En cas d'erreur, on termine la session pour ce relais.
			}

		default:
			log.Printf("Unknown command type received: %s", cmd.Type)
		}
	}
}

func processSendWindowCommand(stream kwik.Stream, cmd Command) error {
	file, err := os.Open(cmd.FileName)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, _ := file.Stat()
	fileSize := fileInfo.Size()

	const chunkHeaderSize = 8 + 4 // uint64 for index, uint32 for size

	// StartOffset est l'offset d'écriture au moment de commencer cette fenêtre
	// On itère chunk par chunk en incrémentant l'offset, même pour les chunks qu'on saute
	currentOffset := cmd.StartOffset

	// Parcourir la fenêtre de chunks assignée
	for i := cmd.StartChunkIndex + 1; i < cmd.EndChunkIndex; i++ {
		// Calculer la taille de ce chunk
		dataSize := cmd.ChunkSize
		fileOffset := int64(i * cmd.ChunkSize)
		if fileOffset+int64(cmd.ChunkSize) > fileSize {
			dataSize = int(fileSize - fileOffset)
		}
		chunkTotalSize := uint64(chunkHeaderSize + dataSize)

		// Le relais ne traite que les chunks impairs (step=2, donc pairs=0, impairs=1)
		if i%cmd.Step == 0 {
			// Chunk pair : on saute mais on incrémente l'offset
			currentOffset += chunkTotalSize
			continue
		}

		// Chunk impair : on écrit à l'offset courant
		err := stream.(*kwik.StreamImpl).SetWriteOffset(currentOffset)
		if err != nil {
			log.Printf("Failed to set write offset for chunk %d: %v", i, err)
			return err
		}

		// Lire les données du fichier
		file.Seek(fileOffset, 0)

		chunkData := make([]byte, dataSize)
		_, err = io.ReadFull(file, chunkData)
		if err != nil {
			return err
		}

		// Construire le payload applicatif
		payloadBuf := new(bytes.Buffer)
		binary.Write(payloadBuf, binary.BigEndian, uint64(i))
		binary.Write(payloadBuf, binary.BigEndian, uint32(len(chunkData)))
		payloadBuf.Write(chunkData)

		log.Printf("Relay sending chunk %d at stream offset %d", i, currentOffset)
		_, err = stream.Write(payloadBuf.Bytes())
		if err != nil {
			return err
		}

		// Incrémenter l'offset pour le prochain chunk
		currentOffset += chunkTotalSize
	}

	log.Println("Relay finished sending its chunks for the window.")
	return nil
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
