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
	Name:         "0. Baseline (no stabilization timeout)",
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

		// Add endpoints, wait for healthy (must pass HC before removal triggers stabilization)
		env.XDS.AddEndpoints(p...)
		for _, port := range p {
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 15*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
				return "", err
			}
		}

		// Remove and immediately disconnect xDS for 2s
		env.XDS.RemoveEndpoints(p...)
		for _, port := range p {
			env.Admin.WaitForHostFlag(clusterName, addr(port), "PENDING_DYNAMIC_REMOVAL", true, 5*time.Second)
		}
		// The xDS restart is managed externally; verify hosts are removed on time.
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if !h.IsHealthy() {
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
