package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

// BenchConfig holds CLI flags for the bench subcommand.
type BenchConfig struct {
	Envoy          string
	AdminPort      int
	XDSPort        int
	BasePort       int
	QPS            int
	WarmupDuration time.Duration
	TestDuration   time.Duration
	HealthyCount   int
	TotalCount     int
}

func parseBenchFlags(args []string) *BenchConfig {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfg := &BenchConfig{}

	fs.StringVar(&cfg.Envoy, "envoy", "./envoy-static", "Path to envoy binary")
	fs.IntVar(&cfg.AdminPort, "admin-port", 9901, "Envoy admin port")
	fs.IntVar(&cfg.XDSPort, "xds-port", 5678, "xDS gRPC port")
	fs.IntVar(&cfg.BasePort, "base-port", 8081, "Base port for backend pool")
	fs.IntVar(&cfg.QPS, "qps", 500, "Target QPS for load generator")
	fs.DurationVar(&cfg.WarmupDuration, "warmup", 10*time.Second, "Warmup duration before endpoint swap")
	fs.DurationVar(&cfg.TestDuration, "duration", 30*time.Second, "Total test duration (warmup + transition)")
	fs.IntVar(&cfg.HealthyCount, "healthy-count", 3, "Number of healthy backends after swap")
	fs.IntVar(&cfg.TotalCount, "total-count", 5, "Total backends after swap (healthy + black holes)")

	fs.Parse(args)
	return cfg
}

// runBench orchestrates two sequential benchmark runs and compares results.
func runBench(cfg *BenchConfig) {
	if _, err := os.Stat(cfg.Envoy); err != nil {
		log.Fatalf("binary not found: %s", cfg.Envoy)
	}

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║           EDS Stabilization Timeout Benchmark               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Run 1: Legacy (ignore_health_on_host_removal=true, timeout=0)
	fmt.Println("━━━ Run 1: Legacy (ignore_health_on_host_removal=true) ━━━")
	legacyResult := runBenchScenario(cfg, benchScenarioConfig{
		label:                 "legacy",
		ignoreHealthOnRemoval: true,
		timeoutMs:             0,
	})

	// Run 2: Fixed (ignore_health_on_host_removal=false, timeout=3s)
	fmt.Println()
	fmt.Println("━━━ Run 2: Fixed (stabilization_timeout=3s) ━━━")
	fixedResult := runBenchScenario(cfg, benchScenarioConfig{
		label:                 "fixed",
		ignoreHealthOnRemoval: false,
		ignoreNewHostsUntilHC: true,
		timeoutMs:             3000,
	})

	// Compare and print summary
	fmt.Println()
	compareBenchResults(legacyResult, fixedResult)
}

type benchScenarioConfig struct {
	label                 string
	ignoreHealthOnRemoval bool
	ignoreNewHostsUntilHC bool
	timeoutMs             uint
}

type benchResult struct {
	Label      string
	Load       *LoadResult
	SwapTime   time.Time
	SwapOffset time.Duration
}

func runBenchScenario(cfg *BenchConfig, sc benchScenarioConfig) *benchResult {
	xds := NewXDSController()
	admin := NewAdminClient(cfg.AdminPort)
	backends := NewBackendPool()
	defer backends.StopAll()

	configPath := "envoy-bench.yaml"

	// Start xDS
	if err := xds.Start(cfg.XDSPort); err != nil {
		log.Fatalf("[%s] xds start: %v", sc.label, err)
	}
	defer xds.Stop()

	// Configure cluster
	var opts []func(*XDSController)
	if sc.ignoreHealthOnRemoval {
		opts = append(opts, WithIgnoreHealthOnRemoval(true))
	}
	if sc.ignoreNewHostsUntilHC {
		opts = append(opts, WithIgnoreNewHostsUntilFirstHC(true))
	}
	xds.SetClusterConfig(sc.timeoutMs, true, opts...)

	// Start initial backends (all healthy)
	initialPorts := make([]int, cfg.TotalCount)
	for i := range initialPorts {
		initialPorts[i] = cfg.BasePort + i
	}
	for _, port := range initialPorts {
		if err := backends.Start(port); err != nil {
			log.Fatalf("[%s] backend start %d: %v", sc.label, port, err)
		}
	}

	// Push initial endpoints via EDS
	if err := xds.AddEndpoints(initialPorts...); err != nil {
		log.Fatalf("[%s] add endpoints: %v", sc.label, err)
	}

	// Start Envoy
	envoy, err := startEnvoy(cfg.Envoy, configPath, cfg.AdminPort, 99)
	if err != nil {
		log.Fatalf("[%s] envoy start: %v", sc.label, err)
	}
	defer envoy.Stop()

	if err := admin.WaitForReady(15 * time.Second); err != nil {
		log.Fatalf("[%s] envoy not ready: %v", sc.label, err)
	}

	// Wait for all hosts to become healthy
	for _, port := range initialPorts {
		if err := admin.WaitForHostHealthy(clusterName, addr(port), 15*time.Second); err != nil {
			hosts, herr := admin.GetClusterHosts(clusterName)
			if herr == nil {
				log.Printf("[%s] Cluster state at failure: %+v", sc.label, hosts)
			}
			log.Fatalf("[%s] host %d not healthy: %v", sc.label, port, err)
		}
	}
	log.Printf("[%s] All %d hosts healthy", sc.label, len(initialPorts))

	// Start load generator in background
	target := "http://127.0.0.1:10000/"
	lg := NewLoadGen(target, cfg.QPS, 16)
	loadCtx, loadCancel := context.WithTimeout(context.Background(), cfg.TestDuration)
	defer loadCancel()

	loadDone := make(chan *LoadResult, 1)
	go func() {
		loadDone <- lg.Run(loadCtx)
	}()

	// Warmup phase
	log.Printf("[%s] Warming up for %v...", sc.label, cfg.WarmupDuration)
	time.Sleep(cfg.WarmupDuration)

	// Endpoint swap: replace with new set (healthy-count respond 200, rest are black holes)
	log.Printf("[%s] Swapping endpoints: %d healthy + %d black holes",
		sc.label, cfg.HealthyCount, cfg.TotalCount-cfg.HealthyCount)

	// Start new backends BEFORE the EDS push — healthy ones get real HTTP servers,
	// remaining ports get no listener (black holes / connection refused)
	newPorts := make([]int, cfg.TotalCount)
	for i := range newPorts {
		newPorts[i] = cfg.BasePort + 100 + i // offset to avoid port conflicts
	}
	for i := 0; i < cfg.HealthyCount; i++ {
		if err := backends.Start(newPorts[i]); err != nil {
			log.Printf("[%s] warning: backend start %d: %v", sc.label, newPorts[i], err)
		}
	}
	// Black hole ports: no listener started

	// EDS push: atomically replace old endpoints with new ones.
	swapTime := time.Now()
	if err := xds.ReplaceEndpoints(newPorts...); err != nil {
		log.Fatalf("[%s] replace endpoints: %v", sc.label, err)
	}
	log.Printf("[%s] Endpoints swapped at %s", sc.label, swapTime.Format("15:04:05.000"))

	// Wait for load generator to finish
	log.Printf("[%s] Waiting for load generator to complete...", sc.label)
	loadResult := <-loadDone

	swapOffset := swapTime.Sub(loadResult.StartTime)
	log.Printf("[%s] Load gen done: %d requests, %.1f%% success rate",
		sc.label, loadResult.TotalRequests(), loadResult.SuccessRate()*100)

	// Save CSV results
	csvPath := fmt.Sprintf("bench-result-%s.csv", sc.label)
	if err := loadResult.WriteCSV(csvPath, swapOffset); err != nil {
		log.Printf("[%s] warning: failed to write CSV: %v", sc.label, err)
	} else {
		log.Printf("[%s] Results saved to %s", sc.label, csvPath)
	}

	return &benchResult{
		Label:      sc.label,
		Load:       loadResult,
		SwapTime:   swapTime,
		SwapOffset: swapOffset,
	}
}

func compareBenchResults(legacy, fixed *benchResult) {
	printBenchSummary(legacy, fixed)
}

func printBenchSummary(legacy, fixed *benchResult) {
	w := 30 // column width

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                   Benchmark Comparison                      ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")

	// Header
	fmt.Printf("║ %-28s │ %12s │ %12s ║\n", "Metric", "Legacy", "Fixed")
	fmt.Printf("╠%s╬%s╬%s╣\n", strings.Repeat("═", w), strings.Repeat("═", 14), strings.Repeat("═", 14))

	// QPS
	fmt.Printf("║ %-28s │ %12.1f │ %12.1f ║\n", "Actual QPS",
		legacy.Load.ActualQPS(), fixed.Load.ActualQPS())

	// Total requests
	fmt.Printf("║ %-28s │ %12d │ %12d ║\n", "Total Requests",
		legacy.Load.TotalRequests(), fixed.Load.TotalRequests())

	// Success rate
	fmt.Printf("║ %-28s │ %11.2f%% │ %11.2f%% ║\n", "Success Rate",
		legacy.Load.SuccessRate()*100, fixed.Load.SuccessRate()*100)

	// Error count
	fmt.Printf("║ %-28s │ %12d │ %12d ║\n", "Errors",
		legacy.Load.ErrorCount(), fixed.Load.ErrorCount())

	// Latency percentiles
	for _, pct := range []float64{50, 75, 90, 99} {
		lVal := legacy.Load.GetPercentile(pct)
		fVal := fixed.Load.GetPercentile(pct)
		label := fmt.Sprintf("p%.0f Latency", pct)
		fmt.Printf("║ %-28s │ %12s │ %12s ║\n", label,
			fmtDuration(lVal), fmtDuration(fVal))
	}

	fmt.Printf("╚%s╩%s╩%s╝\n", strings.Repeat("═", w), strings.Repeat("═", 14), strings.Repeat("═", 14))

	// Return codes breakdown
	fmt.Println()
	fmt.Println("Return Code Breakdown:")
	allCodes := make(map[int]bool)
	for code := range legacy.Load.StatusCounts() {
		allCodes[code] = true
	}
	for code := range fixed.Load.StatusCounts() {
		allCodes[code] = true
	}
	codes := make([]int, 0, len(allCodes))
	for code := range allCodes {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	lCounts := legacy.Load.StatusCounts()
	fCounts := fixed.Load.StatusCounts()
	for _, code := range codes {
		label := fmt.Sprintf("HTTP %d", code)
		if code == 0 {
			label = "Conn Error"
		}
		fmt.Printf("  %s: legacy=%d  fixed=%d\n", label, lCounts[code], fCounts[code])
	}
}

// fmtDuration formats a time.Duration for display.
func fmtDuration(d time.Duration) string {
	if d == 0 {
		return "N/A"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.0fus", float64(d.Microseconds()))
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000.0)
	}
	return fmt.Sprintf("%.3fs", d.Seconds())
}
