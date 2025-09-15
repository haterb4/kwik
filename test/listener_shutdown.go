package main

import (
	"context"
	"fmt"
	"log"
	"time"

	kwik "github.com/s-anzie/kwik"
	"github.com/s-anzie/kwik/internal/config"
	utils_tls "github.com/s-anzie/kwik/internal/utils/tls"
)

func main() {
	fmt.Println("Testing KWIK listener graceful shutdown...")

	// Generate TLS configuration
	tlsConfig, err := utils_tls.GenerateTLSConfig()
	if err != nil {
		log.Fatalf("Failed to generate TLS config: %v", err)
	}

	// Create listener
	listener, err := kwik.ListenAddr("127.0.0.1:0", tlsConfig, &config.Config{})
	if err != nil {
		log.Fatalf("Failed to create listener: %v", err)
	}

	fmt.Printf("Listener created on %s\n", listener.Addr())
	fmt.Printf("Active sessions: %d\n", listener.GetActiveSessionCount())

	// Start a goroutine to accept connections (simulates server behavior)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		for {
			sess, err := listener.Accept(ctx)
			if err != nil {
				fmt.Printf("Accept error: %v\n", err)
				return
			}
			fmt.Printf("Accepted session from %s\n", sess.RemoteAddr())

			// Simulate some work with the session
			go func() {
				time.Sleep(100 * time.Millisecond)
				sess.CloseWithError(0, "session work done")
			}()
		}
	}()

	// Let the listener run for a short time
	time.Sleep(100 * time.Millisecond)

	fmt.Println("Closing listener...")
	start := time.Now()

	// Close the listener
	err = listener.Close()
	if err != nil {
		log.Printf("Error closing listener: %v", err)
	}

	elapsed := time.Since(start)
	fmt.Printf("Listener closed in %v\n", elapsed)
	fmt.Printf("Final active sessions: %d\n", listener.GetActiveSessionCount())

	// Cancel context to stop accept goroutine
	cancel()

	fmt.Println("Listener shutdown test completed successfully!")
}
