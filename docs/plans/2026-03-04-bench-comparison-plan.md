# Bench Comparison Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a `bench` subcommand to test-runner that runs two Envoy configurations through an endpoint replacement scenario under 500 RPS load, then compares latency and error rates using Fortio.

**Architecture:** Sequential orchestration — run the same scenario twice (legacy config, then fixed config), each with a Fortio subprocess generating constant load. Parse Fortio's JSON output for a terminal comparison table. Reuse existing XDSController, BackendPool, and AdminClient infrastructure.

**Tech Stack:** Go (existing module), Fortio CLI (subprocess via `go install fortio.org/fortio@latest`), existing go-control-plane XDS infrastructure.

---

### Task 1: Install Fortio and verify it works

**Files:**
- None (system setup)

**Step 1: Install Fortio**

Run:
```bash
cd /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go install fortio.org/fortio@latest
```

**Step 2: Verify Fortio is available**

Run:
```bash
fortio version
```
Expected: Version string like `1.x.x`

**Step 3: Test Fortio JSON output format**

Run:
```bash
# Start a quick test server and hit it
fortio server &
SERVER_PID=$!
sleep 1
fortio load -qps 100 -t 2s -json /tmp/fortio-test.json http://localhost:8080/echo
kill $SERVER_PID
cat /tmp/fortio-test.json | python3 -m json.tool | head -50
```
Expected: JSON with `DurationHistogram`, `RetCodes`, `Percentiles` fields.

---

### Task 2: Add HCM listener to envoy bootstrap for bench

The existing `envoy.yaml` uses a TCP proxy listener — Fortio needs an HTTP listener to get proper status codes.

**Files:**
- Create: `e2e/stabilization_timeout/envoy-bench.yaml`

**Step 1: Write the HCM bootstrap config**

```yaml
node:
  id: test-node
  cluster: test-cluster

dynamic_resources:
  cds_config:
    api_config_source:
      api_type: GRPC
      transport_api_version: V3
      grpc_services:
        - envoy_grpc:
            cluster_name: xds_cluster

static_resources:
  listeners:
    - name: bench-listener
      address:
        socket_address:
          address: 0.0.0.0
          port_value: 10000
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: bench
                codec_type: AUTO
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: backend
                      domains: ["*"]
                      routes:
                        - match:
                            prefix: "/"
                          route:
                            cluster: test-cluster
                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
    - name: xds_cluster
      type: STATIC
      connect_timeout: 1s
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: xds_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: 127.0.0.1
                      port_value: 5678

admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: 9901
```

**Step 2: Commit**

```bash
git add e2e/stabilization_timeout/envoy-bench.yaml
git commit -m "feat(e2e): add HCM bootstrap config for bench subcommand"
```

---

### Task 3: Update BackendPool to respond on all paths

Currently backends only respond 200 on `/healthz` and 404 on everything else. Fortio will hit `/` — we need backends to respond 200 on the proxy path too.

**Files:**
- Modify: `e2e/stabilization_timeout/backend.go:35-39`

**Step 1: Add catch-all handler**

In `backend.go`, the `Start` method's mux setup currently has only a `/healthz` handler. Add a catch-all that responds 200:

```go
mux := http.NewServeMux()
mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("ok"))
})
mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("ok"))
})
```

**Step 2: Verify existing tests still pass**

Run:
```bash
cd /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go build -o test-runner .
```
Expected: Compiles without errors. (Existing scenarios don't send HTTP through the listener, so no behavior change.)

**Step 3: Commit**

```bash
git add e2e/stabilization_timeout/backend.go
git commit -m "feat(e2e): add catch-all 200 handler to backend pool for bench traffic"
```

---

### Task 4: Add `ignore_health_on_host_removal` support to XDSController

The XDS controller needs to be able to set this field on the Cluster proto for the legacy configuration.

**Files:**
- Modify: `e2e/stabilization_timeout/xds.go`

**Step 1: Add field to XDSController struct**

Add `ignoreHealthOnRemoval bool` to the struct (line ~35-44):

```go
type XDSController struct {
    mu                      sync.Mutex
    endpoints               map[int]bool
    cache                   cachev3.SnapshotCache
    version                 atomic.Int64
    timeoutMs               uint
    healthChecks            bool
    ignoreHealthOnRemoval   bool
    grpcServer              *grpc.Server
    listener                net.Listener
}
```

**Step 2: Update SetClusterConfig to accept the new flag**

Change the signature and implementation:

```go
func (x *XDSController) SetClusterConfig(timeoutMs uint, healthChecks bool, opts ...func(*XDSController)) error {
    x.mu.Lock()
    x.timeoutMs = timeoutMs
    x.healthChecks = healthChecks
    for _, opt := range opts {
        opt(x)
    }
    x.mu.Unlock()
    return x.pushSnapshot()
}
```

Add option function:

```go
func WithIgnoreHealthOnRemoval(v bool) func(*XDSController) {
    return func(x *XDSController) {
        x.ignoreHealthOnRemoval = v
    }
}
```

**Step 3: Update makeCluster to set the field**

In `makeCluster` (around line 160-210), after the existing cluster construction, add:

```go
if x.ignoreHealthOnRemoval {
    c.IgnoreHealthOnHostRemoval = true
}
```

Note: Read the `ignoreHealthOnRemoval` field under the lock in `pushSnapshot`, similar to how `timeoutMs` and `healthChecks` are read. Pass it to `makeCluster`.

Update `pushSnapshot` to read the field:

```go
func (x *XDSController) pushSnapshot() error {
    x.mu.Lock()
    timeoutMs := x.timeoutMs
    hc := x.healthChecks
    ignoreHC := x.ignoreHealthOnRemoval
    ports := make([]int, 0, len(x.endpoints))
    for p := range x.endpoints {
        ports = append(ports, p)
    }
    x.mu.Unlock()

    v := fmt.Sprintf("%d", x.version.Add(1))
    c := x.makeCluster(timeoutMs, hc, ignoreHC)
    // ... rest unchanged
```

Update `makeCluster` signature:

```go
func (x *XDSController) makeCluster(timeoutMs uint, healthChecks bool, ignoreHealthOnRemoval bool) *cluster.Cluster {
    // existing code...
    if ignoreHealthOnRemoval {
        c.IgnoreHealthOnHostRemoval = true
    }
    return c
}
```

**Step 4: Verify compilation**

Run:
```bash
cd /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go build -o test-runner .
```
Expected: Compiles. Existing callers of `SetClusterConfig` don't pass opts, so no change needed.

**Step 5: Commit**

```bash
git add e2e/stabilization_timeout/xds.go
git commit -m "feat(e2e): add ignore_health_on_host_removal support to XDSController"
```

---

### Task 5: Add ReplaceEndpoints method to XDSController

The bench needs an atomic endpoint replacement (remove all old, add all new in one EDS push).

**Files:**
- Modify: `e2e/stabilization_timeout/xds.go`

**Step 1: Add ReplaceEndpoints method**

```go
// ReplaceEndpoints atomically replaces all endpoints with the given set.
func (x *XDSController) ReplaceEndpoints(ports ...int) error {
    x.mu.Lock()
    x.endpoints = make(map[int]bool)
    for _, p := range ports {
        x.endpoints[p] = true
    }
    x.mu.Unlock()
    return x.pushSnapshot()
}
```

**Step 2: Verify compilation**

Run:
```bash
go build -o test-runner .
```

**Step 3: Commit**

```bash
git add e2e/stabilization_timeout/xds.go
git commit -m "feat(e2e): add ReplaceEndpoints for atomic endpoint swap"
```

---

### Task 6: Create fortio.go — Fortio subprocess management and JSON parsing

**Files:**
- Create: `e2e/stabilization_timeout/fortio.go`

**Step 1: Write fortio.go**

```go
package main

import (
    "encoding/json"
    "fmt"
    "os"
    "os/exec"
    "time"
)

// FortioResult represents parsed Fortio JSON output.
type FortioResult struct {
    Labels          string             `json:"Labels"`
    StartTime       string             `json:"StartTime"`
    RequestedQPS    string             `json:"RequestedQPS"`
    ActualQPS       float64            `json:"ActualQPS"`
    ActualDuration  int64              `json:"ActualDuration"` // nanoseconds
    NumThreads      int                `json:"NumThreads"`
    RetCodes        map[string]int     `json:"RetCodes"`
    DurationHist    DurationHistogram  `json:"DurationHistogram"`
}

// DurationHistogram is Fortio's latency histogram.
type DurationHistogram struct {
    Count       int         `json:"Count"`
    Min         float64     `json:"Min"`
    Max         float64     `json:"Max"`
    Sum         float64     `json:"Sum"`
    Avg         float64     `json:"Avg"`
    StdDev      float64     `json:"StdDev"`
    Percentiles []Percentile `json:"Percentiles"`
}

// Percentile is a single percentile entry.
type Percentile struct {
    Percentile float64 `json:"Percentile"`
    Value      float64 `json:"Value"`
}

// FortioRun starts Fortio as a subprocess and returns the result.
// target is the URL to hit (e.g., http://127.0.0.1:10000/).
// qps is requests/second, duration is how long to run, jsonPath is where to save output.
func FortioRun(target string, qps int, duration time.Duration, jsonPath string) (*FortioResult, error) {
    args := []string{
        "load",
        "-qps", fmt.Sprintf("%d", qps),
        "-t", duration.String(),
        "-json", jsonPath,
        "-c", "16",           // 16 concurrent connections
        "-nocatchup",         // don't try to catch up if behind on QPS
        target,
    }

    cmd := exec.Command("fortio", args...)
    cmd.Stdout = os.Stderr // forward fortio output to stderr
    cmd.Stderr = os.Stderr
    if err := cmd.Run(); err != nil {
        return nil, fmt.Errorf("fortio load: %w", err)
    }

    return ParseFortioJSON(jsonPath)
}

// ParseFortioJSON reads and parses a Fortio JSON result file.
func ParseFortioJSON(path string) (*FortioResult, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("read fortio json: %w", err)
    }
    var result FortioResult
    if err := json.Unmarshal(data, &result); err != nil {
        return nil, fmt.Errorf("parse fortio json: %w", err)
    }
    return &result, nil
}

// SuccessRate returns the fraction of 200 responses out of total.
func (r *FortioResult) SuccessRate() float64 {
    total := 0
    ok := 0
    for code, count := range r.RetCodes {
        total += count
        if code == "200" {
            ok += count
        }
    }
    if total == 0 {
        return 0
    }
    return float64(ok) / float64(total)
}

// ErrorCount returns the count of non-200 responses.
func (r *FortioResult) ErrorCount() int {
    total := 0
    ok := 0
    for code, count := range r.RetCodes {
        total += count
        if code == "200" {
            ok += count
        }
    }
    return total - ok
}

// GetPercentile returns the latency at the given percentile (e.g., 50, 90, 99).
func (r *FortioResult) GetPercentile(p float64) float64 {
    target := p / 100.0
    for _, pct := range r.DurationHist.Percentiles {
        if pct.Percentile >= target {
            return pct.Value
        }
    }
    return r.DurationHist.Max
}

// TotalRequests returns the total number of requests sent.
func (r *FortioResult) TotalRequests() int {
    total := 0
    for _, count := range r.RetCodes {
        total += count
    }
    return total
}
```

**Step 2: Verify compilation**

Run:
```bash
go build -o test-runner .
```

**Step 3: Commit**

```bash
git add e2e/stabilization_timeout/fortio.go
git commit -m "feat(e2e): add fortio subprocess management and JSON parsing"
```

---

### Task 7: Create bench.go — Main bench orchestration

**Files:**
- Create: `e2e/stabilization_timeout/bench.go`

**Step 1: Write bench.go**

```go
package main

import (
    "encoding/json"
    "flag"
    "fmt"
    "log"
    "os"
    "path/filepath"
    "time"
)

// BenchConfig holds CLI flags for the bench subcommand.
type BenchConfig struct {
    EnvoyWithFix    string
    EnvoyWithoutFix string
    QPS             int
    OldCount        int
    NewCount        int
    HealthyCount    int
    WarmupDuration  time.Duration
    TransDuration   time.Duration
    OutputDir       string
    AdminPort       int
    XDSPort         int
    ListenerPort    int
    BasePort        int
}

// BenchRunResult holds metrics from one bench run.
type BenchRunResult struct {
    Config      string        `json:"config"`
    Fortio      *FortioResult `json:"fortio"`
    FortioJSON  string        `json:"fortio_json"`
}

// BenchSummary is the final comparison output.
type BenchSummary struct {
    Legacy  *BenchRunResult `json:"legacy"`
    Fixed   *BenchRunResult `json:"fixed"`
    Verdict string          `json:"verdict"`
    Passed  bool            `json:"passed"`
}

func parseBenchFlags(args []string) *BenchConfig {
    fs := flag.NewFlagSet("bench", flag.ExitOnError)
    cfg := &BenchConfig{}
    fs.StringVar(&cfg.EnvoyWithFix, "envoy-with-fix", "./envoy-static-with-fix", "Path to envoy binary with fix")
    fs.StringVar(&cfg.EnvoyWithoutFix, "envoy-without-fix", "./envoy-static-without-fix", "Path to envoy binary without fix")
    fs.IntVar(&cfg.QPS, "qps", 500, "Fortio request rate (requests/second)")
    fs.IntVar(&cfg.OldCount, "old-count", 10, "Number of old endpoints")
    fs.IntVar(&cfg.NewCount, "new-count", 10, "Number of new endpoints")
    fs.IntVar(&cfg.HealthyCount, "healthy-count", 6, "Number of healthy new endpoints (rest are black holes)")
    fs.DurationVar(&cfg.WarmupDuration, "warmup", 5*time.Second, "Steady-state duration before swap")
    fs.DurationVar(&cfg.TransDuration, "transition", 15*time.Second, "Duration after swap")
    fs.StringVar(&cfg.OutputDir, "output-dir", "./bench-results", "Output directory for results")
    fs.IntVar(&cfg.AdminPort, "admin-port", 9901, "Envoy admin port")
    fs.IntVar(&cfg.XDSPort, "xds-port", 5678, "xDS gRPC port")
    fs.IntVar(&cfg.ListenerPort, "listener-port", 10000, "Envoy HTTP listener port")
    fs.IntVar(&cfg.BasePort, "base-port", 8081, "Base port for backend pool")
    fs.Parse(args)
    return cfg
}

func runBench(cfg *BenchConfig) {
    // Validate
    for _, bin := range []string{cfg.EnvoyWithFix, cfg.EnvoyWithoutFix} {
        if _, err := os.Stat(bin); err != nil {
            log.Fatalf("binary not found: %s", bin)
        }
    }
    if cfg.HealthyCount > cfg.NewCount {
        log.Fatalf("healthy-count (%d) > new-count (%d)", cfg.HealthyCount, cfg.NewCount)
    }

    os.MkdirAll(cfg.OutputDir, 0755)

    log.Printf("=== Bench Comparison ===")
    log.Printf("QPS: %d, Old: %d, New: %d (healthy: %d, black holes: %d)",
        cfg.QPS, cfg.OldCount, cfg.NewCount, cfg.HealthyCount, cfg.NewCount-cfg.HealthyCount)
    log.Printf("Warmup: %v, Transition: %v", cfg.WarmupDuration, cfg.TransDuration)

    // Run 1: Legacy (ignore_hc=true, no timeout)
    log.Printf("\n--- Run 1: Legacy (ignore_health_on_host_removal=true, no timeout) ---")
    legacyJSON := filepath.Join(cfg.OutputDir, "legacy.json")
    legacyResult := runBenchScenario(cfg, cfg.EnvoyWithoutFix, true, 0, legacyJSON)

    // Run 2: Fixed (ignore_hc=false, timeout=60s)
    log.Printf("\n--- Run 2: Fixed (ignore_health_on_host_removal=false, timeout=60s) ---")
    fixedJSON := filepath.Join(cfg.OutputDir, "fixed.json")
    fixedResult := runBenchScenario(cfg, cfg.EnvoyWithFix, false, 60000, fixedJSON)

    // Compare
    summary := compareBenchResults(legacyResult, fixedResult, legacyJSON, fixedJSON)
    printBenchSummary(summary)

    // Save summary
    summaryPath := filepath.Join(cfg.OutputDir, "summary.json")
    data, _ := json.MarshalIndent(summary, "", "  ")
    os.WriteFile(summaryPath, data, 0644)
    log.Printf("Results saved to %s/", cfg.OutputDir)

    if !summary.Passed {
        os.Exit(1)
    }
}

func runBenchScenario(cfg *BenchConfig, envoyBin string, ignoreHC bool, timeoutMs uint, jsonPath string) *FortioResult {
    xds := NewXDSController()
    admin := NewAdminClient(cfg.AdminPort)
    backends := NewBackendPool()
    defer backends.StopAll()

    configPath := "envoy-bench.yaml"

    // Start XDS
    if err := xds.Start(cfg.XDSPort); err != nil {
        log.Fatalf("xds start: %v", err)
    }
    defer xds.Stop()

    // Configure cluster
    opts := []func(*XDSController){}
    if ignoreHC {
        opts = append(opts, WithIgnoreHealthOnRemoval(true))
    }
    xds.SetClusterConfig(timeoutMs, true, opts...)

    // Start old backends
    oldPorts := make([]int, cfg.OldCount)
    for i := range oldPorts {
        oldPorts[i] = cfg.BasePort + i
        if err := backends.Start(oldPorts[i]); err != nil {
            log.Fatalf("start old backend %d: %v", oldPorts[i], err)
        }
    }

    // Push old endpoints
    xds.AddEndpoints(oldPorts...)

    // Start envoy
    envoy, err := startEnvoy(envoyBin, configPath, cfg.AdminPort, 99)
    if err != nil {
        log.Fatalf("envoy start: %v", err)
    }
    defer envoy.Stop()

    if err := admin.WaitForReady(15 * time.Second); err != nil {
        log.Fatalf("envoy not ready: %v", err)
    }

    // Wait for old endpoints to become healthy
    for _, port := range oldPorts {
        addr := fmt.Sprintf("127.0.0.1:%d", port)
        if err := admin.WaitForHostHealthy("test-cluster", addr, 10*time.Second); err != nil {
            log.Fatalf("old endpoint %d not healthy: %v", port, err)
        }
    }
    log.Printf("All %d old endpoints healthy", cfg.OldCount)

    // Prepare new backend ports (offset to avoid collision with old)
    newPorts := make([]int, cfg.NewCount)
    for i := range newPorts {
        newPorts[i] = cfg.BasePort + cfg.OldCount + i
    }

    // Start only the healthy new backends (rest are black holes — no listener)
    for i := 0; i < cfg.HealthyCount; i++ {
        if err := backends.Start(newPorts[i]); err != nil {
            log.Fatalf("start new backend %d: %v", newPorts[i], err)
        }
    }
    log.Printf("Started %d healthy new backends, %d black holes",
        cfg.HealthyCount, cfg.NewCount-cfg.HealthyCount)

    // Start Fortio in background
    totalDuration := cfg.WarmupDuration + cfg.TransDuration
    target := fmt.Sprintf("http://127.0.0.1:%d/", cfg.ListenerPort)
    log.Printf("Starting Fortio: %d QPS for %v against %s", cfg.QPS, totalDuration, target)

    fortioDone := make(chan *FortioResult, 1)
    fortioErr := make(chan error, 1)
    go func() {
        result, err := FortioRun(target, cfg.QPS, totalDuration, jsonPath)
        if err != nil {
            fortioErr <- err
        } else {
            fortioDone <- result
        }
    }()

    // Warmup phase
    log.Printf("Warmup: %v with old endpoints...", cfg.WarmupDuration)
    time.Sleep(cfg.WarmupDuration)

    // SWAP: Replace old endpoints with new
    log.Printf("SWAP: Replacing %d old endpoints with %d new endpoints", cfg.OldCount, cfg.NewCount)
    xds.ReplaceEndpoints(newPorts...)

    // Stop old backends (they're no longer in EDS, but stop them to free ports)
    for _, port := range oldPorts {
        backends.Stop(port)
    }

    // Wait for Fortio to complete
    select {
    case result := <-fortioDone:
        return result
    case err := <-fortioErr:
        log.Fatalf("fortio failed: %v", err)
        return nil
    }
}

func compareBenchResults(legacy, fixed *FortioResult, legacyJSON, fixedJSON string) *BenchSummary {
    legacyErrors := legacy.ErrorCount()
    fixedErrors := fixed.ErrorCount()

    passed := fixedErrors < legacyErrors && fixed.SuccessRate() > 0.99
    verdict := "INCONCLUSIVE"
    if passed {
        verdict = fmt.Sprintf("FIXED eliminates transition errors (legacy: %d errors / %.1f%% success, fixed: %d errors / %.1f%% success)",
            legacyErrors, legacy.SuccessRate()*100, fixedErrors, fixed.SuccessRate()*100)
    } else if fixedErrors >= legacyErrors {
        verdict = fmt.Sprintf("UNEXPECTED: fixed config had >= errors than legacy (legacy: %d, fixed: %d)",
            legacyErrors, fixedErrors)
    }

    return &BenchSummary{
        Legacy: &BenchRunResult{
            Config:     "ignore_health_on_host_removal=true, timeout=0",
            Fortio:     legacy,
            FortioJSON: legacyJSON,
        },
        Fixed: &BenchRunResult{
            Config:     "ignore_health_on_host_removal=false, timeout=60s",
            Fortio:     fixed,
            FortioJSON: fixedJSON,
        },
        Verdict: verdict,
        Passed:  passed,
    }
}

func printBenchSummary(s *BenchSummary) {
    fmt.Println()
    fmt.Println("╔═══════════════════════╦══════════════╦══════════════╗")
    fmt.Println("║ Metric                ║ Legacy       ║ Fixed        ║")
    fmt.Println("╠═══════════════════════╬══════════════╬══════════════╣")
    printRow("Total requests", fmt.Sprintf("%d", s.Legacy.Fortio.TotalRequests()), fmt.Sprintf("%d", s.Fixed.Fortio.TotalRequests()))
    printRow("Success rate", fmt.Sprintf("%.1f%%", s.Legacy.Fortio.SuccessRate()*100), fmt.Sprintf("%.1f%%", s.Fixed.Fortio.SuccessRate()*100))
    printRow("p50 latency", fmtLatency(s.Legacy.Fortio.GetPercentile(50)), fmtLatency(s.Fixed.Fortio.GetPercentile(50)))
    printRow("p90 latency", fmtLatency(s.Legacy.Fortio.GetPercentile(90)), fmtLatency(s.Fixed.Fortio.GetPercentile(90)))
    printRow("p99 latency", fmtLatency(s.Legacy.Fortio.GetPercentile(99)), fmtLatency(s.Fixed.Fortio.GetPercentile(99)))
    printRow("Errors", fmt.Sprintf("%d", s.Legacy.Fortio.ErrorCount()), fmt.Sprintf("%d", s.Fixed.Fortio.ErrorCount()))
    fmt.Println("╚═══════════════════════╩══════════════╩══════════════╝")
    fmt.Println()

    if s.Passed {
        fmt.Printf("VERDICT: %s\n", s.Verdict)
    } else {
        fmt.Printf("VERDICT: %s\n", s.Verdict)
    }
}

func printRow(label, legacy, fixed string) {
    fmt.Printf("║ %-21s ║ %-12s ║ %-12s ║\n", label, legacy, fixed)
}

func fmtLatency(seconds float64) string {
    if seconds < 0.001 {
        return fmt.Sprintf("%.0fus", seconds*1_000_000)
    }
    if seconds < 1.0 {
        return fmt.Sprintf("%.1fms", seconds*1000)
    }
    return fmt.Sprintf("%.2fs", seconds)
}
```

**Step 2: Verify compilation**

Run:
```bash
go build -o test-runner .
```

**Step 3: Commit**

```bash
git add e2e/stabilization_timeout/bench.go
git commit -m "feat(e2e): add bench orchestration for comparative endpoint swap testing"
```

---

### Task 8: Wire bench subcommand into main.go

**Files:**
- Modify: `e2e/stabilization_timeout/main.go`

**Step 1: Add subcommand routing**

At the very beginning of `main()`, before `flag.Parse()`, add subcommand detection:

```go
func main() {
    // Check for subcommands before flag.Parse()
    if len(os.Args) > 1 && os.Args[1] == "bench" {
        cfg := parseBenchFlags(os.Args[2:])
        runBench(cfg)
        return
    }

    flag.Parse()
    // ... rest of existing main unchanged
```

**Step 2: Verify compilation**

Run:
```bash
go build -o test-runner .
```

**Step 3: Verify help output**

Run:
```bash
./test-runner bench --help
```
Expected: Shows bench-specific flags.

**Step 4: Commit**

```bash
git add e2e/stabilization_timeout/main.go
git commit -m "feat(e2e): wire bench subcommand into test-runner main"
```

---

### Task 9: Integration test — run the full bench

**Step 1: Run the bench**

```bash
cd /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go build -o test-runner .
./test-runner bench \
    --envoy-with-fix=./envoy-static-with-fix \
    --envoy-without-fix=./envoy-static-without-fix \
    --qps=500 \
    --old-count=10 \
    --new-count=10 \
    --healthy-count=6
```

**Step 2: Verify output**

Expected:
- Legacy run shows errors (503s, connection failures) during transition
- Fixed run shows clean transition (zero or near-zero errors)
- Terminal table shows side-by-side comparison
- `bench-results/` directory contains `legacy.json`, `fixed.json`, `summary.json`
- Exit code 0 if fixed outperforms legacy

**Step 3: Debug and fix any issues**

If Fortio can't connect, check listener port. If no errors in legacy run, may need more aggressive scenario (more black holes, higher QPS). Adjust defaults if needed.

**Step 4: Final commit**

```bash
git add -A e2e/stabilization_timeout/
git commit -m "feat(e2e): complete bench comparison for stabilization timeout"
```
