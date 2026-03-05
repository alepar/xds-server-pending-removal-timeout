package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

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
	Scenarios      []string // names of scenarios to run
}

// Available benchmark scenario presets.
var benchPresets = map[string]benchScenarioConfig{
	"ignore-hc-true": {
		label:                 "ignore-hc-true",
		ignoreHealthOnRemoval: true,
		timeoutMs:             0,
		description:           "ignore_health_on_host_removal=true (immediate removal)",
	},
	"ignore-hc-false": {
		label:                 "ignore-hc-false",
		ignoreHealthOnRemoval: false,
		timeoutMs:             0,
		description:           "ignore_health_on_host_removal=false (hosts linger indefinitely)",
	},
}

// RegisterTimeoutPreset adds a "timeout-Xms" preset dynamically.
func timeoutPreset(ms uint) benchScenarioConfig {
	return benchScenarioConfig{
		label:                 fmt.Sprintf("timeout-%dms", ms),
		ignoreHealthOnRemoval: false,
		ignoreNewHostsUntilHC: true,
		timeoutMs:             ms,
		description:           fmt.Sprintf("host_removal_stabilization_timeout=%dms", ms),
	}
}

func parseBenchFlags(args []string) *BenchConfig {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfg := &BenchConfig{}

	fs.StringVar(&cfg.Envoy, "envoy", "./envoy-static", "Path to envoy binary")
	fs.IntVar(&cfg.AdminPort, "admin-port", 9901, "Envoy admin port")
	fs.IntVar(&cfg.XDSPort, "xds-port", 5678, "xDS gRPC port")
	fs.IntVar(&cfg.BasePort, "base-port", 8081, "Base port for backend pool")
	fs.IntVar(&cfg.QPS, "qps", 500, "Target QPS for load generator")
	fs.DurationVar(&cfg.WarmupDuration, "warmup", 10*time.Second, "Warmup duration")
	fs.DurationVar(&cfg.TestDuration, "duration", 30*time.Second, "Total test duration")
	fs.IntVar(&cfg.HealthyCount, "healthy-count", 3, "Healthy backends after swap")
	fs.IntVar(&cfg.TotalCount, "total-count", 5, "Total backends after swap")

	fs.Parse(args)

	// Remaining positional args are scenario names
	cfg.Scenarios = fs.Args()
	if len(cfg.Scenarios) == 0 {
		cfg.Scenarios = []string{"ignore-hc-true", "ignore-hc-false"}
	}

	return cfg
}

type benchScenarioConfig struct {
	label                 string
	description           string
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

// EndpointRecord holds a point-in-time cluster host count.
type EndpointRecord struct {
	TimestampMs int64
	Count       int
}

func runBench(cfg *BenchConfig) {
	if _, err := os.Stat(cfg.Envoy); err != nil {
		log.Fatalf("binary not found: %s", cfg.Envoy)
	}

	// Resolve scenarios
	var scenarios []benchScenarioConfig
	for _, name := range cfg.Scenarios {
		if sc, ok := benchPresets[name]; ok {
			scenarios = append(scenarios, sc)
		} else if strings.HasPrefix(name, "timeout-") {
			var ms uint
			fmt.Sscanf(name, "timeout-%d", &ms)
			if ms == 0 {
				log.Fatalf("invalid timeout scenario: %s", name)
			}
			scenarios = append(scenarios, timeoutPreset(ms))
		} else {
			log.Fatalf("unknown scenario: %s (available: ignore-hc-true, ignore-hc-false, timeout-<ms>)", name)
		}
	}

	for i, sc := range scenarios {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("━━━ Run %d: %s ━━━\n", i+1, sc.description)
		runBenchScenario(cfg, sc)
	}

	fmt.Println()
	fmt.Println("Generate plot:")
	fmt.Println("  Rscript plot.R bench-result-*.csv")
}

func runBenchScenario(cfg *BenchConfig, sc benchScenarioConfig) {
	xds := NewXDSController()
	admin := NewAdminClient(cfg.AdminPort)
	backends := NewBackendPool()
	defer backends.StopAll()

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
	if err := xds.AddEndpoints(initialPorts...); err != nil {
		log.Fatalf("[%s] add endpoints: %v", sc.label, err)
	}

	// Start Envoy
	envoy, err := startEnvoy(cfg.Envoy, "envoy-bench.yaml", cfg.AdminPort, 99)
	if err != nil {
		log.Fatalf("[%s] envoy start: %v", sc.label, err)
	}
	defer envoy.Stop()

	if err := admin.WaitForReady(15 * time.Second); err != nil {
		log.Fatalf("[%s] envoy not ready: %v", sc.label, err)
	}
	for _, port := range initialPorts {
		if err := admin.WaitForHostHealthy(clusterName, addr(port), 15*time.Second); err != nil {
			log.Fatalf("[%s] host %d not healthy: %v", sc.label, port, err)
		}
	}
	log.Printf("[%s] All %d hosts healthy", sc.label, len(initialPorts))

	// Start endpoint count poller
	pollCtx, pollCancel := context.WithCancel(context.Background())
	epRecords := make(chan []EndpointRecord, 1)
	pollStart := time.Now()
	go func() {
		var records []EndpointRecord
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				epRecords <- records
				return
			case <-ticker.C:
				hosts, err := admin.GetClusterHosts(clusterName)
				if err == nil {
					records = append(records, EndpointRecord{
						TimestampMs: time.Since(pollStart).Milliseconds(),
						Count:       len(hosts),
					})
				}
			}
		}
	}()

	// Start load generator
	lg := NewLoadGen("http://127.0.0.1:10000/", cfg.QPS, 16)
	loadCtx, loadCancel := context.WithTimeout(context.Background(), cfg.TestDuration)
	defer loadCancel()
	loadDone := make(chan *LoadResult, 1)
	go func() { loadDone <- lg.Run(loadCtx) }()

	// Warmup
	log.Printf("[%s] Warming up for %v...", sc.label, cfg.WarmupDuration)
	time.Sleep(cfg.WarmupDuration)

	// Endpoint swap
	log.Printf("[%s] Swapping endpoints: %d healthy + %d black holes",
		sc.label, cfg.HealthyCount, cfg.TotalCount-cfg.HealthyCount)
	newPorts := make([]int, cfg.TotalCount)
	for i := range newPorts {
		newPorts[i] = cfg.BasePort + 100 + i
	}
	for i := 0; i < cfg.HealthyCount; i++ {
		if err := backends.Start(newPorts[i]); err != nil {
			log.Printf("[%s] warning: backend start %d: %v", sc.label, newPorts[i], err)
		}
	}

	swapTime := time.Now()
	swapOffset := swapTime.Sub(pollStart)
	if err := xds.ReplaceEndpoints(newPorts...); err != nil {
		log.Fatalf("[%s] replace endpoints: %v", sc.label, err)
	}
	log.Printf("[%s] Endpoints swapped at %s", sc.label, swapTime.Format("15:04:05.000"))

	// Wait for load gen
	loadResult := <-loadDone
	pollCancel()
	endpoints := <-epRecords

	log.Printf("[%s] %d requests, %.1f%% success, %d errors",
		sc.label, loadResult.TotalRequests(), loadResult.SuccessRate()*100, loadResult.ErrorCount())

	// Write CSV: requests + endpoint counts in one file
	csvPath := fmt.Sprintf("bench-result-%s.csv", sc.label)
	if err := writeBenchCSV(csvPath, loadResult, endpoints, swapOffset, pollStart); err != nil {
		log.Printf("[%s] warning: csv write: %v", sc.label, err)
	} else {
		log.Printf("[%s] Saved to %s", sc.label, csvPath)
	}
}

func writeBenchCSV(path string, load *LoadResult, endpoints []EndpointRecord, swapOffset time.Duration, startTime time.Time) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Fprintf(f, "# swap_ms=%d\n", swapOffset.Milliseconds())

	// Requests section
	fmt.Fprintln(f, "# section=requests")
	fmt.Fprintln(f, "start_ms,duration_us,status")
	for i := range load.Records {
		tMs := load.Records[i].Timestamp.Sub(startTime).Milliseconds()
		fmt.Fprintf(f, "%d,%d,%d\n", tMs, load.Records[i].Latency.Microseconds(), load.Records[i].Status)
	}

	// Endpoints section
	fmt.Fprintln(f, "# section=endpoints")
	fmt.Fprintln(f, "start_ms,count")
	for _, ep := range endpoints {
		fmt.Fprintf(f, "%d,%d\n", ep.TimestampMs, ep.Count)
	}

	return nil
}
