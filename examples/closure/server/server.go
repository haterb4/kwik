package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
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
func main() {
	// Générer la configuration TLS
	tlsConfig := generateTLSConfig()

	// Écouter sur le port 4433
	listener, err := kwik.ListenAddr("localhost:4433", tlsConfig, nil)
	if err != nil {
		log.Fatal("Erreur lors du démarrage du serveur:", err)
	}
	defer listener.Close()
	log.Println("Serveur QUIC/KWIK en écoute sur localhost:4433")

	// Accepter une nouvelle session
	session, err := listener.Accept(context.Background())
	if err != nil {
		log.Println("Erreur acceptation session:", err)
		return
	}
	log.Println("Nouvelle session acceptée")
	defer session.CloseWithError(0, "Serveur arrêté")

	// Accepter un flux logique
	stream, err := session.AcceptStream(context.Background())
	if err != nil {
		log.Println("Erreur acceptation flux:", err)
		return
	}
	defer stream.Close()
	log.Println("Nouveau flux logique accepté")

	// Lire les données envoyées par le client
	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		log.Println("Erreur lecture flux:", err)
		return
	}
	log.Printf("Données reçues: %s\n", string(buf[:n]))

	// Envoyer une réponse au client
	response := "Bonjour depuis le serveur QUIC/KWIK!"
	_, err = stream.Write([]byte(response))
	if err != nil {
		log.Println("Erreur écriture flux:", err)
		return
	}
	log.Println("Réponse envoyée au client")

	log.Println("Fermeture du flux et de la session")
}
