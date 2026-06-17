# ADR 0021: Large state payloads, embedded artifacts, and object-storage offload

- **Status:** Proposed
- **Date:** 2026-06-04
- **Decision makers:** Kilolock maintainers
- **Relates to:** [ADR 0001](./0001-foundations.md), [ADR 0004](./0004-resource-lifecycles.md), [ADR 0013](./0013-environment-isolation.md), [ADR 0019](./0019-cloud-first-managed-operations.md), [`docs/protocol.md`](../protocol.md)

## Context

Today Kilolock stores the **full raw Terraform/OpenTofu state snapshot**
inside Postgres for every `state_version`.

That works well for ordinary IaC state, but some providers and workflows can
embed **large payloads** into state, for example:

- Lambda zip/archive metadata
- generated `archive_file` outputs
- large inline templates or rendered blobs
- provider-managed values that include base64 or other large strings

Postgres can technically tolerate large rows using TOAST, so the current model
is not immediately broken. However, repeated full-snapshot history with embedded
blobs creates real operational pressure:

- larger write transactions
- more TOAST churn and vacuum pressure
- slower backup/restore and replication
- larger history retention cost
- more expensive cross-database migration / export workflows

This matters more as we move toward:

- paid SaaS environments
- larger enterprise states
- dedicated-host / BYODB offerings
- stronger support expectations around rollback/history

We also already enforce **resource-count** quotas, but those do not protect us
against **payload-size** explosions. A state with modest resource count can
still be operationally heavy if it embeds large binary-like strings.

## Decision

Kilolock will keep the **raw state JSON as the authoritative logical state
document**, but future large-payload handling will become **size-aware**.

We will introduce a two-tier storage model:

1. **Small/ordinary state versions** stay inline in Postgres.
2. **Large state payloads** are offloaded to object storage, while Postgres
   keeps metadata, digest, size, and retrieval pointers.

This ADR does **not** require immediate implementation. It defines the target
design and phased path.

## Rationale

### Why not treat the database as generic blob storage forever

Postgres TOAST is a good safety net, but it is not a product strategy for
artifact-heavy Terraform states. We want the database to remain strong at:

- transactional metadata
- state version indexing
- normalized resources
- audit and control-plane joins

We do **not** want state-history economics to be dominated by archived zip-like
payloads living forever inside row storage.

### Why keep raw state as the logical source of truth

Terraform/OpenTofu backend compatibility depends on serving and accepting a full
raw state document. Kilolock should not redefine the state format or rely only
on normalized tables.

So the target is:

- raw state remains authoritative
- normalized rows remain query/index substrate
- storage location of the raw blob becomes an implementation detail

## Target design

### D1. Split state-version metadata from raw payload placement

Each `state_version` should conceptually carry:

- version identity / serial / lineage
- created time / actor
- logical raw state digest
- raw state byte size
- compression metadata
- payload location

Payload location has two modes:

- `inline_db`
- `object_storage`

For `inline_db`, current behavior remains.

For `object_storage`, Postgres stores:

- payload digest
- compressed size / uncompressed size
- object key / URI reference
- encryption metadata if needed

### D2. Add byte-size aware policy

In addition to resource-count quotas, add payload-aware controls:

- `max_state_bytes_soft`
- `max_state_bytes_hard`
- optionally later: `max_environment_history_bytes`

These are separate from resource-count limits because they protect a different
risk class.

### D3. Compression first, offload second

Before offloading, Kilolock should support compression of raw state payloads.

Preferred write path:

1. receive raw state
2. normalize resources as today
3. compute digest and byte size
4. compress payload
5. decide inline vs offload based on configured threshold

This preserves one retrieval path while reducing storage cost even for inline
states.

### D4. Object storage is implementation-specific, not protocol-visible

Clients continue to see the normal HTTP backend behavior:

- `GET` returns raw state JSON
- `POST` accepts raw state JSON

No client should know whether the payload is stored:

- inline in Postgres
- compressed in Postgres
- offloaded to object storage

### D5. History and rollback remain supported

Rollback/history semantics stay intact:

- each version still has a logical raw state payload
- retrieval may load from object storage instead of row storage
- normalized resources still reference the same logical version

This preserves current product direction:

- operator rollback remains possible
- historical queryability remains possible

### D6. Dedicated-host / BYODB boundaries

When environments are on dedicated DB or BYODB, we must keep storage boundary
rules explicit:

- default expectation: large payload object storage lives within the same trust
  boundary as the environment
- shared-host customers may use platform-managed object storage
- dedicated/BYODB customers may later require:
  - customer-owned bucket
  - platform-managed bucket with encryption guarantees
  - “no offload allowed” mode

This ADR defines the need for that policy, not the final enterprise SKU matrix.

## Consequences

### Positive

- reduces pressure on Postgres for blob-heavy states
- keeps raw state compatibility intact
- gives us a clean future path for enterprise-scale state history
- lets us add real payload-byte quotas instead of relying only on resource count

### Tradeoffs

- more operational complexity
- more moving parts on read/write path
- backup/restore now spans DB + object storage
- BYODB policy becomes more nuanced

## Non-goals

This ADR does **not** propose:

- storing provider artifacts as first-class structured objects
- changing Terraform/OpenTofu state format
- removing raw state history entirely
- implementing customer-facing large-object migration workflows now

## Phased implementation path

### Phase 1: observability only

1. record raw state byte size and compressed size
2. add operator queries / dashboards for large states
3. add warning thresholds in control/operator surfaces

### Phase 2: quota policy

1. add soft/hard byte quotas per state
2. optionally add environment-level history byte visibility
3. improve error messages for oversized state writes

### Phase 3: compression

1. compress raw state payloads at rest
2. transparently decompress on read
3. validate rollback/export behavior unchanged

### Phase 4: object-storage offload

1. add payload location metadata
2. write large state versions to object storage
3. keep smaller states inline
4. preserve full HTTP backend compatibility

### Phase 5: enterprise boundary options

1. dedicated-host / BYODB storage policy
2. customer-managed bucket support if needed
3. backup/restore and retention alignment across DB + object storage

## Practical product guidance before implementation

Until this ADR is implemented:

- Kilolock should be positioned as **state management**, not artifact storage.
- Large payloads embedded into state are supported only on a best-effort basis
  by current Postgres storage behavior.
- Resource-count quotas must not be treated as sufficient protection against
  oversized payload states.

## References

- Terraform/OpenTofu HTTP backend contract
- PostgreSQL TOAST behavior
- Existing protocol limits in [`docs/protocol.md`](../protocol.md)
