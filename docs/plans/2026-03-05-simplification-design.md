# E2E Test Simplification Design

Date: 2026-03-05
Goal: Simplify e2e test setup for readability by external reviewers

## Single Binary Architecture

Replace two 700MB Envoy binaries with one. Behavior controlled via XDS config:

| Mode | timeoutMs | ignoreHealthOnRemoval | healthChecks | Effect |
|------|-----------|----------------------|--------------|--------|
| Baseline (bug) | 0 | false | true | Hosts stuck in PENDING_DYNAMIC_REMOVAL |
| Fixed | 5000 | false | true | Hosts stabilize, removed after timeout |
| No-HC | 0 | false | false | Immediate removal, no HC |
| Immediate | 0 | true | true | Hosts removed immediately (bench legacy) |

## go.mod Fix

- Module path: `github.com/alepar/xds-server-pending-removal-timeout`
- Drop `replace ../../` directive
- Use upstream `github.com/envoyproxy/go-control-plane` directly

## Scenario 0 Rework

Same binary, `SetClusterConfig(0, true)` with `ignoreHealthOnRemoval=false`.
Hosts enter PENDING_DYNAMIC_REMOVAL and stay stuck (the upstream bug).

## Bench Changes

Both runs use same binary, differ only in XDS config:
- Baseline: `timeoutMs=0, ignoreHealthOnRemoval=true` (immediate removal)
- Fixed: `timeoutMs=3000, ignoreHealthOnRemoval=false` (stabilization)

## Code Changes

Removed:
- `flagEnvoyWithoutFix` flag and dual binary validation in main.go
- `envoyWithoutFix` fields in report.go
- `EnvoyWithoutFix` in bench.go BenchConfig

Changed:
- main.go: single `-envoy` flag (default `./envoy`)
- scenarios.go: Scenario 0 uses SetClusterConfig instead of binary selection
- bench.go: both scenarios use cfg.Envoy
- go.mod: new module path, drop replace

## File Structure

Unchanged — keep current file-per-concern layout (10 files).

## Kept

- Stress test (stress.go) — valuable for robustness proof
- Bench subcommand — two-run comparison (baseline vs fix) with same binary
