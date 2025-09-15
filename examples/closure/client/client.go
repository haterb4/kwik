package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"

	"github.com/s-anzie/kwik"
)

func main() {
	// Configuration TLS pour le client (accepter les certificats auto-signés)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-example"},
	}

	// Se connecter au serveur
	fmt.Println("Connexion au serveur QUIC...")
	sess, err := kwik.DialAddr(context.Background(), "localhost:4433", tlsConfig, nil)
	if err != nil {
		log.Fatal("Erreur connexion au serveur:", err)
	}
	defer sess.CloseWithError(0, "client fermé")
	fmt.Println("Connecté au serveur.")

	// Ouvrir un flux logique
	stream, err := sess.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatal("Erreur ouverture flux:", err)
	}
	defer stream.Close()
	fmt.Println("Flux logique ouvert.")

	// Envoyer un message au serveur
	message := "Bonjour depuis le client QUIC/KWIK!"
	_, err = stream.Write([]byte(message))
	if err != nil {
		log.Fatal("Erreur écriture flux:", err)
	}
	fmt.Println("Message envoyé au serveur:", message)

	// Lire la réponse du serveur
	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		log.Fatal("Erreur lecture flux:", err)
	}
	fmt.Printf("Réponse reçue du serveur: %s\n", string(buf[:n]))

	fmt.Println("Fermeture du flux et de la session.")
}
