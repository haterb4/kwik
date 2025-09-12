package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"time"
)

// Même structure Metrics que dans le benchmark principal
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

type BenchmarkAnalyzer struct {
	results []Metrics
}

func NewBenchmarkAnalyzer(filename string) (*BenchmarkAnalyzer, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var results []Metrics
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&results); err != nil {
		return nil, err
	}

	return &BenchmarkAnalyzer{results: results}, nil
}

func (ba *BenchmarkAnalyzer) GenerateReport() {
	fmt.Println("=== RAPPORT D'ANALYSE DES PERFORMANCES QUIC ===")

	ba.printSummaryStats()
	ba.printThroughputAnalysis()
	ba.printLatencyAnalysis()
	ba.printScalabilityAnalysis()
	ba.printResourceUsageAnalysis()
	ba.printRecommendations()
}

func (ba *BenchmarkAnalyzer) printSummaryStats() {
	fmt.Println("📊 STATISTIQUES GÉNÉRALES")
	fmt.Println("─────────────────────────")

	totalTests := len(ba.results)
	totalBytes := int64(0)
	totalDuration := time.Duration(0)
	errorCount := int64(0)

	maxThroughput := 0.0
	minThroughput := 999999.0

	for _, result := range ba.results {
		totalBytes += result.TotalBytes
		totalDuration += result.Duration
		errorCount += result.ErrorCount

		if result.Throughput > maxThroughput {
			maxThroughput = result.Throughput
		}
		if result.Throughput < minThroughput {
			minThroughput = result.Throughput
		}
	}

	avgThroughput := ba.calculateAvgThroughput()

	fmt.Printf("Nombre total de tests: %d\n", totalTests)
	fmt.Printf("Données transférées: %.2f MB\n", float64(totalBytes)/(1024*1024))
	fmt.Printf("Temps total de test: %v\n", totalDuration)
	fmt.Printf("Erreurs totales: %d\n", errorCount)
	fmt.Printf("Débit max: %.2f Mbps\n", maxThroughput)
	fmt.Printf("Débit min: %.2f Mbps\n", minThroughput)
	fmt.Printf("Débit moyen: %.2f Mbps\n", avgThroughput)
	fmt.Printf("Taux d'erreur: %.2f%%\n\n", float64(errorCount)/float64(totalTests)*100)
}

func (ba *BenchmarkAnalyzer) printThroughputAnalysis() {
	fmt.Println("🚀 ANALYSE DE DÉBIT")
	fmt.Println("─────────────────────")

	// Grouper par taille de message
	throughputByMsgSize := make(map[int][]float64)
	for _, result := range ba.results {
		msgSize := int(result.TotalBytes / int64(result.StreamCount*result.ConnectionCount))
		throughputByMsgSize[msgSize] = append(throughputByMsgSize[msgSize], result.Throughput)
	}

	fmt.Println("Débit moyen par taille de message:")
	var sizes []int
	for size := range throughputByMsgSize {
		sizes = append(sizes, size)
	}
	sort.Ints(sizes)

	for _, size := range sizes {
		throughputs := throughputByMsgSize[size]
		avg := ba.calculateMean(throughputs)
		std := ba.calculateStdDev(throughputs, avg)
		fmt.Printf("  %s: %.2f ± %.2f Mbps\n", formatBytes(size), avg, std)
	}
	fmt.Println()
}

func (ba *BenchmarkAnalyzer) printLatencyAnalysis() {
	fmt.Println("⏱️ ANALYSE DE LATENCE")
	fmt.Println("──────────────────────")

	latencies := make([]float64, len(ba.results))
	for i, result := range ba.results {
		latencies[i] = float64(result.FirstByteLatency.Nanoseconds()) / 1e6 // ms
	}

	sort.Float64s(latencies)

	avgLatency := ba.calculateMean(latencies)
	p50 := latencies[len(latencies)/2]
	p95 := latencies[int(float64(len(latencies))*0.95)]
	p99 := latencies[int(float64(len(latencies))*0.99)]

	fmt.Printf("Latence moyenne: %.2f ms\n", avgLatency)
	fmt.Printf("Médiane (P50): %.2f ms\n", p50)
	fmt.Printf("P95: %.2f ms\n", p95)
	fmt.Printf("P99: %.2f ms\n", p99)
	fmt.Println()
}

func (ba *BenchmarkAnalyzer) printScalabilityAnalysis() {
	fmt.Println("📈 ANALYSE DE SCALABILITÉ")
	fmt.Println("──────────────────────────")

	// Analyse par nombre de connexions
	throughputByConnCount := make(map[int][]float64)
	for _, result := range ba.results {
		throughputByConnCount[result.ConnectionCount] = append(
			throughputByConnCount[result.ConnectionCount],
			result.Throughput,
		)
	}

	fmt.Println("Débit par nombre de connexions:")
	var connCounts []int
	for count := range throughputByConnCount {
		connCounts = append(connCounts, count)
	}
	sort.Ints(connCounts)

	for _, count := range connCounts {
		throughputs := throughputByConnCount[count]
		avg := ba.calculateMean(throughputs)
		fmt.Printf("  %d connexions: %.2f Mbps\n", count, avg)
	}

	// Analyse par nombre de streams
	fmt.Println("\nDébit par nombre de streams:")
	throughputByStreamCount := make(map[int][]float64)
	for _, result := range ba.results {
		throughputByStreamCount[result.StreamCount] = append(
			throughputByStreamCount[result.StreamCount],
			result.Throughput,
		)
	}

	var streamCounts []int
	for count := range throughputByStreamCount {
		streamCounts = append(streamCounts, count)
	}
	sort.Ints(streamCounts)

	for _, count := range streamCounts {
		throughputs := throughputByStreamCount[count]
		avg := ba.calculateMean(throughputs)
		fmt.Printf("  %d streams: %.2f Mbps\n", count, avg)
	}
	fmt.Println()
}

func (ba *BenchmarkAnalyzer) printResourceUsageAnalysis() {
	fmt.Println("💾 ANALYSE D'UTILISATION DES RESSOURCES")
	fmt.Println("────────────────────────────────────────")

	totalMemory := int64(0)
	maxMemory := int64(0)

	for _, result := range ba.results {
		totalMemory += result.MemoryUsage
		if result.MemoryUsage > maxMemory {
			maxMemory = result.MemoryUsage
		}
	}

	avgMemory := float64(totalMemory) / float64(len(ba.results))

	fmt.Printf("Utilisation mémoire moyenne: %.2f MB\n", avgMemory)
	fmt.Printf("Pic d'utilisation mémoire: %d MB\n", maxMemory)
	fmt.Println()
}

func (ba *BenchmarkAnalyzer) printRecommendations() {
	fmt.Println("💡 RECOMMANDATIONS")
	fmt.Println("──────────────────")

	avgThroughput := ba.calculateAvgThroughput()
	errorRate := ba.calculateErrorRate()

	if avgThroughput < 100 {
		fmt.Println("⚠️  Débit faible détecté. Considérez:")
		fmt.Println("   • Augmenter la taille des buffers")
		fmt.Println("   • Optimiser la taille des messages")
		fmt.Println("   • Vérifier la configuration réseau")
	}

	if errorRate > 5 {
		fmt.Println("❌ Taux d'erreur élevé détecté. Vérifiez:")
		fmt.Println("   • La stabilité de la connexion réseau")
		fmt.Println("   • Les timeouts de connexion")
		fmt.Println("   • La charge système")
	}

	// Recommandation de configuration optimale
	bestResult := ba.findBestConfiguration()
	fmt.Printf("🎯 Configuration optimale trouvée:\n")
	fmt.Printf("   • %d connexions\n", bestResult.ConnectionCount)
	fmt.Printf("   • %d streams par connexion\n", bestResult.StreamCount)
	fmt.Printf("   • Débit atteint: %.2f Mbps\n", bestResult.Throughput)
	fmt.Println()
}

// Fonctions utilitaires
func (ba *BenchmarkAnalyzer) calculateAvgThroughput() float64 {
	total := 0.0
	for _, result := range ba.results {
		total += result.Throughput
	}
	return total / float64(len(ba.results))
}

func (ba *BenchmarkAnalyzer) calculateErrorRate() float64 {
	totalErrors := int64(0)
	for _, result := range ba.results {
		totalErrors += result.ErrorCount
	}
	return float64(totalErrors) / float64(len(ba.results)) * 100
}

func (ba *BenchmarkAnalyzer) findBestConfiguration() Metrics {
	best := ba.results[0]
	for _, result := range ba.results {
		if result.Throughput > best.Throughput {
			best = result
		}
	}
	return best
}

func (ba *BenchmarkAnalyzer) calculateMean(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func (ba *BenchmarkAnalyzer) calculateStdDev(values []float64, mean float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += (v - mean) * (v - mean)
	}
	variance := sum / float64(len(values))
	return math.Sqrt(variance) // écart-type = racine carrée de la variance
}

func formatBytes(bytes int) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run analyzer.go <results_file.json>")
	}

	filename := os.Args[1]
	analyzer, err := NewBenchmarkAnalyzer(filename)
	if err != nil {
		log.Fatalf("Erreur lors du chargement du fichier: %v", err)
	}

	analyzer.GenerateReport()
}
