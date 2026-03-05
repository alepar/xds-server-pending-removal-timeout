# EDS Stabilization Timeout E2E Test — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a minimal e2e test that proves the EDS host removal stabilization timeout fix works with a real envoy binary.

**Architecture:** A Go xds_server (gRPC xDS + HTTP control API), an envoy binary (Bazel-built from our rig), and socat dummy backends, orchestrated by a shell script that asserts timing of host removal via envoy's admin `/clusters` endpoint.

**Tech Stack:** Go (go-control-plane), Bash, socat, Bazel (envoy build)

---

### Task 1: Create Go module and xds_server binary

**Files:**
- Create: `e2e/stabilization_timeout/go.mod`
- Create: `e2e/stabilization_timeout/main.go`

**Step 1: Create go.mod**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go mod init github.com/envoyproxy/go-control-plane/e2e/stabilization_timeout
```

Then edit `go.mod` to add a `replace` directive pointing to the local go-control-plane checkout:

```
replace github.com/envoyproxy/go-control-plane => ../../
```

**Step 2: Write main.go**

This is the complete xds_server binary. It does three things:
1. Starts a gRPC server serving CDS and EDS via go-control-plane's `SnapshotCache`
2. Starts an HTTP server with `/ready`, `/add-targets`, `/remove-targets` endpoints
3. Takes CLI flags for ports, node ID, and stabilization timeout

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

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

var (
	xdsPort   uint
	httpPort  uint
	timeoutMs uint
	nodeID    string
)

func init() {
	flag.UintVar(&xdsPort, "xds-port", 5678, "gRPC xDS port")
	flag.UintVar(&httpPort, "http-port", 5679, "HTTP control port")
	flag.UintVar(&timeoutMs, "timeout-ms", 0, "host_removal_stabilization_timeout_ms (0=disabled)")
	flag.StringVar(&nodeID, "node-id", "test-node", "Envoy node ID")
}

func makeCluster(timeoutMs uint) *cluster.Cluster {
	c := &cluster.Cluster{
		Name:                 "test-cluster",
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
		HealthChecks: []*core.HealthCheck{{
			Timeout:            durationpb.New(1 * time.Second),
			Interval:           durationpb.New(1 * time.Second),
			UnhealthyThreshold: wrappedUint32(1),
			HealthyThreshold:   wrappedUint32(1),
			HealthChecker: &core.HealthCheck_HttpHealthCheck_{
				HttpHealthCheck: &core.HealthCheck_HttpHealthCheck{
					Path: "/healthz",
				},
			},
		}},
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

func wrappedUint32(v uint32) *wrapperspb.UInt32Value {
	return &wrapperspb.UInt32Value{Value: v}
}

func makeEndpoints(ports []uint32) *endpoint.ClusterLoadAssignment {
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
									PortValue: port,
								},
							},
						},
					},
				},
			},
		})
	}
	return &endpoint.ClusterLoadAssignment{
		ClusterName: "test-cluster",
		Endpoints: []*endpoint.LocalityLbEndpoints{{
			LbEndpoints: lbEndpoints,
		}},
	}
}

func main() {
	flag.Parse()

	snapshotCache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	ctx := context.Background()
	srv := serverv3.NewServer(ctx, snapshotCache, nil)

	version := &atomic.Int64{}

	// --- gRPC xDS server ---
	grpcServer := grpc.NewServer(
		grpc.MaxConcurrentStreams(1000),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 5 * time.Second,
		}),
	)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, srv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, srv)

	grpcLis, err := net.Listen("tcp", fmt.Sprintf(":%d", xdsPort))
	if err != nil {
		log.Fatalf("gRPC listen: %v", err)
	}
	go func() {
		log.Printf("xDS server listening on :%d", xdsPort)
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatalf("gRPC serve: %v", err)
		}
	}()

	// --- HTTP control server ---
	pushSnapshot := func(ports []uint32) error {
		v := fmt.Sprintf("%d", version.Add(1))
		c := makeCluster(timeoutMs)
		e := makeEndpoints(ports)
		snap, err := cachev3.NewSnapshot(v, map[resource.Type][]types.Resource{
			resource.ClusterType:  {c},
			resource.EndpointType: {e},
		})
		if err != nil {
			return fmt.Errorf("new snapshot: %w", err)
		}
		return snapshotCache.SetSnapshot(ctx, nodeID, snap)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/add-targets", func(w http.ResponseWriter, r *http.Request) {
		if err := pushSnapshot([]uint32{8081, 8082}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		log.Println("pushed snapshot: 2 targets (8081, 8082)")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/remove-targets", func(w http.ResponseWriter, r *http.Request) {
		if err := pushSnapshot(nil); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		log.Println("pushed snapshot: 0 targets")
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("HTTP control listening on :%d", httpPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", httpPort), mux))
}
```

**Important:** The `wrappedUint32` function uses `wrapperspb.UInt32Value`. Add the import:
```go
wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
```

**Step 3: Resolve dependencies**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
go mod tidy
```

**Step 4: Verify it compiles**

```bash
go build -o /dev/null .
```

Expected: clean build, no errors.

**Step 5: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/go.mod e2e/stabilization_timeout/go.sum e2e/stabilization_timeout/main.go
git commit -m "e2e: add xds_server binary for stabilization timeout test"
```

---

### Task 2: Create envoy bootstrap config

**Files:**
- Create: `e2e/stabilization_timeout/envoy.yaml`

**Step 1: Write envoy.yaml**

This is a static envoy bootstrap. CDS and EDS are dynamic (from our xds_server). Everything else is static.

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
    - name: test-listener
      address:
        socket_address:
          address: 0.0.0.0
          port_value: 10000
      filter_chains:
        - filters:
            - name: envoy.filters.network.tcp_proxy
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
                stat_prefix: tcp
                cluster: test-cluster
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
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/envoy.yaml
git commit -m "e2e: add envoy bootstrap config for stabilization timeout test"
```

---

### Task 3: Create test.sh orchestrator

**Files:**
- Create: `e2e/stabilization_timeout/test.sh`

**Step 1: Write test.sh**

The script runs two scenarios: without fix (timeout=0), then with fix (timeout=5000ms). It asserts timing of host removal.

```bash
#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_BINARY="${ENVOY_BINARY:?ERROR: Set ENVOY_BINARY to path of envoy-static}"
XDS_SERVER="${SCRIPT_DIR}/xds-server"

XDS_PORT=5678
HTTP_PORT=5679
ADMIN_PORT=9901
BACKEND_PORT_1=8081
BACKEND_PORT_2=8082
STABILIZATION_TIMEOUT_MS=5000

PIDS=()

cleanup() {
    echo "--- Cleaning up ---"
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    PIDS=()
}
trap cleanup EXIT

start_socat() {
    for port in $BACKEND_PORT_1 $BACKEND_PORT_2; do
        socat TCP-LISTEN:${port},fork,reuseaddr \
            SYSTEM:'echo -e "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"' &
        PIDS+=($!)
    done
    echo "socat backends on :${BACKEND_PORT_1}, :${BACKEND_PORT_2}"
}

start_xds_server() {
    local timeout_ms=$1
    "$XDS_SERVER" \
        --xds-port="$XDS_PORT" \
        --http-port="$HTTP_PORT" \
        --timeout-ms="$timeout_ms" \
        --node-id=test-node &
    PIDS+=($!)
    echo "xds_server pid=$! timeout_ms=$timeout_ms"
}

start_envoy() {
    "$ENVOY_BINARY" \
        -c "${SCRIPT_DIR}/envoy.yaml" \
        --log-level warn \
        --base-id 99 &
    PIDS+=($!)
    echo "envoy pid=$!"
}

wait_for_url() {
    local url=$1 max_wait=${2:-10}
    for ((i=0; i<max_wait; i++)); do
        if curl -sf "$url" >/dev/null 2>&1; then return 0; fi
        sleep 1
    done
    echo "FAIL: $url did not become ready in ${max_wait}s"
    return 1
}

# Count hosts in test-cluster from /clusters admin endpoint.
# Returns the number of host entries for test-cluster.
count_cluster_hosts() {
    curl -sf "http://127.0.0.1:${ADMIN_PORT}/clusters" \
        | grep -c "^test-cluster::" || echo 0
}

# Check if any host has pending_dynamic_removal flag
has_pending_removal() {
    curl -sf "http://127.0.0.1:${ADMIN_PORT}/clusters" \
        | grep "test-cluster::" | grep -q "pending_dynamic_removal"
}

stop_process() {
    local pid=$1
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    # Remove from PIDS array
    PIDS=("${PIDS[@]/$pid/}")
}

# ========================================================
# Build xds_server
# ========================================================
echo "=== Building xds_server ==="
cd "$SCRIPT_DIR"
go build -o "$XDS_SERVER" .
echo "Built: $XDS_SERVER"

# ========================================================
# Scenario 1: Without fix (timeout=0) — hosts stay forever
# ========================================================
echo ""
echo "=== SCENARIO 1: Without fix (timeout_ms=0) ==="

start_socat
start_xds_server 0
wait_for_url "http://127.0.0.1:${HTTP_PORT}/ready"
start_envoy
wait_for_url "http://127.0.0.1:${ADMIN_PORT}/ready"

# Add targets, wait for health checks to pass
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/add-targets"
echo "Waiting 3s for health checks..."
sleep 3

HOST_COUNT=$(count_cluster_hosts)
echo "Host count after add: $HOST_COUNT"
if [[ "$HOST_COUNT" -lt 2 ]]; then
    echo "FAIL: Expected at least 2 hosts, got $HOST_COUNT"
    exit 1
fi

# Remove targets
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/remove-targets"
echo "Waiting 10s (hosts should remain with PENDING_DYNAMIC_REMOVAL)..."
sleep 10

if ! has_pending_removal; then
    echo "FAIL: Expected hosts with pending_dynamic_removal flag"
    exit 1
fi

HOST_COUNT=$(count_cluster_hosts)
echo "Host count after 10s: $HOST_COUNT (expected: still present)"
if [[ "$HOST_COUNT" -lt 2 ]]; then
    echo "FAIL: Hosts were removed without fix — they should have stayed"
    exit 1
fi

echo "PASS: Scenario 1 — hosts remain in PENDING_DYNAMIC_REMOVAL without fix"

# Stop envoy and xds_server (keep socat for scenario 2)
ENVOY_PID=${PIDS[-1]}
XDS_PID=${PIDS[-2]}  # before envoy
stop_process "$ENVOY_PID"
stop_process "$XDS_PID"
sleep 1

# ========================================================
# Scenario 2: With fix (timeout=5000ms) — hosts removed on time
# ========================================================
echo ""
echo "=== SCENARIO 2: With fix (timeout_ms=${STABILIZATION_TIMEOUT_MS}) ==="

start_xds_server "$STABILIZATION_TIMEOUT_MS"
wait_for_url "http://127.0.0.1:${HTTP_PORT}/ready"
start_envoy
wait_for_url "http://127.0.0.1:${ADMIN_PORT}/ready"

# Add targets, wait for health checks
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/add-targets"
echo "Waiting 3s for health checks..."
sleep 3

HOST_COUNT=$(count_cluster_hosts)
echo "Host count after add: $HOST_COUNT"
if [[ "$HOST_COUNT" -lt 2 ]]; then
    echo "FAIL: Expected at least 2 hosts, got $HOST_COUNT"
    exit 1
fi

# Remove targets and record time
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/remove-targets"
T0=$(date +%s)
echo "Targets removed at T0=$T0. Polling..."

# Poll every 500ms
REMOVED=false
while true; do
    NOW=$(date +%s)
    ELAPSED=$((NOW - T0))

    HOST_COUNT=$(count_cluster_hosts)

    if [[ "$HOST_COUNT" -lt 2 ]]; then
        # Hosts are gone
        if [[ "$ELAPSED" -lt 4 ]]; then
            echo "FAIL: Hosts removed too early at ${ELAPSED}s (expected >= 5s)"
            exit 1
        fi
        echo "Hosts removed at ${ELAPSED}s"
        REMOVED=true
        break
    fi

    if [[ "$ELAPSED" -ge 7 ]]; then
        echo "FAIL: Hosts still present at ${ELAPSED}s (expected removal by 7s)"
        exit 1
    fi

    sleep 0.5
done

if [[ "$REMOVED" != "true" ]]; then
    echo "FAIL: Hosts were never removed"
    exit 1
fi

echo "PASS: Scenario 2 — hosts removed within expected window"

echo ""
echo "=== ALL TESTS PASSED ==="
```

**Step 2: Make executable**

```bash
chmod +x ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/test.sh
```

**Step 3: Commit**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add e2e/stabilization_timeout/test.sh
git commit -m "e2e: add test.sh orchestrator for stabilization timeout test"
```

---

### Task 4: Build envoy binary from rig

**Files:**
- None created; this is a build step

The envoy rig is at `~/AleCode/gt/envoy/mayor/rig/` and already has our cherry-picked fix (`6f6efef`).

**Step 1: Set up Bazel cache volume mount**

The envoy build runs inside Docker via `ci/run_envoy_docker.sh`. The Bazel cache at `~/envoy-bazel-cache` must be mounted as `/build` inside the container (that's where Bazel stores its output base).

```bash
export ENVOY_DOCKER_BUILD_DIR="$HOME/envoy-bazel-cache"
```

This environment variable is read by `ci/docker-compose.yml` which mounts `${ENVOY_DOCKER_BUILD_DIR:-/tmp/envoy-docker-build}:/build`.

**Step 2: Build envoy-static**

```bash
cd ~/AleCode/gt/envoy/mayor/rig
ENVOY_DOCKER_BUILD_DIR="$HOME/envoy-bazel-cache" \
    ./ci/run_envoy_docker.sh \
    './ci/do_ci.sh bazel.release.server_only'
```

If `do_ci.sh` doesn't have a `bazel.release.server_only` target, use:

```bash
ENVOY_DOCKER_BUILD_DIR="$HOME/envoy-bazel-cache" \
    ./ci/run_envoy_docker.sh \
    'bazel build -c opt //source/exe:envoy-static'
```

The binary will be at `~/envoy-bazel-cache/execroot/envoy/bazel-out/k8-opt/bin/source/exe/envoy-static` (or similar path under the Bazel output tree). Alternatively it may be symlinked at `bazel-bin/source/exe/envoy-static` inside the source tree.

**Step 3: Verify the binary runs**

```bash
ENVOY_BINARY=$(find ~/envoy-bazel-cache -name envoy-static -type f | head -1)
"$ENVOY_BINARY" --version
```

Expected: prints envoy version string.

---

### Task 5: Run the e2e test

**Files:**
- None

**Step 1: Run test.sh**

```bash
ENVOY_BINARY=/path/to/envoy-static \
    ~/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/test.sh
```

Expected output:
```
=== Building xds_server ===
Built: ./xds-server

=== SCENARIO 1: Without fix (timeout_ms=0) ===
...
PASS: Scenario 1 — hosts remain in PENDING_DYNAMIC_REMOVAL without fix

=== SCENARIO 2: With fix (timeout_ms=5000) ===
...
Hosts removed at 5s
PASS: Scenario 2 — hosts removed within expected window

=== ALL TESTS PASSED ===
```

**Step 2: If anything fails, debug**

- Check `curl http://127.0.0.1:9901/clusters` manually to see the admin output format
- Check envoy logs for xDS connection issues
- Verify socat is responding: `curl http://127.0.0.1:8081/healthz`

**Step 3: Commit final state**

```bash
cd ~/AleCode/gt/xds_server/mayor/rig
git add -A e2e/stabilization_timeout/
git commit -m "e2e: complete stabilization timeout end-to-end test"
```
