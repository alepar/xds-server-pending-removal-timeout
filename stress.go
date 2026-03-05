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
	portPool    chan int
	scenarios   []weightedScenario
	totalWeight int
	duration    time.Duration
	totalPorts  int
	xds         *XDSController
	admin       *AdminClient
	backends    *BackendPool
	mu          sync.Mutex
	results     map[string]*ScenarioStats
	totalRun    int
	maxConc     atomic.Int32
	currentConc atomic.Int32
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
		PassRate:       float64(passed) / float64(max(st.totalRun, 1)),
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
				if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
			if err := env.Admin.WaitForHostHealthy(clusterName, addr(port), 10*time.Second); err != nil {
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
