# EDS Host Removal Stabilization Timeout — E2E Tests & Benchmarks

End-to-end tests and benchmarks for a proposed Envoy feature: **configurable stabilization timeout for hosts in `PENDING_DYNAMIC_REMOVAL` state**.

## The Problem

When Envoy uses EDS with active health checking and `ignore_health_on_host_removal: false`, hosts removed by an EDS update enter `PENDING_DYNAMIC_REMOVAL` state and wait for a health check failure before being removed. If the host remains healthy (e.g., the backend is still running but being decommissioned), **the host is never removed** — it stays in `PENDING_DYNAMIC_REMOVAL` indefinitely.

The common workaround is `ignore_health_on_host_removal: true`, which removes hosts immediately on EDS update. This avoids the stuck-host problem but introduces a different one: **in-flight requests get 503 errors** because Envoy drops hosts before connections can drain.

## The Fix

A configurable stabilization timeout (`envoy.eds/host_removal_stabilization_timeout_ms` in cluster metadata) that:

1. Lets hosts stay in `PENDING_DYNAMIC_REMOVAL` for a bounded duration, giving in-flight requests time to complete
2. Force-removes hosts after the timeout expires via an HC callback
3. Still allows early removal if health checks fail (host is actually down)

## Prerequisites

- **Go 1.22+** (for building the test harness)
- **Envoy binary** built with the stabilization timeout patch (from [alepar/envoy](https://github.com/alepar/envoy))

## Quick Start

```bash
# Build
go build -o xds_e2e .

# Run all scenarios (requires envoy binary at ./envoy-static)
./xds_e2e -envoy ./envoy-static

# Run with medium stress test (100 endpoints, 2 min)
./xds_e2e -envoy ./envoy-static -stress medium

# Run benchmark comparison (legacy vs fixed)
./xds_e2e bench -envoy ./envoy-static
```

## Test Scenarios

The test harness starts an embedded XDS server (CDS + EDS), manages Envoy as a child process, and runs HTTP backends for health checking. Each scenario gets a fresh Envoy instance.

| Scenario | What it tests | Expected result |
|----------|--------------|-----------------|
| **0. Baseline** | No timeout configured (`timeout=0`) | Hosts stuck in PENDING forever |
| **A. Host re-add** | Re-add endpoint during stabilization | Removal cancelled, host stays healthy |
| **B. HC failure** | Kill backend during stabilization | Host removed immediately (fast path) |
| **C. Partial removal** | Remove 1 of 2 endpoints | Only removed host enters stabilization |
| **D. Short timeout** | 500ms timeout | Hosts removed at ~500ms |
| **E. Many hosts** | 8 endpoints removed at once | All removed after timeout |
| **F. xDS disconnect** | xDS server restart during stabilization | Timeout survives reconnect |
| **G. No health checks** | Cluster without HC configured | Immediate removal (no PENDING state) |
| **H. Admin fidelity** | Validate /clusters output parsing | Flags and states parsed correctly |

### Example Output

```
[PASS]  0. Baseline (no stabilization timeout)    10206ms  hosts stuck after 10s
[PASS]  A. Host re-add during stabilization        7313ms  hosts healthy after re-add
[PASS]  B. HC failure during stabilization          306ms  HC-failed host removed at 101ms
[PASS]  C. Partial removal                         5354ms  1 stayed healthy, 1 removed at 5149ms
[PASS]  D. Short timeout (500ms)                    809ms  removed at 707ms
[PASS]  E. Many hosts (8)                          5055ms  all 8 removed at 5051ms
[PASS]  F. xDS disconnect during stabilization     5351ms  removed at 5146ms despite xDS restart
[PASS]  G. No health checks                         204ms  immediate removal at 101ms
[PASS]  H. Admin fidelity                          5354ms  all fields parsed correctly
```

## Benchmark

The `bench` subcommand runs a load test comparing legacy and fixed behavior using the **same Envoy binary** with different XDS configurations:

| Run | Config | Behavior |
|-----|--------|----------|
| **Legacy** | `ignore_health_on_host_removal: true`, no timeout | Hosts removed immediately on EDS update |
| **Fixed** | `stabilization_timeout: 3s`, HC-driven removal | Hosts drain gracefully before removal |

During each run, the harness:
1. Starts 5 healthy backends behind Envoy
2. Sends sustained HTTP load at 500 QPS
3. Mid-test, swaps endpoints (3 healthy + 2 black holes)
4. Measures error rate and latency throughout

### Example Results

```
╔══════════════════════════════════════════════════════════════╗
║                   Benchmark Comparison                      ║
╠══════════════════════════════════════════════════════════════╣
║ Metric                       │       Legacy │        Fixed ║
╠══════════════════════════════╬══════════════╬══════════════╣
║ Success Rate                 │       98.64% │      100.00% ║
║ Errors                       │          203 │            0 ║
║ p50 Latency                  │        427us │        421us ║
║ p99 Latency                  │        1.8ms │        1.7ms ║
╚══════════════════════════════╩══════════════╩══════════════╝

Return Code Breakdown:
  HTTP 200: legacy=14714  fixed=14945
  HTTP 503: legacy=203  fixed=0
```

Legacy mode produces **203 HTTP 503 errors** during the endpoint swap. The fixed mode produces **zero errors** — the stabilization timeout gives Envoy time to drain connections before removing old hosts.

## Stress Test

The `--stress medium` flag runs a 2-minute stress test with 100 endpoint ports, executing randomized scenario variants concurrently:

- **add/remove/timeout** (40%) — basic timeout verification
- **HC failure** (20%) — health check failure fast path
- **host re-add** (20%) — cancellation of pending removal
- **partial removal** (15%) — mixed active/removed endpoints
- **long-lived healthy** (5%) — verify healthy hosts aren't affected

## File Structure

| File | Purpose |
|------|---------|
| `main.go` | CLI entry point, Envoy process management |
| `scenarios.go` | 9 isolated test scenarios |
| `xds.go` | Embedded XDS server (CDS + EDS via go-control-plane) |
| `admin.go` | Envoy admin API client (`/clusters` parser) |
| `backend.go` | HTTP backend pool for health checks |
| `bench.go` | Load test orchestration and comparison |
| `loadgen.go` | HTTP load generator (configurable QPS) |
| `stress.go` | Concurrent randomized scenario runner |
| `report.go` | JSON and text report generation |
| `envoy.yaml` | Envoy config for isolated scenarios (TCP proxy) |
| `envoy-bench.yaml` | Envoy config for benchmarks (HTTP) |
| `analyze.R` | R script for analyzing benchmark CSV output |

## CLI Reference

```
# Isolated scenarios (default)
./xds_e2e -envoy <path> [-admin-port 9901] [-xds-port 5678] [-base-port 8081]

# With stress test
./xds_e2e -envoy <path> -stress medium|large

# Benchmark comparison
./xds_e2e bench -envoy <path> [-qps 500] [-warmup 10s] [-duration 30s]
                [-healthy-count 3] [-total-count 5]
```
