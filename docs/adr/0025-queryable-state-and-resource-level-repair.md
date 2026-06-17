# ADR 0025: Queryable state API and resource-level state repair

Date: 2026-06-11

## Status

Accepted

## Context

Kilolock now stores Terraform state as:

- immutable `state_versions.raw_state`
- lifecycle-ranged normalized `resources`
- append-only operational metadata around applies, reservations, and history

That gives us a strong foundation for two operator and customer-facing
capabilities:

1. a genuinely queryable backend
2. per-resource emergency repair / rollback

Today, those capabilities are incomplete:

- `kl query` is still a direct database tool, not a backend-scoped
  customer-safe query surface
- `kl rollback` replays a whole historical state version, not one
  selected resource address

For OSS and Cloud, the desired trust boundary is:

- CLI talks to backend/control APIs
- backend/control talk to Postgres
- direct database access stays operator-only

## Decision

We will evolve the execution plane in three connected layers.

### 1. Backend query API

We will expose state-oriented backend query endpoints rather than arbitrary SQL
for normal users.

The supported query model is:

- list live resources in one state
- inspect one resource by exact Terraform address
- inspect history for one resource address across state versions

Authorization is backend-native:

- automation token → environment-scoped access
- PAT → membership + environment grant scoped access
- operator token → broader administrative access where appropriate

Arbitrary SQL remains an operator-only tool and is not the customer-facing
“queryable backend” promise.

### 2. Per-resource history

Per-resource history will be derived from existing append-only state versions and
lifecycle-ranged resource rows.

The first implementation slice does not require a new persistent history table:

- live resource query comes from current-version / open resource rows
- per-resource history comes from state versions plus lifecycle-ranged resource
  rows

If later usage shows we need lower-latency history scans, we may add a derived
history table or materialized projection.

### 3. Resource-level rollback

Resource rollback is defined as:

- read current state
- read a historical version
- patch one exact Terraform address in state bookkeeping
- write a new current state version

Important boundary:

- this is **state repair**, not cloud rollback automation
- it does not directly change cloud resources

The first implementation slice supports exact-address rollback semantics:

- if the address exists in the target version and not current → restore it
- if the address exists in both → replace current instance with historical one
- if the address exists only in current → remove it from current state

The command must default to dry-run preview and require explicit apply.

## Consequences

### Positive

- stronger OSS boundary: customer CLI no longer needs raw DB access for state
  query use cases
- credible “queryable backend” story
- safer emergency repair than whole-state rollback in narrow incidents
- good fit for Kilolock’s append-only state model

### Negative

- resource-level rollback introduces more safety surface than whole-state
  rollback
- exact-address patching is still bookkeeping only and can create cloud/state
  divergence if used carelessly
- initial history queries may be less optimized than a dedicated derived table

## Guardrails

Resource-level rollback must:

- default to dry-run
- require exact address
- show current vs target version context
- clearly state whether the action is restore / replace / remove / no-op
- repeat that this changes state bookkeeping, not cloud resources

## Follow-on work

- extend CLI query to backend-native state/resource subcommands
- add resource history rendering in table/json forms
- add backend-native resource rollback preview/apply endpoints
- later consider:
  - multi-resource rollback sets
  - richer dependency warnings
  - derived history tables if needed for performance
