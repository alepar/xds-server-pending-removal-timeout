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
	mu                    sync.Mutex
	endpoints             map[int]bool // port -> present
	cache                 cachev3.SnapshotCache
	version               atomic.Int64
	timeoutMs             uint
	healthChecks          bool
	ignoreHealthOnRemoval   bool
	ignoreNewHostsUntilHC   bool
	grpcServer              *grpc.Server
	listener              net.Listener
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

// WithIgnoreHealthOnRemoval sets the ignore_health_on_host_removal cluster field.
func WithIgnoreHealthOnRemoval(v bool) func(*XDSController) {
	return func(x *XDSController) {
		x.ignoreHealthOnRemoval = v
	}
}

// WithIgnoreNewHostsUntilFirstHC sets ignore_new_hosts_until_first_hc on CommonLbConfig.
func WithIgnoreNewHostsUntilFirstHC(v bool) func(*XDSController) {
	return func(x *XDSController) {
		x.ignoreNewHostsUntilHC = v
	}
}

// SetClusterConfig changes the cluster's HC and timeout settings.
// Call before adding endpoints (or call pushSnapshot to apply immediately).
func (x *XDSController) SetClusterConfig(timeoutMs uint, healthChecks bool, opts ...func(*XDSController)) error {
	x.mu.Lock()
	x.timeoutMs = timeoutMs
	x.healthChecks = healthChecks
	x.ignoreHealthOnRemoval = false
	x.ignoreNewHostsUntilHC = false
	for _, opt := range opts {
		opt(x)
	}
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

func (x *XDSController) pushSnapshot() error {
	x.mu.Lock()
	timeoutMs := x.timeoutMs
	hc := x.healthChecks
	ignoreHC := x.ignoreHealthOnRemoval
	ignoreNewHosts := x.ignoreNewHostsUntilHC
	ports := make([]int, 0, len(x.endpoints))
	for p := range x.endpoints {
		ports = append(ports, p)
	}
	x.mu.Unlock()

	v := fmt.Sprintf("%d", x.version.Add(1))
	c := x.makeCluster(timeoutMs, hc, ignoreHC, ignoreNewHosts)
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

func (x *XDSController) makeCluster(timeoutMs uint, healthChecks bool, ignoreHealthOnRemoval bool, ignoreNewHostsUntilHC bool) *cluster.Cluster {
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
			Timeout:            durationpb.New(250 * time.Millisecond),
			Interval:           durationpb.New(250 * time.Millisecond),
			NoTrafficInterval:  durationpb.New(250 * time.Millisecond),
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

	if ignoreHealthOnRemoval {
		c.IgnoreHealthOnHostRemoval = true
	}

	if ignoreNewHostsUntilHC {
		if c.CommonLbConfig == nil {
			c.CommonLbConfig = &cluster.Cluster_CommonLbConfig{}
		}
		c.CommonLbConfig.IgnoreNewHostsUntilFirstHc = true
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
