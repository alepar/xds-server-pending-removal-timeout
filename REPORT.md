# EDS Host Removal Stabilization Timeout — E2E Test Report

## Test Date and Environment

- **Date**: 2026-03-04 05:45 UTC (run 3, with dispatcher.post() fix)
- **Previous runs**:
  - Run 2 (05:22 UTC): CRASH — re-entrancy bug in HC callback
  - Run 1 (03:26 UTC): FAIL — HC not running due to `no_traffic_interval` default
- **Host**: Linux 6.12.63+deb13-cloud-amd64
- **Test location**: `/home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/`

## Binary Versions

| Binary | Version | Commit | Build |
|--------|---------|--------|-------|
| envoy-static-with-fix | 1.38.0-dev | bd434e1 (Modified) | RELEASE/BoringSSL |
| envoy-static-without-fix | 1.38.0-dev | 6f6efef31f (Modified) | RELEASE/BoringSSL |

The with-fix binary includes:
1. HC callback fix (en-4v2): `maybeRemoveTimedOutHosts()` via `dispatcher.post()` to avoid re-entrancy
2. Uses `reloadHealthyHostsHelper(nullptr)` since `reloadHealthyHosts()` is private

## Test Setup

- **xDS server**: Custom Go control plane pushing CDS+EDS snapshots via gRPC
- **Backend**: Python HTTP servers on ports 8081, 8082 (respond 200 to all requests)
- **Health checking**: HTTP health checks every 1s on `/healthz`, `no_traffic_interval=1s`
- **Cluster**: EDS-type `test-cluster` with active health checking
- **Stabilization timeout**: Configured via cluster metadata `envoy.eds.host_removal_stabilization_timeout_ms`

## Scenario 1: Without Fix (timeout_ms=0)

**Purpose**: Verify that without the stabilization timeout, removed EDS hosts remain
stuck in `PENDING_DYNAMIC_REMOVAL` indefinitely.

| Step | Result |
|------|--------|
| Hosts after EDS add | 2 (healthy) |
| Hosts 10s after EDS remove | 2 (pending_dynamic_removal) |
| PENDING_DYNAMIC_REMOVAL flag | Present on both hosts |

**Result: PASS**

Hosts correctly remain in `PENDING_DYNAMIC_REMOVAL` state indefinitely.

## Scenario 2: With Fix (timeout_ms=5000)

### Run 3 (05:45 UTC) — PASS

With the `dispatcher.post()` fix, the stabilization timeout works correctly:

| Step | Result |
|------|--------|
| Hosts after EDS add | 2 (healthy) |
| Hosts removed after target removal | 5s (within expected 5-7s window) |

**Result: PASS**

Hosts are correctly removed within the expected stabilization timeout window.
The `dispatcher.post()` fix successfully defers `maybeRemoveTimedOutHosts()` to
the next event loop iteration, avoiding the re-entrancy crash that occurred in run 2.

### Previous Runs

**Run 2 (05:22 UTC) — CRASH**: Re-entrancy bug. The HC callback chain caused
`maybeRemoveTimedOutHosts()` → `reloadHealthyHosts()` → `onClusterMemberUpdate()`
to execute within the same call stack, leading to use-after-free (SIGSEGV).

**Run 1 (03:26 UTC) — FAIL**: HC not running. `no_traffic_interval` defaults to
60s in Envoy. Without traffic, health checks only ran once initially and the
callback never fired within the 15s test window.

## Summary

| Scenario | Result | Notes |
|----------|--------|-------|
| 1: Without fix (baseline) | PASS | Hosts stuck in PENDING_DYNAMIC_REMOVAL as expected |
| 2: With fix (run 3, dispatcher.post()) | PASS | Hosts removed at 5s (expected 5-7s window) |

## Issues Found and Resolved

1. **Test infrastructure**: xDS control plane missing `NoTrafficInterval` in health check config.
   **Status**: Fixed in `main.go` (run 2).

2. **Envoy fix (en-4v2)**: Re-entrancy bug — calling `reloadHealthyHosts()` from within a HC
   callback causes use-after-free crash.
   **Status**: Fixed via `dispatcher.post()` deferral (commit bd434e1).

3. **Test script**: `count_cluster_hosts()` in `test.sh` failed with `set -euo pipefail` when
   `grep` found no matches (host count = 0), causing premature script exit.
   **Status**: Fixed — wrapped grep in `{ grep || true; }` to handle zero-match case.

## Raw Test Results (Run 3)

```
=== EDS Stabilization Timeout E2E Test ===
Date: 2026-03-04 05:45:56 UTC
Binary WITHOUT fix: /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/envoy-static-without-fix
Binary WITH fix:    /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/envoy-static-with-fix

=== SCENARIO 1: Without fix ===
Hosts after add: 2
Hosts after 10s wait: 2
PENDING_DYNAMIC_REMOVAL: YES (hosts stuck as expected)
RESULT: PASS — hosts remain stuck in PENDING_DYNAMIC_REMOVAL without fix

=== SCENARIO 2: With fix (timeout_ms=5000) ===
Hosts after add: 2
Targets removed, polling for removal...
Hosts removed at: 5s
RESULT: PASS — hosts removed within expected window

=== ALL TESTS PASSED ===
```
