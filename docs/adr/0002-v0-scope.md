# ADR 0002: v0 scope — Queryable state

- **Status:** Accepted
- **Date:** 2026-05-12
- **Decider(s):** @davesade (David Kubec)
- **Supersedes / Relates to:** [ADR 0001](./0001-foundations.md)

## Context

Kilolock's eventual value lies in graph-scoped plans and parallel applies,
but those depend on a working data model, a stable on-disk schema, and a
reliable migration path from existing `.tfstate` files. Building the plan
engine first means building it on top of an unproven schema, which guarantees
rework.

A separate problem: shipping nothing for a year while a full graph-scoped
engine matures is a known failure mode for ambitious infrastructure projects.
There needs to be a usable v0 that justifies the schema work on its own.

## Decision

v0 of Kilolock is scoped to **queryable state**. The goal: take an existing
`.tfstate` file, normalize it into the Postgres schema, and let users query
the result with SQL. No plan or apply changes; users keep running
`terraform` / `tofu` exactly as today.

This is intentionally narrow. It is also intentionally useful: many of the
real-world pains around large states (inventory, drift detection, compliance
reporting, cross-state queries) are addressable with read-only queries alone.

## Goals (in scope for v0)

1. **HTTP backend protocol server.** Implement the documented Terraform
   `http` backend so users can point their `backend "http"` block at
   Kilolock and have state stored in Postgres.
2. **Round-trip fidelity.** A `.tfstate` imported into Kilolock and then
   exported back must be byte-equivalent (modulo whitespace and key
   ordering) to the original. This is the non-lock-in guarantee.
3. **Normalized schema.** Resources, attributes, dependencies, outputs, and
   modules are stored as proper rows, not as opaque JSON blobs. Attributes
   stay in JSONB for shape flexibility, but the structural edges of the
   graph live in real relational tables.
4. **Query CLI.** `kl query "SELECT ..."` runs read-only SQL against
   the graph and returns formatted results (table, JSON, CSV).
5. **Standard inventory queries documented.** A handful of canonical example
   queries shipped in `docs/queries/`: "find resources by type across all
   states," "show transitive dependents of resource X," "find resources
   matching attribute predicate."
6. **Single-command local setup.** `docker compose up` brings Postgres up;
   `kl migrate` applies the schema; `kl import path.tfstate`
   loads a state. Total time: under 60 seconds on a developer laptop.

## Non-goals (explicitly deferred)

- **Plan or apply orchestration.** No subgraph plans, no scoped refresh, no
  apply orchestration. v0 is read-mostly with respect to the cloud — it
  stores and serves state, it does not interact with providers.
- **HCL parsing.** v0 does not look at HCL at all. It receives a `.tfstate`
  blob (via import or via the HTTP backend `POST`) and normalizes it.
- **Authentication / authorization.** v0 ships with a single trusted-network
  deployment model. RBAC and per-resource permissions belong to a later ADR.
- **UI.** No web UI in v0. SQL via the CLI is the user-facing surface.
- **Multi-tenancy.** One logical Kilolock instance per organization. Tenant
  isolation is a much later concern.
- **Drift detection.** Inferring drift requires querying providers, which is
  v1 territory (provider-protocol-native engine).
- **High availability.** v0 is single-instance. Postgres is the durability
  boundary; the Kilolock process is restartable and stateless.

## Acceptance criteria

v0 ships when **all** of the following are true:

1. **`kld`** starts an HTTP server that passes the Terraform
   `http` backend contract: `GET`, `POST`, `LOCK`, `UNLOCK`, `DELETE`.
2. **`kl import <file.tfstate>`** loads any valid Terraform v4 state
   file produced by Terraform 1.x or OpenTofu 1.x, with no data loss.
3. **`kl export <state-name> -o <file.tfstate>`** produces a state
   file that, when used by `terraform plan`, results in **no diff** against
   the original state file.
4. **`kl query "SELECT ..."`** executes against the graph and
   returns results as table, JSON, or CSV (via `--format`).
5. **Round-trip property test.** A test corpus of at least 10 real-world
   `.tfstate` files (sizes ranging from ~50KB to ~50MB) round-trips through
   import/export with verified byte-equivalence.
6. **End-to-end demo.** A repeatable demo: spin up Postgres via
   docker-compose, configure a sample Terraform project to use Kilolock
   as backend, run `terraform apply`, observe rows in Postgres, run
   sample SQL queries against the result.

## Out of scope for v0 — explicitly listed to set expectations

- Migration support for the `s3`, `gcs`, `azurerm`, `consul`, `pg`, and
  Terraform Cloud backends. v0 supports the `http` backend only. Users with
  state in other backends are expected to `terraform state pull > x.tfstate`
  and `kl import x.tfstate`.
- Web hooks or notifications on state change.
- Sensitive attribute handling beyond what Terraform already does in the
  source state file. Kilolock stores what Terraform writes; treating
  sensitive values specially is a later ADR.
- Performance optimization beyond "fast enough on a developer laptop."
  Benchmarks and tuning are a v1 concern.

## Consequences

**Positive.**

- A working v0 ships in weeks to months of weekend work rather than years.
- The schema gets battle-tested on real states before the plan engine
  depends on it.
- Inventory, compliance, and drift-after-the-fact use cases are immediately
  unlocked, which provides reason for users to adopt before the bigger
  features land.
- The non-lock-in guarantee (round-trip fidelity) is established as a
  core property up front, before users have a chance to assume otherwise.

**Negative.**

- v0 alone does not solve the original motivating problem (slow plans on
  huge states). Users hitting that pain will need to wait for v1.
- The HTTP backend protocol is straightforward but does not expose Kilolock's
  graph nature to Terraform itself. Terraform still asks for and writes the
  whole blob over the wire on every plan/apply. Network round-trip overhead
  is the v0 baseline; reducing it is a v1+ concern.
