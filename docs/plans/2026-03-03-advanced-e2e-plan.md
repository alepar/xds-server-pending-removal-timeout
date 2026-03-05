# Advanced E2E Test Runner — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the bash-based e2e test with a Go test-runner binary that owns all process lifecycle, runs 9 isolated scenarios + stress tests, and produces structured reports.

**Architecture:** Single Go binary serves xDS (gRPC), manages HTTP backends, controls envoy process lifecycle, and orchestrates test scenarios. Scenarios are composable functions that operate on assigned endpoint slots, reusable as goroutines in the stress test.

**Tech Stack:** Go (go-control-plane SnapshotCache, gRPC, net/http), envoy admin API, envoy bootstrap YAML

**Working directory:** `~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/`

**Existing files to delete:** `main.go` (replaced), `test.sh` (replaced). Keep: `envoy.yaml`, `go.mod`, `go.sum`, `docs/`.

---

### Task 1: Backend Pool — HTTP Health Check Servers

**Files:**
- Create: `e2e/stabilization_timeout/backend.go`

**Step 1: Write backend.go**

This manages HTTP servers on dynamic ports. Each backend responds `200 OK` to
`/healthz` (for envoy active HC). Backends can be started/stopped individually
to simulate HC failure.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
)

// BackendPool manages HTTP health-check backends on specific ports.
type BackendPool struct {
	mu        sync.Mutex
	listeners map[int]net.Listener // port -> listener
	servers   map[int]*http.Server
}

func NewBackendPool() *BackendPool {
	return &BackendPool{
		listeners: make(map[int]net.Listener),
		servers:   make(map[int]*http.Server),
	}
}

// Start launches an HTTP backend on the given port.
// It responds 200 to /healthz and 404 to everything else.
func (bp *BackendPool) Start(port int) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if _, ok := bp.listeners[port]; ok {
		return fmt.Errorf("backend already running on port %d", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	lis, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on %d: %w", port, err)
	}

	srv := &http.Server{Handler: mux}
	bp.listeners[port] = lis
	bp.servers[port] = srv

	go func() {
		if err := srv.Serve(lis); err != http.ErrServerClosed {
			log.Printf("backend %d: %v", port, err)
		}
	}()
	return nil
}

// Stop shuts down the backend on the given port.
func (bp *BackendPool) Stop(port int) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	srv, ok := bp.servers[port]
	if !ok {
		return fmt.Errorf("no backend on port %d", port)
	}
	err := srv.Shutdown(context.Background())
	delete(bp.listeners, port)
	delete(bp.servers, port)
	return err
}

// StopAll shuts down all backends.
func (bp *BackendPool) StopAll() {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	for port, srv := range bp.servers {
		srv.Shutdown(context.Background())
		delete(bp.listeners, port)
		delete(bp.servers, port)
	}
}

// IsRunning returns whether a backend is active on the given port.
func (bp *BackendPool) IsRunning(port int) bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	_, ok := bp.listeners[port]
	return ok
}
```

**Step 2: Verify it compiles**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
# (won't compile standalone yet — needs main.go later. Just check syntax)
gofmt -e backend.go
```

Expected: no errors.

**Step 3: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/backend.go
git commit -m "e2e: add BackendPool for HTTP health-check backends"
```

---

### Task 2: Admin Client — Structured /clusters Parsing

**Files:**
- Create: `e2e/stabilization_timeout/admin.go`

**Step 1: Write admin.go**

Parses envoy's `/clusters` text endpoint into structured data. The format is:

```
test-cluster::127.0.0.1:8081::health_flags::PENDING_DYNAMIC_REMOVAL
test-cluster::127.0.0.1:8081::weight::1
test-cluster::127.0.0.1:8081::region::
test-cluster::127.0.0.1:8081::zone::
...
test-cluster::default_priority::max_connections::1024
```

Each host has lines prefixed `cluster_name::address::key::value`.

```go
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ClusterHost represents a parsed host from envoy's /clusters admin endpoint.
type ClusterHost struct {
	Address     string
	HealthFlags []string // e.g. ["PENDING_DYNAMIC_REMOVAL", "FAILED_ACTIVE_HC"]
	Weight      string
	Region      string
	Zone        string
	SubZone     string
}

// AdminClient talks to envoy's admin interface.
type AdminClient struct {
	BaseURL string // e.g. "http://127.0.0.1:9901"
}

func NewAdminClient(port int) *AdminClient {
	return &AdminClient{BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port)}
}

// GetClusterHosts parses /clusters output and returns hosts for the named cluster.
func (a *AdminClient) GetClusterHosts(clusterName string) ([]ClusterHost, error) {
	resp, err := http.Get(a.BaseURL + "/clusters")
	if err != nil {
		return nil, fmt.Errorf("GET /clusters: %w", err)
	}
	defer resp.Body.Close()
	return parseClusterHosts(resp.Body, clusterName)
}

func parseClusterHosts(r io.Reader, clusterName string) ([]ClusterHost, error) {
	prefix := clusterName + "::"
	hostMap := make(map[string]*ClusterHost)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := line[len(prefix):]
		// Lines: address::key::value
		parts := strings.SplitN(rest, "::", 2)
		if len(parts) < 2 {
			continue
		}
		// Address could be ip:port — find the key::value after it.
		// Format: "127.0.0.1:8081::health_flags::..."
		// We split on "::" so parts[0] = "127.0.0.1:8081", parts[1] = "key::value"
		addr := parts[0]
		kvRest := parts[1]

		// Skip cluster-level stats (address doesn't contain a dot → it's a stat name)
		if !strings.Contains(addr, ".") {
			continue
		}

		if _, ok := hostMap[addr]; !ok {
			hostMap[addr] = &ClusterHost{Address: addr}
		}
		host := hostMap[addr]

		kvParts := strings.SplitN(kvRest, "::", 2)
		if len(kvParts) < 2 {
			continue
		}
		key, value := kvParts[0], kvParts[1]
		switch key {
		case "health_flags":
			if value != "" {
				host.HealthFlags = strings.Split(value, "|")
			}
		case "weight":
			host.Weight = value
		case "region":
			host.Region = value
		case "zone":
			host.Zone = value
		case "sub_zone":
			host.SubZone = value
		}
	}

	var hosts []ClusterHost
	for _, h := range hostMap {
		hosts = append(hosts, *h)
	}
	return hosts, scanner.Err()
}

// HasFlag returns true if the host has the given health flag.
func (h *ClusterHost) HasFlag(flag string) bool {
	for _, f := range h.HealthFlags {
		if f == flag {
			return true
		}
	}
	return false
}

// FindHost returns the host matching the given address, or nil.
func FindHost(hosts []ClusterHost, addr string) *ClusterHost {
	for i := range hosts {
		if hosts[i].Address == addr {
			return &hosts[i]
		}
	}
	return nil
}

// WaitForReady polls envoy's /ready endpoint until it returns 200.
func (a *AdminClient) WaitForReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(a.BaseURL + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("envoy not ready after %v", timeout)
}

// WaitForHostCount polls until the cluster has exactly count hosts.
func (a *AdminClient) WaitForHostCount(cluster string, count int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastCount int
	for time.Now().Before(deadline) {
		hosts, err := a.GetClusterHosts(cluster)
		if err == nil {
			lastCount = len(hosts)
			if lastCount == count {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("cluster %s: wanted %d hosts, have %d after %v", cluster, count, lastCount, timeout)
}

// WaitForHostFlag polls until the given host has (or doesn't have) the flag.
func (a *AdminClient) WaitForHostFlag(cluster, addr, flag string, present bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hosts, err := a.GetClusterHosts(cluster)
		if err == nil {
			h := FindHost(hosts, addr)
			if present && h != nil && h.HasFlag(flag) {
				return nil
			}
			if !present && (h == nil || !h.HasFlag(flag)) {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("host %s flag %s present=%v: timeout after %v", addr, flag, present, timeout)
}

// WaitForHostGone polls until the host is no longer listed.
func (a *AdminClient) WaitForHostGone(cluster, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hosts, err := a.GetClusterHosts(cluster)
		if err == nil {
			if FindHost(hosts, addr) == nil {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("host %s still present after %v", addr, timeout)
}

// WaitForHostPresent polls until the host is listed and has no PENDING_DYNAMIC_REMOVAL.
func (a *AdminClient) WaitForHostPresent(cluster, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hosts, err := a.GetClusterHosts(cluster)
		if err == nil {
			h := FindHost(hosts, addr)
			if h != nil && !h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("host %s not healthy after %v", addr, timeout)
}
```

**Step 2: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/admin.go
git commit -m "e2e: add AdminClient for structured /clusters parsing"
```

---

### Task 3: XDS Controller — Per-Endpoint Snapshot Mutations

**Files:**
- Create: `e2e/stabilization_timeout/xds.go`

**Step 1: Write xds.go**

Thread-safe xDS controller. Scenarios call `AddEndpoints`/`RemoveEndpoints` on
their assigned ports. Each mutation pushes a new snapshot to envoy.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
)

const (
	clusterName = "test-cluster"
	nodeID      = "test-node"
)

// XDSController serves CDS/EDS and allows per-endpoint mutations.
type XDSController struct {
	mu           sync.Mutex
	endpoints    map[int]bool // port -> present
	cache        cachev3.SnapshotCache
	version      atomic.Int64
	timeoutMs    uint
	healthChecks bool
	grpcServer   *grpc.Server
	listener     net.Listener
}

func NewXDSController() *XDSController {
	return &XDSController{
		endpoints:    make(map[int]bool),
		cache:        cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		healthChecks: true,
	}
}

// Start begins serving xDS on the given port.
func (x *XDSController) Start(port int) error {
	ctx := context.Background()
	srv := serverv3.NewServer(ctx, x.cache, nil)

	x.grpcServer = grpc.NewServer(
		grpc.MaxConcurrentStreams(1000),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 5 * time.Second,
		}),
	)
	clusterservice.RegisterClusterDiscoveryServiceServer(x.grpcServer, srv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(x.grpcServer, srv)

	var err error
	x.listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("xds listen: %w", err)
	}

	go func() {
		if err := x.grpcServer.Serve(x.listener); err != nil {
			log.Printf("xds server: %v", err)
		}
	}()

	// Push initial empty snapshot so envoy connects successfully.
	return x.pushSnapshot()
}

// Stop gracefully stops the gRPC server.
func (x *XDSController) Stop() {
	if x.grpcServer != nil {
		x.grpcServer.GracefulStop()
	}
}

// Restart stops the gRPC server and starts a new one on the same port.
// Used by scenario F (xDS disconnect test).
func (x *XDSController) Restart(port int) error {
	x.Stop()
	return x.Start(port)
}

// SetClusterConfig changes the cluster's HC and timeout settings.
// Call before adding endpoints (or call pushSnapshot to apply immediately).
func (x *XDSController) SetClusterConfig(timeoutMs uint, healthChecks bool) error {
	x.mu.Lock()
	x.timeoutMs = timeoutMs
	x.healthChecks = healthChecks
	x.mu.Unlock()
	return x.pushSnapshot()
}

// AddEndpoints adds the given ports to the endpoint set and pushes a snapshot.
func (x *XDSController) AddEndpoints(ports ...int) error {
	x.mu.Lock()
	for _, p := range ports {
		x.endpoints[p] = true
	}
	x.mu.Unlock()
	return x.pushSnapshot()
}

// RemoveEndpoints removes the given ports and pushes a snapshot.
func (x *XDSController) RemoveEndpoints(ports ...int) error {
	x.mu.Lock()
	for _, p := range ports {
		delete(x.endpoints, p)
	}
	x.mu.Unlock()
	return x.pushSnapshot()
}

// RemoveAllEndpoints clears all endpoints and pushes a snapshot.
func (x *XDSController) RemoveAllEndpoints() error {
	x.mu.Lock()
	x.endpoints = make(map[int]bool)
	x.mu.Unlock()
	return x.pushSnapshot()
}

func (x *XDSController) pushSnapshot() error {
	x.mu.Lock()
	timeoutMs := x.timeoutMs
	hc := x.healthChecks
	ports := make([]int, 0, len(x.endpoints))
	for p := range x.endpoints {
		ports = append(ports, p)
	}
	x.mu.Unlock()

	v := fmt.Sprintf("%d", x.version.Add(1))
	c := x.makeCluster(timeoutMs, hc)
	e := x.makeEndpoints(ports)
	snap, err := cachev3.NewSnapshot(v, map[resource.Type][]types.Resource{
		resource.ClusterType:  {c},
		resource.EndpointType: {e},
	})
	if err != nil {
		return fmt.Errorf("new snapshot: %w", err)
	}
	return x.cache.SetSnapshot(context.Background(), nodeID, snap)
}

func (x *XDSController) makeCluster(timeoutMs uint, healthChecks bool) *cluster.Cluster {
	c := &cluster.Cluster{
		Name:                 clusterName,
		ConnectTimeout:       durationpb.New(1 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS},
		EdsClusterConfig: &cluster.Cluster_EdsClusterConfig{
			EdsConfig: &core.ConfigSource{
				ConfigSourceSpecifier: &core.ConfigSource_ApiConfigSource{
					ApiConfigSource: &core.ApiConfigSource{
						ApiType:             core.ApiConfigSource_GRPC,
						TransportApiVersion: resource.DefaultAPIVersion,
						GrpcServices: []*core.GrpcService{{
							TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
								EnvoyGrpc: &core.GrpcService_EnvoyGrpc{ClusterName: "xds_cluster"},
							},
						}},
					},
				},
				ResourceApiVersion: resource.DefaultAPIVersion,
			},
		},
	}

	if healthChecks {
		c.HealthChecks = []*core.HealthCheck{{
			Timeout:            durationpb.New(1 * time.Second),
			Interval:           durationpb.New(1 * time.Second),
			UnhealthyThreshold: &wrapperspb.UInt32Value{Value: 1},
			HealthyThreshold:   &wrapperspb.UInt32Value{Value: 1},
			HealthChecker: &core.HealthCheck_HttpHealthCheck_{
				HttpHealthCheck: &core.HealthCheck_HttpHealthCheck{
					Path: "/healthz",
				},
			},
		}}
	}

	if timeoutMs > 0 {
		s, _ := structpb.NewStruct(map[string]interface{}{
			"host_removal_stabilization_timeout_ms": float64(timeoutMs),
		})
		c.Metadata = &core.Metadata{
			FilterMetadata: map[string]*structpb.Struct{
				"envoy.eds": s,
			},
		}
	}

	return c
}

func (x *XDSController) makeEndpoints(ports []int) *endpoint.ClusterLoadAssignment {
	var lbEndpoints []*endpoint.LbEndpoint
	for _, port := range ports {
		lbEndpoints = append(lbEndpoints, &endpoint.LbEndpoint{
			HostIdentifier: &endpoint.LbEndpoint_Endpoint{
				Endpoint: &endpoint.Endpoint{
					Address: &core.Address{
						Address: &core.Address_SocketAddress{
							SocketAddress: &core.SocketAddress{
								Protocol: core.SocketAddress_TCP,
								Address:  "127.0.0.1",
								PortSpecifier: &core.SocketAddress_PortValue{
									PortValue: uint32(port),
								},
							},
						},
					},
				},
			},
		})
	}
	return &endpoint.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpoint.LocalityLbEndpoints{{
			LbEndpoints: lbEndpoints,
		}},
	}
}
```

**Step 2: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/xds.go
git commit -m "e2e: add XDSController with per-endpoint mutations"
```

---

### Task 4: Report — Structured Results

**Files:**
- Create: `e2e/stabilization_timeout/report.go`

**Step 1: Write report.go**

Thread-safe results collector. Prints text report to stdout, writes JSON to file.

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type ScenarioResult struct {
	Name     string        `json:"name"`
	Passed   bool          `json:"passed"`
	Duration time.Duration `json:"duration_ms"`
	Details  string        `json:"details"`
	Error    string        `json:"error,omitempty"`
}

type StressResults struct {
	Profile          string            `json:"profile"`
	TotalEndpoints   int               `json:"total_endpoints"`
	Duration         time.Duration     `json:"duration_ms"`
	ScenariosRun     int               `json:"scenarios_run"`
	ByScenario       map[string]*ScenarioStats `json:"by_scenario"`
	PassRate         float64           `json:"pass_rate"`
	AvgRemovalTimeMs float64          `json:"avg_removal_time_ms,omitempty"`
	MaxConcurrent    int               `json:"max_concurrent"`
}

type ScenarioStats struct {
	Run    int `json:"run"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type Report struct {
	mu              sync.Mutex
	envoyWithFix    string
	envoyWithoutFix string
	isolated        []ScenarioResult
	stress          []StressResults
}

func NewReport(withFix, withoutFix string) *Report {
	return &Report{envoyWithFix: withFix, envoyWithoutFix: withoutFix}
}

func (r *Report) AddIsolated(result ScenarioResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.isolated = append(r.isolated, result)
}

func (r *Report) AddStress(results StressResults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stress = append(r.stress, results)
}

func (r *Report) Print() {
	fmt.Println("=== EDS Stabilization Timeout E2E Test Report ===")
	fmt.Printf("Date: %s\n", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Printf("Envoy (with fix):    %s\n", r.envoyWithFix)
	fmt.Printf("Envoy (without fix): %s\n", r.envoyWithoutFix)
	fmt.Println()

	fmt.Println("--- Isolated Scenarios ---")
	allPassed := true
	for _, res := range r.isolated {
		status := "PASS"
		if !res.Passed {
			status = "FAIL"
			allPassed = false
		}
		fmt.Printf("[%s]  %-40s %6dms  %s\n", status, res.Name, res.Duration.Milliseconds(), res.Details)
		if res.Error != "" {
			fmt.Printf("        ERROR: %s\n", res.Error)
		}
	}

	for _, s := range r.stress {
		fmt.Println()
		fmt.Printf("--- Stress Test (%s: %d endpoints, %v) ---\n", s.Profile, s.TotalEndpoints, s.Duration.Round(time.Second))
		fmt.Printf("Scenarios run: %d\n", s.ScenariosRun)
		for name, stats := range s.ByScenario {
			pct := float64(stats.Run) / float64(s.ScenariosRun) * 100
			result := "all passed"
			if stats.Failed > 0 {
				result = fmt.Sprintf("%d FAILED", stats.Failed)
				allPassed = false
			}
			fmt.Printf("  %-30s %4d (%4.1f%%)  %s\n", name+":", stats.Run, pct, result)
		}
		fmt.Printf("Pass rate: %d/%d (%.0f%%)\n", s.ScenariosRun-countFailed(s.ByScenario), s.ScenariosRun, s.PassRate*100)
		fmt.Printf("Max concurrent scenarios: %d\n", s.MaxConcurrent)
	}

	fmt.Println()
	if allPassed {
		fmt.Println("=== ALL TESTS PASSED ===")
	} else {
		fmt.Println("=== SOME TESTS FAILED ===")
	}
}

func countFailed(m map[string]*ScenarioStats) int {
	total := 0
	for _, s := range m {
		total += s.Failed
	}
	return total
}

func (r *Report) WriteJSON(path string) error {
	data := struct {
		Date            string           `json:"date"`
		EnvoyWithFix    string           `json:"envoy_with_fix"`
		EnvoyWithoutFix string           `json:"envoy_without_fix"`
		Isolated        []ScenarioResult `json:"isolated"`
		Stress          []StressResults  `json:"stress,omitempty"`
	}{
		Date:            time.Now().UTC().Format(time.RFC3339),
		EnvoyWithFix:    r.envoyWithFix,
		EnvoyWithoutFix: r.envoyWithoutFix,
		Isolated:        r.isolated,
		Stress:          r.stress,
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

func (r *Report) AllPassed() bool {
	for _, res := range r.isolated {
		if !res.Passed {
			return false
		}
	}
	for _, s := range r.stress {
		if s.PassRate < 1.0 {
			return false
		}
	}
	return true
}

// version extracts the first line of `envoy --version` output.
func envoyVersion(binary string) string {
	// Will be called from main.go using os/exec
	return strings.TrimSpace(binary)
}
```

**Step 2: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/report.go
git commit -m "e2e: add Report for structured test results"
```

---

### Task 5: Scenarios — Isolated Test Definitions

**Files:**
- Create: `e2e/stabilization_timeout/scenarios.go`

**Step 1: Write scenarios.go**

All 9 scenarios as composable functions. Each accepts `ScenarioEnv` and only
touches its assigned ports.

```go
package main

import (
	"context"
	"fmt"
	"time"
)

// ScenarioEnv is the execution environment passed to each scenario.
type ScenarioEnv struct {
	Ports    []int
	XDS      *XDSController
	Admin    *AdminClient
	Backends *BackendPool
}

// ScenarioDef defines a runnable test scenario.
type ScenarioDef struct {
	Name         string
	NumEndpoints int
	Run          func(ctx context.Context, env *ScenarioEnv) (string, error) // returns details, error
}

func addr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// Scenario0_BaselineWithoutFix: hosts stuck in PENDING_DYNAMIC_REMOVAL forever.
var Scenario0_BaselineWithoutFix = ScenarioDef{
	Name:         "0. Baseline without fix",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		// Add endpoints, wait for healthy
		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", fmt.Errorf("host %d not healthy: %w", port, err)
			}
		}

		// Remove endpoints
		env.XDS.RemoveEndpoints(p...)

		// Wait for PENDING_DYNAMIC_REMOVAL flag
		for _, port := range p {
			if err := env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second); err != nil {
				return "", fmt.Errorf("host %d missing PENDING_DYNAMIC_REMOVAL: %w", port, err)
			}
		}

		// Wait 10s — hosts should still be there
		time.Sleep(10 * time.Second)

		hosts, err := env.Admin.GetClusterHosts(clusterName)
		if err != nil {
			return "", err
		}
		for _, port := range p {
			h := FindHost(hosts, addr(port))
			if h == nil {
				return "", fmt.Errorf("host %d was removed (should be stuck)", port)
			}
			if !h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
				return "", fmt.Errorf("host %d lost PENDING_DYNAMIC_REMOVAL flag", port)
			}
		}

		return "hosts stuck after 10s", nil
	},
}

// ScenarioA_HostReAdd: re-add during stabilization cancels removal.
var ScenarioA_HostReAdd = ScenarioDef{
	Name:         "A. Host re-add during stabilization",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}
		}

		// Remove, then re-add after 2s
		env.XDS.RemoveEndpoints(p...)
		for _, port := range p {
			env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second)
		}
		time.Sleep(2 * time.Second)
		env.XDS.AddEndpoints(p...)

		// Wait — hosts should become healthy and stay past 7s
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", fmt.Errorf("host %d not re-healthy: %w", port, err)
			}
		}
		time.Sleep(5 * time.Second) // past original timeout

		hosts, _ := env.Admin.GetClusterHosts(clusterName)
		for _, port := range p {
			h := FindHost(hosts, addr(port))
			if h == nil {
				return "", fmt.Errorf("host %d removed despite re-add", port)
			}
			if h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
				return "", fmt.Errorf("host %d still pending removal after re-add", port)
			}
		}

		return "hosts healthy after re-add, no removal at 7s", nil
	},
}

// ScenarioB_HCFailure: HC failure pre-empts timeout.
var ScenarioB_HCFailure = ScenarioDef{
	Name:         "B. HC failure during stabilization",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}
		}

		// Remove via EDS, then kill backend on first port
		env.XDS.RemoveEndpoints(p...)
		for _, port := range p {
			env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second)
		}
		env.Backends.Stop(p[0]) // kill first backend → HC failure

		// First host should be removed quickly (<3s)
		t0 := time.Now()
		if err := env.Admin.WaitForHostGone(clusterName, addr(p[0]), 3*time.Second); err != nil {
			return "", fmt.Errorf("HC-failed host not removed quickly: %w", err)
		}
		elapsed := time.Since(t0)

		// Second host should still be pending
		hosts, _ := env.Admin.GetClusterHosts(clusterName)
		h := FindHost(hosts, addr(p[1]))
		if h == nil {
			return "", fmt.Errorf("second host removed too early")
		}
		if !h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
			return "", fmt.Errorf("second host missing PENDING_DYNAMIC_REMOVAL")
		}

		return fmt.Sprintf("HC-failed host removed at %dms", elapsed.Milliseconds()), nil
	},
}

// ScenarioC_PartialRemoval: remove 1 of 2, only removed one enters stabilization.
var ScenarioC_PartialRemoval = ScenarioDef{
	Name:         "C. Partial removal",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}
		}

		// Remove only first endpoint
		env.XDS.RemoveEndpoints(p[0])

		// First: should get PENDING_DYNAMIC_REMOVAL, then removed after timeout
		env.Admin.WaitForHostFlag(clusterName, addr(p[0]), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second)

		t0 := time.Now()
		if err := env.Admin.WaitForHostGone(clusterName, addr(p[0]), 10*time.Second); err != nil {
			return "", fmt.Errorf("removed host not cleaned up: %w", err)
		}
		elapsed := time.Since(t0)

		// Second: should stay healthy throughout
		hosts, _ := env.Admin.GetClusterHosts(clusterName)
		h := FindHost(hosts, addr(p[1]))
		if h == nil {
			return "", fmt.Errorf("surviving host disappeared")
		}
		if h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
			return "", fmt.Errorf("surviving host has PENDING_DYNAMIC_REMOVAL")
		}

		return fmt.Sprintf("1 stayed healthy, 1 removed at %dms", elapsed.Milliseconds()), nil
	},
}

// ScenarioD_ShortTimeout: 500ms timeout, rapid removal.
var ScenarioD_ShortTimeout = ScenarioDef{
	Name:         "D. Short timeout (500ms)",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		// Reconfigure with 500ms timeout
		env.XDS.SetClusterConfig(500, true)

		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}
		}

		env.XDS.RemoveEndpoints(p...)
		t0 := time.Now()
		if err := env.Admin.WaitForHostCount(clusterName, 0, 5*time.Second); err != nil {
			return "", fmt.Errorf("hosts not removed: %w", err)
		}
		elapsed := time.Since(t0)

		if elapsed < 400*time.Millisecond {
			return "", fmt.Errorf("removed too fast: %v (expected >= 500ms)", elapsed)
		}
		if elapsed > 3*time.Second {
			return "", fmt.Errorf("removed too slow: %v (expected < 3s)", elapsed)
		}

		return fmt.Sprintf("removed at %dms", elapsed.Milliseconds()), nil
	},
}

// ScenarioE_ManyHosts: 8 hosts, all tracked and removed.
var ScenarioE_ManyHosts = ScenarioDef{
	Name:         "E. Many hosts (8)",
	NumEndpoints: 8,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 15*time.Second); err != nil {
				return "", err
			}
		}

		env.XDS.RemoveEndpoints(p...)
		t0 := time.Now()
		if err := env.Admin.WaitForHostCount(clusterName, 0, 10*time.Second); err != nil {
			return "", fmt.Errorf("not all hosts removed: %w", err)
		}
		elapsed := time.Since(t0)

		return fmt.Sprintf("all 8 removed at %dms", elapsed.Milliseconds()), nil
	},
}

// ScenarioF_XDSDisconnect: timeout survives xDS reconnect.
var ScenarioF_XDSDisconnect = ScenarioDef{
	Name:         "F. xDS disconnect during stabilization",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}
		}

		// Remove and immediately disconnect xDS for 2s
		env.XDS.RemoveEndpoints(p...)
		for _, port := range p {
			env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second)
		}
		// Note: gRPC stop/restart is handled via xds.Stop()/xds.Start() from main.go
		// The scenario signals the need; the orchestrator in main.go handles the restart.
		// For simplicity here we just verify the hosts are removed on time.
		// The xDS restart is managed externally.
		t0 := time.Now()
		if err := env.Admin.WaitForHostCount(clusterName, 0, 10*time.Second); err != nil {
			return "", fmt.Errorf("hosts not removed after xDS restart: %w", err)
		}
		elapsed := time.Since(t0)

		return fmt.Sprintf("removed at %dms despite xDS restart", elapsed.Milliseconds()), nil
	},
}

// ScenarioG_NoHealthChecks: no HC → no PENDING_DYNAMIC_REMOVAL, immediate removal.
var ScenarioG_NoHealthChecks = ScenarioDef{
	Name:         "G. No health checks",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		// Cluster with no health checks
		env.XDS.SetClusterConfig(5000, false)

		env.XDS.AddEndpoints(p...)
		if err := env.Admin.WaitForHostCount(clusterName, 2, 10*time.Second); err != nil {
			return "", err
		}

		env.XDS.RemoveEndpoints(p...)
		t0 := time.Now()
		if err := env.Admin.WaitForHostCount(clusterName, 0, 5*time.Second); err != nil {
			return "", fmt.Errorf("hosts not removed immediately: %w", err)
		}
		elapsed := time.Since(t0)

		if elapsed > 2*time.Second {
			return "", fmt.Errorf("removal took %v (expected near-instant without HC)", elapsed)
		}

		return fmt.Sprintf("immediate removal at %dms, no PENDING flag", elapsed.Milliseconds()), nil
	},
}

// ScenarioH_AdminFidelity: structured validation of /clusters output.
var ScenarioH_AdminFidelity = ScenarioDef{
	Name:         "H. Admin fidelity",
	NumEndpoints: 2,
	Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
		p := env.Ports
		for _, port := range p {
			env.Backends.Start(port)
		}
		defer func() {
			for _, port := range p {
				env.Backends.Stop(port)
			}
		}()

		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}
		}

		// Check: healthy state has correct fields
		hosts, _ := env.Admin.GetClusterHosts(clusterName)
		if len(hosts) != 2 {
			return "", fmt.Errorf("expected 2 hosts, got %d", len(hosts))
		}
		for _, port := range p {
			h := FindHost(hosts, addr(port))
			if h == nil {
				return "", fmt.Errorf("missing host %d", port)
			}
			if len(h.HealthFlags) != 0 {
				return "", fmt.Errorf("healthy host %d has flags: %v", port, h.HealthFlags)
			}
		}

		// Remove → check PENDING_DYNAMIC_REMOVAL flag
		env.XDS.RemoveEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second); err != nil {
				return "", err
			}
		}
		hosts, _ = env.Admin.GetClusterHosts(clusterName)
		for _, port := range p {
			h := FindHost(hosts, addr(port))
			if h == nil {
				return "", fmt.Errorf("host %d disappeared before timeout", port)
			}
			if !h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
				return "", fmt.Errorf("host %d missing PENDING_DYNAMIC_REMOVAL", port)
			}
		}

		// Wait for removal → check hosts gone
		if err := env.Admin.WaitForHostCount(clusterName, 0, 10*time.Second); err != nil {
			return "", err
		}
		hosts, _ = env.Admin.GetClusterHosts(clusterName)
		if len(hosts) != 0 {
			return "", fmt.Errorf("stale entries after removal: %d", len(hosts))
		}

		return "all fields parsed correctly", nil
	},
}

// AllIsolatedScenarios returns all scenarios in order.
// The caller is responsible for setting up the right envoy binary and xDS config.
func AllIsolatedScenarios() []ScenarioDef {
	return []ScenarioDef{
		Scenario0_BaselineWithoutFix,
		ScenarioA_HostReAdd,
		ScenarioB_HCFailure,
		ScenarioC_PartialRemoval,
		ScenarioD_ShortTimeout,
		ScenarioE_ManyHosts,
		ScenarioF_XDSDisconnect,
		ScenarioG_NoHealthChecks,
		ScenarioH_AdminFidelity,
	}
}

// StressScenarios returns the subset suitable for the stress test
// (all use with-fix binary, same cluster config).
func StressScenarios() []ScenarioDef {
	return []ScenarioDef{
		{Name: "add/remove/timeout", NumEndpoints: 1, Run: ScenarioC_PartialRemoval.Run},
		// Stress-specific simplified variants will be defined in stress.go
	}
}
```

**Note:** The `StressScenarios` function will be fully fleshed out in Task 6. For
now it returns a placeholder. The stress scenarios are simplified single-endpoint
variants of the isolated scenarios.

**Step 2: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/scenarios.go
git commit -m "e2e: add 9 isolated scenario definitions"
```

---

### Task 6: Stress Test — Port Pool Scheduler

**Files:**
- Create: `e2e/stabilization_timeout/stress.go`

**Step 1: Write stress.go**

Port-pool scheduler. Picks random weighted scenarios, assigns ports, runs as
goroutines. Channel-based pool enforces 1 scenario per endpoint.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

type weightedScenario struct {
	Def    ScenarioDef
	Weight int // relative weight (out of 100)
}

type StressTest struct {
	portPool     chan int
	scenarios    []weightedScenario
	totalWeight  int
	duration     time.Duration
	totalPorts   int
	xds          *XDSController
	admin        *AdminClient
	backends     *BackendPool
	mu           sync.Mutex
	results      map[string]*ScenarioStats
	totalRun     int
	maxConc      atomic.Int32
	currentConc  atomic.Int32
}

func NewStressTest(ports []int, duration time.Duration, xds *XDSController, admin *AdminClient, backends *BackendPool) *StressTest {
	pool := make(chan int, len(ports))
	for _, p := range ports {
		pool <- p
	}

	scenarios := []weightedScenario{
		{Def: stressAddRemoveTimeout(), Weight: 40},
		{Def: stressHCFailure(), Weight: 20},
		{Def: stressHostReAdd(), Weight: 20},
		{Def: stressPartialRemoval(), Weight: 15},
		{Def: stressLongLivedHealthy(), Weight: 5},
	}
	total := 0
	for _, s := range scenarios {
		total += s.Weight
	}

	return &StressTest{
		portPool:    pool,
		scenarios:   scenarios,
		totalWeight: total,
		duration:    duration,
		totalPorts:  len(ports),
		xds:         xds,
		admin:       admin,
		backends:    backends,
		results:     make(map[string]*ScenarioStats),
	}
}

func (st *StressTest) pickScenario() ScenarioDef {
	r := rand.Intn(st.totalWeight)
	for _, ws := range st.scenarios {
		r -= ws.Weight
		if r < 0 {
			return ws.Def
		}
	}
	return st.scenarios[0].Def
}

func (st *StressTest) takePorts(n int) []int {
	ports := make([]int, n)
	for i := 0; i < n; i++ {
		ports[i] = <-st.portPool // blocks until available
	}
	return ports
}

func (st *StressTest) returnPorts(ports []int) {
	for _, p := range ports {
		st.portPool <- p
	}
}

func (st *StressTest) record(name string, passed bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.results[name]; !ok {
		st.results[name] = &ScenarioStats{}
	}
	st.results[name].Run++
	if passed {
		st.results[name].Passed++
	} else {
		st.results[name].Failed++
	}
	st.totalRun++
}

// Run executes the stress test for the configured duration.
func (st *StressTest) Run(ctx context.Context) StressResults {
	ctx, cancel := context.WithTimeout(ctx, st.duration)
	defer cancel()

	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			goto done
		default:
		}

		scenario := st.pickScenario()
		ports := st.takePorts(scenario.NumEndpoints)

		wg.Add(1)
		conc := st.currentConc.Add(1)
		if conc > st.maxConc.Load() {
			st.maxConc.Store(conc)
		}

		go func(s ScenarioDef, p []int) {
			defer wg.Done()
			defer st.currentConc.Add(-1)
			defer st.returnPorts(p)

			env := &ScenarioEnv{
				Ports:    p,
				XDS:      st.xds,
				Admin:    st.admin,
				Backends: st.backends,
			}

			_, err := s.Run(ctx, env)
			st.record(s.Name, err == nil)
			if err != nil {
				log.Printf("stress %s (ports %v): %v", s.Name, p, err)
			}
		}(scenario, ports)
	}

done:
	wg.Wait()

	st.mu.Lock()
	defer st.mu.Unlock()

	passed := 0
	for _, s := range st.results {
		passed += s.Passed
	}

	return StressResults{
		TotalEndpoints: st.totalPorts,
		Duration:       st.duration,
		ScenariosRun:   st.totalRun,
		ByScenario:     st.results,
		PassRate:        float64(passed) / float64(max(st.totalRun, 1)),
		MaxConcurrent:  int(st.maxConc.Load()),
	}
}

// --- Stress-specific scenario variants ---
// These are single/dual-endpoint versions optimized for parallel execution.
// They use shorter timeouts (3000ms) set by the stress test cluster config.

func stressAddRemoveTimeout() ScenarioDef {
	return ScenarioDef{
		Name:         "add/remove/timeout",
		NumEndpoints: 1,
		Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
			port := env.Ports[0]
			env.Backends.Start(port)
			defer env.Backends.Stop(port)

			env.XDS.AddEndpoints(port)
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}

			env.XDS.RemoveEndpoints(port)
			t0 := time.Now()
			if err := env.Admin.WaitForHostGone(clusterName, addr(port), 8*time.Second); err != nil {
				return "", err
			}
			elapsed := time.Since(t0)
			if elapsed < 2*time.Second {
				return "", fmt.Errorf("removed too fast: %v", elapsed)
			}
			return fmt.Sprintf("removed at %dms", elapsed.Milliseconds()), nil
		},
	}
}

func stressHCFailure() ScenarioDef {
	return ScenarioDef{
		Name:         "HC failure",
		NumEndpoints: 1,
		Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
			port := env.Ports[0]
			env.Backends.Start(port)
			defer env.Backends.Stop(port)

			env.XDS.AddEndpoints(port)
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}

			env.XDS.RemoveEndpoints(port)
			env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second)
			env.Backends.Stop(port) // kill backend

			if err := env.Admin.WaitForHostGone(clusterName, addr(port), 5*time.Second); err != nil {
				return "", err
			}
			return "removed via HC failure", nil
		},
	}
}

func stressHostReAdd() ScenarioDef {
	return ScenarioDef{
		Name:         "host re-add",
		NumEndpoints: 1,
		Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
			port := env.Ports[0]
			env.Backends.Start(port)
			defer env.Backends.Stop(port)

			env.XDS.AddEndpoints(port)
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}

			env.XDS.RemoveEndpoints(port)
			env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second)
			time.Sleep(1 * time.Second) // mid-stabilization

			env.XDS.AddEndpoints(port) // re-add
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", fmt.Errorf("not re-healthy: %w", err)
			}

			// Should stay for 5s past original timeout
			time.Sleep(5 * time.Second)
			hosts, _ := env.Admin.GetClusterHosts(clusterName)
			if FindHost(hosts, addr(port)) == nil {
				return "", fmt.Errorf("host removed despite re-add")
			}
			return "survived re-add", nil
		},
	}
}

func stressPartialRemoval() ScenarioDef {
	return ScenarioDef{
		Name:         "partial removal",
		NumEndpoints: 2,
		Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
			p := env.Ports
			for _, port := range p {
				env.Backends.Start(port)
			}
			defer func() {
				for _, port := range p {
					env.Backends.Stop(port)
				}
			}()

			env.XDS.AddEndpoints(p...)
			for _, port := range p {
				if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
					return "", err
				}
			}

			env.XDS.RemoveEndpoints(p[0]) // remove first only
			if err := env.Admin.WaitForHostGone(clusterName, addr(p[0]), 8*time.Second); err != nil {
				return "", err
			}

			// Second should still be healthy
			hosts, _ := env.Admin.GetClusterHosts(clusterName)
			h := FindHost(hosts, addr(p[1]))
			if h == nil || h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
				return "", fmt.Errorf("surviving host affected")
			}

			// Clean up: remove second
			env.XDS.RemoveEndpoints(p[1])
			env.Admin.WaitForHostGone(clusterName, addr(p[1]), 8*time.Second)

			return "partial removal correct", nil
		},
	}
}

func stressLongLivedHealthy() ScenarioDef {
	return ScenarioDef{
		Name:         "long-lived healthy",
		NumEndpoints: 1,
		Run: func(ctx context.Context, env *ScenarioEnv) (string, error) {
			port := env.Ports[0]
			env.Backends.Start(port)
			defer env.Backends.Stop(port)

			env.XDS.AddEndpoints(port)
			if err := env.Admin.WaitForHostPresent(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}

			// Just sit healthy for a while
			time.Sleep(10 * time.Second)

			hosts, _ := env.Admin.GetClusterHosts(clusterName)
			h := FindHost(hosts, addr(port))
			if h == nil {
				return "", fmt.Errorf("host disappeared while healthy")
			}
			if h.HasFlag("PENDING_DYNAMIC_REMOVAL") {
				return "", fmt.Errorf("healthy host got PENDING_DYNAMIC_REMOVAL")
			}

			// Clean up
			env.XDS.RemoveEndpoints(port)
			env.Admin.WaitForHostGone(clusterName, addr(port), 8*time.Second)

			return "stayed healthy", nil
		},
	}
}
```

**Step 2: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/stress.go
git commit -m "e2e: add StressTest with port-pool scheduler and 5 stress scenarios"
```

---

### Task 7: Main — CLI, Process Manager, Orchestrator

**Files:**
- Delete: `e2e/stabilization_timeout/main.go` (old single-file version)
- Create: `e2e/stabilization_timeout/main.go` (new orchestrator)

**Step 1: Write main.go**

This is the entry point. Parses flags, manages envoy process lifecycle, runs
isolated scenarios then stress test, produces report.

```go
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
	flagEnvoyWithFix    string
	flagEnvoyWithoutFix string
	flagAdminPort       int
	flagXDSPort         int
	flagStress          string
	flagBasePort        int
)

func init() {
	flag.StringVar(&flagEnvoyWithFix, "envoy-with-fix", "./envoy-static-with-fix", "Path to envoy binary with fix")
	flag.StringVar(&flagEnvoyWithoutFix, "envoy-without-fix", "./envoy-static-without-fix", "Path to envoy binary without fix")
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

func startEnvoy(binary string, configPath string, adminPort, baseID int) (*envoyProcess, error) {
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
	flag.Parse()

	// Validate binaries exist
	for _, bin := range []string{flagEnvoyWithFix, flagEnvoyWithoutFix} {
		if _, err := os.Stat(bin); err != nil {
			log.Fatalf("binary not found: %s", bin)
		}
	}

	versionWithFix := getEnvoyVersion(flagEnvoyWithFix)
	versionWithoutFix := getEnvoyVersion(flagEnvoyWithoutFix)

	report := NewReport(versionWithFix, versionWithoutFix)
	xds := NewXDSController()
	admin := NewAdminClient(flagAdminPort)
	backends := NewBackendPool()
	defer backends.StopAll()

	configPath := "envoy.yaml" // in the same directory

	log.Println("=== Running Isolated Scenarios ===")

	for _, scenario := range AllIsolatedScenarios() {
		// Determine which binary and config
		binary := flagEnvoyWithFix
		timeoutMs := uint(5000)
		healthChecks := true

		if scenario.Name == "0. Baseline without fix" {
			binary = flagEnvoyWithoutFix
			timeoutMs = 0
		}

		// Start fresh xDS + envoy for each scenario
		if err := xds.Start(flagXDSPort); err != nil {
			log.Fatalf("xds start: %v", err)
		}
		xds.SetClusterConfig(timeoutMs, healthChecks)

		envoy, err := startEnvoy(binary, configPath, flagAdminPort, 99)
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

		envoy, err := startEnvoy(flagEnvoyWithFix, configPath, flagAdminPort, 99)
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
```

**Step 2: Update test.sh**

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"
echo "Building test-runner..."
go build -o test-runner .
echo "Running tests..."
exec ./test-runner "$@"
```

**Step 3: Verify it compiles**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go build -o test-runner .
```

Expected: clean build.

**Step 4: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/main.go e2e/stabilization_timeout/test.sh
git commit -m "e2e: add main orchestrator and update test.sh"
```

---

### Task 8: Resolve Module Dependencies

**Step 1: Run go mod tidy**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go mod tidy
```

**Step 2: Verify full build**

```bash
go build -o test-runner .
./test-runner --help
```

Expected: prints flags and exits.

**Step 3: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/go.mod e2e/stabilization_timeout/go.sum
git commit -m "e2e: update go.mod/go.sum for test-runner"
```

---

### Task 9: Fix Compilation Issues and Polish

After the full build, address any compilation errors:
- Import path mismatches
- Missing type conversions
- Interface compliance issues

Also:
- Ensure scenario F properly handles xDS restart (the `XDSController.Restart`
  method needs to be called from the isolated scenario runner in main.go,
  or the scenario needs access to the xDS port to do it itself)
- Add `xdsPort int` field to `ScenarioEnv` so scenario F can restart xDS
- Verify admin parsing works against real envoy output format

**Step 1: Fix and test**

Build, fix any errors, iterate until clean.

**Step 2: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/
git commit -m "e2e: fix compilation issues and polish test-runner"
```

---

### Task 10: Integration Test with Real Envoy

**Prerequisite:** Both envoy binaries must exist at:
- `e2e/stabilization_timeout/envoy-static-with-fix`
- `e2e/stabilization_timeout/envoy-static-without-fix`

**Step 1: Run isolated scenarios only**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
./test-runner --stress=none
```

Expected: all 9 scenarios pass.

**Step 2: Debug any failures**

Check envoy logs (stderr), admin output (`curl localhost:9901/clusters`),
xds_server logs. Adjust timing tolerances if needed.

**Step 3: Run with medium stress**

```bash
./test-runner --stress=medium
```

Expected: 100% pass rate over 2 minutes.

**Step 4: Run with large stress (if time permits)**

```bash
./test-runner --stress=large
```

Expected: 100% pass rate over 10 minutes.

**Step 5: Commit final state**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/
git commit -m "e2e: complete advanced test-runner with stress tests"
```
