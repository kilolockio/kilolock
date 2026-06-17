# Graph schema (v0)

This document explains the v0 PostgreSQL schema and the reasoning behind it.
The actual DDL lives in `internal/migrate/migrations/0001_baseline.sql`.

The v0 schema has one job: faithfully represent a Terraform v4 state file as
a normalized, queryable graph, with full round-trip fidelity. Optimization
for the v1+ graph-scoped engine is explicitly out of scope here; the schema
will evolve as that engine takes shape.

## Design principles

1. **Round-trip fidelity is non-negotiable.** Anything Terraform writes into
   a state file must survive import-then-export with byte-equivalence
   (modulo whitespace and key ordering). The schema reflects this: anything
   the state file carries that we cannot meaningfully normalize is preserved
   verbatim in a JSONB column.
2. **Structural edges are relational; payload is JSONB.** Resource identity,
   dependencies, module membership, and provider references are first-class
   rows. Provider-specific attribute trees (the actual fields of an
   `aws_instance`, `kubernetes_deployment`, etc.) are JSONB, because their
   shape is provider-specific and changes with each provider release.
3. **State versions are immutable.** Each write produces a new
   `state_versions` row. The latest version is the "current" state; older
   versions are kept for history, debugging, and the kind of audit queries
   that file-based state cannot answer.
4. **UUIDs everywhere.** No incrementing primary keys exposed across API
   boundaries. UUIDs allow IDs to be generated client-side, simplify
   eventual sharding, and avoid leaking row counts.
5. **Postgres-native, not portable.** GIN indexes on JSONB, recursive CTEs
   for traversal, and `EXCLUDE` constraints for lock semantics are all used
   without apology. Portability to other databases is not a v0 goal.

## Tables

### `states`

One row per logical Terraform state. A state has a stable name (which
Terraform's HTTP backend uses to identify it) and a Terraform-issued
`lineage` UUID that survives state moves.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` | PK |
| `name` | `text` | Unique. Matches the HTTP backend's state identifier. |
| `lineage` | `uuid` | Terraform's `lineage` field; survives renames. |
| `created_at` | `timestamptz` | |
| `updated_at` | `timestamptz` | Bumped on every new version. |
| `current_version_id` | `uuid` | FK to `state_versions.id`. Latest version pointer. |

### `state_versions`

Immutable. One row per state write. The `raw_state` column preserves the
original `.tfstate` JSON byte-for-byte; the normalized tables (`resources`,
`outputs`, `resource_dependencies`) are projections of this.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` | PK |
| `state_id` | `uuid` | FK to `states.id`. |
| `serial` | `bigint` | Terraform's monotonic `serial` field. Unique per state. |
| `terraform_version` | `text` | Source Terraform/OpenTofu version. |
| `raw_state` | `jsonb` | Verbatim parsed `.tfstate`. Source of truth for export. |
| `source` | `text` | `import`, `http_backend`, `migration`, etc. |
| `created_at` | `timestamptz` | |
| `created_by` | `text` | Free-form actor identifier; v0 does not enforce auth. |

`UNIQUE (state_id, serial)` enforces Terraform's monotonic-serial invariant.

### `resources`

One row per resource instance (i.e. one row per element after `for_each` /
`count` expansion). Sub-resources (Terraform's nested blocks) live inside
`attributes`, not as separate rows; treating them as separate would diverge
from Terraform's own resource-graph model.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` | PK |
| `state_version_id` | `uuid` | FK to `state_versions.id`. |
| `address` | `text` | Canonical Terraform address: `module.foo.aws_instance.bar[0]`. |
| `mode` | `text` | `managed` or `data`. |
| `type` | `text` | e.g. `aws_instance`. |
| `name` | `text` | The local name in HCL. |
| `provider` | `text` | Full provider reference: `provider["registry.terraform.io/hashicorp/aws"]`. |
| `module_path` | `text` | Empty string for root module; otherwise `module.foo.module.bar`. |
| `index_kind` | `text` | `none`, `int`, or `string`. |
| `index_value` | `text` | Stringified index for `for_each` / `count` instances. |
| `attributes` | `jsonb` | Provider attribute tree, exactly as Terraform writes it. |
| `sensitive_paths` | `jsonb` | List of attribute paths Terraform flagged sensitive. |
| `dependencies_raw` | `jsonb` | Denormalized copy of the `dependencies` array, for export fidelity. |

`UNIQUE (state_version_id, address)` because Terraform addresses are unique
within a state version.

JSONB indexes:

- GIN on `attributes` for ad-hoc attribute queries.
- Functional B-tree indexes on common type filters (`type`, `provider`)
  expected from inventory queries.

### `resource_dependencies`

The edges of the graph. One row per declared dependency.

| Column | Type | Notes |
|---|---|---|
| `from_resource_id` | `uuid` | FK to `resources.id`. The dependent. |
| `to_resource_id` | `uuid` | FK to `resources.id`. The dependency. |
| `kind` | `text` | `explicit` (from `depends_on`) or `implicit` (Terraform's inferred references). v0 cannot distinguish these from state alone, so all rows are `unknown` for now. |

`UNIQUE (from_resource_id, to_resource_id)` prevents duplicate edges.

**Why a separate table when `dependencies_raw` is also stored on
`resources`?** The denormalized column on `resources` preserves the
state-file representation (which is just a list of address strings) for
exact round-tripping. The separate `resource_dependencies` table is for
traversal queries (recursive CTEs, blast-radius analysis). Both must agree.

### `outputs`

State-level outputs.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` | PK |
| `state_version_id` | `uuid` | FK to `state_versions.id`. |
| `name` | `text` | |
| `value` | `jsonb` | Output value, possibly any JSON shape. |
| `value_type` | `jsonb` | Terraform's encoded type, preserved verbatim. |
| `sensitive` | `boolean` | |

`UNIQUE (state_version_id, name)`.

### `state_locks`

Implements the HTTP backend's `LOCK` / `UNLOCK` semantics. v0 enforces
whole-state locks only; subgraph-scoped locks are a v1+ schema extension.

| Column | Type | Notes |
|---|---|---|
| `state_id` | `uuid` | PK. Exactly one lock per state. |
| `lock_id` | `text` | Terraform's lock ID. Echoed back on `UNLOCK`. |
| `info` | `text` | Free-form lock metadata Terraform sends. |
| `who` | `text` | User+host string. |
| `version` | `text` | Terraform version. |
| `created` | `text` | Timestamp as Terraform sends it. |
| `path` | `text` | Resource path Terraform reports. |
| `acquired_at` | `timestamptz` | Server-side acquisition timestamp. |
| `expires_at` | `timestamptz` | Optional lock TTL for orphan recovery. |

The single-row-per-state model uses a primary key on `state_id` directly.
Lock contention is resolved by `INSERT ... ON CONFLICT DO NOTHING`: if a
row already exists, the requester sees `409 Conflict`, which matches the
HTTP backend contract.

### `events`

A simple append-only audit trail. Every state write, lock, unlock, import,
and export produces a row. Cheap to write; useful for "what happened to
this state last week" queries.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` | PK |
| `kind` | `text` | `state_write`, `lock_acquire`, `lock_release`, `import`, `export`, etc. |
| `state_id` | `uuid` | Nullable — some events are not state-scoped. |
| `state_version_id` | `uuid` | Nullable. |
| `actor` | `text` | Free-form. |
| `payload` | `jsonb` | Event-specific details. |
| `created_at` | `timestamptz` | |

Indexed on `(state_id, created_at)` for the common audit-trail query.

## What the schema is _not_ doing in v0

- **No `projects` / `organizations` / `workspaces` table.** State names are
  globally unique within an instance. Hierarchy and multi-tenancy come
  later.
- **No separate `modules` table.** Module identity in v0 is a string
  (`module_path`) on each resource. Calls per module, module sources, and
  module version pinning are HCL concepts, not state concepts, and HCL
  parsing is v1.
- **No provider-aware attribute schemas.** `attributes` is opaque JSONB.
  Type-aware querying (e.g. "find all `aws_s3_bucket` resources whose
  `server_side_encryption_configuration` is missing") works via JSONB
  operators, not a typed projection.
- **No write-through caching.** Every read hits Postgres. v0 is
  single-instance; aggressive caching is unnecessary and would risk
  serving stale state.

## Indexes and queries

Planned indexes for v0:

```sql
CREATE INDEX ON resources (type);
CREATE INDEX ON resources (state_version_id);
CREATE INDEX ON resources USING gin (attributes);
CREATE INDEX ON resource_dependencies (to_resource_id);
CREATE INDEX ON state_versions (state_id, serial DESC);
CREATE INDEX ON events (state_id, created_at DESC);
```

Common queries the schema is shaped for:

- **Resource inventory by type, across all states.** Joins `resources` to
  `state_versions` filtered to current versions; `GROUP BY type`.
- **Blast radius from a resource.** Recursive CTE over
  `resource_dependencies`, starting from a resource address.
- **Attribute predicate search.** GIN-indexed JSONB lookup against
  `attributes` (`attributes @> '{"public_access": true}'`).
- **State version history.** `state_versions` ordered by `serial`,
  joined to `events` for actor information.

## Open questions for v1+

- How should subgraph-level locks coexist with whole-state locks during a
  transition period? Likely a separate `resource_locks` table with an
  `EXCLUDE` constraint to prevent overlap.
- Should `resource_dependencies` distinguish explicit vs implicit edges
  once we parse HCL? Yes, but only after HCL parsing lands.
- Cross-state edges (one state's resource referencing another state's
  output, via `terraform_remote_state`) deserve their own edge table once
  v1 cares about cross-state graphs.
