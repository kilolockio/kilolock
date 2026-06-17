# ADR 0001: Foundations

- **Status:** Accepted
- **Date:** 2026-05-12
- **Decider(s):** @davesade (David Kubec)

## Context

Kilolock is a new open-source project that aims to replace Terraform's flat
`.tfstate` file with a normalized PostgreSQL database. Before writing any code,
six foundational decisions need to be locked in because they shape every
downstream choice: license, name, implementation language, compatibility
target, storage philosophy, and CLI strategy.

This ADR records those decisions and the reasoning behind them.

## Decisions

### D1. License: Apache 2.0

**Decision.** The project is licensed under Apache License 2.0.

**Why.** Apache 2.0 is the dominant license in the Terraform / OpenTofu
ecosystem and infrastructure tooling generally. It allows commercial use and
forks, includes an explicit patent grant, and matches user expectations for an
open-source alternative to a commercial product. Alternatives considered:

- **AGPL v3** — discourages SaaS competitors, but adds friction for legitimate
  users and is poorly received by enterprise legal teams.
- **BSL / SSPL** — open-but-not-OSS; conflicts with the project's stated
  identity as "open source alternative."
- **MPL 2.0** — what Terraform used pre-BSL, but Apache 2.0 has won the
  ecosystem.

### D2. Name: Kilolock

**Decision.** The project is named **Kilolock**.

**Why.** The name reflects the core thesis (infrastructure as a graph) without
borrowing from "state" terminology, which keeps the door open for the project
to extend beyond pure state management (drift detection, inventory, cost
attribution) without a rename. Short, pronounceable, available as a top-level
GitHub project name.

### D3. Implementation language: Go

**Decision.** The reference implementation is written in Go (target 1.22+).

**Why.**

- Direct interoperability with the Terraform / OpenTofu ecosystem, which is
  entirely Go: `hashicorp/hcl`, `hashicorp/terraform-json`,
  `terraform-plugin-go` (provider gRPC client bindings), and OpenTofu's own
  internals.
- Single static binary distribution, which suits a backend server and a CLI
  equally.
- Strong concurrency primitives, which will matter once parallel
  subgraph-scoped plans land in v1+.

Rust was the only serious alternative considered. Rejected for v0 because the
library ecosystem (HCL parser, provider protocol client) is immature and would
add substantial yak-shaving before the project produces user value.

### D4. Compatibility target: provider-protocol native

**Decision.** Kilolock speaks the documented public protocols directly: the
**Terraform HTTP backend protocol** on the state-storage side, and the
**provider gRPC protocol** (`go-plugin` over gRPC) on the provider side. It is
not a fork of Terraform or OpenTofu, and it does not shell out to the
`terraform` / `tofu` CLI in steady-state operation.

**Why.**

- Both protocols are stable, documented, and shared between Terraform and
  OpenTofu. Targeting the protocols rather than the products gives Kilolock
  compatibility with both ecosystems for free.
- Forking OpenTofu would create a permanent maintenance burden and tie
  Kilolock's release cadence to OpenTofu's.
- Wrapping the CLI inherits the CLI's slowness — refresh, state-file
  serialization, and global lock acquisition — which directly defeats the
  project's purpose.

**Cost of this decision.** Provider-protocol-native is the most ambitious of
the three viable architectures. It is not relevant in v0 (which touches only
the HTTP backend protocol, not the provider protocol), but it represents
significant work in v1+. This trade-off is accepted; the project's value
proposition collapses without scoped refresh, and scoped refresh requires
direct provider communication.

### D5. Storage: PostgreSQL-only for v1

**Decision.** PostgreSQL 14+ is the only supported storage backend for the
foreseeable future. Internal interfaces will be designed for cleanliness, not
for swappability.

**Why.**

- PostgreSQL handles graph traversal cleanly via recursive CTEs and `LATERAL`
  joins; the use case does not require a dedicated graph database.
- JSONB allows storing provider-specific resource attributes without rigid
  schemas, while still being queryable with operators and GIN indexes.
- Transactional guarantees (advisory locks, serializable isolation) are
  exactly what state locking and scoped writes need.
- Operationally familiar to every team that would adopt this. Managed
  offerings (RDS, Cloud SQL, Aiven, Neon) cover deployment.

Neo4j and other graph databases were considered and rejected: licensing
friction (Neo4j Enterprise is commercial; Community lacks clustering),
weaker transactional story for this exact pattern, and ops surface that the
target audience does not already run. The "graph" in Kilolock is a logical
model, not a database type.

### D6. CLI strategy: separate `kl` binary

**Decision.** Kilolock ships a single binary, `kl`, that provides
both the backend server and the user-facing CLI. Users keep using
`terraform` / `tofu` for HCL evaluation; Kilolock is configured as the
backend they point at.

**Why.**

- The user-facing surface for v0 is a CLI (`kl query`,
  `kl import`, `kl export`) plus a long-running server. One
  binary, multiple subcommands, follows the pattern users already know from
  `terraform`, `kubectl`, `git`.
- Not replacing the user's `terraform` / `tofu` invocation means no CLI
  compatibility table to maintain in v0.
- Once the provider-protocol-native engine arrives in v1+, the same binary
  can grow `kl plan` and `kl apply` subcommands without
  changing the deployment story.

## Consequences

**Positive.**

- The combination Apache 2.0 + Go + Postgres + HTTP backend protocol matches
  every assumption a Terraform/OpenTofu user already holds. Adoption friction
  is minimized at every interface.
- All six decisions point at the same implementation language and runtime,
  which keeps the dependency graph small.
- v0 ships without needing to commit to the provider-protocol-native engine,
  which is the largest piece of work.

**Negative.**

- The provider-protocol-native commitment for v1+ is a multi-month project on
  its own. Solo weekend pace puts a real v1 release at least a year out.
- PostgreSQL-only excludes some deployment targets (e.g. teams that run only
  SQLite or only DynamoDB). Acceptable for v0; revisit only with evidence of
  demand.
- Not wrapping `terraform` / `tofu` means re-implementing parts of their
  state handling rather than reusing it. Mitigated by the HTTP backend
  protocol being narrow and stable.

## Revisit conditions

These decisions should be revisited if:

- v1 effort exceeds 12 months of nominal weekend work without a release; in
  that case the **compatibility target (D4)** should likely relax to a
  wrapper around `tofu` for the engine portion.
- A second storage backend has concrete user demand and a maintainer; D5 can
  then move to a pluggable interface.
- The project gains contributors and the solo-project framing in
  `README.md` no longer applies; D2 (project identity) and license still hold,
  but governance docs become necessary.
