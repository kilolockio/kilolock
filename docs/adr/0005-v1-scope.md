# ADR 0005: v1 scope — Provider-aware refresh

- **Status:** Accepted
- **Date:** 2026-05-13
- **Decider(s):** @davesade (David Kubec)
- **Supersedes / Relates to:** [ADR 0001](./0001-foundations.md), [ADR 0002](./0002-v0-scope.md), [ADR 0004](./0004-resource-lifecycles.md)
- **Informed by:** `_spike/v1-provider-protocol/` (commit `06eb84e`)

## Context

v0 ships queryable state: import a `.tfstate`, normalize it into Postgres,
query the graph with SQL. v0 does not address the original motivating
problem — `terraform plan` on a 300 MB state taking hours and outliving
AWS STS credentials. ADR 0002 named provider-protocol-native scoped
plans as the v1 territory and left it at that.

The provider-protocol spike (`_spike/v1-provider-protocol/`) has now
proven the technical premise: Kilolock can launch a real Terraform
provider binary, complete its plugin handshake, and exercise tfprotov5
and tfprotov6 RPCs without depending on the `terraform` CLI at runtime.
GetSchema and ReadResource succeeded against four production HashiCorp
providers (null, time, tls, random) in single-digit milliseconds each.

What the spike did **not** validate is the rest of a plan engine: HCL
parsing, desired-vs-actual diffing, a CLI that replaces `terraform plan`,
or any of the provider-config / authentication scaffolding that real
providers (AWS, Azure, GCP) need. Those are real and large.

This ADR commits to a v1 scope that uses the proven primitive (provider
RPCs) to solve the proven pain (slow refresh on large state), without
attempting the unproven parts (HCL diff engine, CLI replacement).

## Decision

v1 of Kilolock is scoped to **provider-aware refresh**: Kilolock
talks to providers directly to refresh resource state into the graph,
out-of-band from `terraform plan`. Users continue running `terraform`
and `tofu` for plan and apply, with one change to their workflow:

```
terraform plan -refresh=false
```

When Kilolock keeps the state in Postgres continuously fresh,
`terraform plan -refresh=false` is *strictly faster* than the default
plan path. It skips the per-resource provider RPCs that dominate plan
time on large states, while still producing a correct diff against
configured desired state.

This is the smallest v1 that:

1. Solves the motivating performance problem (the refresh phase is
   where hour-long plans live).
2. Uses the spike-proven mechanism (direct provider RPCs).
3. Sidesteps the v0 punt on HCL parsing — `terraform` still owns
   parsing config and computing the diff.
4. Adds a second user-facing reason to adopt Kilolock beyond
   "queryable state": drift visibility as queryable data.

A future ADR (presumably 0007 or later) can take the next step:
provider-protocol-native plans, which requires HCL parsing. That is
explicitly v2, not v1.

## Goals (in scope for v1)

1. **`kl refresh <state-name>` command.** Walks all resources
   in the named state, talks to the appropriate providers via the
   tfprotov5/tfprotov6 protocol, calls `ReadResource` for each, and
   updates the normalized graph in Postgres. Reuses the lifecycle
   model from ADR 0004 — changed resources close their existing
   lifecycle and open a new one at the current serial.
2. **Provider binary discovery.** Reuse the layout Terraform writes
   into `.terraform/providers/registry.terraform.io/<source>/<version>/<os_arch>/`.
   Required-providers metadata can come from either the state itself
   (each resource carries its `provider` address) or an explicit
   `kl providers <state>` lock file.
3. **Provider configuration.** A `provider_configs` table keyed by
   `(state_id, provider_address)` stores configuration passed to
   `Configure()` before any RPC. Initial form: JSON values + env-var
   indirection (`{"region": {"env":"AWS_REGION"}}`). Same shape
   Terraform uses internally.
4. **Schema cache.** A `provider_schemas` table keyed by
   `(provider_source, provider_version)` caches `GetSchema`
   responses as JSONB. Invalidated only when the version pin in
   the state's required-providers changes.
5. **Resource-state encode/decode.** A small encoder converts our
   stored JSONB attributes into the cty/msgpack `DynamicValue`
   payload the provider expects. Reuses
   `github.com/vmihailenco/msgpack/v5` (already a transitive dep
   of terraform-plugin-go).
6. **Schema upgrades.** Before any other RPC against a resource, if
   the stored `schema_version` is older than what the provider's
   schema declares, call `UpgradeResourceState` and persist the
   upgraded form. Skipping this corrupts data on first refresh
   after a provider upgrade.
7. **Drift surfacing.** A `current_resource_drift` view exposes
   resources whose `attributes_hash` changed on the most recent
   refresh — i.e. where the cloud disagrees with the recorded
   state. Queryable from `kl query` like everything else.
8. **Cancellation.** `kl refresh` honors context cancellation
   (Ctrl-C, signal, HTTP request abort if exposed) by calling the
   provider's `Stop()` RPC on each open connection. This is the
   "AWS credentials expire mid-plan" failure mode addressed
   directly: a stuck refresh can be interrupted cleanly.
9. **Documentation: integration with `terraform plan -refresh=false`.**
   The README and a new `docs/usage/refresh.md` explain the workflow
   shift: `kl refresh` runs out-of-band (cron, CI, hook);
   plan runs with `-refresh=false`. Document the staleness trade-off
   honestly.

## Non-goals (explicitly deferred to a later major version)

- **HCL parsing.** v1 does not parse HCL. `terraform` continues to
  own desired-state extraction.
- **`kl plan` command.** v1 has no plan command. Users run
  `terraform plan -refresh=false`.
- **Apply orchestration.** v1 does not apply changes. Users run
  `terraform apply`.
- **Import via provider RPC.** `ImportResourceState` is not wired up
  in v1. Initial state population continues via `kl import`
  reading `.tfstate` files.
- **Resource graph muxing.** A future v2 will need to recognize that
  the `aws` provider is itself a plugin-mux server hosting many
  internal SDKs. For v1, we treat each provider binary as opaque and
  call its RPCs as-is.
- **Differential refresh by drift signal.** v1 refreshes everything
  in the state on each `kl refresh` run. A future ADR can
  introduce "only refresh resources older than X" or provider-driven
  change feeds (CloudTrail, Azure Event Grid, etc.).
- **Multi-region / multi-account fan-out.** Each refresh is single-
  process. Parallelism is per-provider via a goroutine pool, not
  cross-region or cross-account orchestration.
- **Web UI for drift visualization.** SQL via `kl query`
  remains the surface, consistent with v0.

## Architecture

Informed by the spike. New package:

```
internal/provider/
    discovery.go      # resolve required-providers → on-disk binary paths
    launch.go         # go-plugin client lifecycle, handshake, Stop() routing
    client.go         # protocol-version-agnostic Client interface
    client_v5.go      # tfprotov5 implementation
    client_v6.go      # tfprotov6 implementation
    encode.go         # JSONB attrs ↔ cty/msgpack DynamicValue
    schema.go         # GetSchema caching, version pinning
    config.go         # provider config resolution
    pool.go           # one provider process per (state_id, provider_address),
                      # bounded concurrency per process
```

`internal/grpcwire/` contains the vendored MPL-2.0 tfplugin5 and
tfplugin6 generated bindings, with a NOTICE preserving attribution.
Same approach used in the spike, moved into the production tree.

New tables:

```sql
CREATE TABLE provider_schemas (
    provider_source   TEXT  NOT NULL,
    provider_version  TEXT  NOT NULL,
    protocol_version  SMALLINT NOT NULL,        -- 5 or 6
    schema_jsonb      JSONB NOT NULL,
    fetched_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_source, provider_version)
);

CREATE TABLE provider_configs (
    state_id          BIGINT NOT NULL REFERENCES states(id) ON DELETE CASCADE,
    provider_address  TEXT   NOT NULL,
    config_jsonb      JSONB  NOT NULL,
    PRIMARY KEY (state_id, provider_address)
);

CREATE TABLE refresh_runs (
    id                BIGSERIAL PRIMARY KEY,
    state_id          BIGINT NOT NULL REFERENCES states(id) ON DELETE CASCADE,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    serial            BIGINT,                    -- new state serial issued
    resources_checked INTEGER NOT NULL DEFAULT 0,
    resources_drifted INTEGER NOT NULL DEFAULT 0,
    error             TEXT
);
```

A view exposes drift:

```sql
CREATE VIEW current_resource_drift AS
SELECT r.* FROM resources r
WHERE r.delete_serial IS NULL
  AND r.create_serial = (SELECT serial FROM refresh_runs
                          WHERE state_id = r.state_id
                            AND completed_at IS NOT NULL
                            AND error IS NULL
                          ORDER BY completed_at DESC LIMIT 1);
```

The lifecycle model from ADR 0004 carries this for free: a drift
manifestation closes one lifecycle and opens another at the new serial.
Querying "what drifted on the last successful refresh" becomes a
straightforward predicate.

## Acceptance criteria

v1 ships when **all** of the following are true:

1. **`kl refresh <state>`** completes against a real cloud
   provider (target: `hashicorp/random` or `hashicorp/null` for
   smoke; `hashicorp/aws` for the realism demo) with credentials
   passed via env vars, and updates the graph.
2. **Drift detection works end-to-end.** A test scenario where a
   resource is mutated out-of-band (e.g. `aws_s3_bucket_versioning`
   flipped via console) is detected on the next refresh, surfaces
   in `current_resource_drift`, and is reflected in the resource's
   lifecycle (old lifecycle closed, new one opened).
3. **`terraform plan -refresh=false` produces correct diffs** against
   an Kilolock-refreshed state. Test: introduce a desired-state
   change in HCL, run plan, observe the diff matches what
   `terraform plan` (with refresh) would produce on the same inputs.
4. **Cancellation is clean.** A long-running refresh interrupted via
   SIGINT terminates within 5 seconds, with `Stop()` calls observed
   on each open provider connection in logs. No leaked provider
   processes.
5. **Schema upgrade is verified.** A test corpus includes at least
   one resource whose schema version increments between two
   real provider releases; refresh handles the upgrade and stores
   the upgraded form.
6. **Both protocol versions in production paths.** Integration tests
   exercise refresh against at least one v5-only provider and one
   v6-only provider, end to end.
7. **Big-state realism.** Refresh against the existing
   `examples/big-state` demo at `size=10000` (20k+ resources)
   completes in less than the time `terraform plan` would take on
   the same state. Order of magnitude faster is the goal; even
   parity would prove the architecture.

## Risks and mitigations

1. **Staleness gives users wrong plans.** Users who run
   `terraform plan -refresh=false` against a stale Kilolock
   refresh see a plan that omits real drift. Mitigation: refresh
   freshness exposed in `refresh_runs.completed_at`, surfaced in
   `kl status`, documented prominently. Future work: opt-in
   per-resource refresh during plan via a wrapper command.
2. **Provider config drift between Kilolock and the user's HCL.**
   If Kilolock's stored provider config differs from the HCL's
   provider block, refresh queries the wrong account/region.
   Mitigation: an `kl providers verify <state>` command
   compares declared config against the most recent refresh and
   flags discrepancies.
3. **Provider binary version drift.** A provider upgraded in HCL
   but not in Kilolock's lock causes schema mismatch. Mitigation:
   honor the same `.terraform.lock.hcl` semantics for version
   pinning; refuse to refresh when versions differ.
4. **gRPC message size for large schemas.** The AWS provider's
   schema is ~25 MB. Default gRPC recv limit is 4 MB. Mitigation:
   set `grpc.MaxCallRecvMsgSize` to a large value (terraform uses
   `math.MaxInt32`); already on the spike's gotcha list.
5. **Provider credentials.** Real providers need real credentials.
   Storing them in `provider_configs.config_jsonb` directly is
   wrong. Mitigation: env-var indirection in the config schema
   (`{"region": {"env":"AWS_REGION"}}`), with secret-broker hooks
   (Vault, AWS Secrets Manager, k8s secrets) deferred to v1.1.
6. **The "scoped plan" framing in the spike NOTES is bigger than
   v1 delivers.** The NOTES described scoped plans as the v1 goal;
   this ADR narrows v1 to refresh-only and reframes scoped plans
   as v2. The reframing is intentional and the docs in the spike
   should be read as forward-looking, not contractual.

## Consequences

**Positive.**

- Solves the original motivating pain (slow refresh on large state)
  with the smallest possible v1 surface.
- Compounds with v0 — drift becomes queryable from the same SQL
  surface users already learned.
- Establishes the production codepath for provider RPCs early. Any
  future v2 work (scoped plans, apply, import via RPC) reuses
  `internal/provider/` directly.
- Honest scoping — no HCL parser, no plan engine, no replacement of
  `terraform` itself. A weekend hobbyist can realistically build
  this without quitting their day job.

**Negative.**

- Users must opt into `-refresh=false` to see the speedup. The
  default `terraform plan` workflow is unchanged.
- Refresh staleness is a real failure mode. Regulated workflows
  that require plan-time freshness must either run `kl refresh`
  immediately before plan, or wait for v2.
- v1 carries a permanent two-protocol burden (v5 and v6). Future
  protocol versions add to this list. The abstraction layer cost
  is real and ongoing.
- Provider binary management is on us. We inherit Terraform's
  provider distribution model and its operational concerns
  (signature verification, registry availability, version pinning).

## Out of scope for v1 — explicitly listed to set expectations

- `kl plan` command (a v2 ADR will address this).
- `kl apply` command (likely v3, possibly later).
- HCL parsing of any kind.
- Auto-scheduled refresh (no internal cron; users invoke the
  command from their existing scheduler).
- Webhook / event-driven invalidation (CloudTrail, etc.).
- Per-resource freshness signaling at plan time.
- Secret broker integrations (Vault, AWS SM, k8s secrets) — v1.1.
- Multi-tenant provider credential isolation — see ADR 0002 non-goal.

## References

- Spike: `_spike/v1-provider-protocol/` and its `NOTES.md` (commit `06eb84e`)
- Lifecycle model this refresh path writes against: [ADR 0004](./0004-resource-lifecycles.md)
- v0 boundary: [ADR 0002](./0002-v0-scope.md)
- HashiCorp provider plugin protocol: <https://github.com/hashicorp/terraform-plugin-go/tree/v0.31.0/tfprotov6>
