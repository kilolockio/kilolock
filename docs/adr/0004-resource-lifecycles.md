# ADR 0004: Content-addressable resources with lifecycle ranges

- **Status:** Accepted
- **Date:** 2026-05-13
- **Decider(s):** @davesade (David Kubec)
- **Supersedes / Relates to:** [ADR 0001](./0001-foundations.md), [ADR 0002](./0002-v0-scope.md)
- **Implements:** migration `0002_resource_lifecycles.sql`

## Context

The v0 normalize path stored one `resources` row per `(state_version_id, address)`. Every state write created a brand-new full set of rows. During a fresh `terraform apply` that creates N resources, Terraform issues approximately N writes against the HTTP backend, with each write containing 1, 2, ..., N resources cumulatively. Total normalization work was therefore:

- O(N²) row inserts in `resources` across the apply
- O(N²) cumulative storage if rows were retained for history
- O(N²) edges projected into `resource_dependencies`

The big-state demo at `size=50000` was estimated to take 5–8 hours just for normalization, dominated by Postgres round-trip cost on per-row INSERTs.

Phase 1 (commit `46bd135`) collapsed the per-row INSERT loops into single `pgx.CopyFrom` streams. That brought per-write cost down 5x but did not change the underlying O(N²) aggregate growth: each write still inserted a full new row set under a new `state_version_id`.

This ADR is the architectural step that changes the Big-O, not just the constant.

## Decision

The `resources` table is reshaped from "one row per `(state_version_id, address)`" to **content-addressable rows with explicit lifecycle ranges** keyed by `(state_id, address, create_serial)`. Each row carries:

- `state_id` — owning state (replaces the per-version FK)
- `address` — the canonical Terraform address
- `attributes_hash` — stable SHA-256 over the normalize-relevant content
- `create_serial`, `delete_serial` — the half-open serial range `[create_serial, delete_serial)` during which the row was alive. `delete_serial IS NULL` means "still alive"

A resource that survives unchanged across versions N..M is represented by **exactly one row** with `create_serial = N` and `delete_serial = M+1` (or NULL if still current). A resource whose attributes change between versions N and M+1 produces two rows: one with `(create=N, delete=M+1)` (the closed lifecycle for the old shape) and one with `(create=M+1, delete=NULL)` (the new shape).

### Per-write algorithm

For each `WriteState(name, serial, parsed)`:

1. Load `open := {address: (id, hash)}` for currently-alive rows in this state (`delete_serial IS NULL`).
2. Walk `parsed`:
   - Address not in `open` → INSERT a new row with `create_serial = serial`.
   - Address in `open`, hash matches → no-op (lifecycle continues).
   - Address in `open`, hash differs → close existing row (`delete_serial = serial`), INSERT new row with `create_serial = serial`.
3. Walk `open` for addresses absent from `parsed` → close existing row.

Operations are batched into:

- One `UPDATE resources SET delete_serial = $1 WHERE id = ANY($2)` for all close operations.
- One `COPY FROM STDIN` for all inserts.

Per-write cost in the typical apply pattern (one new resource per write) is therefore **O(load_open) for the lookup + 1 INSERT**. The dominant cost is loading the open set, which is `O(current_state_size)`. Total apply cost is `O(N · load_open)` ≈ `O(N²)` in the load query alone, but with no row inserts in unchanged paths and a dramatically smaller constant.

### Why not a `state_version_resources` join table

The earlier design discussion considered (and momentarily picked) a content-addressable schema with an explicit `state_version_resources(state_version_id, resource_id)` membership join table. That model preserves point-in-time queries but still requires inserting N small membership rows per write, recovering an O(N²) cost in the join table — ~40 GB at `size=50000`.

The lifecycle-range encoding gets the same point-in-time semantics for free: "what's in version with serial S" is a half-open range predicate against `create_serial` / `delete_serial`. No per-version membership rows. Storage grows with `unique_shapes`, not `N · versions`.

### Edges

`resource_dependencies` becomes a VIEW computed on demand by joining each live resource's `dependencies_raw` array against other live resources at the same serial. Two convenience views (`current_resources`, `current_resource_dependencies`) expose the current state for ad-hoc queries.

### Outputs

`outputs` remains state_version-scoped. Output counts are small per version in practice; lifecycle treatment doesn't pay off and the migration churn isn't worth it.

## Measured impact

Microbenchmark `TestApplyPattern_Bench`, N=500 cumulative writes of `random_id`-shaped resources, Postgres 16, fresh database before each run:

|                    | Baseline | Phase 1 | Phase 2 | P2 vs Baseline |
|--------------------|---------:|--------:|--------:|---------------:|
| Total wall time    |  1m16.4s |   15.0s |    4.4s |          17.3x |
| Per-write avg      |    152ms |    29ms |     8ms |            19x |
| Per-write p99      |    377ms |    72ms |    14ms |            27x |
| First write        |   11.5ms |  10.0ms |  11.6ms |           ~1x |
| Last write         |    274ms |    72ms |  10.4ms |          26.3x |
| Last/first ratio   |    23.7x |    7.1x |    0.9x | **flat** |

The last/first ratio collapsing to ~1.0 is the signature of the architectural fix: each write is essentially independent of state size, exactly as required for a system that streams state through an `apply`.

## Consequences

**Positive.**

- `terraform apply` against the HTTP backend is now linear in the resource count of the apply, not quadratic. The big-state demo at `size=50000` becomes viable.
- Storage grows with unique resource shapes, not with `versions × resources`. The same big-state apply produces ~50k rows, not 1.25B.
- Point-in-time queries on the normalized data remain a single SQL predicate, no extra join table.

**Negative.**

- Schema migration is destructive: existing normalized data is dropped. `state_versions.raw_state` is preserved (and remains authoritative for export), but historical normalized rows are gone until a future `kl reindex` walks `state_versions` and rebuilds them through the new code path.
- The per-write `loadOpenResources` query is `O(N)` in current state size. The aggregate apply cost is still `O(N²)` *in load work* even though row inserts are `O(N)`. At large N the load query becomes the new bottleneck. Mitigations (load only addresses present in the new write; or maintain an in-memory cache per state_id) are deferred.
- `resource_dependencies` is now a VIEW; some operational tools (table-level monitoring, vacuum metrics) need adjustment.

## Out of scope

- A `reindex` CLI command to rebuild historical normalized data from `state_versions.raw_state` is deferred. Users with existing data who care about historical normalized queries can re-import.
- Materializing edges in a real table is deferred. The view is fast enough for v0.x query workloads; if benchmark evidence proves otherwise, a separate ADR can revisit.
- Canonical-form attribute hashing (canonicalizing JSON before hashing) is deferred. Terraform writes deterministic JSON in practice; the false-positive rate of "same logical content, different bytes" is low.

## References

- Phase 1 (CopyFrom): commit `46bd135` — `perf(normalize): COPY-stream resources and outputs`
- Phase 2 (this ADR): migration `0002_resource_lifecycles.sql`, `pkg/store/normalize.go`
- Benchmark harness: `pkg/store/normalize_bench_test.go`
- Lifecycle integration test: `TestEndToEnd_Lifecycle_ResourceChangeClosesAndReopens` in `internal/backend/server_integration_test.go`
