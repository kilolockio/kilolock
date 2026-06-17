# ADR 0006: Refresh implementation — orchestrator, factory, and CLI

- **Status:** Accepted
- **Date:** 2026-05-14
- **Decider(s):** @davesade (David Kubec)
- **Implements:** [ADR 0005](./0005-v1-scope.md) goals 1–5
- **Migrations:** `0003_provider_schemas.sql`, `0004_provider_configs.sql`, `0005_refresh_runs.sql`

## Context

ADR 0005 sets the v1 *scope* — provider-aware refresh out-of-band from
`terraform plan -refresh=false` — but stops short of saying *how*.
v1.6c lands the implementation: an orchestrator, a production
`ClientFactory`, an encoding layer between JSONB state and msgpack
`DynamicValue`, and the `kl refresh <state>` CLI.

The choices below were made incrementally across commits v1.6a–c. This
ADR captures them in one place so the next contributor doesn't have to
reconstruct intent from `git log`. Anything ADR 0005 already pins
(goals, non-goals, table shapes) is referenced rather than restated.

## Decision

The refresh path is a three-layer pipeline:

```
kl refresh <state>     (cmd/kl/cmd_refresh.go)
        │
        ▼
refresh.Run(ctx, store, factory, opts)     (internal/refresh/refresh.go)
        │
        ▼
ProductionFactory.Open(...)     (internal/refresh/factory.go)
        │   ↓ Discover → Launch → GetSchema(cache) → Configure → wrap
        ▼
encodingClient                  (internal/refresh/factory.go)
        │   ↓ JSONB ↔ msgpack DynamicValue per cty type
        ▼
provider.Client (tfprotov5 or tfprotov6)     (internal/provider/...)
```

Each layer has a single responsibility, swappable for testing.

### Layer 1: orchestrator (`internal/refresh`)

The orchestrator owns the *what*:

- Load the named state's current version and parse it into a
  `tfstate.State`.
- Group every managed resource instance by `(provider source, alias)`.
  Data sources are skipped — refresh is a managed-resource concept.
- For each group, ask the factory for an opened client. Iterate the
  group's entries serially (one provider process at a time), but run
  groups in parallel (one goroutine per group, bounded by
  `Options.Concurrency`). This matches Terraform's own concurrency
  model: providers are not thread-safe but separate provider
  processes are.
- For each entry, send `ReadResource` with the prior attributes,
  splice the response back into the parsed state, count the result
  as drifted or not by byte-equal JSON comparison.
- Commit a new state version with `source='refresh'` iff (a) not
  `--dry-run` and (b) zero per-resource failures. Always-write on
  no-drift is intentional: a refresh that confirmed every resource
  is unchanged still records that fact as a version, keeping the
  version chain honest about when refresh ran.
- Write an audit row to `refresh_runs` covering the run's outcome
  regardless of whether a commit happened. The audit row is the
  durable record; `Result` is the in-memory mirror.

The orchestrator does *not* know about provider binaries, msgpack,
or schema cache lookups. Those live in the factory.

### Layer 2: `ProductionFactory` (`internal/refresh/factory.go`)

The factory owns the *with what*:

- `Discover` finds the provider binary on disk using the operator-
  supplied search paths.
- `Launch` forks the binary and negotiates the plugin handshake.
- `GetSchema` is called only if `provider_schemas` has no row for
  `(source, version)`; the response is cached on first miss so
  subsequent runs against the same provider version skip the
  schema-fetch RPC entirely.
- `Configure` is called with the persisted `provider_configs` row,
  or an empty object when none exists. Empty-config-as-default is
  intentional: providers like `null`, `random`, and `local` declare
  no config block; requiring an explicit `kl provider
  configure` for them would be friction with no value.
- The factory then wraps the raw `provider.Client` in an
  `encodingClient` and returns it to the orchestrator. The
  orchestrator never holds the raw client.

One `ProductionFactory` instance lives for one Run. Across runs,
caching happens at the database layer (`provider_schemas`,
`provider_configs`), not in process memory. This matters for the
eventual server mode (`kld` someday hosting a refresh
endpoint) where multiple concurrent runs must not share mutable
provider state.

### Layer 3: `encodingClient`

The encoding layer is the most subtle. Providers speak msgpack-encoded
cty values on the wire (`DynamicValue` proto messages). Kilolock
stores resource attributes as JSONB. The orchestrator wants to deal
in JSON; the wire wants msgpack. `encodingClient` is a transparent
`provider.Client` wrapper that translates:

- **Inbound to provider:** parse JSON attributes from the state,
  walk the schema's resource block, produce a `tftypes.Value` per
  cty type, marshal to msgpack `DynamicValue`. Type inference is
  schema-driven, so a JSON `123` becomes a `cty.Number` when the
  schema says number and a `cty.String` when the schema says
  string — no guesswork.
- **Outbound from provider:** unmarshal the response's
  `DynamicValue`, walk the resulting `tftypes.Value`, render to
  a Go map[string]any, then marshal to JSON.

`DynamicPseudoType` (cty's catch-all for unknown type) needs special
handling: the codec must recurse with the *inferred* concrete type
(string, number, bool, etc.) when stamping the value, otherwise
`MarshalMsgPack` errors with "unknown type DynamicPseudoType". This
was the only real surprise in the spike.

Other `provider.Client` methods (`GetSchema`, `Configure`, etc.)
pass through unchanged. The wrap is targeted to `ReadResource` for
v1.6c; `UpgradeResourceState` (v1.6.5) and friends will hook the same
machinery.

### Layer 4: CLI (`cmd/kl/cmd_refresh.go`)

The CLI is intentionally thin: parse flags, resolve provider search
paths, construct factory + orchestrator, render the result. Exit
codes:

- `0` — run succeeded; zero per-resource failures
- `1` — per-resource failure(s), or a run-level error
- `2` — argv/usage error

Search-path precedence is `--provider-search-path` > `KL_
PROVIDER_PATH` > built-in defaults (`~/.terraform.d/plugin-cache`,
`./.terraform/providers`, `$TF_PLUGIN_CACHE_DIR`). Defaults always
apply as fallback so the operator's flag does not silently disable
discovery from places binaries typically live.

Output is a key/value table rendered by `tabwriter`. The shape is
stable across runs so the output itself is grep-friendly; diffing
two refresh outputs is a poor man's drift report until v1.7 lands
the proper `current_resource_drift` view.

## Goals (in scope for v1.6c, the implementation slice)

1. **Orchestrator with bounded concurrency.** Parallel across
   provider groups, serial within a group, bounded by `--concurrency`
   (default: NumCPU). Errors collected by default; `--fail-fast`
   stops on first.
2. **Production `ClientFactory`.** Discover → Launch → schema cache
   → Configure → wrap, end-to-end, with the schema cache populated
   on miss.
3. **Encoding pipeline.** JSON↔msgpack via schema-driven type
   inference, with `DynamicPseudoType` recursion fixed.
4. **CLI command.** `kl refresh <state>` with `--fail-fast`,
   `--concurrency`, `--dry-run`, `--actor`, `--provider-search-path`
   (repeatable). Exit codes per the table above.
5. **Audit table.** `refresh_runs` records every attempt with
   start/finish, counters, status, and operator (`actor`).
6. **Smoke coverage.** `scripts/smoke.sh` includes a refresh
   round-trip step (dry-run + live) that asserts on `refresh_runs`
   and `state_versions.source='refresh'`.

## Non-goals (deferred to v1.6.5+)

- **`UpgradeResourceState`.** *Closed in v1.6.5* — see the
  addendum at the bottom of this ADR.
- **Dynamic sensitivity (`sensitive_paths`).** *Closed.*
  Our vendored `tfplugin6.proto` (protocol 6.11) does not declare
  the `ReadResource.Response.sensitive_paths` field. However, newer
  protocol minor versions may still send it, and Go protobuf
  preserves unknown fields on messages. We therefore parse the
  response's unknown-field bytes and decode repeated
  `AttributePath` payloads out of field number `6`, then encode
  them into Terraform state JSON shape and persist to
  `instances[].sensitive_attributes`.
  
  This avoids a `protoc` regen while remaining forward-compatible
  with providers that already emit dynamic sensitivity.
- **Drift surfacing as a queryable view.** Counters are surfaced;
  per-resource changed addresses are not. v1.7 adds
  `current_resource_drift` and the matching CLI list output.
- **`Stop()` on cancellation.** *Closed.*
  When the refresh context is canceled (timeout, SIGINT/SIGTERM, or
  fail-fast), the orchestrator now issues a best-effort `Stop()`
  to each opened provider client to interrupt in-flight work before
  force-killing the process via `Close()`.
- **`kl providers verify <state>`.** Comparing stored
  provider config against HCL is not in scope; flagged in ADR 0005
  as a risk-mitigation we'll add later.
- **Server-mode refresh endpoint.** The orchestrator is built to be
  embeddable, but no HTTP route exposes it yet.

## Architecture

New packages and files added in v1.6:

```
internal/refresh/
    refresh.go                  (orchestrator: Run, groupByProvider, processGroups)
    refresh_test.go             (unit: recordingClient + fakeFactory)
    refresh_integration_test.go (end-to-end via the real null provider)
    factory.go                  (ProductionFactory + encodingClient)
    factory_test.go             (unit: encoding round-trips, pass-through)

pkg/store/
    refresh_runs.go             (BeginRefreshRun, FinishRefreshRun, GetRefreshRun, ListRefreshRuns)
    refresh_runs_integration_test.go

internal/migrate/migrations/
    0005_refresh_runs.sql       (audit table + status CHECK constraints)

cmd/kl/
    cmd_refresh.go              (`kl refresh` subcommand)
    cmd_refresh_test.go         (search-path resolution, render output shape)
```

`refresh_runs` table shape (from migration 0005):

```sql
CREATE TABLE refresh_runs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    state_id        uuid        NOT NULL REFERENCES states(id) ON DELETE CASCADE,
    from_version_id uuid        NOT NULL REFERENCES state_versions(id),
    to_version_id   uuid                 REFERENCES state_versions(id),
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    resources_checked  int,
    resources_changed  int,
    resources_failed   int,
    status          text        NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    error_summary   text,
    actor           text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
```

Note this shape diverges slightly from the ADR 0005 sketch:

- UUID PK rather than `BIGSERIAL`, matching the project-wide UUID
  convention introduced in `0001_init.sql`.
- `from_version_id` and `to_version_id` columns track which version
  the run started from and (if it committed) ended at, instead of
  a single `serial` column. This lets `kl status` show
  "refreshed from v17 to v18" without a separate join.
- `actor` column records who/what invoked the run. Defaults to
  `${USER}@cli`; CI runners and future server endpoints can set
  their own actor strings.

## Trade-offs (call-outs operators care about)

1. **Whole-state refresh, no per-address filtering.** ADR 0005 already
   commits to this. v1.6c reaffirms it. An operator wanting to
   refresh only `aws_s3_bucket.foo` runs Terraform's targeted plan
   instead; refresh is a coarse-grained operation.
2. **Commit-on-zero-failures, never partial.** If any resource
   fails, no new state version is written even if 999 others
   succeeded. The reasoning is symmetric to Terraform's: a
   partially-refreshed state pretends to be the truth but is
   actually a mix of new and stale attributes, and that is a worse
   failure mode than "the refresh didn't run".
3. **Always-commit on no-drift.** A refresh against an in-sync state
   still writes a version with `source='refresh'`. This is the
   minority opinion (Terraform's own refresh elides the write
   when nothing changed) but it serves Kilolock's queryable-
   audit-trail story: operators can see *when* refresh confirmed a
   state, not only when it changed it.
4. **One factory per Run, no in-process schema cache.** Schema reads
   go through Postgres on every call after the first within a Run.
   The trade is "extra DB hop" against "no stale schema sitting in
   process memory if another process upgraded the provider". Given
   how rare schema fetches are (once per provider per Run), the
   extra hop is negligible.
5. **JSON in the orchestrator, msgpack at the wire.** The encoding
   layer is centralized in `encodingClient`. The orchestrator never
   touches msgpack. This costs one extra serialize/deserialize per
   resource but keeps the rest of the codebase JSON-native, which
   matters for testability — every other layer can be mocked with
   plain `json.Marshal`.

## Acceptance signals (already met by v1.6c)

- `go test ./...` green, including `internal/refresh/` integration
  tests that exercise a real `null_resource` provider end-to-end.
- `scripts/smoke.sh` exercises `kl refresh --dry-run` and
  `kl refresh` against the existing smoke fixture (random,
  null, local) and asserts on `refresh_runs` rows and a new
  `state_versions` row tagged `source='refresh'`.
- The schema cache populates on first run and is hit on subsequent
  runs; verified in `TestRun_RealNullProvider_UsesCachedSchema`.

## Remaining v1 work

Not in scope for this ADR but pinned for completeness:

- **Proto refresh (optional hardening).** Re-vendor `tfplugin5/6`
  from a newer `terraform-plugin-go` release to pick up fields and
  RPCs not present in our current 6.11 snapshot (for example newer
  deferred-actions enums and other newer protocol additions). Note:
  dynamic sensitivity for refresh is now implemented via unknown-
  field parsing and does not strictly require a proto regen.
- **v1.7** — Drift surfacing (`current_resource_drift` view +
  per-resource changed addresses in the CLI output).

ADR 0005 already names these; future work tracks under that ADR's
acceptance criteria, not new ADRs, unless a design issue forces a
revisit.

## Addendum: v1.6.5 — `UpgradeResourceState`

**Date:** 2026-05-14

v1.6.5 closes the schema-upgrade gap. State persisted by older
provider releases is now migrated through the provider's own
`UpgradeResourceState` RPC before any `ReadResource` call, matching
Terraform's own behavior. Without this step refresh would have failed
on any state whose recorded `schema_version` lagged behind the live
provider — common after any provider upgrade.

What changed:

- `provider.Client` interface gained `UpgradeResourceState`. Both
  `clientV5` and `clientV6` implement it against the symmetric
  wire RPC (`tfplugin{5,6}.UpgradeResourceState`).
- `encodingClient` exposes the same method with a deliberately
  *asymmetric* encoding contract: the request carries the raw
  JSON bytes verbatim (the wire takes `bytes json` directly, not
  a `DynamicValue`), and the response is decoded from msgpack
  back into JSON for the orchestrator. The orchestrator stays
  JSON-shaped end to end.
- The orchestrator's `refreshOne` consults a new `needsUpgrade`
  gate: when the live schema's `Version` exceeds the instance's
  recorded `schema_version`, an `UpgradeResourceState` RPC is
  issued. On success, the upgraded JSON is spliced into the
  parsed state and the instance's `schema_version` is bumped to
  the live value. On failure (transport error or Error
  diagnostics), the per-resource error is recorded and
  `ReadResource` is *not* attempted for that instance.
- Per-resource counters: an upgrade failure counts as
  `ResourcesFailed` (the instance never made it to `ReadResource`).
  An upgrade success collapses into the existing change signal —
  if the original-vs-final byte diff is non-empty, the instance
  also counts as `ResourcesChanged`. Splitting the upgrade signal
  from the cloud-drift signal is v1.7's concern.

What did **not** change:

- The commit policy is unchanged: a refresh writes a new state
  version iff `--dry-run=false` AND zero per-resource failures.
- Provider-side flatmap input (the Terraform 0.11-era legacy
  format on `RawState.flatmap`) is still unsupported. Kilolock
  imports v4+ state exclusively, which has always been JSON.

Tests added:

- `provider/upgraderesourcestate_test.go` — pre-RPC validation
  gates on both protocol versions.
- `provider/upgraderesourcestate_integration_test.go` — wire
  round-trip against the real null provider.
- `refresh/factory_test.go` — `encodingClient` round-trip,
  shape-change migration, unknown-type rejection, transport
  error propagation.
- `refresh/refresh_test.go` — `needsUpgrade` truth table,
  end-to-end orchestrator flow when upgrade triggers, and the
  upgrade-failure-aborts-read contract.
- `refresh/refresh_integration_test.go` —
  `TestRun_RealNullProvider_UpgradeRunsOnVersionMismatch` doctors
  the cached schema's `Version` between runs to force the upgrade
  path and asserts the committed state's `schema_version` was
  bumped.

The acceptance signal for the v1.6.5 gap-close: in the worked test
case, an upgrade-triggering refresh against a real provider
succeeds, the state's `schema_version` is bumped, and zero
diagnostics surface. That's the same provider-side contract
`terraform refresh` exercises.

## Addendum: v1.7 — Drift surfacing

v1.6c's orchestrator can detect drift but exposes only the
aggregate counter `ResourcesChanged`. v1.7 layers two
operator-facing views on top — one per-run, one cross-run — so
"what's drifted?" stops being an export-and-diff job.

### What v1.7a ships

`refresh.Result` gains `ChangedAddresses []string`. The
orchestrator collects per-resource addresses inside the same
critical section that increments the counter, then sorts the
slice at Run boundary so two refreshes of the same state produce
diff-stable output. `cmd_refresh.go` renders a `drift addresses:`
section beneath the existing counter line, capped at 25 entries
with a `... and N more` footer pointing at the SQL view for the
full picture.

Memory shape: addresses only, no full attribute blobs. The full
before/after view is recoverable from the database via
`state_versions.raw_state`, which is what v1.7b's view exposes for
SQL consumers. A pathological 10k-resource drift carries a 10k-
entry string slice, not 10k × KB attribute blobs.

### What v1.7b ships

Migration `0006_resource_drift_view.sql` adds a lifecycle-aware
view `current_resource_drift` and a partial index
`resources_closed_at_idx` on
`(state_id, address, delete_serial) WHERE delete_serial IS NOT NULL`.

The view's predicate is exact: a resource is "currently drifted"
iff

  1. its currently-open lifecycle was opened by a refresh-sourced
     state_version (replacing a prior closed lifecycle at the
     same `(state_id, address)`), AND
  2. no subsequent state_version with `source='apply'` exists for
     that state.

Clause (2) is the non-obvious one. Without it, an apply that
re-asserts the refresh-discovered value (same content hash → no
new lifecycle row by ADR 0004's dedup rule) would leave a stale
drift row visible forever. With clause (2), any apply — regardless
of whether it changed the address in question — clears the drift
signal, which matches operator intuition: "I've had a chance to
look at it, the drift is no longer pending."

The view exposes `current_attributes`, `previous_attributes`, the
detected-at serial / version-id, and (best-effort) the
`refresh_runs.id` that produced it. `Store.ListCurrentDrift(ctx,
stateName, limit)` wraps the view with `ErrStateNotFound`
discrimination so callers can distinguish "no drift" from "no
such state".

### What v1.7c ships

`examples/big-state/drift-demo.sh` — the deliverable the operator sees first
when evaluating Kilolock. A three-resource Terraform fixture,
a script that mutates one resource's keepers out-of-band, and a
timed side-by-side comparison of:

  a. Raw `.tfstate`: `curl GET` + `jq` against a stored snapshot,
     join in user-space, format the changed-key set.
  b. Kilolock: one `SELECT` against `current_resource_drift`,
     same answer.

The script also surfaces the one demo-specific quirk honestly: the
byte-form normalization step between Terraform's pretty JSON and
Postgres JSONB's canonical form. This is a property of the demo's
HTTP-backend round-trip, not of a real refresh — a refresh writes
already-compact bytes coming off the provider RPC and skips the
normalization step. The `kl import --source=refresh` path
is the same `WriteState` codepath the orchestrator commits
through; the lifecycle + drift view machinery sees the two
identically.

### Why the "demo angle" matters

ADR 0002 frames v0 as "queryable state". v1 (provider-aware
refresh) closes the freshness gap. v1.7 closes the
**discoverability** gap: the refresh runs, but until v1.7 the
results lived in `state_versions.raw_state` and the operator
needed a custom diff harness to extract them. After v1.7, a
60-character SQL query (or a `--format json` invocation) is the
whole story.

This is the difference between "Kilolock stores Terraform state"
and "Kilolock makes the questions you ask of Terraform state
actually answerable". The drift demo is the smallest concrete
example; the same shape recurs for inventory, compliance, blast
radius, etc.

### What v1.7 does not do

- **No continuous-drift background job.** v3's
  "drift detection as a continuous background job" still needs a
  scheduler. v1.7 leaves drift detection user-initiated (via
  `kl refresh`).
- **No multi-resource correlation.** The view is per-(state, address),
  not "all S3 buckets that drifted across all states with a
  matching tag." Correlated queries are SQL on top of the view;
  this ADR adds the primitive, not the dashboards.
- **No drift-clearing CLI.** `kl refresh` clearing drift
  signals (e.g. `kl refresh ack <state> <addr>`) is a
  future ergonomics tweak. The current model says "drift clears
  on next apply"; we'll add explicit ack semantics if real users
  miss them.

## References

- ADR 0005 (v1 scope): [./0005-v1-scope.md](./0005-v1-scope.md)
- Lifecycle model refresh writes against: [ADR 0004](./0004-resource-lifecycles.md)
- HashiCorp provider plugin protocol: <https://github.com/hashicorp/terraform-plugin-go/tree/v0.31.0/tfprotov6>
- Provider plugin handshake reference: <https://github.com/hashicorp/go-plugin>
