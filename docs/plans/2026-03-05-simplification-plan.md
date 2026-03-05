# E2E Test Simplification Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Simplify e2e test to use a single Envoy binary with behavior controlled via XDS config, and fix go.mod for standalone repo.

**Architecture:** Remove dual-binary flags/logic. All scenarios use one `-envoy` flag. Baseline vs fixed behavior is driven by XDS cluster config (timeoutMs, ignoreHealthOnRemoval). go.mod points to upstream go-control-plane directly.

**Tech Stack:** Go, go-control-plane, gRPC, Envoy admin API

---

### Task 1: Fix go.mod for standalone repo

**Files:**
- Modify: `go.mod`

**Step 1: Update module path and remove replace directive**

Change module line to:
```
module github.com/alepar/xds-server-pending-removal-timeout
```

Remove the `replace github.com/envoyproxy/go-control-plane => ../../` line.

**Step 2: Run go mod tidy**

Run: `go mod tidy`
Expected: Downloads dependencies, updates go.sum. No errors.

**Step 3: Verify build**

Run: `go build .`
Expected: Compiles successfully.

**Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "fix: update go.mod for standalone repo, drop replace directive"
```

---

### Task 2: Simplify main.go to single binary

**Files:**
- Modify: `main.go`

**Step 1: Replace dual binary flags with single flag**

Replace these lines:
```go
var (
	flagEnvoyWithFix    string
	flagEnvoyWithoutFix string
	...
)

func init() {
	flag.StringVar(&flagEnvoyWithFix, "envoy-with-fix", "./envoy-static-with-fix", "Path to envoy binary with fix")
	flag.StringVar(&flagEnvoyWithoutFix, "envoy-without-fix", "./envoy-static-without-fix", "Path to envoy binary without fix")
	...
}
```

With:
```go
var (
	flagEnvoy     string
	flagAdminPort int
	flagXDSPort   int
	flagStress    string
	flagBasePort  int
)

func init() {
	flag.StringVar(&flagEnvoy, "envoy", "./envoy", "Path to envoy binary")
	flag.IntVar(&flagAdminPort, "admin-port", 9901, "Envoy admin port")
	flag.IntVar(&flagXDSPort, "xds-port", 5678, "xDS gRPC port")
	flag.StringVar(&flagStress, "stress", "none", "Stress test profile: none|medium|large")
	flag.IntVar(&flagBasePort, "base-port", 8081, "Base port for backend pool")
}
```

**Step 2: Simplify binary validation**

Replace the dual-binary validation loop:
```go
for _, bin := range []string{flagEnvoyWithFix, flagEnvoyWithoutFix} {
	if _, err := os.Stat(bin); err != nil {
		log.Fatalf("binary not found: %s", bin)
	}
}
```

With:
```go
if _, err := os.Stat(flagEnvoy); err != nil {
	log.Fatalf("binary not found: %s", flagEnvoy)
}
```

**Step 3: Simplify version and report creation**

Replace:
```go
versionWithFix := getEnvoyVersion(flagEnvoyWithFix)
versionWithoutFix := getEnvoyVersion(flagEnvoyWithoutFix)

report := NewReport(versionWithFix, versionWithoutFix)
```

With:
```go
envoyVersion := getEnvoyVersion(flagEnvoy)
report := NewReport(envoyVersion)
```

**Step 4: Remove binary selection from scenario loop**

In the scenario loop, replace:
```go
binary := flagEnvoyWithFix
timeoutMs := uint(5000)
healthChecks := true

if scenario.Name == "0. Baseline without fix" {
	binary = flagEnvoyWithoutFix
	timeoutMs = 0
}
```

With:
```go
timeoutMs := uint(5000)
healthChecks := true

if scenario.Name == "0. Baseline (no stabilization timeout)" {
	timeoutMs = 0
}
```

And change the `startEnvoy` call from `binary` to `flagEnvoy`:
```go
envoy, err := startEnvoy(flagEnvoy, configPath, flagAdminPort, 99)
```

**Step 5: Fix stress test section**

Change:
```go
envoy, err := startEnvoy(flagEnvoyWithFix, configPath, flagAdminPort, 99)
```
To:
```go
envoy, err := startEnvoy(flagEnvoy, configPath, flagAdminPort, 99)
```

**Step 6: Verify build**

Run: `go build .`
Expected: Fails — `NewReport` signature mismatch (fixed in Task 4).

Note: This task creates a temporary build break resolved by Task 4. That's fine — we commit together at the end.

---

### Task 3: Update Scenario 0 for single-binary baseline

**Files:**
- Modify: `scenarios.go`

**Step 1: Rename and update Scenario 0**

Change the scenario definition:
```go
var Scenario0_BaselineWithoutFix = ScenarioDef{
	Name:         "0. Baseline without fix",
```

To:
```go
var Scenario0_BaselineWithoutFix = ScenarioDef{
	Name:         "0. Baseline (no stabilization timeout)",
```

No other changes needed — the scenario logic (verify hosts stuck after 10s) is correct. The XDS config with `timeoutMs=0` already produces the stuck behavior. `main.go` (Task 2) handles setting `timeoutMs=0` for this scenario.

---

### Task 4: Simplify report.go

**Files:**
- Modify: `report.go`

**Step 1: Remove dual-version tracking**

Replace:
```go
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
```

With:
```go
type Report struct {
	mu           sync.Mutex
	envoyVersion string
	isolated     []ScenarioResult
	stress       []StressResults
}

func NewReport(envoyVersion string) *Report {
	return &Report{envoyVersion: envoyVersion}
}
```

**Step 2: Simplify Print()**

Replace:
```go
fmt.Printf("Envoy (with fix):    %s\n", r.envoyWithFix)
fmt.Printf("Envoy (without fix): %s\n", r.envoyWithoutFix)
```

With:
```go
fmt.Printf("Envoy: %s\n", r.envoyVersion)
```

**Step 3: Simplify WriteJSON()**

Replace the data struct fields:
```go
EnvoyWithFix    string           `json:"envoy_with_fix"`
EnvoyWithoutFix string           `json:"envoy_without_fix"`
```

With:
```go
EnvoyVersion string           `json:"envoy_version"`
```

And the field assignment:
```go
EnvoyWithFix:    r.envoyWithFix,
EnvoyWithoutFix: r.envoyWithoutFix,
```

With:
```go
EnvoyVersion: r.envoyVersion,
```

---

### Task 5: Simplify bench.go for single binary

**Files:**
- Modify: `bench.go`

**Step 1: Remove EnvoyWithoutFix from BenchConfig**

Replace:
```go
type BenchConfig struct {
	EnvoyWithFix    string
	EnvoyWithoutFix string
	AdminPort       int
	...
}
```

With:
```go
type BenchConfig struct {
	Envoy          string
	AdminPort      int
	...
}
```

**Step 2: Update parseBenchFlags**

Replace the two binary flags:
```go
fs.StringVar(&cfg.EnvoyWithFix, "envoy-with-fix", "./envoy-static-with-fix", "Path to envoy binary with fix")
fs.StringVar(&cfg.EnvoyWithoutFix, "envoy-without-fix", "./envoy-static-without-fix", "Path to envoy binary without fix")
```

With:
```go
fs.StringVar(&cfg.Envoy, "envoy", "./envoy", "Path to envoy binary")
```

**Step 3: Update runBench binary validation**

Replace:
```go
for _, bin := range []string{cfg.EnvoyWithFix, cfg.EnvoyWithoutFix} {
	if _, err := os.Stat(bin); err != nil {
		log.Fatalf("binary not found: %s", bin)
	}
}
```

With:
```go
if _, err := os.Stat(cfg.Envoy); err != nil {
	log.Fatalf("binary not found: %s", cfg.Envoy)
}
```

**Step 4: Update both benchmark scenario configs**

Legacy run — change:
```go
legacyResult := runBenchScenario(cfg, benchScenarioConfig{
	label:                 "legacy",
	envoyBinary:           cfg.EnvoyWithoutFix,
	ignoreHealthOnRemoval: true,
	timeoutMs:             0,
})
```

To:
```go
legacyResult := runBenchScenario(cfg, benchScenarioConfig{
	label:                 "legacy",
	envoyBinary:           cfg.Envoy,
	ignoreHealthOnRemoval: true,
	timeoutMs:             0,
})
```

Fixed run — change:
```go
fixedResult := runBenchScenario(cfg, benchScenarioConfig{
	label:                 "fixed",
	envoyBinary:           cfg.EnvoyWithFix,
	ignoreHealthOnRemoval: false,
	ignoreNewHostsUntilHC: true,
	timeoutMs:             3000,
})
```

To:
```go
fixedResult := runBenchScenario(cfg, benchScenarioConfig{
	label:                 "fixed",
	envoyBinary:           cfg.Envoy,
	ignoreHealthOnRemoval: false,
	ignoreNewHostsUntilHC: true,
	timeoutMs:             3000,
})
```

---

### Task 6: Update .gitignore

**Files:**
- Modify: `.gitignore`

**Step 1: Replace binary entries**

Replace:
```
envoy-static-with-fix
envoy-static-without-fix
xds-server
test-runner
```

With:
```
envoy
envoy-*
xds-server
test-runner
```

---

### Task 7: Build, verify, and commit

**Step 1: Build**

Run: `go build .`
Expected: Compiles successfully.

**Step 2: Run go vet**

Run: `go vet ./...`
Expected: No issues.

**Step 3: Commit all changes**

```bash
git add -A
git commit -m "simplify: single envoy binary, behavior controlled via XDS config

- Replace -envoy-with-fix/-envoy-without-fix with single -envoy flag
- Baseline behavior via timeoutMs=0 (no stabilization timeout)
- Fix go.mod for standalone repo (drop replace directive)
- Simplify report to single envoy version

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

**Step 4: Push**

```bash
git push origin main
```
