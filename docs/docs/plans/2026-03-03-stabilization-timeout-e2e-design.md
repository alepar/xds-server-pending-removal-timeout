# EDS Host Removal Stabilization Timeout — E2E Test Design

Date: 2026-03-03

## Problem

When active health checking is configured and `ignore_health_on_host_removal` is false, hosts removed by EDS enter `PENDING_DYNAMIC_REMOVAL` and stay there indefinitely. Our fix adds a configurable timeout via cluster metadata (`envoy.eds/host_removal_stabilization_timeout_ms`) that force-removes stabilized hosts after a bounded duration.

We need an end-to-end test that demonstrates the fix works with a real envoy binary.

## Components

4 processes, 1 shell script orchestrator:

```
┌─────────────┐  gRPC (xDS)   ┌─────────────┐  active HC   ┌──────────┐
│  xds_server │◄──────────────│    envoy     │─────────────►│  socat   │
│  :5678 gRPC │               │  :9901 admin │              │  :8081   │
│  :5679 HTTP │               │  :10000 lst  │              │  :8082   │
└─────────────┘               └─────────────┘              └──────────┘
       ▲                             ▲
       │ /add-targets                │ /clusters
       │ /remove-targets             │
       └──────── test.sh ────────────┘
```

- **socat** — 2 instances on ports 8081, 8082. Respond HTTP 200 to any request (active health checks).
- **xds_server** — Go binary. gRPC xDS on :5678, HTTP control on :5679. Two HTTP endpoints: `POST /add-targets` pushes EDS snapshot with both endpoints, `POST /remove-targets` pushes snapshot with 0 endpoints.
- **envoy** — bootstrap config pointing to xds_server for CDS/EDS, admin on :9901. Active HC configured via CDS (HTTP /healthz, 1s interval, 1 threshold). Cluster metadata sets the stabilization timeout.
- **test.sh** — orchestrates everything, polls `/clusters`, asserts timing.

## xds_server Go Binary (~150 lines)

Based on go-control-plane's `internal/example/`.

**xDS side:**
- `SnapshotCache` with ADS=false
- Registers `EndpointDiscoveryService` and `ClusterDiscoveryService`
- `test-cluster`: type EDS, active HC (HTTP /healthz, 1s interval, 1 threshold), metadata `envoy.eds/host_removal_stabilization_timeout_ms` set from CLI flag

**Snapshot contents:**
- Cluster: name `test-cluster`, type EDS, with HC and optional metadata
- Endpoints: `ClusterLoadAssignment` with 2 `LbEndpoint`s at 127.0.0.1:8081 and :8082

**HTTP control side:**
- `POST /add-targets` → push snapshot (cluster + 2 endpoints)
- `POST /remove-targets` → push snapshot (same cluster, 0 endpoints)
- `GET /ready` → 200 when gRPC server is listening

**CLI flags:**
- `--xds-port=5678`
- `--http-port=5679`
- `--timeout-ms=5000` (stabilization timeout for cluster metadata; 0 = don't set)
- `--node-id=test-node`

Same binary serves both scenarios — just change `--timeout-ms`.

## Envoy Bootstrap Config

Static YAML:
- `node.id: test-node`
- Dynamic CDS via `xds_cluster` (static cluster pointing to 127.0.0.1:5678, HTTP/2)
- Static TCP proxy listener on :10000 forwarding to `test-cluster`
- Admin on 127.0.0.1:9901
- No routes/listeners via xDS — only CDS/EDS are dynamic

## Test Script & Timing Assertions

`test.sh` runs two scenarios sequentially.

**Scenario 1: Without fix (baseline)**
1. Start socat on :8081, :8082
2. Start xds_server --timeout-ms=0
3. Start envoy
4. Wait for envoy admin ready (poll /ready)
5. POST /add-targets → wait 3s for HC to pass
6. Assert: /clusters shows 2 healthy hosts
7. POST /remove-targets
8. Wait 10s
9. Assert: /clusters STILL shows hosts (PENDING_DYNAMIC_REMOVAL) → confirms the bug
10. Kill envoy, kill xds_server

**Scenario 2: With fix**
1. (socat still running)
2. Start xds_server --timeout-ms=5000
3. Start envoy
4. Wait for envoy admin ready
5. POST /add-targets → wait 3s for HC to pass
6. Assert: /clusters shows 2 healthy hosts
7. Record timestamp T0, POST /remove-targets
8. Poll /clusters every 500ms:
   - Until T0+4s: assert hosts STILL present (not removed early)
   - Between T0+5s and T0+7s: assert hosts GONE (removed on time)
   - If T0+7s and hosts still present: FAIL (removed too late)
9. Kill envoy, kill xds_server, kill socat

**Timing tolerance:** The fix fires during HC callbacks (1s interval). With a 5s timeout, removal happens between 5s and ~6.5s. Assertions:
- Not before 4s (must not remove early)
- Gone by 7s (5s timeout + 1s HC interval + 1s slack)

**Parsing `/clusters`:** grep for `test-cluster` lines, count hosts, check for `pending_dynamic_removal` flag.

## File Layout

```
xds_server rig: ~/AleCode/gt/xds_server/mayor/rig/
└── e2e/
    └── stabilization_timeout/
        ├── main.go           # xds_server binary
        ├── go.mod             # separate module
        ├── envoy.yaml         # static bootstrap
        └── test.sh            # orchestrator (single entry point)
```

Envoy binary built from envoy rig via Bazel. test.sh accepts `--envoy-binary` arg.

## Build Requirements

- Envoy built via Bazel from envoy rig, using persistent cache at `~/envoy-bazel-cache`
- Docker volume mounts configured for Bazel cache persistence across container restarts
- Go toolchain for building xds_server
