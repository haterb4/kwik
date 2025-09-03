package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

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

	fmt.Printf("Connecté au serveur %s\n", sess.RemoteAddr())
	fmt.Println("Tapez vos messages (tapez 'quit' pour quitter):")

	// Créer un reader pour lire l'entrée utilisateur
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("> ")

		// Lire l'entrée utilisateur
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Erreur lecture entrée: %v\n", err)
			continue
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		if input == "quit" {
			fmt.Println("Fermeture de la connexion...")
			break
		}

		// Envoyer le message dans un nouveau stream
		err = sendMessage(sess, input)
		if err != nil {
			fmt.Printf("Erreur envoi message: %v\n", err)
			continue
		}
	}
}

func sendMessage(conn kwik.Session, message string) error {
	// Ouvrir un nouveau stream
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("erreur ouverture stream: %v", err)
	}
	defer stream.Close()
	fmt.Println("Nouveau stream ouvert")
	// Envoyer le message
	_, err = stream.Write([]byte(message))
	if err != nil {
		return fmt.Errorf("erreur écriture stream: %v", err)
	}

	// Fermer l'écriture pour signaler la fin du message
	err = stream.Close()
	if err != nil {
		return fmt.Errorf("erreur fermeture écriture stream: %v", err)
	}

	// Lire la réponse avec un timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	responseChan := make(chan string, 1)
	errorChan := make(chan error, 1)

	go func() {
		buffer := make([]byte, 1024)
		n, err := stream.Read(buffer)
		if err != nil {
			if err != io.EOF {
				errorChan <- err
				return
			}
		}
		responseChan <- string(buffer[:n])
	}()

	select {
	case response := <-responseChan:
		fmt.Printf("Réponse du serveur: %s\n", response)
		return nil
	case err := <-errorChan:
		return fmt.Errorf("erreur lecture réponse: %v", err)
	case <-ctx.Done():
		return fmt.Errorf("timeout lors de la lecture de la réponse")
	}
}
