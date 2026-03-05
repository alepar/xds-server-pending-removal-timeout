# HC Callback Fix Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix the EDS stabilization timeout by registering a second HC callback that fires on every health check completion, triggering timeout-based host removal.

**Architecture:** Add `maybeRemoveTimedOutHosts()` method to `EdsClusterImpl`, register it via `addHostCheckCompleteCb()` in `startPreInit()`. No base class changes.

**Tech Stack:** C++ (Envoy), Bazel build system

---

### Task 1: Add maybeRemoveTimedOutHosts() declaration to eds.h

**Files:**
- Modify: `source/extensions/clusters/eds/eds.h:86` (after `validateAllLedsUpdated()`)

**Step 1: Add the declaration**

After line 86 (`bool validateAllLedsUpdated() const;`), add:

```cpp
  // Checks if any PENDING_DYNAMIC_REMOVAL hosts have exceeded the stabilization
  // timeout and triggers a reload if so. Called on every HC completion.
  void maybeRemoveTimedOutHosts();
```

**Step 2: Commit**

```bash
git add source/extensions/clusters/eds/eds.h
git commit -m "eds: declare maybeRemoveTimedOutHosts() method"
```

---

### Task 2: Implement maybeRemoveTimedOutHosts() in eds.cc

**Files:**
- Modify: `source/extensions/clusters/eds/eds.cc` (add method after `recordPendingRemovalTimestamps()`, around line 555)

**Step 1: Add the method**

After the closing brace of `recordPendingRemovalTimestamps()` (line ~554), add:

```cpp
void EdsClusterImpl::maybeRemoveTimedOutHosts() {
  if (pending_removal_timestamps_.empty()) {
    return;
  }
  const auto now =
      transport_factory_context_->serverFactoryContext().timeSource().monotonicTime();
  bool any_expired = false;
  for (const auto& [host, timestamp] : pending_removal_timestamps_) {
    if ((now - timestamp) >= host_removal_stabilization_timeout_) {
      any_expired = true;
      break;
    }
  }
  if (any_expired) {
    reloadHealthyHosts(nullptr);
  }
}
```

**Step 2: Commit**

```bash
git add source/extensions/clusters/eds/eds.cc
git commit -m "eds: implement maybeRemoveTimedOutHosts()"
```

---

### Task 3: Modify startPreInit() to register HC callback

**Files:**
- Modify: `source/extensions/clusters/eds/eds.cc:94`

**Step 1: Replace the one-liner startPreInit()**

Replace:
```cpp
void EdsClusterImpl::startPreInit() { subscription_->start({edsServiceName()}); }
```

With:
```cpp
void EdsClusterImpl::startPreInit() {
  if (host_removal_stabilization_timeout_.count() > 0 && healthChecker() != nullptr) {
    healthChecker()->addHostCheckCompleteCb(
        [this](const HostSharedPtr&, HealthTransition, HealthState) {
          maybeRemoveTimedOutHosts();
        });
  }
  subscription_->start({edsServiceName()});
}
```

**Step 2: Commit**

```bash
git add source/extensions/clusters/eds/eds.cc
git commit -m "eds: register HC callback for stabilization timeout in startPreInit()"
```

---

### Task 4: Verify existing unit tests pass

**Step 1: Run eds_test**

```bash
# Inside Docker build container with cache mount
bazel test //test/extensions/clusters/eds:eds_test -c opt
```

Expected: All tests pass, including:
- `StabilizationTimeoutRemovesHost`
- `StabilizationTimeoutHcFailureBeforeTimeout`
- `StabilizationTimeoutHostReAddedBeforeTimeout`
- `StabilizationTimeoutMultipleHosts`
- `StabilizationTimeoutDisabledByDefault`
- `StabilizationTimeoutIgnoredWithDrainOnHostRemoval`

**Step 2: Commit (no-op, just verification)**

---

### Task 5: Build envoy-static binary with fix

**Step 1: Build the binary**

```bash
cd /home/debian/AleCode/gt/envoy/mayor/rig
source ci/envoy_build_sha.sh

docker run --rm \
  -v "/home/debian/envoy-bazel-cache:/build" \
  -v "$(pwd):/source" \
  -e HOME=/build \
  -w /source \
  "$BUILD_CONTAINER" \
  /bin/bash -c 'bazel build -c opt //source/exe:envoy-static'
```

**Step 2: Copy binary to e2e folder**

```bash
cp /home/debian/envoy-bazel-cache/envoy-static \
   /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/envoy-static-with-fix
```

**Step 3: Verify binary version**

```bash
./envoy-static-with-fix --version
```

---

### Task 6: Run e2e test suite

This task belongs to the xds_server rig. Once the fixed binary is copied to
the e2e folder, run the test suite (either the existing bash test or the
advanced Go test-runner if ready).

```bash
cd /home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout
./test.sh
```

Expected: Both Scenario 1 (baseline) and Scenario 2 (with fix) should PASS.
