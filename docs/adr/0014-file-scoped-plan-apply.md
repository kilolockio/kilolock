# ADR 0014: File-scoped fast plan/apply

- **Status:** Proposed
- **Date:** 2026-05-27
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0004](./0004-resource-lifecycles.md), [ADR 0006](./0006-refresh-implementation.md), [ADR 0007](./0007-parallel-apply.md), [ADR 0013](./0013-environment-isolation.md)

## Context

ADR 0007 fixes the apply-side lock bottleneck: two operators with
disjoint write sets can apply to the same state concurrently. The
current v2 path still depends on Terraform to produce the initial
plan for the full configuration:

```sh
kl plan <config-dir>
kl apply --plan-spec kl-plan.json
```

For large customers, this is only half the problem. A 50k- or
100k-resource state can spend a long time in `terraform plan` even
when the operator changed one file and expects one resource or one
module to move. Refresh can be disabled or moved out of band by
ADR 0006, but Terraform still loads the whole configuration, builds
the graph, expands resources, compares many no-op instances, and
emits a huge plan JSON so Kilolock can discover a tiny write set.

The product goal for hosted Kilolock is stronger:

```sh
kl plan  -f database.tf --out database.igplan
kl apply database.igplan

# convenience form
kl apply -f database.tf
```

The operator should be able to say: "this file is my intended
change boundary." Kilolock should derive the affected resources,
build the smallest safe Terraform workspace and state slice, acquire
row-level reservations, run Terraform only over that slice, and
commit the result back to the shared trunk. The HCL file remains
ordinary Terraform. When it is later committed and pushed, vanilla
Terraform over the full repository must still see normal Terraform
semantics.

This ADR defines the desired state and development path for that
feature.

## Decision

Kilolock will add **file-scoped plan/apply**: a fast path that
starts from one or more `.tf` files instead of a full-repository
Terraform plan.

The command surface:

```sh
kl plan --file database.tf --out database.igplan
kl plan -f database.tf -f db-alarms.tf --out database.igplan
kl apply database.igplan
kl apply -f database.tf
```

The plan artifact remains an Kilolock plan spec, not a Terraform
binary plan. It records:

- selected source files
- generated time, config directory, Terraform version, and state name
- file-owned write footprint
- dependency/read footprint
- HCL files copied into the temporary workspace
- pinned input variables
- expected reservations
- safety checks and unsupported constructs discovered during analysis

The feature is an **execution optimization**, not a Terraform
language fork. The source files are still plain HCL. A later
vanilla `terraform plan` or `terraform apply` against the full
repository must remain valid and converge to the same state, modulo
normal drift and concurrent changes.

## Desired workflow

Two engineers can work in the same large state without paying full
plan latency:

```sh
# engineer A
vim database.tf
kl apply -f database.tf
git commit -am "change database"

# engineer B
vim cdn.tf
kl apply -f cdn.tf
git commit -am "change cdn"
```

If `database.tf` and `cdn.tf` expand to disjoint write sets, both
applies proceed concurrently through ADR 0007 reservations. If the
files overlap, or one file reads what the other writes, the existing
reservation matrix blocks or waits with the same operator-facing
conflict output as `wait-demo.sh`.

## Architecture

The file-scoped path is a new frontend to the existing v2 apply
substrate:

```
operator                  kl plan -f              database
   │                             │                            │
   │ database.tf ───────────────►│                            │
   │                             │ parse selected HCL         │
   │                             │ derive file footprint      │
   │                             │ load graph/lifecycles ────►│
   │                             │ build read/write sets      │
   │                             │ write .igplan              │
   │◄────────────────────────────│                            │

operator                  kl apply                 database
   │                             │                            │
   │ database.igplan ───────────►│                            │
   │                             │ acquire reservations ─────►│
   │                             │ build HCL workspace        │
   │                             │ build state slice ────────►│
   │                             │ terraform plan/apply       │
   │                             │ row-level commit ─────────►│
   │◄────────────────────────────│                            │
```

### File analysis

The analyzer starts with one or more selected files. It parses HCL
and extracts the blocks that directly belong to the file:

- `resource`
- `data`
- `module`
- `locals`
- `variable`
- `moved`, `removed`, and `import` blocks
- provider requirements and provider alias references

The initial write footprint is:

- root resources declared in selected files
- module call prefixes declared in selected files (`module.db.*`)
- addresses affected by selected `moved`, `removed`, or `import`
  blocks

The read footprint comes from:

- references found in selected block expressions
- explicit `depends_on`
- dependency edges already known in the Kilolock graph
- provider configuration dependencies
- variables, locals, and data sources needed to evaluate selected
  blocks

The analyzer must be conservative. If it cannot prove a smaller
footprint is safe, it widens the footprint or fails with a useful
diagnostic. A fast plan that silently ignores a dependency is worse
than no fast plan.

### Temporary HCL workspace

The apply path materializes a temporary Terraform workspace that
contains only the HCL needed for the selected footprint:

- selected files
- required provider and Terraform version constraints
- provider configuration blocks and aliases used by selected
  resources/modules
- variable declarations and pinned values
- local definitions referenced by selected expressions
- module blocks or copied module directories when a selected file
  owns a module call
- generated backend-free override glue when needed

The temporary workspace must not use the remote HTTP backend. It
uses local `-state=` and `-state-out=` files exactly like the
current sliced apply path. Terraform never writes directly to the
Kilolock backend during file-scoped apply.

### State slicing

The state slice is built from the current trunk using the same
principle as ADR 0007:

```
slice = write_footprint ∪ read_footprint ∪ validation_footprint
```

The `validation_footprint` exists because Terraform often requires
objects that are not direct read dependencies to validate references,
outputs, provider aliases, or module structure. It is included in
the local state so Terraform can plan/apply, but row-level commit
ignores it unless the address is in the write footprint.

### Reservations

Reservations are derived before Terraform apply:

- write reservations for file-owned write addresses/prefixes
- read reservations for dependency/read addresses
- optional whole-module prefixes when a module call is selected and
  exact expanded resources cannot be determined cheaply

The same conflict matrix from ADR 0007 applies. This preserves the
parallelism story: file-scoped plan reduces plan latency; row-level
reservations preserve concurrency safety.

### Commit safety

Before commit, Kilolock must parse Terraform's post-apply state
and enforce:

```
changed_addresses ⊆ reserved_write_footprint
```

Any surprise write aborts the commit and releases reservations. The
error must include the unexpected addresses and the files/operators
that caused the inferred footprint, so the user can either add files
to the scope or fall back to a full plan.

The existing optimistic serial retry from `internal/apply` still
applies. If another disjoint writer commits first, Kilolock
re-reads trunk, overlays the file-scoped post-apply changes, and
retries the state write while holding the write reservations.

## MVP scope

The first implementation should intentionally be narrow:

1. Root-module `resource` blocks in selected files.
2. Provider blocks may live in separate files and are copied into
   the temporary workspace.
3. Variables are pinned into the plan spec using the same effective
   variable capture already used by current `kl plan`.
4. Dependencies outside selected files are read-only and included
   in the state slice when referenced.
5. `module` blocks in selected files are treated as whole-module
   prefixes (`module.name.*`) rather than exact resource expansion.
6. Unsupported constructs fail early with diagnostics and a
   recommended full-plan fallback.

Explicitly deferred from MVP:

- perfect partial evaluation of complex locals
- precise `for_each` key inference when keys depend on dynamic data
- fine-grained resources inside selected modules
- cross-state output slicing
- provider-side planning without Terraform
- semantic HCL rewriting beyond minimal backend-free workspace glue

## Development path

| Phase | Deliverable |
|-------|-------------|
| **F1** | HCL file analyzer spike: parse selected files, list resource/data/module/provider/variable/local blocks, collect expression references, emit diagnostics |
| **F2** | Footprint model: `FilePlanSpec` additions for selected files, write footprint, read footprint, validation footprint, unsupported constructs |
| **F3** | Workspace builder: create backend-free temporary Terraform workspace from selected files plus required provider/variable/local support files |
| **F4** | State slicer integration: build slice from current trunk using file-derived footprint instead of full plan-derived footprint |
| **F5** | CLI: `kl plan -f FILE --out SPEC`, initially root resources only, with stable JSON output and tests |
| **F6** | Apply integration: `kl apply SPEC` accepts file-derived specs and reuses ADR 0007 reservations/commit path |
| **F7** | Safety hardening: surprise-write detection, plan-staleness guard, better diagnostics, and full-plan fallback hints |
| **F8** | Multi-file and module-prefix support; support `-f` repeated and `--changed-from <git-ref>` as a later convenience |
| **F9** | Large-state benchmark/demo: 10k, 50k, 100k resources showing file-scoped plan latency vs full Terraform plan latency |

## Consequences

**Positive**

- Attacks the second large-state bottleneck: plan latency, not only
  apply lock contention.
- Makes Kilolock's graph database the planner's starting point,
  which is the product's core architectural advantage.
- Gives SaaS customers a premium workflow: fast file-level changes
  on huge states without abandoning vanilla Terraform compatibility.
- Pairs naturally with ADR 0007: file-scoped plans produce smaller
  reservation footprints, which increases parallelism.

**Negative**

- HCL analysis is subtle. Terraform configuration semantics are
  broad, and a partial analyzer must be conservative to stay safe.
- The temporary workspace builder becomes a new compatibility
  surface with Terraform/OpenTofu versions.
- Some real repositories will initially fall back to full planning,
  especially module-heavy or highly dynamic configurations.
- The feature needs excellent diagnostics; otherwise "fast plan"
  failures will feel mysterious.

## Safety invariants

1. File-scoped apply never commits an address outside its reserved
   write footprint.
2. File-scoped apply never writes directly through Terraform's HTTP
   backend; all writes go through Kilolock's row-level commit path.
3. A selected file remains valid ordinary Terraform HCL. The feature
   does not introduce Kilolock-only source syntax.
4. When analysis is uncertain, the implementation fails closed or
   widens scope. It never guesses a narrower footprint silently.
5. Exported `.tfstate` after file-scoped apply remains acceptable to
   vanilla Terraform/OpenTofu.
6. Mutating `apply --file` runs require explicit scope acknowledgement
   (`--confirm-scope`). `--dry-run` prints the derived scope and a
   copy/paste rerun command.

## Open questions

- Should `kl apply -f FILE` run an implicit scoped plan and
  immediately apply, or should production usage require an explicit
  saved `.igplan` artifact?
- How much provider configuration should be copied versus generated
  into the temporary workspace?
- Should module blocks selected by `-f` always reserve
  `module.name.*`, or should exact module expansion be an early
  requirement?
- How should file-scoped apply interact with `moved`, `removed`, and
  `import` blocks when those blocks live in separate migration files?
- Should the hosted product expose `--changed-from <git-ref>` as a
  first-class workflow, mapping changed files to repeated `-f`
  arguments?
- What is the minimum Terraform/OpenTofu version matrix for reliable
  plan JSON, config parsing, and local-state sliced apply behavior?
