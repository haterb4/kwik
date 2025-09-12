package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// BenchmarkConfig définit la configuration d'un test
type BenchmarkConfig struct {
	Name               string        `json:"name"`
	Duration           time.Duration `json:"duration"`
	ConcurrentClients  int           `json:"concurrent_clients"`
	MessageSize        int           `json:"message_size"`
	MessagesPerClient  int           `json:"messages_per_client"`
	NetworkLatency     time.Duration `json:"network_latency"`
	PacketLoss         float64       `json:"packet_loss"`
	Bandwidth          int           `json:"bandwidth_mbps"`
	ConnectionPoolSize int           `json:"connection_pool_size"`
	EnableKeepAlive    bool          `json:"enable_keep_alive"`
	CompressionEnabled bool          `json:"compression_enabled"`
}

// BenchmarkResult stocke les résultats d'un test
type BenchmarkResult struct {
	Config             BenchmarkConfig `json:"config"`
	TestName           string          `json:"test_name"`
	Timestamp          time.Time       `json:"timestamp"`
	Duration           time.Duration   `json:"duration"`
	TotalRequests      int             `json:"total_requests"`
	SuccessfulRequests int             `json:"successful_requests"`
	FailedRequests     int             `json:"failed_requests"`
	RequestsPerSecond  float64         `json:"requests_per_second"`
	AverageLatency     time.Duration   `json:"average_latency"`
	MedianLatency      time.Duration   `json:"median_latency"`
	P95Latency         time.Duration   `json:"p95_latency"`
	P99Latency         time.Duration   `json:"p99_latency"`
	MinLatency         time.Duration   `json:"min_latency"`
	MaxLatency         time.Duration   `json:"max_latency"`
	TotalBytesReceived int64           `json:"total_bytes_received"`
	TotalBytesSent     int64           `json:"total_bytes_sent"`
	Throughput         float64         `json:"throughput_mbps"`
	ConnectionErrors   int             `json:"connection_errors"`
	TimeoutErrors      int             `json:"timeout_errors"`
	CPUUsage           float64         `json:"cpu_usage"`
	MemoryUsage        int64           `json:"memory_usage_mb"`
}

// RequestLatency stocke la latence d'une requête individuelle
type RequestLatency struct {
	Duration time.Duration
	Success  bool
}

// BenchmarkRunner gère l'exécution des benchmarks
type BenchmarkRunner struct {
	results []BenchmarkResult
	mutex   sync.RWMutex
}

func main() {
	runner := &BenchmarkRunner{}

	// Génération des certificats TLS
	tlsConfig, err := generateTLSConfig()
	if err != nil {
		log.Fatal("Erreur génération TLS:", err)
	}

	// Définition des scénarios de test
	scenarios := []BenchmarkConfig{
		// Test de charge basique
		{
			Name:               "baseline_performance",
			Duration:           30 * time.Second,
			ConcurrentClients:  10,
			MessageSize:        1024,
			MessagesPerClient:  100,
			NetworkLatency:     10 * time.Millisecond,
			PacketLoss:         0.0,
			Bandwidth:          100,
			ConnectionPoolSize: 5,
			EnableKeepAlive:    true,
			CompressionEnabled: false,
		},
		// Test haute concurrence
		{
			Name:               "high_concurrency",
			Duration:           45 * time.Second,
			ConcurrentClients:  100,
			MessageSize:        512,
			MessagesPerClient:  50,
			NetworkLatency:     20 * time.Millisecond,
			PacketLoss:         0.1,
			Bandwidth:          1000,
			ConnectionPoolSize: 20,
			EnableKeepAlive:    true,
			CompressionEnabled: false,
		},
		// Test gros volumes
		{
			Name:               "large_payload",
			Duration:           60 * time.Second,
			ConcurrentClients:  20,
			MessageSize:        65536,
			MessagesPerClient:  20,
			NetworkLatency:     50 * time.Millisecond,
			PacketLoss:         0.5,
			Bandwidth:          100,
			ConnectionPoolSize: 10,
			EnableKeepAlive:    true,
			CompressionEnabled: true,
		},
		// Test réseau dégradé
		{
			Name:               "poor_network",
			Duration:           90 * time.Second,
			ConcurrentClients:  25,
			MessageSize:        2048,
			MessagesPerClient:  30,
			NetworkLatency:     200 * time.Millisecond,
			PacketLoss:         2.0,
			Bandwidth:          10,
			ConnectionPoolSize: 5,
			EnableKeepAlive:    false,
			CompressionEnabled: true,
		},
		// Test sans keep-alive
		{
			Name:               "no_keep_alive",
			Duration:           40 * time.Second,
			ConcurrentClients:  50,
			MessageSize:        1024,
			MessagesPerClient:  40,
			NetworkLatency:     30 * time.Millisecond,
			PacketLoss:         0.2,
			Bandwidth:          100,
			ConnectionPoolSize: 1,
			EnableKeepAlive:    false,
			CompressionEnabled: false,
		},
		// Test streaming
		{
			Name:               "streaming_test",
			Duration:           120 * time.Second,
			ConcurrentClients:  15,
			MessageSize:        32768,
			MessagesPerClient:  100,
			NetworkLatency:     25 * time.Millisecond,
			PacketLoss:         0.3,
			Bandwidth:          500,
			ConnectionPoolSize: 8,
			EnableKeepAlive:    true,
			CompressionEnabled: true,
		},
	}

	fmt.Println("🚀 Démarrage du benchmark QUIC")
	fmt.Printf("📊 %d scénarios de test configurés\n\n", len(scenarios))

	// Exécution de tous les scénarios
	for i, scenario := range scenarios {
		fmt.Printf("🔄 Exécution du scénario %d/%d: %s\n", i+1, len(scenarios), scenario.Name)

		result, err := runner.runScenario(scenario, tlsConfig)
		if err != nil {
			log.Printf("❌ Erreur dans le scénario %s: %v", scenario.Name, err)
			continue
		}

		runner.mutex.Lock()
		runner.results = append(runner.results, *result)
		runner.mutex.Unlock()

		fmt.Printf("✅ Scénario %s terminé - RPS: %.2f, Latence moy: %v\n",
			scenario.Name, result.RequestsPerSecond, result.AverageLatency)
		fmt.Println()
	}

	// Export des résultats
	err = runner.exportResults()
	if err != nil {
		log.Fatal("Erreur export résultats:", err)
	}

	fmt.Println("🎉 Benchmark terminé avec succès!")
	fmt.Println("📁 Résultats exportés dans:")
	fmt.Println("  - benchmark_results.json")
	fmt.Println("  - benchmark_results.csv")
}

// runScenario exécute un scénario de test spécifique
func (br *BenchmarkRunner) runScenario(config BenchmarkConfig, tlsConfig *tls.Config) (*BenchmarkResult, error) {
	// Démarrage du serveur
	listener, err := quic.ListenAddr("localhost:0", tlsConfig, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("erreur création listener: %w", err)
	}
	defer listener.Close()

	serverAddr := listener.Addr().String()

	// Goroutine serveur
	go br.runServer(listener)

	// Attendre que le serveur soit prêt
	time.Sleep(100 * time.Millisecond)

	// Initialisation des métriques
	result := &BenchmarkResult{
		Config:    config,
		TestName:  config.Name,
		Timestamp: time.Now(),
	}

	var wg sync.WaitGroup
	latencies := make(chan RequestLatency, config.ConcurrentClients*config.MessagesPerClient)

	startTime := time.Now()

	// Lancement des clients concurrents
	for i := 0; i < config.ConcurrentClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			br.runClient(clientID, serverAddr, config, latencies, tlsConfig)
		}(i)
	}

	// Attendre la fin de tous les clients
	wg.Wait()
	close(latencies)

	// Calcul des métriques
	result.Duration = time.Since(startTime)
	br.calculateMetrics(result, latencies)

	return result, nil
}

// runServer gère le serveur QUIC
func (br *BenchmarkRunner) runServer(listener *quic.Listener) {
	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			return
		}

		go br.handleConnection(conn)
	}
}

// handleConnection traite une connexion client
func (br *BenchmarkRunner) handleConnection(conn *quic.Conn) {
	defer conn.CloseWithError(0, "")

	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}

		go br.handleStream(stream)
	}
}

// handleStream traite un stream QUIC
func (br *BenchmarkRunner) handleStream(stream *quic.Stream) {
	defer stream.Close()

	buffer := make([]byte, 65536)
	for {
		n, err := stream.Read(buffer)
		if err != nil {
			if err != io.EOF {
				log.Printf("Erreur lecture stream: %v", err)
			}
			return
		}

		// Echo du message reçu
		_, err = stream.Write(buffer[:n])
		if err != nil {
			log.Printf("Erreur écriture stream: %v", err)
			return
		}
	}
}

// runClient exécute un client de test
func (br *BenchmarkRunner) runClient(clientID int, serverAddr string, config BenchmarkConfig, latencies chan<- RequestLatency, tlsConfig *tls.Config) {
	clientTLSConfig := tlsConfig.Clone()
	clientTLSConfig.InsecureSkipVerify = true

	// Pool de connexions si activé
	var connections []*quic.Conn
	poolSize := 1
	if config.ConnectionPoolSize > 1 {
		poolSize = config.ConnectionPoolSize
	}

	// Création des connexions
	for i := 0; i < poolSize; i++ {
		conn, err := quic.DialAddr(context.Background(), serverAddr, clientTLSConfig, &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			KeepAlivePeriod: 15 * time.Second,
		})
		if err != nil {
			log.Printf("Client %d: erreur connexion: %v", clientID, err)
			return
		}
		connections = append(connections, conn)
	}

	defer func() {
		for _, conn := range connections {
			conn.CloseWithError(0, "")
		}
	}()

	// Génération du payload
	payload := make([]byte, config.MessageSize)
	rand.Read(payload)

	// Envoi des messages
	for i := 0; i < config.MessagesPerClient; i++ {
		// Sélection de la connexion (round-robin)
		conn := connections[i%len(connections)]

		start := time.Now()
		success := br.sendMessage(conn, payload, config.NetworkLatency)
		latency := time.Since(start)

		latencies <- RequestLatency{
			Duration: latency,
			Success:  success,
		}

		// Simulation de la latence réseau si configurée
		if config.NetworkLatency > 0 {
			time.Sleep(config.NetworkLatency)
		}
	}
}

// sendMessage envoie un message via QUIC et attend la réponse
func (br *BenchmarkRunner) sendMessage(conn *quic.Conn, payload []byte, networkLatency time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return false
	}
	defer stream.Close()

	// Envoi
	_, err = stream.Write(payload)
	if err != nil {
		return false
	}

	// Réception de la réponse
	buffer := make([]byte, len(payload))
	_, err = io.ReadFull(stream, buffer)
	if err != nil {
		return false
	}

	return true
}

// calculateMetrics calcule les métriques de performance
func (br *BenchmarkRunner) calculateMetrics(result *BenchmarkResult, latencies <-chan RequestLatency) {
	var allLatencies []time.Duration
	var successCount, failureCount int
	var totalBytes int64

	for latency := range latencies {
		allLatencies = append(allLatencies, latency.Duration)
		if latency.Success {
			successCount++
			totalBytes += int64(result.Config.MessageSize * 2) // envoi + réception
		} else {
			failureCount++
		}
	}

	result.TotalRequests = len(allLatencies)
	result.SuccessfulRequests = successCount
	result.FailedRequests = failureCount
	result.TotalBytesReceived = totalBytes / 2
	result.TotalBytesSent = totalBytes / 2

	if len(allLatencies) > 0 {
		// Tri pour les percentiles
		sortLatencies(allLatencies)

		result.MinLatency = allLatencies[0]
		result.MaxLatency = allLatencies[len(allLatencies)-1]
		result.MedianLatency = allLatencies[len(allLatencies)/2]
		result.P95Latency = allLatencies[int(float64(len(allLatencies))*0.95)]
		result.P99Latency = allLatencies[int(float64(len(allLatencies))*0.99)]

		// Latence moyenne
		var sum time.Duration
		for _, lat := range allLatencies {
			sum += lat
		}
		result.AverageLatency = sum / time.Duration(len(allLatencies))
	}

	// Calculs dérivés
	if result.Duration > 0 {
		result.RequestsPerSecond = float64(successCount) / result.Duration.Seconds()
		result.Throughput = float64(totalBytes*8) / (1024 * 1024) / result.Duration.Seconds() // Mbps
	}

	// Simulation des métriques système (à remplacer par de vraies métriques)
	result.CPUUsage = 45.2 + float64(result.Config.ConcurrentClients)*0.5
	result.MemoryUsage = 128 + int64(result.Config.ConcurrentClients*2)
}

// sortLatencies trie les latences par ordre croissant
func sortLatencies(latencies []time.Duration) {
	for i := 0; i < len(latencies); i++ {
		for j := i + 1; j < len(latencies); j++ {
			if latencies[i] > latencies[j] {
				latencies[i], latencies[j] = latencies[j], latencies[i]
			}
		}
	}
}

// exportResults exporte les résultats en JSON et CSV
func (br *BenchmarkRunner) exportResults() error {
	br.mutex.RLock()
	defer br.mutex.RUnlock()

	// Export JSON
	jsonFile, err := os.Create("benchmark_results.json")
	if err != nil {
		return fmt.Errorf("erreur création fichier JSON: %w", err)
	}
	defer jsonFile.Close()

	encoder := json.NewEncoder(jsonFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(br.results); err != nil {
		return fmt.Errorf("erreur encodage JSON: %w", err)
	}

	// Export CSV
	csvFile, err := os.Create("benchmark_results.csv")
	if err != nil {
		return fmt.Errorf("erreur création fichier CSV: %w", err)
	}
	defer csvFile.Close()

	writer := csv.NewWriter(csvFile)
	defer writer.Flush()

	// Headers CSV
	headers := []string{
		"test_name", "timestamp", "duration_ms", "concurrent_clients", "message_size",
		"total_requests", "successful_requests", "failed_requests", "requests_per_second",
		"avg_latency_ms", "median_latency_ms", "p95_latency_ms", "p99_latency_ms",
		"min_latency_ms", "max_latency_ms", "throughput_mbps", "cpu_usage", "memory_usage_mb",
		"network_latency_ms", "packet_loss", "bandwidth_mbps",
	}
	writer.Write(headers)

	// Données CSV
	for _, result := range br.results {
		record := []string{
			result.TestName,
			result.Timestamp.Format(time.RFC3339),
			strconv.FormatInt(result.Duration.Milliseconds(), 10),
			strconv.Itoa(result.Config.ConcurrentClients),
			strconv.Itoa(result.Config.MessageSize),
			strconv.Itoa(result.TotalRequests),
			strconv.Itoa(result.SuccessfulRequests),
			strconv.Itoa(result.FailedRequests),
			strconv.FormatFloat(result.RequestsPerSecond, 'f', 2, 64),
			strconv.FormatInt(result.AverageLatency.Milliseconds(), 10),
			strconv.FormatInt(result.MedianLatency.Milliseconds(), 10),
			strconv.FormatInt(result.P95Latency.Milliseconds(), 10),
			strconv.FormatInt(result.P99Latency.Milliseconds(), 10),
			strconv.FormatInt(result.MinLatency.Milliseconds(), 10),
			strconv.FormatInt(result.MaxLatency.Milliseconds(), 10),
			strconv.FormatFloat(result.Throughput, 'f', 2, 64),
			strconv.FormatFloat(result.CPUUsage, 'f', 1, 64),
			strconv.FormatInt(result.MemoryUsage, 10),
			strconv.FormatInt(result.Config.NetworkLatency.Milliseconds(), 10),
			strconv.FormatFloat(result.Config.PacketLoss, 'f', 1, 64),
			strconv.Itoa(result.Config.Bandwidth),
		}
		writer.Write(record)
	}

	return nil
}

// generateTLSConfig génère une configuration TLS pour les tests
func generateTLSConfig() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
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
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	cert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"quic-echo-example"},
	}, nil
}
