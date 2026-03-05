# Bench Comparison: Stabilization Timeout vs Legacy Behavior

**Date:** 2026-03-04
**Status:** Approved

## Problem

The existing e2e suite validates correctness (hosts get removed after timeout, re-add cancels removal, etc.) but doesn't demonstrate the real-world traffic impact of the fix. The key value proposition is:

- **Legacy behavior** (`ignore_health_on_host_removal=true`, no stabilization timeout): When endpoints get replaced, old ones are removed immediately. New ones haven't been health-checked or had connections warmed. Envoy enters `lb_panic` mode, sending traffic to all endpoints including unhealthy ones. Result: 503s, high latency, connection errors.

- **Fixed behavior** (`ignore_health_on_host_removal=false`, `stabilization_timeout=60s`): Old endpoints stay active during the timeout window. New endpoints get health-checked and connection pools warm up. When old endpoints finally get removed, the new ones are already stable. Result: zero disruption.

## Design

### Subcommand

New `bench` subcommand added to the existing `test-runner` binary:

```
test-runner bench [flags]
```

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--envoy-with-fix` | (required) | Path to envoy binary with stabilization timeout fix |
| `--envoy-without-fix` | (required) | Path to envoy binary without fix |
| `--qps` | 500 | Fortio request rate (requests/second) |
| `--old-count` | 10 | Number of old endpoints |
| `--new-count` | 10 | Number of new endpoints |
| `--healthy-count` | 6 | How many new endpoints are healthy (remainder are black holes) |
| `--warmup-duration` | 5s | Steady-state period before endpoint swap |
| `--transition-duration` | 15s | How long Fortio runs after the swap |
| `--output-dir` | ./bench-results/ | Directory for Fortio JSON output and summary |

### Test Scenario

Each configuration runs the same scenario sequentially:

```
Time:  0s          5s              20s          25s
       |-- warm-up --|-- SWAP --------|-- drain --|

Old endpoints (all healthy) → EDS push replaces with new endpoints
                               (healthy-count respond 200,
                                remainder have no listener)
```

### Per-Run Orchestration

1. Start old backends (all respond 200 on `/healthz` and proxy path)
2. Start Envoy with run-specific config
3. Push cluster + old endpoints via CDS/EDS, wait for healthy
4. Start Fortio at `--qps` RPS against Envoy's HTTP listener
5. Wait `--warmup-duration`
6. Start new backends (healthy ones only — unhealthy ports have no listener)
7. Push new endpoints via EDS (complete replacement)
8. Wait `--transition-duration`
9. Stop Fortio, save JSON to output-dir
10. Teardown Envoy + all backends

### Run Configurations

**Run 1 — "Legacy":**
- Uses `--envoy-without-fix` binary
- CDS config: `ignore_health_on_host_removal=true`, no stabilization timeout metadata
- Envoy bootstrap with HTTP connection manager listener

**Run 2 — "Fixed":**
- Uses `--envoy-with-fix` binary
- CDS config: `ignore_health_on_host_removal=false`, `stabilization_timeout=60000` (60s) in EDS filter metadata
- Same bootstrap config

### Fortio Integration

- Invoked as subprocess: `fortio load` with `--qps`, `--json`, `-t` (duration) flags
- Target: Envoy's HTTP listener (HCM route → test cluster)
- Two output files: `legacy.json` and `fixed.json` in `--output-dir`
- Fortio's JSON parsed for terminal summary

### Envoy Bootstrap

The bench needs an HTTP connection manager (HCM) listener instead of the existing TCP proxy listener, so that:
- Fortio gets proper HTTP responses with status codes
- 503s from lb_panic are visible in Fortio's status code breakdown
- Latency measurements reflect actual HTTP round-trips

Bootstrap config: static HCM listener on configurable port, route to dynamic `test-cluster`, plus dynamic CDS via gRPC to XDS controller.

### Backend Servers

Reuse existing `BackendPool` from backend.go:
- Old backends: Go HTTP servers responding 200 on both `/healthz` and `/` (proxy path)
- Healthy new backends: same as old
- Unhealthy new backends: no listener started — port is unbound, connections get refused

### Terminal Summary

```
┌─────────────────────┬──────────────┬──────────────┐
│ Metric              │ Legacy       │ Fixed        │
├─────────────────────┼──────────────┼──────────────┤
│ Total requests      │ 10000        │ 10000        │
│ Success rate        │ 87.3%        │ 100.0%       │
│ p50 latency         │ 1.2ms        │ 0.8ms        │
│ p90 latency         │ 45ms         │ 1.1ms        │
│ p99 latency         │ 230ms        │ 1.5ms        │
│ 503 count           │ 1270         │ 0            │
│ Connection errors   │ 340          │ 0            │
└─────────────────────┴──────────────┴──────────────┘
Verdict: FIXED configuration eliminates transition errors ✓
```

### Pass/Fail Criteria

- Fixed run error rate must be strictly lower than legacy run error rate
- Fixed run should have zero (or near-zero) 503s during transition

### Output Artifacts

All saved to `--output-dir`:
- `legacy.json` — Fortio raw output for legacy run
- `fixed.json` — Fortio raw output for fixed run
- `summary.json` — Parsed comparison with verdict

### New Files

| File | Purpose |
|------|---------|
| `bench.go` | Subcommand entry point, orchestration of both runs |
| `fortio.go` | Fortio subprocess management, JSON parsing |
| `envoy_bench.yaml` | Bootstrap config with HCM listener for bench |

### Modified Files

| File | Change |
|------|--------|
| `main.go` | Add `bench` subcommand routing |
| `backend.go` | Ensure backends respond on proxy path (not just `/healthz`) |
| `xds.go` | Add method to push cluster with `ignore_health_on_host_removal` flag |
