package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"

	quic "github.com/quic-go/quic-go"
)

func main() {
	// Test simple pour vérifier les types QUIC
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"kwik-http"},
	}

	// Test de dial
	fmt.Println("Testing QUIC types...")
	
	session, err := quic.DialAddr(context.Background(), "localhost:8080", tlsConfig, nil)
	if err != nil {
		log.Printf("Dial error (expected): %v", err)
	} else {
		fmt.Printf("Session type: %T\n", session)
		session.CloseWithError(0, "test")
	}
}
