package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik"
)

const (
	PING_MSG = "PING"
	PONG_MSG = "PONG"
)

// Structure pour les messages ping/pong
type Message struct {
	Type      string
	Counter   uint64
	Timestamp time.Time
}

// Générer un certificat TLS auto-signé pour les tests
func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization:  []string{"QUIC Ping-Pong Test"},
			Country:       []string{"FR"},
			Province:      []string{""},
			Locality:      []string{"Nantes"},
			StreetAddress: []string{""},
			PostalCode:    []string{""},
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(time.Hour * 24 * 180),
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}

	cert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"quic-ping-pong"},
	}
}

// Serveur QUIC Ping-Pong
type PingPongServer struct {
	listener kwik.Listener
	addr     string
	cancel   context.CancelFunc
}

func NewPingPongServer(addr string) (*PingPongServer, error) {
	tlsConf := generateTLSConfig()

	config := &quic.Config{
		EnableDatagrams: true,
	}

	listener, err := kwik.ListenAddr(addr, tlsConf, config)
	if err != nil {
		return nil, err
	}

	return &PingPongServer{
		listener: listener,
		addr:     addr,
	}, nil
}

func (s *PingPongServer) Start() error {
	fmt.Printf("🏓 Serveur Ping-Pong QUIC démarré sur %s\n", s.addr)
	fmt.Println("En attente de connexions...")
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			return err
		}

		fmt.Printf("📡 Nouvelle connexion acceptée depuis %s\n", conn.RemoteAddr())
		go s.handleConnection(conn)
	}
}

func (s *PingPongServer) handleConnection(conn kwik.Session) {
	defer func() {
		fmt.Printf("🔌 Connexion fermée avec %s\n", conn.RemoteAddr())
		conn.CloseWithError(0, "Au revoir!")
	}()

	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			fmt.Printf("❌ Erreur lors de l'acceptation du stream: %v\n", err)
			return
		}

		go s.handleStream(stream, conn.RemoteAddr())
	}
}

func (s *PingPongServer) handleStream(stream kwik.Stream, clientAddr string) {
	defer stream.Close()

	var pongCounter uint64 = 0

	fmt.Printf("🎯 Nouveau stream ouvert avec %s\n", clientAddr)

	for {
		// Lire le message du client
		msg, err := s.readMessage(stream)
		if err != nil {
			if err == io.EOF {
				fmt.Printf("📥 Stream fermé par le client %s\n", clientAddr)
				break
			}
			fmt.Printf("❌ Erreur lors de la lecture: %v\n", err)
			return
		}

		// Afficher le PING reçu
		fmt.Printf("📨 [%s] Reçu: %s #%d (RTT potentiel: %v)\n",
			clientAddr, msg.Type, msg.Counter, time.Since(msg.Timestamp))

		// Préparer et envoyer le PONG
		if msg.Type == PING_MSG {
			pongCounter++
			pongMsg := Message{
				Type:      PONG_MSG,
				Counter:   pongCounter,
				Timestamp: time.Now(),
			}

			err = s.writeMessage(stream, pongMsg)
			if err != nil {
				fmt.Printf("❌ Erreur lors de l'envoi du PONG: %v\n", err)
				return
			}

			fmt.Printf("📤 [%s] Envoyé: %s #%d\n", clientAddr, pongMsg.Type, pongMsg.Counter)
		}
	}
}

func (s *PingPongServer) readMessage(stream kwik.Stream) (*Message, error) {
	// Lire le message (20 bytes)
	msgBytes := make([]byte, 20)
	_, err := io.ReadFull(stream, msgBytes)
	if err != nil {
		return nil, err
	}

	// Parser le message
	msg := &Message{}

	// Type (4 bytes pour PING/PONG)
	msg.Type = string(msgBytes[:4])

	// Counter (8 bytes)
	msg.Counter = binary.BigEndian.Uint64(msgBytes[4:12])

	// Timestamp (8 bytes - nanoseconds depuis Unix epoch)
	timestampNanos := int64(binary.BigEndian.Uint64(msgBytes[12:20]))
	msg.Timestamp = time.Unix(0, timestampNanos)

	return msg, nil
}

func (s *PingPongServer) writeMessage(stream kwik.Stream, msg Message) error {
	// TRACK: log chaque demande d'envoi côté serveur (sans streamID)
	// fmt.Printf("TRACK AppServerWrite: type=%s, counter=%d, ts=%d\n", msg.Type, msg.Counter, msg.Timestamp.UnixNano())
	// Préparer le message
	msgBytes := make([]byte, 20) // 4 + 8 + 8 bytes

	// Type
	copy(msgBytes[:4], msg.Type)

	// Counter
	binary.BigEndian.PutUint64(msgBytes[4:12], msg.Counter)

	// Timestamp
	binary.BigEndian.PutUint64(msgBytes[12:20], uint64(msg.Timestamp.UnixNano()))

	// Écrire le message (20 bytes)
	_, err := stream.Write(msgBytes)
	return err
}

func (s *PingPongServer) Stop() error {
	fmt.Println("🛑 Arrêt du serveur...")
	s.cancel() // annule le ctx => débloque Accept
	return s.listener.Close()
}

// Client QUIC Ping-Pong
type PingPongClient struct {
	serverAddr string
	interval   time.Duration
}

func NewPingPongClient(serverAddr string, interval time.Duration) *PingPongClient {
	return &PingPongClient{
		serverAddr: serverAddr,
		interval:   interval,
	}
}

func (c *PingPongClient) Start() error {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-ping-pong"},
	}

	quicConf := &quic.Config{
		EnableDatagrams: true,
	}

	fmt.Printf("🔗 Connexion au serveur %s...\n", c.serverAddr)

	conn, err := kwik.DialAddr(context.Background(), c.serverAddr, tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("impossible de se connecter: %v", err)
	}
	defer conn.CloseWithError(0, "Client terminé")

	fmt.Printf("✅ Connecté au serveur %s\n", c.serverAddr)

	// Ouvrir un stream
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("impossible d'ouvrir un stream: %v", err)
	}
	defer stream.Close()

	fmt.Println("🎯 Stream ouvert, démarrage du ping-pong...")

	var pingCounter uint64 = 0

	// Channel pour gérer l'arrêt propre
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-sigChan:
			fmt.Println("\n🛑 Arrêt demandé par l'utilisateur")
			return nil

		case <-ticker.C:
			// Envoyer PING
			pingCounter++
			pingMsg := Message{
				Type:      PING_MSG,
				Counter:   pingCounter,
				Timestamp: time.Now(),
			}

			err = c.writeMessage(stream, pingMsg)
			if err != nil {
				fmt.Printf("❌ Erreur lors de l'envoi du PING: %v\n", err)
				return err
			}

			fmt.Printf("📤 Envoyé: %s #%d\n", pingMsg.Type, pingMsg.Counter)

			// Lire PONG
			pongMsg, err := c.readMessage(stream)
			if err != nil {
				fmt.Printf("❌ Erreur lors de la lecture du PONG: %v\n", err)
				return err
			}

			rtt := time.Since(pingMsg.Timestamp)
			fmt.Printf("📨 Reçu: %s #%d (RTT: %v)\n", pongMsg.Type, pongMsg.Counter, rtt)
			fmt.Println("---")
		}
	}
}

func (c *PingPongClient) readMessage(stream kwik.Stream) (*Message, error) {
	// Lire le message (20 bytes)
	msgBytes := make([]byte, 20)
	_, err := io.ReadFull(stream, msgBytes)
	if err != nil {
		return nil, err
	}

	msg := &Message{}
	msg.Type = string(msgBytes[:4])
	msg.Counter = binary.BigEndian.Uint64(msgBytes[4:12])
	timestampNanos := int64(binary.BigEndian.Uint64(msgBytes[12:20]))
	msg.Timestamp = time.Unix(0, timestampNanos)

	return msg, nil
}

func (c *PingPongClient) writeMessage(stream kwik.Stream, msg Message) error {
	// Même implémentation que le serveur
	msgBytes := make([]byte, 20)
	copy(msgBytes[:4], msg.Type)
	binary.BigEndian.PutUint64(msgBytes[4:12], msg.Counter)
	binary.BigEndian.PutUint64(msgBytes[12:20], uint64(msg.Timestamp.UnixNano()))

	_, err := stream.Write(msgBytes)
	return err
}

func main() {
	var (
		mode     = flag.String("mode", "server", "Mode: 'server' ou 'client'")
		addr     = flag.String("addr", "localhost:4242", "Adresse du serveur")
		interval = flag.Duration("interval", 1*time.Second, "Intervalle entre les pings (mode client)")
	)
	flag.Parse()

	switch *mode {
	case "server":
		server, err := NewPingPongServer(*addr)
		if err != nil {
			log.Fatalf("❌ Erreur lors de la création du serveur: %v", err)
		}

		// Gestion de l'arrêt propre
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			<-sigChan
			fmt.Println("\n🛑 Signal d'arrêt reçu")
			server.Stop()
			os.Exit(0)
		}()

		if err := server.Start(); err != nil {
			log.Fatalf("❌ Erreur du serveur: %v", err)
		}

	case "client":
		client := NewPingPongClient(*addr, *interval)
		if err := client.Start(); err != nil {
			log.Fatalf("❌ Erreur du client: %v", err)
		}

	default:
		fmt.Println("❌ Mode invalide. Utilisez 'server' ou 'client'")
		flag.Usage()
		os.Exit(1)
	}
}
