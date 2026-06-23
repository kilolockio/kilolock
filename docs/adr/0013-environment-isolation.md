# ADR 0013: Environment isolation (control plane + database per environment)

- **Status:** Accepted (E1–E2 implemented)
- **Date:** 2026-05-19
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0003](./0003-governance-and-monetization.md) (commercial tiers), [ADR 0007](./0007-parallel-apply.md) (parallel apply), migration `0009_tenants.sql`, `0012_api_tokens.sql`

## Context

Hosted Kilolock must serve **many customers** on shared infrastructure
without relying solely on application-level `tenant_id` filters in one
large Postgres database. A bug in a `WHERE` clause must not be able to
expose another customer’s Terraform state.

We agreed on a **tiered isolation model**:

| Tier | Host | Database | Schema | States |
|------|------|----------|--------|--------|
| **Standard (default)** | Shared Cloud SQL **instance** | **One database per environment** | Single app schema per DB (e.g. `public` or `kl`) | Rows in shared tables, unique by `name` within that DB |
| **Dedicated (paid)** | **Dedicated Cloud SQL instance** per environment | One (or more) databases on that instance | Same as standard | Same as standard |

**Explicitly not in scope for v1 of this ADR:**

- Schema-per-state (rejected — too much migration/ops overhead; environment DB is the boundary).
- Schema-per-environment as a separate Postgres `SCHEMA` namespace (rejected for now — **one database per environment** is enough; tables live in the default schema unless we need extensions later).
- Multiple environments on a dedicated host beyond “this environment’s instance” (dedicated tier = **isolated host for that environment**; extra DBs on that host are possible but not the default story).

Self-hosted deployments remain **single-tenant**: one Postgres, one
logical environment, no control plane — compatible with today’s
`.kl.toml` + optional bootstrap token.

## Decision

### D1. Split control plane and data plane

**Control plane** — one small, always-on Postgres (the “metadata” DB):

- `tenants` — customer account
- `environments` — deployable isolation unit (dev, prod, …)
- `api_tokens` — credentials scoped to **one environment**
- `provisioning_jobs` (or columns on `environments`) — async host/DB creation
- Optional: `states_registry` — mapping of state name → data-plane facts if needed later

**Data plane** — per-environment Postgres **database** on a host:

- All tables from migrations `0001`–`0011` (states, resources, locks, …)
- **No `tenant_id` column required** in the data plane for SaaS (tenant is implied by which database you connected to)
- State names unique within that database only (`UNIQUE(name)` or keep composite only if we ever merge DBs — we won’t for SaaS)

The HTTP server resolves every request to:

`(environment_id) → connection string + database name → *pgxpool.Pool`

### D2. Environment is the isolation and billing unit

- On **tenant registration**: create tenant + **default environment** (slug `default`) + provision its database on the **shared host**.
- Customer may create additional environments (e.g. `staging`, `prod`). Each creation triggers **database provisioning** on the shared host unless tier is dedicated.
- **API tokens** are issued per environment, not per tenant-wide. A token for `acme/staging` cannot read `acme/prod`.

### D3. Dedicated tier = isolated host only

When a customer upgrades an environment to **dedicated**:

1. Provision a new Cloud SQL **instance** (region, tier, disk from product config).
2. Create the environment database on that instance (same schema/migrations as standard).
3. Cut over: update `environments` row with new host/DSN; optional data migration job from old DB → new DB.
4. Decommission old database on shared host after retention period.

No “dedicated schema” or “dedicated database on shared host” as a separate paid SKU — paid means **host isolation**.

### D4. Terraform HTTP backend contract

Terraform’s `backend "http"` only supports HTTP Basic auth. Contract:

```hcl
terraform {
  backend "http" {
    address        = "https://api.kl.example/v1/states/my-workspace"
    lock_address   = "https://api.kl.example/v1/states/my-workspace"
    unlock_address = "https://api.kl.example/v1/state-unlock/my-workspace"
    lock_method    = "LOCK"
    unlock_method  = "POST"
    username       = "acme"      # tenant slug (informational + Basic auth user)
    password       = "kl_…"     # API token; bound to ONE environment
  }
}
```

**Server rules:**

1. Validate `password` → `api_tokens` → `(tenant_id, environment_id)`.
2. Optionally require `username` = tenant slug and reject mismatch (defense in depth).
3. All `/v1/states/{name}` operations run against the **data-plane pool** for that `environment_id`.
4. Path does **not** include environment slug (token already scopes env). Alternative URL shape `/envs/{env}/v1/states/{name}` is reserved for future explicit routing but not required if token is env-scoped.

Bearer tokens (`Authorization: Bearer kl_…`) remain supported for non-Terraform clients; same env binding.

### D5. Connection pooling and limits

- **Pool cache** keyed by `environment_id` (or by DSN string), with LRU eviction and a configurable max open pools per `kld` replica.
- Cloud SQL connection limits bound max environments actively used per replica; scale horizontally or raise instance `max_connections` for large fleets.
- **Migrations** run:
  - On control plane DB at deploy (metadata only).
  - On each **new** environment database at provision time (`kl migrate` against env DSN).
  - On application upgrade: migration worker iterates active environments (or lazy-migrate on first request after version bump).

### D6. Provisioning (standard tier)

Synchronous minimum viable path:

1. `CREATE DATABASE kl_<tenant>_<env>` on shared instance (name rules: lowercase, unique per instance).
2. Run embedded migrations against new DB.
3. Mark `environments.status = ready`.

Asynchronous path (recommended for cloud):

1. Insert `environments` with `status = provisioning`.
2. Worker (or Cloud Run job) creates DB + migrates + stores DSN secret reference.
3. Flip `status = ready`; failures → `status = failed` + operator alert.

Shared host connection details live in platform config (env vars / Secret Manager), not per row, until dedicated tier.

### D7. Provisioning (dedicated tier) — deferred implementation, fixed intent

- Trigger: operator or billing webhook sets `environments.tier = dedicated`.
- Async: Terraform module / GCP API creates instance → database → secret → cutover.
- Document RPO/RTO and customer communication in runbook (not in this ADR’s implementation scope).

### D8. Self-hosted mode unchanged

`KL_DATABASE_URL` points at a single database. No control plane
required. Optional `KL_AUTH_MODE=static|database|open` as today.
`tenant_id` columns may remain in schema for backward compatibility but
are fixed to the singleton tenant row.

SaaS and self-hosted share the **same migration SQL** for data-plane
tables; SaaS omits reliance on `tenant_id` in queries when connected to
an environment-scoped pool.

## Assumptions

1. **One Cloud SQL instance** (shared) holds hundreds of environment databases for standard tier; instance sizing is an ops concern, not app logic.
2. **Environment slug** is unique per tenant: `(tenant_id, env_slug)` unique.
3. **State name** is unique per environment database (not globally).
4. Customers accept that **standard tier** shares CPU/RAM/IO with other customers on the same instance (noisy neighbor risk); dedicated tier removes that.
5. **Control plane** is highly available but small; data plane outage is per-environment.
6. **Secrets**: per-environment DSN (or password) stored in Secret Manager on GCP; never returned to clients except at provision time for break-glass.
7. **No cross-environment queries** in product v1 (no “search all my envs’ resources” without connecting to each DB).
8. **GDPR / delete customer**: drop environment database + control plane rows; dedicated instance destroyed when last environment on it is removed.

## Consequences

**Positive**

- Strong isolation without schema-per-state operational cost.
- Clear mapping to commercial tiers (standard vs dedicated host).
- Terraform contract stays simple (token-scoped env).
- Aligns with GCP primitives (Cloud SQL instance + `CREATE DATABASE`).

**Negative**

- `kld` must become a **router** (pool per environment).
- Migrations and schema upgrades are **N times** per environment count.
- Provisioning automation is required for acceptable SaaS UX.
- Integration tests need multi-DB fixtures or docker-compose with multiple databases.

## Implementation phases

| Phase | Deliverable |
|-------|-------------|
| **E1** | Control plane schema: `environments`, token → `environment_id`, provision status |
| **E2** | `EnvironmentRouter` + pool cache; serve uses env pool for `/v1/states/*` |
| **E3** | control API environment create + sync provision on shared host |
| **E4** | Token create binds to environment; integration tests (two DBs, no cross-read) — done |
| **E5** | `store.NewIsolated`: data-plane reads omit `tenant_id` filter; router uses isolated store when `database_dsn` set — done |
| **E5b** | `migrate --all-environments`; control API tenant create with provision; bootstrap auto-provision when admin URL set — done |
| **E6** | Dedicated tier: `upgrade-dedicated` + `provision dedicated` + Terraform module — done |

## Open questions (park for later)

1. **Default environment** slug: `default` vs `prod` on signup?
2. **Database naming** convention: `ig_<tenant_slug>_<env_slug>` vs UUID-only names?
3. **Cross-region** environments: environment row carries `region`; dedicated instance in that region only?
4. **Read replicas** per dedicated instance — product SKU or internal ops?
5. **kl CLI** (`query`, `apply`): `--environment` flag vs infer from token only?

## Relation to current code

Today (pre-ADR):

- Single `KL_DATABASE_URL`, `tenant_id` on all tables, `api_tokens.tenant_id`.
- [ADR 0013](./0013-environment-isolation.md) does not invalidate `0009` immediately; it defines the **target** for hosted SaaS.

Migration path:

1. Add `environments` + `environment_id` on `api_tokens`.
2. Implement router + second database in dev (compose: `postgres` + `acme_dev` DB).
3. New SaaS deploys use env DBs; existing single-DB dev keeps working in self-hosted mode.

## References

- PostgreSQL: one instance, many databases — https://www.postgresql.org/docs/current/manage-ag-overview.html
- Terraform HTTP backend authentication — `username` / `password` in backend block
- GCP Cloud SQL: multiple databases per instance; separate instances for isolation
