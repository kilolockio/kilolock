# ADR 0017: State-first targeted plan/apply (`--target`)

- **Status:** Proposed
- **Date:** 2026-05-29
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0006](./0006-refresh-implementation.md), [ADR 0007](./0007-parallel-apply.md), [ADR 0014](./0014-file-scoped-plan-apply.md), [ADR 0015](./0015-control-plane-separation.md)

## Context

For large states, vanilla Terraform targeting still pays heavy planning costs
because planning starts from full configuration graph construction and provider
planner behavior. Kilolock already has a state-native model (resources,
lifecycles, dependency graph, reservations) that can drive narrower execution.

We want a workflow where operators can target specific addresses and get:

- fast plan latency
- safe dependency closure
- parallel apply semantics from ADR 0007
- normal Terraform/OpenTofu compatibility for later full runs

## Decision

Kilolock will add first-class `--target` support for plan/apply, implemented
as **state-first targeted execution**.

### Command surface

```sh
kl plan  --target module.db.aws_db_instance.primary
kl plan  --target module.db --target random_id.release
kl apply --target module.db.aws_db_instance.primary
```

`--target` is repeatable on both `plan` and `apply`.

## Design principles

### D1. State-first closure, not naive narrowing

Given target addresses, Kilolock computes required closure from state metadata
and dependency graph:

- write closure: target-owned write candidates
- read closure: transitive dependencies required for correct evaluation
- validation closure: objects needed so Terraform can validate references

If closure cannot be proven safely, command fails with explicit diagnostics.

### D2. Safety over minimalism

Kilolock auto-widens scope when necessary. Narrow-but-unsafe execution is not
default behavior.

Optional future escape hatch:

- `--unsafe-target`: disables some widening checks and accepts higher risk.

### D3. Keep Terraform/OpenTofu execution path

Kilolock still runs Terraform/OpenTofu for actual planning/apply. The speedup
comes from a smaller workspace + sliced local state + reduced target set, not
from replacing Terraform semantics.

### D4. Preserve ADR 0007 concurrency model

Reservations are derived from scoped write/read sets. Surprise-write guard
remains mandatory:

```
changed_addresses ⊆ reserved_write_set
```

Any unexpected write aborts commit.

## Architecture

1. Parse/normalize target selectors (`resource`, `module.*`, optional instance keys).
2. Resolve ownership and dependency closure from graph + state.
3. Build temporary backend-free workspace and sliced local state.
4. Run Terraform/OpenTofu plan/apply with narrowed `-target=` set.
5. Emit spec with scoped write/read/reservations.
6. Commit through existing row-level apply path.

## UX behavior

- Summary must separate:
  - actual planned mutations
  - targeted writable ownership scope
- Mutating `apply --target` runs require explicit scope acknowledgement
  (`--confirm-scope`). Use `--dry-run` to inspect derived scope and
  obtain a copy/paste rerun command.
- No-op targeted plans should remain clearly no-op even with non-empty scope.
- Error messages should explain widening/failure reason and recommended fallback:
  - add additional targets, or
  - run full plan.

## Implementation path (MVP)

1. **CLI flags**
   - add repeatable `--target` to `plan` and `apply`.
2. **Target normalization**
   - canonical address handling for resources and module prefixes.
3. **State-first closure**
   - compute read/write/validation closure from existing depgraph + state.
4. **Scoped planning**
   - generate targeted plan spec with scoped reservations.
5. **Apply path**
   - `apply --target` convenience path mirroring `apply --file`.
6. **Tests + demo**
   - no-op parity tests vs full plan
   - dependency inclusion tests
   - conflict/reservation behavior tests
   - large-state timing comparison demo

## Non-goals (initial)

- Perfect support for every dynamic expression shape on day one.
- Cross-state transactional targeting.
- Resource-level RBAC policy decisions (covered by future RBAC work, ADR 0016).

## Consequences

**Positive**

- materially faster operator feedback for large states
- better multi-engineer throughput on shared state
- targeted changes remain auditable and safe

**Negative**

- more complexity in closure logic and diagnostics
- risk of drift from Terraform edge semantics if tests are weak
- additional maintenance surface for target canonicalization
