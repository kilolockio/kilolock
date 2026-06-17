# ADR 0007: v2 scope — Parallel apply on shared state

- **Status:** Proposed
- **Date:** 2026-05-14
- **Decider(s):** @davesade (David Kubec)
- **Supersedes / Relates to:** [ADR 0001](./0001-foundations.md), [ADR 0002](./0002-v0-scope.md), [ADR 0004](./0004-resource-lifecycles.md), [ADR 0005](./0005-v1-scope.md)
- **Reference design:** StateGraph Velocity (commercial, closed-source; <https://stategraph.com/docs/velocity>)

## Context

v0 made Terraform state queryable. v1 closed the freshness gap with
out-of-band provider-aware refresh. Neither addressed the operational
constraint operators feel daily on any team larger than ~3 engineers:
**only one person can `terraform apply` against a state at a time,
even when their changes touch completely different resources.** Two
engineers updating `module.web.*` and `module.db.*` in the same state
must serialize because the HTTP backend protocol Terraform speaks
has whole-state `LOCK` / `UNLOCK` and a whole-state blob write.
S3 + DynamoDB, Terraform Cloud, GCS, Consul — every backend on the
market has the same property.

The industry workaround is to split states aggressively: one state
per team, per service, per module. It works, but it pushes the
modularity problem out to the operator. Big orgs end up with
hundreds of small states glued together with `terraform_remote_state`
data sources, fragile cross-references, and bespoke deployment
ordering scripts. Splitting the state because of how the *backend
locks* is the wrong reason to split state.

The lifecycle model from [ADR 0004](./0004-resource-lifecycles.md)
was always built to make this fixable. Content-addressable
resources with explicit lifecycle ranges, written through a single
delta-aware code path, mean that **two writes that touch disjoint
addresses are commutative at the row level**. Whole-state blob
writes are an artifact of the HTTP backend protocol, not of the
data model underneath. The substrate has been ready since v1.

What's been missing is the layer that turns the substrate into a
product:

- A reservation model that locks **subgraphs**, not whole states.
- A CLI entry point that drives Terraform per-subgraph instead of
  letting Terraform drive the whole state.
- A commit path that merges row-level changes into the shared
  trunk instead of clobbering it.

A commercial product exists that does exactly this: **StateGraph
Velocity**. Their public documentation describes resource-level
locking, parallel subgraph execution, multi-state transactions,
and a `stategraph` CLI that replaces `terraform` as the operator's
entry point — all over a Postgres backend. We are not inventing
the design space. We are committing to ship the open-source
implementation, Apache 2.0, self-hostable.

This ADR commits to that as the v2 scope.

## Decision

v2 of Kilolock is **parallel apply on shared state**. Two
engineers running `kl apply` against the same state with
disjoint resource sets proceed in parallel and commit at the row
level, without splitting the state and without blocking each
other.

Concretely, v2 ships:

1. A **reservations** model that locks addresses (and address
   prefixes), not whole states.
2. A new `kl apply` CLI that consumes a Terraform plan
   file, computes the read- and write-sets, acquires reservations,
   drives Terraform against a per-reservation state slice with
   `-lock=false`, and commits row-level changes back through the
   ADR 0004 lifecycle write path.
3. A documented coexistence story with vanilla `terraform apply`:
   plain Terraform still works against v2 states using a
   whole-state reservation that conflicts with any in-flight
   per-resource reservation.

Non-goals for v2 (deferred to v2.5 / v3):

- **Multi-state transactions.** StateGraph ships this; we treat
  it as a separate scope. One state at a time in v2.
- **Cross-state output slicing.** Same — deferred. v2 reads the
  full referenced state for now.
- **Resource-level RBAC.** Authorization on which addresses an
  identity may reserve. Deferred to v3.
- **Automatic retry on conflict.** v2 fails fast and the operator
  retries. Queueing / waiting is v2.5.
- **Replacing Terraform's plan engine.** v2 still uses
  `terraform plan -out=plan.tfplan` to produce the plan. v2 owns
  the apply orchestration, not the planning.

## Architecture

The data flow:

```
operator                kl apply              database
   │                          │                          │
   │ terraform plan -out=p    │                          │
   │ kl apply -p p   ─►                          │
   │                          │ parse plan JSON          │
   │                          │ → write_set, read_set    │
   │                          │                          │
   │                          │ acquire reservations ───►│ resource_reservations
   │                          │ ◄── ok / conflict        │
   │                          │                          │
   │                          │ build state slice ───────│ resources @ snapshot
   │                          │                          │
   │                          │ terraform apply          │
   │                          │   -state=slice.tfstate   │
   │                          │   -lock=false            │
   │                          │ ◄── new slice            │
   │                          │                          │
   │                          │ row-level commit ───────►│ resources (lifecycle write)
   │                          │ release reservations ───►│ resource_reservations
   │                          │                          │
   │ ◄── apply complete       │                          │
```

### The reservation model

A new table `resource_reservations` replaces `state_locks` on the
v2 path:

```sql
CREATE TABLE resource_reservations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    state_id      uuid NOT NULL REFERENCES states(id) ON DELETE CASCADE,
    address_glob  text NOT NULL,         -- e.g. "module.web.*" or "aws_vpc.main"
    mode          text NOT NULL          -- 'read' or 'write'
                      CHECK (mode IN ('read', 'write')),
    holder        text NOT NULL,         -- actor identifier
    apply_id      uuid NOT NULL,         -- correlates with apply_runs (below)
    info          jsonb NOT NULL DEFAULT '{}'::jsonb,
    acquired_at   timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz,           -- lease; renewed by long-running applies
    CONSTRAINT res_no_self_overlap UNIQUE (state_id, address_glob, holder, apply_id)
);
```

A companion `apply_runs` table mirrors `refresh_runs`: one row per
`kl apply` invocation with status, started_at, finished_at,
counters, error_summary.

**Conflict matrix.** A new reservation request conflicts with an
existing reservation iff their address globs intersect AND at
least one is `write` mode:

|              | existing: read | existing: write |
|---|---|---|
| **new: read** | OK | conflict |
| **new: write** | conflict | conflict |

"Globs intersect" means: any address matched by glob A is also
matched by glob B, or vice versa. For literal addresses, this is
string equality; for prefixed globs (`module.web.*`), it's
prefix containment in either direction. Implementation uses
Postgres's `LIKE` patterns plus a small Go-side check for the
intersection edge cases.

**Leases.** Reservations have an `expires_at` set by the
orchestrator on acquire (default: 15 min) and renewed by a
heartbeat goroutine inside `kl apply` every minute.
Expired reservations are reclaimed lazily by the next acquire
that conflicts with them — this matches `state_locks`' current
behavior and means an `kl apply` killed with SIGKILL
doesn't permanently wedge a subgraph.

### Plan introspection

Terraform's plan file (`plan.tfplan`) is binary, but
`terraform show -json plan.tfplan` produces a documented JSON
representation. From that we extract:

- **Write set:** every address in `resource_changes[]` whose
  `change.actions` is anything other than `["no-op"]` —
  `create`, `update`, `delete`, `replace`. These are the
  addresses we will commit changes to.
- **Read set:** every address referenced by an expression in
  the planned changes' configuration, traversed via the
  graph dependencies recorded in `resource_dependencies`.
  Pragmatically: the transitive closure of `write_set`
  walking dependency edges in both directions.

The read set matters for read-after-write consistency. If A's
plan reads a value from R and B writes R between A's plan and
A's commit, A is computing against stale truth. The reservation
acquired on R in `read` mode tells B to wait.

**Choice we make explicit:** Computing the read set requires a
dependency graph that is already current. In v2, that means a
recent `kl refresh` is a prerequisite for an
`kl apply`. The CLI either runs refresh implicitly or
errors out if the cached graph is older than a configurable
threshold. We pick the latter (explicit) to keep apply runs
fast and deterministic; auto-refresh-before-apply is a v2.5
ergonomics tweak.

### State slicing

Terraform's `apply` needs a state file that **looks complete from
its perspective**: every resource it references (config, plan,
dependencies) must be present, and the serial / lineage must
match what Terraform expects.

For an apply scoped to write set `W` and read set `R`:

```
slice = {
  serial:           current_serial + 1,
  lineage:          same as trunk,
  terraform_version: from current state_version,
  resources:        { r ∈ trunk : r.address ∈ W ∪ R },
  outputs:          all outputs (Terraform validates references)
}
```

Resources in `R \ W` (read-only references) are included so
Terraform's plan validation passes, but the row-level commit
ignores them — we trust whatever the original trunk had.

The slice is materialized in memory and handed to Terraform via
`-state=` and `-state-out=` so Terraform never touches the
backend during the sliced apply. The orchestrator captures the
post-apply state file (`-state-out=`) and feeds it into the
row-level commit.

### Row-level commit

After Terraform's sliced apply finishes, the orchestrator:

1. Parses the post-apply state.
2. Filters resources down to `W` (write set). Anything Terraform
   wrote that we didn't reserve is a bug — fail loud.
3. Hands the filtered set to a variant of the existing
   `applyResourceDelta` (`pkg/store/normalize.go`):
   for each address in `W`, close the open lifecycle on the
   trunk if the hash changed, open a new lifecycle at the new
   serial. Untouched addresses on the trunk are not visited.
4. Writes a new `state_versions` row with `source='apply'` and a
   raw_state computed by re-projecting the trunk view at the new
   serial. The raw_state stays consistent so vanilla `terraform`
   GETs continue to work.
5. Closes the apply_run row with counters, releases all
   reservations.

The lifecycle dedup is what makes this safe: a parallel writer
that touches different addresses doesn't interfere with the
row-level UPDATEs, because the table doesn't have to lock
addresses we're not writing.

The raw_state re-projection at step 4 is non-trivial: two
parallel applies finishing at serials N+1 and N+2 each produce
their own raw_state blob. The blob assembly must be linearized
(one writer at a time produces the canonical raw_state for a
given serial) but the underlying row writes can proceed in
parallel up to that point. The lock taken at step 4 is over
*assemble new raw_state*, not over *write rows*.

### `kl apply` CLI

```sh
kl apply --plan plan.tfplan [--actor=...] [--timeout=15m] [--dry-run]
```

Behavior:

- `--plan` is required. v2 does not own planning.
- `--dry-run` parses the plan, computes write/read sets, attempts
  to acquire reservations (and immediately releases them),
  reports what would happen. No state changes.
- Exit codes:
  - 0 on commit;
  - 1 on apply failure (rolled back, no commit);
  - 2 on reservation conflict (prints the holder + glob);
  - 3 on usage / config errors.

The actor string is recorded on the apply_run row and on every
reservation; the demo (v2d) uses it to label two terminals.

### Coexistence with vanilla `terraform apply`

Engineers using plain `terraform apply` against a v2 state must
still work. The HTTP backend's `LOCK` handler treats a vanilla
lock acquisition as a **whole-state write reservation**:
`address_glob = '*'`, `mode = 'write'`. It conflicts with every
in-flight per-resource reservation; conversely, any
`kl apply` arriving while a vanilla lock is held also
conflicts (its glob is non-`*`, but the whole-state glob `*`
intersects everything).

This is intentional. The vanilla path is a correctness floor:
old workflows keep working, but they don't get the parallelism.
Operators opt in to parallel apply by switching to
`kl apply` for the relevant plans.

## The five hard questions, with answers

These are decisions baked into the design above; calling them
out so they aren't surprises when the code lands.

### 1. Read-set semantics

**Q.** If A's plan reads R but doesn't write R, and B writes R
between A's plan and A's commit, what does A see?

**A.** A acquires a `read` reservation on R when its apply
starts. B's `write` reservation on R conflicts. B must wait
(or error out with exit 2) until A commits and releases. A
proceeds against the value of R that existed at plan time, and
B's write lands after.

This is strict pessimistic locking on the read set, matching
how SERIALIZABLE transactions work in a database. The cost is
that R's writers wait; the win is that A is guaranteed to
commit against the values it planned against.

### 2. Cross-subgraph dependencies

**Q.** If `module.web` reads `module.db.aws_rds.primary.endpoint`
in its plan, and `module.db` is being applied in parallel, what
does `module.web`'s plan see?

**A.** `module.db.aws_rds.primary` is in `module.web`'s read set
(by transitive closure of dependency edges). It conflicts with
the in-flight `module.db` write reservation. `module.web` either
waits or fails fast. Once `module.db` commits, `module.web`
re-plans against the new value.

Cross-state references are out of scope for v2 (deferred). Within
a single state, transitive dependency closure on the read set
gives us cross-subgraph correctness for free.

### 3. Plan staleness window

**Q.** Between `terraform plan -out=plan.tfplan` and
`kl apply --plan`, the world can change. What stops a
plan from being applied against an inconsistent state?

**A.** Two guards:

- The plan file carries the source state's serial. The apply
  orchestrator refuses to proceed if the trunk's current serial
  for any address in the read set is newer than the plan's
  recorded serial. Operator must re-plan.
- The plan file carries the Terraform version + provider
  versions. Mismatch with the trunk's recorded
  `state_versions.terraform_version` is a warning, not a hard
  fail — operators may legitimately upgrade providers between
  plan and apply.

### 4. Failed-apply rollback

**Q.** Terraform's sliced apply fails halfway through. What's
left in the database?

**A.** Nothing. The lifecycle write path runs inside a single
transaction; if the apply fails before commit, no rows are
inserted, no `state_versions` row is created, the trunk is
unchanged. The `apply_runs` row records the failure with
`status='failed'`. Reservations are released.

The cost is that we hold reservations for the duration of the
apply, blocking other writers to the same subgraph. The
alternative (start writing as resources finish) would let
partial failures leave half-applied state, which is strictly
worse than blocking. Vanilla `terraform apply` has the same
behavior at the whole-state level.

### 5. Coexistence with vanilla `terraform apply`

**Q.** Can an engineer still use plain `terraform apply` against
a v2 state?

**A.** Yes, with the caveat above (whole-state reservation that
conflicts with everything). This is the
**correctness-floor-without-the-parallelism** answer: nothing
breaks, nothing speeds up.

## CLI surface and operator UX

A canonical session:

```sh
# Engineer A, module.web changes
terraform plan -out=web.tfplan
kl apply --plan web.tfplan --actor=alice
# → output:
#   reservations acquired (write: module.web.*, read: module.db.aws_rds.primary)
#   apply running... (45s)
#   apply complete: 12 changed, 0 failed
#   committed at serial 47 (source=apply, run=ar_4f2a...)

# Engineer B, module.db changes, simultaneously
terraform plan -out=db.tfplan
kl apply --plan db.tfplan --actor=bob
# → output:
#   reservations acquired (write: module.db.*)
#   apply running... (38s)
#   apply complete: 5 changed, 0 failed
#   committed at serial 48 (source=apply, run=ar_9c0e...)
```

Both succeed. Trunk now has alice's web changes at serial 47
and bob's db changes at serial 48.

A conflicting session:

```sh
# Engineer C tries to update module.web while alice is mid-apply
terraform plan -out=web2.tfplan
kl apply --plan web2.tfplan --actor=carol
# → output:
#   reservation conflict on module.web.*:
#     held by alice (run ar_4f2a..., acquired 12s ago, expires in 14m48s)
#   exit code 2
# carol decides: wait and retry, or rebase the plan after alice finishes.
```

The conflict message is the headline ergonomic — operators must
be able to see immediately who's holding what and how long
they've been holding it. The query backing it is one row in
`resource_reservations`.

## Worked example: the v2d demo

Two terminals against a 3-module fixture (`network` / `web` /
`db`):

```
Terminal A (alice)                Terminal B (bob)
─────────────────                ─────────────────
plan web.tfplan                  plan db.tfplan
kl apply --plan web.tfplan  kl apply --plan db.tfplan
  reservations: write module.web.*    reservations: write module.db.*
  applying...                         applying...
                                      (commits first, serial 24)
  commits, serial 25
done                              done
```

Demo assertion: a single `SELECT * FROM apply_runs ORDER BY started_at`
shows two overlapping run windows (start_at(A) < end_at(B) AND
start_at(B) < end_at(A)). On a flat-state backend, this is
impossible by construction.

A second demo run with **overlapping** subgraphs (both writers
target `module.web.*`) shows the conflict path with the exact
error message above.

## Implementation breakdown

Sized as bite-sized commits that each end on green CI.

**v2a — Reservations substrate.**
- Migration 0007: `resource_reservations` table + indexes.
- Migration 0008: `apply_runs` table.
- `Store.AcquireReservations(ctx, stateID, applyID, want []Reservation)`
  with the conflict matrix as pure-SQL (`SELECT ... FOR UPDATE`).
- `Store.ReleaseReservations(ctx, applyID)`.
- Heartbeat: `Store.RenewReservations(ctx, applyID, lease)`.
- Unit + integration tests for: clean acquire, conflict (read-write,
  write-write, write-read), expiry reclaim, heartbeat.

**v2b — Plan introspection + state slicing.**
- `internal/plan/`: parse `terraform show -json` output; extract
  write set, read set (via transitive dep closure against the
  current `resource_dependencies` view).
- `internal/slice/`: build a `.tfstate` slice from trunk +
  write/read sets.
- `kl apply --plan --dry-run` lands: prints the predicted
  reservations and the slice contents, exits without acquiring.
- Tests: end-to-end against a recorded `terraform show -json`
  fixture covering create / update / delete / replace.

**v2c — Sliced apply + row-level commit.**
- The orchestrator drives `terraform apply` against a slice with
  `-state=`, `-state-out=`, `-lock=false`.
- Row-level commit reuses `applyResourceDelta` filtered to the
  write set.
- `state_versions.raw_state` re-projection from the trunk at
  commit time, under a brief whole-state assembly lock.
- `kl apply` (no `--dry-run`) lands end-to-end.
- Integration tests: one-engineer happy path; apply failure
  leaves trunk untouched; reservations cleared.

**v2d — Parallel-apply demo.**
- `examples/parallel-apply-demo/`: 3-module fixture, two
  `kl apply` invocations in two terminals, scripted via
  `tmux` or `(... &)` to run truly concurrently.
- Asserts on `apply_runs` that the two windows overlap in time.
- Asserts on `resources` that both write sets landed.
- README explaining what just happened, with timings.

**v2e — Coexistence + docs.**
- HTTP backend's LOCK handler emits a `*`-glob write reservation;
  UNLOCK releases it.
- Vanilla `terraform apply` continues to work against v2 states.
- README and ADR updates; positioning page that explicitly names
  StateGraph Velocity as the reference design we're matching.

## Open questions / risks

Calling these out so we don't pretend they're solved:

- **Plan file format stability.** `terraform show -json` output
  has been stable since Terraform 0.12, but provider plan
  encodings can change. We need a compatibility test matrix
  against terraform 1.3+ and the corresponding OpenTofu releases.

- **Outputs.** v2 includes all outputs in every slice (cheap, but
  means an apply that doesn't touch outputs still sees them).
  StateGraph documents per-output slicing; we defer that to v2.5
  with the cross-state work.

- **Provider state.** A sliced apply runs Terraform with its own
  provider configuration (from the slice's `terraform`
  block). If two parallel applies use different provider versions
  / configurations, behavior is undefined. v2 enforces that all
  applies against a state use the same provider versions
  (recorded in `state_versions.terraform_version` + a future
  `provider_versions` column).

- **Read set explosion.** For some states, every resource
  transitively depends on a few foundational ones (e.g. a
  shared VPC). Those resources end up in everyone's read set,
  which means everyone serializes through their writers. We
  accept this as a v2 limitation; the fix is finer-grained
  dependency tracking (per-attribute) which is a research
  project, not a v2 commit.

- **Postgres connection pool.** Long-running applies hold
  reservations and need a DB connection for the heartbeat.
  We must size the pool for `apply_runs.max_parallel × 2 +
  ambient_load`. v2 documents the sizing; v2.5 may move
  heartbeats to a single shared goroutine.

- **Crash recovery semantics.** An `kl apply` SIGKILLed
  mid-commit leaves an `apply_runs` row in `running` and
  reservations held until `expires_at`. The demo script
  documents `kl apply abort <run_id>` as the operator
  escape hatch. Implementation lands in v2a alongside the
  reservations substrate.

## Positioning

The honest framing for v2 in the project's README and pitch:

> Kilolock v2 is the open-source implementation of resource-level
> locking for Terraform state, modeled on StateGraph Velocity.
> Two engineers updating disjoint parts of the same state apply in
> parallel. The data model is shared with v0 (queryable state) and
> v1 (out-of-band refresh): a normalized graph in Postgres with
> content-addressable lifecycles. Apache 2.0, self-hostable, no
> SaaS dependency.

We are not claiming to have invented this. We are claiming that
the value of an OSS implementation — auditable, self-hostable,
extensible — is high enough to be worth doing.

## Addendum: 2026-05-14 — v2 spike findings (`_spike/v2-plan-introspection/`)

A throwaway spike validated the riskiest unknowns above before any
code lands in the main binary. Full write-up in
[`_spike/v2-plan-introspection/NOTES.md`](../../_spike/v2-plan-introspection/NOTES.md);
this addendum records the **design changes** the spike forced.

### What the spike validated as-designed

- **Plan JSON is sufficient.** `resource_changes[].change.actions`
  gives the write set; `configuration.root_module.resources[].expressions`
  yields the static dep graph via recursive `references` collection
  (after filtering `var.*`, `local.*`, `each.*`, `count.*`,
  `path.*`, `terraform.*`). The bidirectional fixed-point closure
  described in the ADR works as written.
- **The conflict matrix and reservation shape** in
  `resource_reservations` survive contact with reality. The spike
  predicted reservations identical to what the formal model
  produces.

### What the spike invalidated

Three concrete assumptions in the design above are wrong:

1. **`terraform apply -state=slice.tfstate -lock=false` is silently
   ignored when a backend is configured.** Terraform sends the
   apply through the configured HTTP backend regardless of `-state`
   / `-state-out`. The "data flow" diagram (`terraform apply
   -state=slice.tfstate`) does not run.

2. **Plan files are bound to the backend they were generated
   against.** Copying `plan.tfplan` to a local-backend working
   directory and applying it sends writes back to the original
   HTTP backend. The CLI shape `kl apply --plan plan.tfplan`
   that uses Terraform to replay an externally-generated plan **does
   not work.** v2 must re-plan inside its own apply working directory.

3. **A state slice limited to `W ∪ R` is too thin.** Terraform
   re-plans inside the apply directory; if any HCL-declared resource
   is missing from the slice, Terraform plans it as `create`. The
   slice must include every trunk resource the HCL configuration
   describes, not just the read/write sets.

### Revised v2 apply model

The apply orchestrator becomes more involved than the original
sketch. The new flow:

```
kl apply [config-dir] [--targets=...]
  1. fetch trunk via Store (read-only snapshot)
  2. preflight: terraform plan inside a tmp dir against trunk
     → terraform show -json → write_set, read_set
  3. acquire reservations on (W, R)
  4. set up apply tmp dir:
     - copy HCL minus any backend.tf
     - inject `terraform { backend "local" {} }`
     - write slice = trunk ∩ HCL-described addresses
       as terraform.tfstate (NOT just W ∪ R)
     - link providers via TF_PLUGIN_CACHE_DIR
  5. terraform init && terraform apply -auto-approve
     (Terraform re-plans against the slice; assert the re-plan's
     write_set matches the predicted write_set or fail loud)
  6. read resulting terraform.tfstate
  7. assert writes ⊆ predicted write_set
  8. for each addr in write_set:
       upsert via the v1 lifecycle write path
  9. release reservations; write state_versions row (source='apply')
```

Two-phase CLI surface (replaces the `--plan plan.tfplan` shape):

```sh
kl plan [config-dir]   # writes kl-plan.json
                               # (our descriptor, not terraform's)
kl apply kl-plan.json
```

`kl-plan.json` is the spike's predicted-reservations payload:
write set, read set, preflight plan summary, source state serial.
Operators review the spec; `kl apply` re-plans against the
slice and asserts the result matches the spec.

### Why "merge only write_set, not no-ops"

Terraform's `apply` re-serializes attribute JSON with alphabetical
keys. The trunk's existing rows may have different key order.
Byte-comparing no-op rows pre- vs post-apply produces false
positives ("drifted" on every no-op). Demonstrated on the deps
fixture: `random_id.vpc` is semantically identical pre and post
but byte-different.

The commit rule the ADR already prescribes — "filter resources down
to W; anything outside is a bug" — is also what makes the merge
correct under terraform's re-canonicalization. The spike confirms
this; the rule survives. It's worth noting that v2c integration
tests must assert this property explicitly (post-apply byte-diff on
no-op rows is expected and ignored; post-apply byte-diff on a
non-write_set row is a fail-loud condition).

### Implementation breakdown — revised

The five-phase breakdown above stays, but with the new model:

- **v2a — Reservations substrate** — unchanged.
- **v2b — Plan introspection + slicing** — the spike code at
  `_spike/v2-plan-introspection/main.go` promotes into
  `internal/plan/` (JSON parsing, dep graph, write/read sets) and
  `internal/slice/` (HCL-footprint slice computation).
  `kl plan` CLI lands here; it writes
  `kl-plan.json` and exits.
- **v2c — Sliced apply + row-level commit** — owns the tmp-dir
  orchestration described above. The orchestrator never calls
  `terraform apply` with `-state=` / `-state-out=`; it materializes
  a working directory with a local backend and runs Terraform
  cleanly inside it.
- **v2d — Parallel-apply demo** — unchanged in scope but the demo
  script will use `kl plan` + `kl apply
  kl-plan.json`, not `terraform plan -out=` + `kl
  apply --plan`.
- **v2e — Coexistence + docs** — unchanged.

### Things the spike did not cover

- Modules with `count` / `for_each` (need indexed-address support
  in dep graph normalization)
- Cross-module references
- Read reservations on data sources (they're `read`-only in plan
  output and need their own reservation class)
- Terraform's `replace_triggered_by` lifecycle blocks (these add
  edges not captured in `references`)

These are v2b implementation work, not blockers on the design.

## References

- ADR 0004 (lifecycle model — the data substrate for row-level
  writes): [./0004-resource-lifecycles.md](./0004-resource-lifecycles.md)
- ADR 0006 (refresh orchestrator — the closest existing analog
  for what `kl apply` will look like):
  [./0006-refresh-implementation.md](./0006-refresh-implementation.md)
- v2 spike (plan introspection + slicing validation):
  [`_spike/v2-plan-introspection/NOTES.md`](../../_spike/v2-plan-introspection/NOTES.md)
- StateGraph Velocity (reference design):
  <https://stategraph.com/docs/velocity>
- StateGraph product overview:
  <https://stategraph.com/how-stategraph-works>
- Terraform plan JSON format:
  <https://developer.hashicorp.com/terraform/internals/json-format>
- Postgres `SELECT ... FOR UPDATE` semantics:
  <https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE>
