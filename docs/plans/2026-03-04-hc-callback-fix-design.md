# EDS Stabilization Timeout — HC Callback Fix Design

**Date:** 2026-03-04
**Status:** Approved

## Problem

The stabilization timeout logic in `reloadHealthyHostsHelper()` never fires
because that method is only called on `HealthTransition::Changed` — i.e., when
a host's health status actually changes. Once hosts enter
`PENDING_DYNAMIC_REMOVAL` and health checks keep passing, no state change
occurs, so `reloadHealthyHostsHelper()` is never invoked and the timeout
check never runs.

The e2e test confirmed this: hosts with a 5000ms stabilization timeout
remained stuck in `PENDING_DYNAMIC_REMOVAL` for 15+ seconds, identical to
the without-fix behavior.

## Root Cause

In `upstream_impl.cc:1835-1842`, the HC callback gates on `Changed`:

```cpp
health_checker_->addHostCheckCompleteCb(
    [this](const HostSharedPtr& host, HealthTransition changed_state,
           HealthState) -> void {
      if (changed_state == HealthTransition::Changed) {
        reloadHealthyHosts(host);
      }
    });
```

The stabilization timeout check lives inside `reloadHealthyHostsHelper()`,
which is called by `reloadHealthyHosts()`. Since passing HC results produce
`HealthTransition::Unchanged`, the timeout logic is dead code in practice.

## Fix: Register Second HC Callback in EdsClusterImpl::startPreInit()

Register an additional `addHostCheckCompleteCb()` in `EdsClusterImpl::startPreInit()`
that fires on every HC completion (not gated on `Changed`). This callback
calls `maybeRemoveTimedOutHosts()`, which checks if any pending hosts have
expired and triggers `reloadHealthyHosts(nullptr)` if so.

### Why startPreInit()

- `startPreInit()` runs after `setHealthChecker()` (verified via
  `cluster_factory_impl.cc:121-153`: creation order is `createClusterImpl()`
  → `setHealthChecker()` → `setOutlierDetector()` → return, then
  `startPreInit()` called later)
- `healthChecker()` is guaranteed non-null at this point
- EDS-specific — no changes to base `ClusterImplBase`

### Code Changes

**eds.cc — startPreInit():**

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

**eds.cc — new method maybeRemoveTimedOutHosts():**

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

**eds.h — add declaration:**

```cpp
void maybeRemoveTimedOutHosts();
```

### Existing Logic Preserved

The timeout check in `reloadHealthyHostsHelper()` stays as-is.
`maybeRemoveTimedOutHosts()` is just the trigger — it calls
`reloadHealthyHosts(nullptr)` which invokes `reloadHealthyHostsHelper()`,
where the actual removal logic (checking timestamps, erasing from maps,
rebuilding host sets) already exists.

### Flow

```
HC completion (every interval)
  → EDS callback fires (not gated on Changed)
  → maybeRemoveTimedOutHosts()
  → if any host past timeout: reloadHealthyHosts(nullptr)
  → reloadHealthyHostsHelper()
  → finds expired hosts, removes from priority set
  → hosts removed from cluster
```

## Files Modified

| File | Change |
|------|--------|
| `source/extensions/clusters/eds/eds.cc` | Modify `startPreInit()`, add `maybeRemoveTimedOutHosts()` |
| `source/extensions/clusters/eds/eds.h` | Add `maybeRemoveTimedOutHosts()` declaration |

## Testing

- Existing unit tests in `eds_test.cc` should still pass (no behavior change
  for the `Changed` path)
- Add unit test for `maybeRemoveTimedOutHosts()` triggering removal after
  timeout expiry
- Re-run e2e test: Scenario 2 should now PASS (hosts removed within 5-7s)
