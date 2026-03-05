# Advanced E2E Test Design — EDS Stabilization Timeout

**Date:** 2026-03-03
**Status:** Approved

## Goal

Comprehensive e2e test suite for the EDS host removal stabilization timeout fix.
Goes beyond basic with/without-fix verification to cover correctness edge cases,
operational confidence, regression safety, and large-scale stress testing.

## Architecture

A single Go binary (`test-runner`) that serves as xDS server, backend pool,
envoy process manager, admin client, and test orchestrator. No external
dependencies beyond the envoy binary itself.

```
test-runner binary
├── xDS server (gRPC :5678)           — CDS/EDS via SnapshotCache
├── Backend HTTP servers (:N dynamic)  — /healthz for envoy HC, managed per-port
├── Admin client                       — polls envoy :9901/clusters, structured parsing
├── Envoy process manager              — starts/stops envoy between scenarios
└── Scenario runner                    — isolated scenarios, then stress test
```

### CLI

```
./test-runner \
  --envoy-with-fix=./envoy-static-with-fix \
  --envoy-without-fix=./envoy-static-without-fix \
  --admin-port=9901 \
  --xds-port=5678 \
  --stress=medium   # none|medium|large
```

### Why Go-owns-everything

- Precise timing assertions (not bash sleep/grep)
- Clean process lifecycle (listener.Close() to simulate HC failure)
- Structured admin parsing (not regex on /clusters text)
- Composable scenarios that run as goroutines for stress test

## File Structure

```
e2e/stabilization_timeout/
├── main.go              # CLI flags, process manager, runs isolated then stress
├── xds.go               # XDSController: snapshot cache, per-endpoint mutations
├── admin.go             # AdminClient: parse /clusters, wait helpers
├── backend.go           # BackendPool: HTTP servers on dynamic ports, start/stop
├── scenarios.go         # Scenario definitions (0, A-H) as composable funcs
├── stress.go            # StressTest: port pool, scheduler, concurrent runner
├── report.go            # Structured results, text + JSON output
├── envoy.yaml           # Bootstrap (static listener + xds_cluster, dynamic CDS)
├── go.mod / go.sum
├── envoy-static-with-fix
├── envoy-static-without-fix
└── test.sh              # go build -o test-runner . && exec ./test-runner "$@"
```

## XDS Controller — Per-Endpoint Mutations

Maintains a thread-safe endpoint set. Scenarios add/remove individual endpoints
without affecting others. Each mutation pushes a new snapshot atomically.

```go
type XDSController struct {
    mu           sync.Mutex
    endpoints    map[int]bool  // port -> present
    cache        cache.SnapshotCache
    version      atomic.Int64
    timeoutMs    uint
    healthChecks bool
}

func (x *XDSController) AddEndpoints(ports ...int) error
func (x *XDSController) RemoveEndpoints(ports ...int) error
func (x *XDSController) SetClusterConfig(timeoutMs uint, healthChecks bool) error
```

## Composable Scenario Interface

Each scenario operates on assigned endpoint slots only. Same interface for
isolated tests and stress test building blocks.

```go
type Scenario struct {
    Name         string
    NumEndpoints int
    Run          func(ctx context.Context, env *ScenarioEnv) error
}

type ScenarioEnv struct {
    Ports    []int          // Backend ports assigned to this scenario
    XDS      *XDSController // Add/remove individual endpoints
    Admin    *AdminClient   // Poll envoy admin, scoped by host:port
    Backends *BackendPool   // Start/stop HTTP backends on assigned ports
}
```

## Admin Client — Structured Parsing

Parses envoy `/clusters` text output into structured data:

```go
type ClusterHost struct {
    Address     string   // "127.0.0.1:8081"
    HealthFlags []string // ["PENDING_DYNAMIC_REMOVAL", "FAILED_ACTIVE_HC"]
    Weight      int
    SuccessRate float64
}

func (a *AdminClient) GetClusterHosts(clusterName string) ([]ClusterHost, error)
func (a *AdminClient) WaitForHostState(addr string, flag string, present bool, timeout time.Duration) error
func (a *AdminClient) WaitForHostGone(addr string, timeout time.Duration) error
func (a *AdminClient) WaitForHostCount(cluster string, count int, timeout time.Duration) error
```

Poll intervals: 100ms for isolated scenarios, 250ms for stress test.

## Isolated Scenarios

Run sequentially. Envoy restarted between scenarios that need different
binaries or clean state.

| # | Name | Binary | Timeout | Tests | Key Assertion |
|---|------|--------|---------|-------|---------------|
| 0 | Baseline without fix | without-fix | 0 | Hosts stuck forever | 2 hosts PENDING_DYNAMIC_REMOVAL after 10s |
| A | Host re-add during stabilization | with-fix | 5000ms | Re-add cancels removal | Hosts healthy after re-add, no removal at 7s |
| B | HC failure during stabilization | with-fix | 5000ms | HC failure pre-empts timeout | Host removed <2s, not at 5s |
| C | Partial removal | with-fix | 5000ms | Only removed host enters stab. | 1 stays healthy, 1 removed after timeout |
| D | Short timeout | with-fix | 500ms | Rapid removal works | Hosts removed within 0.5-2s |
| E | Many hosts (8) | with-fix | 3000ms | Scale — 8 hosts tracked | All 8 removed within window |
| F | xDS disconnect during stab. | with-fix | 5000ms | Timeout survives reconnect | Removed on schedule despite gRPC restart |
| G | No health checks | with-fix | 5000ms | No PENDING_DYNAMIC_REMOVAL | Hosts removed immediately |
| H | Admin fidelity | with-fix | 5000ms | /clusters output correctness | Flags, addresses, health status all correct |

### Scenario Details

**Scenario 0 (Baseline):** Uses without-fix binary. Add 2 endpoints, remove
them, wait 10s. Assert both still present with PENDING_DYNAMIC_REMOVAL flag.

**Scenario A (Re-add):** Add 2 endpoints, remove them, wait 2s (mid-stabilization),
re-add the same endpoints via EDS. Assert hosts exit PENDING_DYNAMIC_REMOVAL
and stay healthy past the 7s mark.

**Scenario B (HC failure):** Add 2 endpoints, remove them via EDS (entering
stabilization). Then close the backend listener on one port (simulating backend
death). Assert that host is removed within 2s (HC failure path) while the other
host remains in PENDING_DYNAMIC_REMOVAL until timeout.

**Scenario C (Partial):** Add 2 endpoints, remove only 1 via EDS. Assert the
removed one enters PENDING_DYNAMIC_REMOVAL and is removed after timeout. Assert
the remaining one stays healthy throughout.

**Scenario D (Short timeout):** Configure timeout=500ms. Add endpoints, remove
them. Assert removal between 0.5s and 2s.

**Scenario E (Many hosts):** Add 8 endpoints, remove all. Assert all 8 enter
PENDING_DYNAMIC_REMOVAL and all are removed within the timeout window (3-5s).

**Scenario F (xDS disconnect):** Add endpoints, remove them (entering
stabilization). Stop the gRPC listener for 2s, restart it. Assert hosts are
still removed on the original timer — envoy's internal clock drives the timeout,
not xDS state.

**Scenario G (No HC):** Push cluster via CDS without HealthChecks field.
Add endpoints, remove them. Assert no PENDING_DYNAMIC_REMOVAL flag — hosts
disappear immediately since there's no health checking to stabilize.

**Scenario H (Admin fidelity):** Add endpoints, trigger various state
transitions. At each step, parse `/clusters` and validate: correct addresses,
correct flags (healthy, PENDING_DYNAMIC_REMOVAL, FAILED_ACTIVE_HC), correct
host count, no stale entries.

## Stress Test

Reuses isolated scenario logic as composable goroutines running against a large
endpoint pool.

### Port Pool

```go
type StressTest struct {
    portPool   chan int        // buffered channel of available ports
    scenarios  []Scenario     // weighted list
    duration   time.Duration
    results    *ConcurrentResults
}
```

The port pool (buffered channel) naturally enforces "at most 1 scenario per
endpoint at a time." Goroutines take ports, run a scenario, return ports.

### Scenario Weights

| Scenario | Weight | Rationale |
|----------|--------|-----------|
| Basic add/remove/timeout | 40% | Core path |
| HC failure | 20% | Backend kill/restart |
| Host re-add | 20% | Cancellation path |
| Partial removal (2 ports) | 15% | Multi-endpoint |
| Long-lived healthy (control) | 5% | Stability baseline |

### Profiles

| Profile | Endpoints | Duration | Purpose |
|---------|-----------|----------|---------|
| `--stress=none` | — | — | Isolated scenarios only |
| `--stress=medium` | 100 | 2min | CI-friendly, catches concurrency bugs |
| `--stress=large` | 1000 | 10min | Full soak, catches leaks and timing drift |

### Stress Flow

1. Start N backends on dynamic ports
2. Start envoy (with-fix, timeout=3000ms)
3. Fill portPool channel with all N ports
4. For the configured duration:
   a. Pick random weighted scenario
   b. Take N ports from portPool (blocks if none available)
   c. Launch goroutine: run scenario, record result, return ports
5. Wait for all goroutines to drain
6. Report per-scenario pass rate, timing distributions, total endpoints exercised

## Report Format

Outputs to both stdout (human-readable) and `test-results.json` (machine-readable).

```
=== EDS Stabilization Timeout E2E Test Report ===
Date: 2026-03-03 17:30:00 UTC
Envoy (with fix):    6f6efef/1.38.0-dev
Envoy (without fix): a6f8f64/1.38.0-dev

--- Isolated Scenarios ---
[PASS]  0. Baseline without fix          1.2s
[PASS]  A. Host re-add during stab.      8.1s
...

--- Stress Test (medium: 100 endpoints, 2min) ---
Scenarios run:     847
  add/remove/timeout:   342 (40.4%)  all passed
  HC failure:           168 (19.8%)  all passed
  ...
Pass rate: 847/847 (100%)
Avg removal time: 3012ms (target: 3000ms, stddev: 48ms)

=== ALL TESTS PASSED ===
```
