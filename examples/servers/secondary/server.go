// server.go
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"time"

	"github.com/s-anzie/kwik"
)

// generateTLSConfig génère un certificat auto-signé pour le serveur
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
		NextProtos:   []string{"quic-example"},
	}
}

func handleConnection(conn kwik.Session) {
	fmt.Printf("Nouvelle connexion établie avec %s\n", conn.RemoteAddr())

	for {
		// Accepter un nouveau stream
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			fmt.Printf("Erreur lors de l'acceptation du stream: %v\n", err)
			return
		}

		// Traiter le stream dans une goroutine séparée
		go handleStream(stream)
	}
}

func handleStream(stream kwik.Stream) {
	defer stream.Close()

	fmt.Printf("Nouveau stream ouvert: %d\n", stream.StreamID())

	// Lire les données du client
	buffer := make([]byte, 1024)
	for {
		n, err := stream.Read(buffer)
		if err != nil {
			if err == io.EOF {
				fmt.Printf("Stream %d fermé par le client\n", stream.StreamID())
				return
			}
			fmt.Printf("Erreur lecture stream: %v\n", err)
			return
		}

		message := string(buffer[:n])
		fmt.Printf("Message reçu sur stream %d: %s\n", stream.StreamID(), message)

		// Répondre au client
		response := fmt.Sprintf("Echo: %s", message)
		_, err = stream.Write([]byte(response))
		if err != nil {
			fmt.Printf("Erreur écriture stream: %v\n", err)
			return
		}
	}
}

func main() {
	// Générer la configuration TLS
	tlsConfig := generateTLSConfig()

	// Écouter sur le port 4433
	listener, err := kwik.ListenAddr("localhost:4434", tlsConfig, nil)
	if err != nil {
		log.Fatal("Erreur lors du démarrage du serveur:", err)
	}
	defer listener.Close()

	fmt.Println("Serveur QUIC démarré sur localhost:4433")
	fmt.Println("En attente de connexions...")

	for {
		// Accepter une nouvelle connexion
		conn, err := listener.Accept(context.Background())
		if err != nil {
			fmt.Printf("Erreur lors de l'acceptation de connexion: %v\n", err)
			continue
		}

		// Traiter la connexion dans une goroutine séparée
		go handleConnection(conn)
	}
}
