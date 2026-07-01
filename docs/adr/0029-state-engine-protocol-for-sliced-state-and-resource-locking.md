# ADR 0029: State engine protocol for sliced state and resource locking

- **Status:** Proposed
- **Date:** 2026-06-24
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0007](./0007-parallel-apply.md), [ADR 0014](./0014-file-scoped-plan-apply.md), [ADR 0017](./0017-state-first-targeted-plan-apply.md), [ADR 0026](./0026-resource-row-authoritative-repair-and-regenerated-raw-state.md), [ADR 0028](./0028-backend-enriched-graph-assisted-slice-planning.md)

## Context

Kilolock already improves on plain Terraform by:

- storing normalized state metadata in PostgreSQL rather than treating state as
  only an opaque blob
- allowing scoped `kl` workflows to run Terraform against a local slice
- allowing multiple scoped applies to proceed concurrently when their write sets
  do not overlap

However, the current `kl` execution model still inherits a major limitation
from Terraform:

- to construct a scoped slice, the client first fetches the full current state
  snapshot from the HTTP backend
- Terraform itself still plans/applies from whole-state snapshots
- the commit path still writes a full next state snapshot

This means very large states remain expensive even when the operator intent is
narrow. A 50 MB or 100 MB state can impose high network, parse, and write costs
even when the user changed one file or one module.

The product requirement is therefore two-lane:

1. keep full compatibility with Terraform's HTTP backend where needed
2. add a state engine protocol that can operate on narrower slices and narrower
   locks while preserving the same logical state

Operators must be free to choose either lane against the same state:

- plain Terraform uses HTTP backend semantics and full snapshots
- `kl` native mode uses richer backend semantics for large-state workflows

At the same time, mixed-mode safety matters. If a state engine write is in
progress, plain Terraform must observe the state as locked rather than silently
proceeding against a concurrently mutating trunk.

## Decision

Kilolock will introduce a **state engine protocol** in parallel with the existing
Terraform-compatible HTTP backend.

The protocol is an optimization and capability layer for `kl`. It does not
replace the HTTP backend contract used by plain Terraform.

The target model is:

- **Terraform lane**
  - plain HTTP backend semantics
  - full snapshot pull / push
  - standard lock / unlock behavior
- **state engine lane**
  - resource- and module-aware slice fetch
  - resource-level reservations
  - delta-style commit for native operations
  - Terraform-visible coarse lock while native writes are active

Both lanes address the same logical state identity:

- workspace / tenant
- environment
- state name
- lineage
- serial family

## Why a native protocol is needed

ADR 0028 already establishes that backend-assisted slice planning is desirable.
This ADR goes one step further and makes the transport split explicit.

Without a state engine lane, large-state workflows remain bounded by whole-state
transfer semantics:

- download full trunk
- derive small slice locally
- commit by writing a full successor state blob

That is acceptable for compatibility, but not for Kilolock's best-case product
story. Large-state operators need a path where:

- the backend returns only relevant current state
- `kl` acquires only relevant resource reservations
- native state operations (`rm`, `mv`, repair, rollback, patch) can mutate a
  narrower surface
- plain Terraform remains fully supported for teams that want or need it

## Decision details

### D1. Two protocol lanes against one state

Kilolock will treat the Terraform HTTP backend and the state engine protocol as
two access lanes to the same state, not as two separate state systems.

This means:

- the user should not have to migrate state to switch between lanes
- the backend remains responsible for keeping both lanes coherent
- state history, lineage, and audit trail remain shared

### D2. State engine lane is primarily for scoped and large-state workflows

The initial target workflows are:

- `kl plan --file ...`
- `kl apply --file ...`
- `kl plan --target ...`
- future native state operations such as:
  - `kl state rm`
  - `kl state mv`
  - resource repair / rollback / patch flows

The state engine lane is explicitly allowed to refuse ambiguous cases and fail
closed, asking the operator to widen scope or fall back to the Terraform lane.

### D3. Slice fetch is based on local intent plus backend enrichment

The client must not depend on state alone to discover scope, because new
resources may exist in configuration but not yet in state.

The slice-construction model is:

1. `kl` analyzes local configuration to identify intended scope
2. `kl` sends candidate addresses, module prefixes, and selectors to the
   backend
3. the backend expands them over enriched realized-state metadata
4. the backend returns:
   - relevant realized resources
   - dependency closure metadata
   - a backend-authored scope contract for fetch/read/write/reservation surfaces
   - explicit classification of addresses it cannot satisfy from state:
     undeployed-vs-unknown
   - reservation candidates
5. `kl` merges the backend slice with the needed local config footprint
6. `kl` proceeds only if the closure is safe enough to prove

This keeps backend enrichment and local config analysis as complementary
sources of truth.

### D4. Native writes use resource reservations

State engine write operations will acquire resource-level reservations over the
effective write set and read set closure.

That reservation model is the fine-grained concurrency contract for native
Kilolock operations:

- disjoint native writes may proceed concurrently
- overlapping native writes block or wait
- native read-only operations may coexist where safe

### D5. Native writes must appear locked to plain Terraform

Plain Terraform cannot understand partial reservations.

Therefore, while any state engine write operation is in progress, the state must
appear locked to plain Terraform. The implementation may use either:

- a standard Terraform-visible state lock sentinel created by native writes, or
- HTTP lock handling that refuses Terraform `LOCK` when active native write
  reservations exist

The user-visible effect must be the same:

- plain Terraform sees the state as locked
- lock metadata should make it clear a state engine operation is holding the lock

This preserves mixed-mode safety even though state engine operations are narrower
internally than plain Terraform operations.

### D6. Native commit path may be narrower than the HTTP backend path

For the Terraform lane, full snapshot ingress and egress remain required.

For the state engine lane, the target direction is narrower mutation surfaces:

- commit only changed resources / instances when the operation semantics allow
  it
- preserve full-state version artifacts and raw-state reconstruction for
  compatibility and history
- continue evolving toward a resource-row-authoritative mutation model as
  described by ADR 0026

This ADR does not require that every native write immediately become a perfect
per-resource delta commit. It establishes the architectural direction and
public contract.

### D7. Configuration is separate from Terraform backend configuration

The Terraform backend block remains the compatibility contract for plain
Terraform. State engine transport choice should not require inventing a new
Terraform backend type.

State engine behavior will therefore be configured in KL-owned configuration:

- CLI flags
- environment variables
- optional `.kl.toml`

Terraform's `backend "http"` block continues to identify the logical state.
State engine config chooses how `kl` talks to that state.

### D8. Native apply must be explicitly trusted, otherwise fallback is real

The state engine lane is not "best effort narrow by default". It is a
trust-based lane.

That means:

- if the backend proves a safe native slice, `kl apply` may use the trusted
  state-engine lane
- if the backend cannot prove that safely, the client must not quietly continue
  on the trusted lane anyway
- fallback must be a real runtime behavior change, not only a planning label

Concretely, the trusted lane means:

- Terraform-visible coarse lock is acquired
- state-engine reservations are used
- commit mode is state-engine delta
- native intent metadata is surfaced to the operator

Concretely, fallback means:

- no state-engine coarse lock
- no trusted delta-commit lane
- runtime stays on the broader snapshot-merge / full-trunk behavior

This distinction is important because the product promise is not merely
"narrow when possible". The promise is:

- narrow when the backend could prove it
- broad when it could not
- never pretend those two cases are equivalent

## Configuration contract

### Terraform backend block

The repository may continue to declare a normal HTTP backend such as:

```hcl
terraform {
  backend "http" {
    address        = "https://api.example.com/v1/states/ws_x/env_y/prod"
    lock_address   = "https://api.example.com/v1/states/ws_x/env_y/prod"
    unlock_address = "https://api.example.com/v1/state-unlock/ws_x/env_y/prod"
    lock_method    = "LOCK"
    unlock_method  = "POST"
  }
}
```

This remains the canonical Terraform-compatible onboarding path.

### `.kl.toml`

An optional `.kl.toml` may tell `kl` to use the native lane:

```toml
state_url = "https://api.example.com/v1/states/ws_x/env_y/prod"
protocol = "kl"

[auth]
token_env = "KL_TOKEN"
```

Future keys may include:

- protocol mode (`http`, `kl`, `auto`)
- state identity override
- desired slice / execution strategy
- native safety policy knobs

### Environment variables

Environment variables must also support native mode, especially for CI:

- `KL_STATE_URL`
- `KL_TOKEN`
- `KL_PROTOCOL`

Proposed resolution precedence:

1. explicit CLI flags
2. KL environment variables
3. `.kl.toml`
4. discovered Terraform backend config

## API direction

Exact endpoint names may change, but the state engine lane should support concepts
such as:

- state metadata lookup
- backend-assisted slice expansion
- slice fetch
- reservation acquire / renew / release
- delta or narrow mutation commit
- native state operations (`rm`, `mv`, repair, patch, rollback`)

The HTTP backend endpoints remain unchanged for Terraform compatibility.

## Current execution semantics note

The current native/orchestrated apply path acquires reservations for the full
scoped write set before execution starts.

That means:

- disjoint scoped writes can proceed concurrently
- overlapping scoped writes block or wait
- a scope that is *mostly* disjoint but overlaps on even one reserved address
  does **not** currently make partial progress on the disjoint subset first

This is an intentional conservative choice for the current implementation. It
keeps one apply_run aligned with one complete reservation set and avoids
half-executed scopes while the product is still proving out the state-engine
lane.

## Consequences

### Positive

- Gives Kilolock a credible large-state fast path without abandoning Terraform
  compatibility.
- Preserves user freedom to choose plain Terraform or state engine mode against
  the same state.
- Enables true fine-grained concurrency for state engine operations.
- Creates a clear home for future native state operations that are awkward or
  expensive under whole-snapshot semantics.

### Tradeoffs

- Backend complexity increases because two access lanes must stay coherent.
- Mixed-mode locking semantics must be carefully tested.
- state engine scoped execution still needs conservative fallback when local
  closure cannot be proven.
- The initial native lane may still rely on some Terraform execution behavior
  before a broader native engine exists.

## Non-goals

- Replacing Terraform's HTTP backend contract.
- Breaking plain Terraform compatibility.
- Requiring users to migrate or fork state to adopt state engine mode.
- Guaranteeing a perfect minimal slice or perfect per-resource delta commit in
  the first implementation.

## Follow-up work

1. Define `.kl.toml` schema and CLI/env precedence.
2. Define backend APIs for:
   - metadata lookup
   - slice expansion
   - slice fetch
   - native reservations
   - native commit
3. Define the mixed-mode lock matrix:
   - state engine read vs state engine write
   - state engine write vs plain Terraform
   - plain Terraform vs state engine write
4. Prototype `kl plan --file` over a backend-assisted reduced slice.
5. Prototype one native state operation (`state rm` is the simplest candidate).
6. Add large-state benchmarks comparing:
   - Terraform HTTP lane
   - current KL sliced lane with full trunk fetch
   - future state engine sliced lane
7. Evolve the state-engine graph cache from per-process memory to a shared
   multi-replica design when cloud deployments need it:
   - current OSS behavior may keep the realized graph snapshot in one `kld`
     process keyed by `(state_id, serial)`
   - future hosted/cloud deployments may need a shared cache so multiple API
     replicas can reuse the same warm snapshot for one hot state head
   - Redis or a similar shared cache may be a good fit, but the storage choice
     remains open; correctness requirements are more important than the exact
     product
