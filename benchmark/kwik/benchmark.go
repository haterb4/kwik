package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik"
)

// Métriques de performance
type Metrics struct {
	TotalBytes       int64         `json:"total_bytes"`
	Duration         time.Duration `json:"duration_ms"`
	Throughput       float64       `json:"throughput_mbps"`
	StreamCount      int           `json:"stream_count"`
	ConnectionCount  int           `json:"connection_count"`
	RTT              time.Duration `json:"rtt_ms"`
	PacketLoss       float64       `json:"packet_loss_percent"`
	CPUUsage         float64       `json:"cpu_usage_percent"`
	MemoryUsage      int64         `json:"memory_usage_mb"`
	ErrorCount       int64         `json:"error_count"`
	ConnectionTime   time.Duration `json:"connection_time_ms"`
	FirstByteLatency time.Duration `json:"first_byte_latency_ms"`
}

// Configuration du benchmark
type BenchmarkConfig struct {
	MessageSizes     []int         // Tailles des messages à tester
	StreamCounts     []int         // Nombre de streams par connexion
	ConnectionCounts []int         // Nombre de connexions simultanées
	Duration         time.Duration // Durée de chaque test
	ServerAddr       string        // Adresse du serveur
}

// Serveur QUIC
type QuicServer struct {
	listener kwik.Listener
	addr     string
}

func NewQuicServer(addr string) (*QuicServer, error) {
	tlsConf := generateTLSConfig()

	config := &quic.Config{
		EnableDatagrams: true,
	}

	listener, err := kwik.ListenAddr(addr, tlsConf, config)
	if err != nil {
		return nil, err
	}

	return &QuicServer{
		listener: listener,
		addr:     addr,
	}, nil
}

func (s *QuicServer) Start() error {
	fmt.Printf("Serveur QUIC démarré sur %s\n", s.addr)

	for {
		conn, err := s.listener.Accept(context.Background())
		if err != nil {
			return err
		}

		go s.handleConnection(conn)
	}
}

func (s *QuicServer) handleConnection(conn kwik.Session) {
	defer conn.CloseWithError(0, "")

	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}

		go s.handleStream(stream)
	}
}

func (s *QuicServer) handleStream(stream kwik.Stream) {
	defer stream.Close()

	buffer := make([]byte, 64*1024)

	for {
		n, err := stream.Read(buffer)
		if err != nil {
			if err == io.EOF {
				break
			}
			return
		}

		// Echo des données reçues
		_, err = stream.Write(buffer[:n])
		if err != nil {
			return
		}
	}
}

func (s *QuicServer) Stop() error {
	return s.listener.Close()
}

// Client QUIC pour les benchmarks
type QuicClient struct {
	conn     kwik.Session
	tlsConf  *tls.Config
	quicConf *quic.Config
}

func NewQuicClient() *QuicClient {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-benchmark"},
	}

	quicConf := &quic.Config{
		EnableDatagrams: true,
	}

	return &QuicClient{
		tlsConf:  tlsConf,
		quicConf: quicConf,
	}
}

func (c *QuicClient) Connect(addr string) error {
	conn, err := kwik.DialAddr(context.Background(), addr, c.tlsConf, c.quicConf)
	if err != nil {
		return err
	}

	c.conn = conn
	return nil
}

func (c *QuicClient) Close() error {
	if c.conn != nil {
		return c.conn.CloseWithError(0, "")
	}
	return nil
}

// Générateur de certificat TLS pour les tests
func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization:  []string{"Test"},
			Country:       []string{"US"},
			Province:      []string{""},
			Locality:      []string{"San Francisco"},
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
		NextProtos:   []string{"quic-benchmark"},
	}
}

// Benchmark Runner
type BenchmarkRunner struct {
	config  BenchmarkConfig
	server  *QuicServer
	results []Metrics
}

func NewBenchmarkRunner(config BenchmarkConfig) *BenchmarkRunner {
	return &BenchmarkRunner{
		config:  config,
		results: make([]Metrics, 0),
	}
}

func (br *BenchmarkRunner) RunBenchmarks() error {
	// Démarrer le serveur
	server, err := NewQuicServer(br.config.ServerAddr)
	if err != nil {
		return err
	}
	br.server = server

	go func() {
		if err := server.Start(); err != nil {
			log.Printf("Erreur serveur: %v", err)
		}
	}()

	// Attendre que le serveur soit prêt
	time.Sleep(time.Second)

	// Exécuter les benchmarks
	for _, msgSize := range br.config.MessageSizes {
		for _, streamCount := range br.config.StreamCounts {
			for _, connCount := range br.config.ConnectionCounts {
				fmt.Printf("Test: %d bytes, %d streams, %d connexions\n",
					msgSize, streamCount, connCount)

				metrics, err := br.runSingleBenchmark(msgSize, streamCount, connCount)
				if err != nil {
					log.Printf("Erreur benchmark: %v", err)
					continue
				}

				br.results = append(br.results, *metrics)
				br.printMetrics(metrics)
			}
		}
	}

	return nil
}

func (br *BenchmarkRunner) runSingleBenchmark(msgSize, streamCount, connCount int) (*Metrics, error) {
	var totalBytes int64
	var errorCount int64
	var firstByteLatency time.Duration

	startTime := time.Now()
	var memStatsBefore, memStatsAfter runtime.MemStats
	runtime.ReadMemStats(&memStatsBefore)

	// Créer les connexions
	clients := make([]*QuicClient, connCount)
	for i := 0; i < connCount; i++ {
		clients[i] = NewQuicClient()
		connStart := time.Now()
		err := clients[i].Connect(br.config.ServerAddr)
		if err != nil {
			atomic.AddInt64(&errorCount, 1)
			continue
		}
		if i == 0 {
			firstByteLatency = time.Since(connStart)
		}
	}

	// Préparer les données de test
	testData := make([]byte, msgSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	var wg sync.WaitGroup

	// Lancer les tests sur chaque connexion
	for i, client := range clients {
		if client.conn == nil {
			continue
		}

		wg.Add(1)
		go func(clientIdx int, c *QuicClient) {
			defer wg.Done()
			defer c.Close()

			// Créer les streams pour cette connexion
			streams := make([]kwik.Stream, streamCount)
			for j := 0; j < streamCount; j++ {
				stream, err := c.conn.OpenStreamSync(context.Background())
				if err != nil {
					atomic.AddInt64(&errorCount, 1)
					return
				}
				streams[j] = stream
			}

			// Test pendant la durée configurée
			endTime := time.Now().Add(br.config.Duration)
			for time.Now().Before(endTime) {
				for _, stream := range streams {
					if stream == nil {
						continue
					}

					// Écriture
					n, err := stream.Write(testData)
					if err != nil {
						atomic.AddInt64(&errorCount, 1)
						continue
					}

					// Lecture (echo du serveur)
					buffer := make([]byte, msgSize)
					_, err = io.ReadFull(stream, buffer)
					if err != nil {
						atomic.AddInt64(&errorCount, 1)
						continue
					}

					atomic.AddInt64(&totalBytes, int64(n*2)) // Write + Read
				}
			}

			// Fermer les streams
			for _, stream := range streams {
				if stream != nil {
					stream.Close()
				}
			}
		}(i, client)
	}

	wg.Wait()
	duration := time.Since(startTime)

	runtime.ReadMemStats(&memStatsAfter)

	// Calculer les métriques
	metrics := &Metrics{
		TotalBytes:       totalBytes,
		Duration:         duration,
		Throughput:       float64(totalBytes*8) / (1024 * 1024) / duration.Seconds(), // Mbps
		StreamCount:      streamCount,
		ConnectionCount:  connCount,
		ErrorCount:       errorCount,
		FirstByteLatency: firstByteLatency,
		MemoryUsage:      int64(memStatsAfter.Alloc-memStatsBefore.Alloc) / (1024 * 1024), // MB
	}

	return metrics, nil
}

func (br *BenchmarkRunner) printMetrics(m *Metrics) {
	fmt.Printf("=== Résultats du benchmark ===\n")
	fmt.Printf("Bytes totaux: %d\n", m.TotalBytes)
	fmt.Printf("Durée: %v\n", m.Duration)
	fmt.Printf("Débit: %.2f Mbps\n", m.Throughput)
	fmt.Printf("Streams: %d\n", m.StreamCount)
	fmt.Printf("Connexions: %d\n", m.ConnectionCount)
	fmt.Printf("Erreurs: %d\n", m.ErrorCount)
	fmt.Printf("Latence première connexion: %v\n", m.FirstByteLatency)
	fmt.Printf("Mémoire utilisée: %d MB\n", m.MemoryUsage)
	fmt.Printf("=============================\n\n")
}

func (br *BenchmarkRunner) SaveResults(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(br.results)
}

func (br *BenchmarkRunner) Stop() error {
	if br.server != nil {
		return br.server.Stop()
	}
	return nil
}

func main() {
	config := BenchmarkConfig{
		MessageSizes:     []int{1024, 4 * 1024, 16 * 1024, 64 * 1024, 256 * 1024, 1024 * 1024}, // 1KB à 1MB
		StreamCounts:     []int{1, 5, 10, 20},                                                  // Nombre de streams par connexion
		ConnectionCounts: []int{1, 5, 10, 25},                                                  // Nombre de connexions simultanées
		Duration:         10 * time.Second,                                                     // Durée de chaque test
		ServerAddr:       "localhost:4243",
	}

	runner := NewBenchmarkRunner(config)
	defer runner.Stop()

	fmt.Println("Démarrage des benchmarks QUIC...")

	if err := runner.RunBenchmarks(); err != nil {
		log.Fatalf("Erreur lors de l'exécution des benchmarks: %v", err)
	}

	// Sauvegarder les résultats
	if err := runner.SaveResults("quic_benchmark_results.json"); err != nil {
		log.Printf("Erreur lors de la sauvegarde: %v", err)
	} else {
		fmt.Println("Résultats sauvegardés dans quic_benchmark_results.json")
	}

	fmt.Println("Benchmarks terminés!")
}
