package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type ScenarioResult struct {
	Name     string        `json:"name"`
	Passed   bool          `json:"passed"`
	Duration time.Duration `json:"duration_ms"`
	Details  string        `json:"details"`
	Error    string        `json:"error,omitempty"`
}

type StressResults struct {
	Profile          string                    `json:"profile"`
	TotalEndpoints   int                       `json:"total_endpoints"`
	Duration         time.Duration             `json:"duration_ms"`
	ScenariosRun     int                       `json:"scenarios_run"`
	ByScenario       map[string]*ScenarioStats `json:"by_scenario"`
	PassRate         float64                   `json:"pass_rate"`
	AvgRemovalTimeMs float64                   `json:"avg_removal_time_ms,omitempty"`
	MaxConcurrent    int                       `json:"max_concurrent"`
}

type ScenarioStats struct {
	Run    int `json:"run"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type Report struct {
	mu              sync.Mutex
	envoyWithFix    string
	envoyWithoutFix string
	isolated        []ScenarioResult
	stress          []StressResults
}

func NewReport(withFix, withoutFix string) *Report {
	return &Report{envoyWithFix: withFix, envoyWithoutFix: withoutFix}
}

func (r *Report) AddIsolated(result ScenarioResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.isolated = append(r.isolated, result)
}

func (r *Report) AddStress(results StressResults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stress = append(r.stress, results)
}

func (r *Report) Print() {
	fmt.Println("=== EDS Stabilization Timeout E2E Test Report ===")
	fmt.Printf("Date: %s\n", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Printf("Envoy (with fix):    %s\n", r.envoyWithFix)
	fmt.Printf("Envoy (without fix): %s\n", r.envoyWithoutFix)
	fmt.Println()

	fmt.Println("--- Isolated Scenarios ---")
	allPassed := true
	for _, res := range r.isolated {
		status := "PASS"
		if !res.Passed {
			status = "FAIL"
			allPassed = false
		}
		fmt.Printf("[%s]  %-40s %6dms  %s\n", status, res.Name, res.Duration.Milliseconds(), res.Details)
		if res.Error != "" {
			fmt.Printf("        ERROR: %s\n", res.Error)
		}
	}

	for _, s := range r.stress {
		fmt.Println()
		fmt.Printf("--- Stress Test (%s: %d endpoints, %v) ---\n", s.Profile, s.TotalEndpoints, s.Duration.Round(time.Second))
		fmt.Printf("Scenarios run: %d\n", s.ScenariosRun)
		for name, stats := range s.ByScenario {
			pct := float64(stats.Run) / float64(s.ScenariosRun) * 100
			result := "all passed"
			if stats.Failed > 0 {
				result = fmt.Sprintf("%d FAILED", stats.Failed)
				allPassed = false
			}
			fmt.Printf("  %-30s %4d (%4.1f%%)  %s\n", name+":", stats.Run, pct, result)
		}
		fmt.Printf("Pass rate: %d/%d (%.0f%%)\n", s.ScenariosRun-countFailed(s.ByScenario), s.ScenariosRun, s.PassRate*100)
		fmt.Printf("Max concurrent scenarios: %d\n", s.MaxConcurrent)
	}

	fmt.Println()
	if allPassed {
		fmt.Println("=== ALL TESTS PASSED ===")
	} else {
		fmt.Println("=== SOME TESTS FAILED ===")
	}
}

func countFailed(m map[string]*ScenarioStats) int {
	total := 0
	for _, s := range m {
		total += s.Failed
	}
	return total
}

func (r *Report) WriteJSON(path string) error {
	data := struct {
		Date            string           `json:"date"`
		EnvoyWithFix    string           `json:"envoy_with_fix"`
		EnvoyWithoutFix string           `json:"envoy_without_fix"`
		Isolated        []ScenarioResult `json:"isolated"`
		Stress          []StressResults  `json:"stress,omitempty"`
	}{
		Date:            time.Now().UTC().Format(time.RFC3339),
		EnvoyWithFix:    r.envoyWithFix,
		EnvoyWithoutFix: r.envoyWithoutFix,
		Isolated:        r.isolated,
		Stress:          r.stress,
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

func (r *Report) AllPassed() bool {
	for _, res := range r.isolated {
		if !res.Passed {
			return false
		}
	}
	for _, s := range r.stress {
		if s.PassRate < 1.0 {
			return false
		}
	}
	return true
}
