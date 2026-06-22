# ADR 0028: Backend-enriched graph assisted slice planning

- **Status:** Proposed
- **Date:** 2026-06-19
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0006](./0006-refresh-implementation.md), [ADR 0007](./0007-parallel-apply.md), [ADR 0014](./0014-file-scoped-plan-apply.md), [ADR 0017](./0017-state-first-targeted-plan-apply.md), [ADR 0025](./0025-queryable-state-and-resource-level-repair.md), [ADR 0026](./0026-resource-row-authoritative-repair-and-regenerated-raw-state.md), [ADR 0027](./0027-split-client-cli-from-server-runtime.md)

## Context

Kilolock already improves on plain Terraform in two important ways:

1. state is normalized in PostgreSQL rather than treated as an opaque blob
2. `kl` scoped workflows can run Terraform against a **local slice** instead of
   always applying against the full trunk

That is a real step forward, but today's slice construction still starts by
fetching the full current trunk state to the client and then narrowing it
locally. For large states this leaves two important problems unsolved:

- **latency / transfer cost**
  - a 50 MB or 100 MB state still has to move over the network to the client
    before `kl --file ...` can narrow it
- **local working set size**
  - even if Terraform later runs against a smaller slice, the client already
    handled the full trunk

This weakens one of the key product promises for large states. An operator who
changes one file should not have to download an entire state graph just to let
Kilolock decide that only a tiny subset was relevant.

At the same time, a naive "just fetch the resources declared in the selected
file" approach is not safe. Terraform planning depends on more than file
ownership:

- transitive resource dependencies
- explicit `depends_on`
- module boundaries
- realized `count` / `for_each` expansions
- provider alias wiring
- outputs, locals, and references
- objects being moved, removed, replaced, or newly added

State alone is not sufficient to reconstruct the entire future plan graph.
Configuration alone is not sufficient to cheaply discover all realized graph
context. We need both.

## Decision

Kilolock will evolve scoped planning toward a **backend-assisted slice
construction model**:

1. enrich the realized graph persisted in PostgreSQL
2. use local configuration analysis to identify intended change scope
3. ask the backend for a dependency closure over the realized graph
4. materialize a reduced candidate slice on the client
5. fail closed or widen the slice when completeness cannot be proven

The goal is not a mathematically perfect minimal slice in all cases.
The goal is a **minimal safe slice for most real-world cases**, with explicit
fallback when proof is incomplete.

## Why this is worth doing

If this works well, Kilolock gains:

- much smaller client downloads for large states
- lower latency for `kl plan -f ...` and `kl apply -f ...`
- less incidental exposure of unrelated state on operator machines
- better leverage from Kilolock's normalized graph storage

Even if some edge cases still require widening to a larger slice or full trunk,
that is still a major win for the majority path.

## Decision details

### D1. Persist a richer realized graph in PostgreSQL

On every successful save / commit path, Kilolock should persist more than the
current resource rows and raw-state reconstruction inputs.

The realized graph should be enriched with data such as:

- canonical resource instance address
- module path / module instance path
- realized `count` and `for_each` instance keys
- provider reference / alias association
- dependency edges between realized instances
- reverse dependency edges
- lifecycle / ownership metadata already inferred elsewhere
- optional source-file provenance when it can be derived safely

This is a graph of the **current realized state**, not a promise about the next
configuration graph.

### D2. Scoped workflows start from local config intent

`kl plan -f file.tf`, `kl apply -f file.tf`, and future target-scoped entry
points must still begin with local configuration analysis.

That local analysis identifies:

- candidate resources/modules owned by the selected files
- new resources not yet present in state
- moved / removed / import declarations
- referenced variables, locals, outputs, providers, and data sources

The local config remains the authoritative source for the **intended change
boundary**.

### D3. Backend expands that intent over the realized graph

Once the client has identified candidate targets, it should ask the backend for
the smallest known realized dependency closure it can compute from PostgreSQL.

The backend may expand across:

- direct resource dependencies
- transitive dependency closure
- module prefixes / module instance containment
- provider configuration dependencies
- previously persisted ownership / provenance hints

This lets the backend return a much smaller candidate state slice than the full
trunk for most changes.

### D4. New resources are handled from config, not state

Resources that do not yet exist in the current state can never be discovered
from PostgreSQL alone.

They must be represented by local config analysis and then merged with the
backend-provided realized closure so Terraform sees:

- the new resource/module being introduced
- the realized objects it depends on
- the provider/module context needed to plan it

### D5. Fail closed when slice completeness is uncertain

The backend-assisted slice is an optimization, not a correctness loophole.

If Kilolock cannot prove a candidate slice is safe enough, it must:

- widen the slice conservatively, or
- fail with an explicit diagnostic instructing the operator to run a broader
  scoped plan or a full plan

Wrongly omitting required context is worse than losing the optimization.

### D6. "Perfect slice" is not a requirement

We explicitly do **not** require a globally perfect minimal slice in all cases.

Some plan-time context is only fully known after Terraform evaluates the
configuration and provider schemas. Examples include:

- data-source driven context
- unknown values that resolve only during plan
- replacement semantics and lifecycle interactions
- configuration graph changes not reflected in the previous state

This ADR aims for:

- **minimal safe slice in most cases**
- **predictable conservative fallback in the rest**

## Architecture

### Current model

Today, the client typically does:

1. fetch full trunk
2. analyze selected files / targets
3. derive slice locally
4. run Terraform against a local backend slice

### Target model

The target flow becomes:

1. client analyzes selected files / targets locally
2. client sends candidate addresses / module prefixes to backend
3. backend expands them over the enriched realized graph
4. backend returns candidate slice payload plus dependency metadata
5. client merges in local-config-only additions (new resources, moves, removals)
6. client runs Terraform against the reduced local slice
7. if Terraform or KL detects missing context, widen or fail closed

In shorthand:

```
safe_slice ≈ config_intent + realized_graph_closure + conservative_fallback
```

## Scope of backend enrichment

The enriched PostgreSQL data should help with:

- module-aware closure expansion
- realized `for_each` / `count` instance discovery
- reverse dependency lookups
- cheaper read/write set estimation
- future reservation precision improvements
- future repair/query UX

It should not be treated as a replacement for configuration analysis.

## Consequences

### Positive

- Better performance on large states without waiting for full server-side
  planning.
- Stronger differentiation from blob-based backends.
- Better foundation for smarter file-scoped and target-scoped workflows.
- Reduced need to move entire trunk state to the client for common changes.

### Tradeoffs

- More metadata must be persisted and kept consistent on save.
- Slice-construction logic becomes more complex.
- We still need a conservative fallback path for edge cases.
- The backend graph can be authoritative for the current realized state, but not
  for every future config change.

## Non-goals

- Replacing Terraform's planning engine.
- Guaranteeing a perfect minimal slice for every configuration.
- Eliminating local slice materialization entirely in this phase.
- Solving full SAML/SCIM/IAM-style auth concerns through this ADR.

## Follow-up work

1. Define the additional realized-graph metadata to persist on save.
2. Add backend API(s) for dependency-closure slice expansion.
3. Teach `kl plan -f` / `kl apply -f` to request backend-assisted slices.
4. Add diagnostics that explain when KL widened or abandoned a reduced slice.
5. Add large-state benchmarks comparing:
   - full trunk fetch
   - backend-assisted reduced slice
   - full fallback path
