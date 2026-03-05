package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

var (
	flagEnvoy     string
	flagAdminPort int
	flagXDSPort   int
	flagStress    string
	flagBasePort  int
)

func init() {
	flag.StringVar(&flagEnvoy, "envoy", "./envoy-static", "Path to envoy binary")
	flag.IntVar(&flagAdminPort, "admin-port", 9901, "Envoy admin port")
	flag.IntVar(&flagXDSPort, "xds-port", 5678, "xDS gRPC port")
	flag.StringVar(&flagStress, "stress", "none", "Stress test profile: none|medium|large")
	flag.IntVar(&flagBasePort, "base-port", 8081, "Base port for backend pool")
}

func getEnvoyVersion(binary string) string {
	out, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return "unknown"
}

// envoyProcess manages an envoy child process.
type envoyProcess struct {
	cmd *exec.Cmd
}

func startEnvoy(binary string, configPath string, _, baseID int) (*envoyProcess, error) {
	cmd := exec.Command(binary,
		"-c", configPath,
		"--log-level", "warn",
		"--base-id", fmt.Sprintf("%d", baseID),
	)
	cmd.Stdout = os.Stderr // forward envoy output to stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start envoy: %w", err)
	}
	return &envoyProcess{cmd: cmd}, nil
}

func (ep *envoyProcess) Stop() {
	if ep.cmd != nil && ep.cmd.Process != nil {
		ep.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- ep.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			ep.cmd.Process.Kill()
			<-done
		}
	}
}

func main() {
	// Route "bench" subcommand before flag.Parse() to avoid flag conflicts.
	if len(os.Args) > 1 && os.Args[1] == "bench" {
		cfg := parseBenchFlags(os.Args[2:])
		runBench(cfg)
		return
	}

	flag.Parse()

	if _, err := os.Stat(flagEnvoy); err != nil {
		log.Fatalf("binary not found: %s", flagEnvoy)
	}

	version := getEnvoyVersion(flagEnvoy)

	report := NewReport(version)
	xds := NewXDSController()
	admin := NewAdminClient(flagAdminPort)
	backends := NewBackendPool()
	defer backends.StopAll()

	configPath := "envoy.yaml" // in the same directory

	log.Println("=== Running Isolated Scenarios ===")

	for _, scenario := range AllIsolatedScenarios() {
		// Determine cluster config: baseline uses no timeout + ignoreHealthOnRemoval,
		// all other scenarios use the stabilization timeout.
		timeoutMs := uint(5000)
		healthChecks := true
		var opts []func(*XDSController)

		if scenario.Name == "0. Baseline without fix" {
			timeoutMs = 0
			opts = append(opts, WithIgnoreHealthOnRemoval(true))
		}

		// Start fresh xDS + envoy for each scenario
		if err := xds.Start(flagXDSPort); err != nil {
			log.Fatalf("xds start: %v", err)
		}
		xds.SetClusterConfig(timeoutMs, healthChecks, opts...)

		envoy, err := startEnvoy(flagEnvoy, configPath, flagAdminPort, 99)
		if err != nil {
			log.Fatalf("envoy start: %v", err)
		}

		if err := admin.WaitForReady(15 * time.Second); err != nil {
			envoy.Stop()
			xds.Stop()
			log.Fatalf("envoy not ready: %v", err)
		}

		// Assign ports for the scenario
		ports := make([]int, scenario.NumEndpoints)
		for i := range ports {
			ports[i] = flagBasePort + i
		}

		env := &ScenarioEnv{
			Ports:    ports,
			XDS:      xds,
			Admin:    admin,
			Backends: backends,
		}

		log.Printf("--- %s ---", scenario.Name)
		t0 := time.Now()
		details, runErr := scenario.Run(context.Background(), env)
		elapsed := time.Since(t0)

		result := ScenarioResult{
			Name:     scenario.Name,
			Passed:   runErr == nil,
			Duration: elapsed,
			Details:  details,
		}
		if runErr != nil {
			result.Error = runErr.Error()
			log.Printf("FAIL: %s: %v", scenario.Name, runErr)
		} else {
			log.Printf("PASS: %s (%s)", scenario.Name, details)
		}
		report.AddIsolated(result)

		// Clean up for next scenario
		xds.RemoveAllEndpoints()
		envoy.Stop()
		xds.Stop()
		time.Sleep(500 * time.Millisecond) // let ports settle
	}

	// --- Stress Test ---
	if flagStress != "none" {
		var numEndpoints int
		var duration time.Duration
		switch flagStress {
		case "medium":
			numEndpoints = 100
			duration = 2 * time.Minute
		case "large":
			numEndpoints = 1000
			duration = 10 * time.Minute
		default:
			log.Fatalf("unknown stress profile: %s", flagStress)
		}

		log.Printf("=== Stress Test (%s: %d endpoints, %v) ===", flagStress, numEndpoints, duration)

		if err := xds.Start(flagXDSPort); err != nil {
			log.Fatalf("xds start: %v", err)
		}
		xds.SetClusterConfig(3000, true) // 3s timeout for stress

		envoy, err := startEnvoy(flagEnvoy, configPath, flagAdminPort, 99)
		if err != nil {
			log.Fatalf("envoy start: %v", err)
		}
		if err := admin.WaitForReady(15 * time.Second); err != nil {
			log.Fatalf("envoy not ready: %v", err)
		}

		// Generate port list
		ports := make([]int, numEndpoints)
		for i := range ports {
			ports[i] = flagBasePort + i
		}

		st := NewStressTest(ports, duration, xds, admin, backends)
		stressResults := st.Run(context.Background())
		stressResults.Profile = flagStress
		report.AddStress(stressResults)

		xds.RemoveAllEndpoints()
		envoy.Stop()
		xds.Stop()
	}

	// --- Report ---
	fmt.Println()
	report.Print()
	if err := report.WriteJSON("test-results.json"); err != nil {
		log.Printf("warning: failed to write JSON report: %v", err)
	}

	if !report.AllPassed() {
		os.Exit(1)
	}
}
