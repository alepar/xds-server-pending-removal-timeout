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
				raw := strings.Split(value, "|")
				for i, f := range raw {
					f = strings.TrimPrefix(f, "/")
					raw[i] = strings.ToUpper(f)
				}
				host.HealthFlags = raw
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

// IsHealthy returns true if the host has no problematic health flags.
// Envoy reports "healthy" as a flag value when the host is healthy.
func (h *ClusterHost) IsHealthy() bool {
	for _, f := range h.HealthFlags {
		if f != "HEALTHY" && f != "" {
			return false
		}
	}
	return true
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

// WaitForHostHealthy polls until the host is listed with no health flags at all
// (no FAILED_ACTIVE_HC, no PENDING_DYNAMIC_REMOVAL, etc). This ensures the host
// has passed its first active health check, which is required before EDS removal
// will trigger the PENDING_DYNAMIC_REMOVAL stabilization path.
func (a *AdminClient) WaitForHostHealthy(cluster, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hosts, err := a.GetClusterHosts(cluster)
		if err == nil {
			h := FindHost(hosts, addr)
			if h != nil && h.IsHealthy() {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("host %s not fully healthy after %v", addr, timeout)
}
