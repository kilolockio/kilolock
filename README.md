# Kilolock

[![ci](https://github.com/davesade/kilolock/actions/workflows/ci.yml/badge.svg)](https://github.com/davesade/kilolock/actions/workflows/ci.yml)
[![lint](https://github.com/davesade/kilolock/actions/workflows/lint.yml/badge.svg)](https://github.com/davesade/kilolock/actions/workflows/lint.yml)
[![go](https://img.shields.io/github/go-mod/go-version/davesade/kilolock)](./go.mod)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](./LICENSE)

Project site: [docs/index.html](./docs/index.html)

**A self-hosted graph-native control plane for Terraform and OpenTofu.**

Kilolock replaces the flat `.tfstate` file with a normalized PostgreSQL database that models your infrastructure as a real dependency graph. State becomes queryable, drift becomes inspectable, refresh can happen out of band, and scoped/orchestrated workflows can coordinate at resource level instead of whole-state-file level.

> **Status:** queryable state, provider-aware refresh, scoped apply, resource-level history/repair, and orchestrated parallel-apply foundations are already working in this OSS repo. See the [roadmap](#roadmap) for the current shape and remaining edges.

### Try it in two commands

```sh
cp .env.example .env
docker-compose up --build -d
```

That starts the default local stack on `http://localhost:8080`. If you want the fuller self-hosted stack with `klc`, multi-instance routing, and explicit bootstrap, jump to [Getting started](#getting-started).

---

## Why

Terraform's state file is a flat JSON blob. As state grows, every operation pays the cost of the whole file:

- **Slow plans.** `terraform plan` refreshes every resource against the cloud provider before computing a diff. A 50,000-resource state means 50,000 API calls, often pushing plan time past credential expiry windows.
- **Global locks.** A single workspace lock serializes every change through the same gate, even when the changes touch disjoint parts of the infrastructure.
- **Weak visibility.** You can see whether a run succeeded. You cannot ask "show me every unencrypted S3 bucket across all states."
- **Tool sprawl.** Inventory, drift detection, cost attribution, and policy each become a separate product parsing the same state file.

These are all symptoms of one missing layer: a real data model under the state. Kilolock adds that layer.

## What

| | File-based backend | Kilolock |
|---|---|---|
| **State model** | Flat JSON blob per workspace | Normalized graph in Postgres |
| **Lock scope** | Whole workspace | Affected subgraph |
| **Plan scope** | Refresh and plan everything | Operate on the affected subgraph |
| **Visibility** | `terraform show` and grep | SQL across every resource and state |
| **Multi-state changes** | One workspace at a time | Atomic across states *(planned)* |

Same HCL. Same providers. Same workflow. Different engine underneath.

## What this is not

- **Not a Terraform Cloud / Spacelift / env0 replacement.** Those wrap state in a runner and a UI. Kilolock replaces the *data layer*. A runner on top is out of scope for the foreseeable future.
- **Not a fork of Terraform or OpenTofu.** Kilolock speaks the Terraform HTTP backend protocol and the provider gRPC protocol directly. Your existing `terraform` / `tofu` CLI keeps working.
- **Not Stategraph.** [Stategraph](https://stategraph.com) is a commercial product with a similar premise; their "Velocity" parallel-apply engine is the reference design for Kilolock v2 (see [ADR 0007](./docs/adr/0007-parallel-apply.md)). Kilolock is an independent open-source alternative under Apache 2.0 — same architectural ideas, self-hostable, auditable, no SaaS dependency.

## What you get today

- **Drop-in HTTP backend** for Terraform/OpenTofu
- **Queryable state** with SQL over normalized resource rows
- **Provider-aware refresh** without paying full `terraform plan` refresh cost every time
- **Scoped planning and apply** for large-state workflows
- **Orchestrated apply** with reservation-aware coordination
- **Per-resource history and emergency repair** through backend-native query/rollback flows
- **Self-hosted control plane** for operator APIs, entitlements, lifecycle, and routing

## OSS scope

This repository focuses on the self-hosted and local-deployment Kilolock stack:

- `kl` client CLI
- `kld` backend/runtime service
- `klc` control-plane service
- local Docker Compose flows
- state/query/repair/operator workflows

Hosted-business features such as a customer portal, billing/signup glue, and cloud-specific deployment packaging are intentionally out of scope for this OSS repository.

## Terraform/OpenTofu versions

Version selection is customer-controlled (local toolchain/CI image, or Kilolock CLI flags for scoped/targeted runs). See the full compatibility and support policy in [docs/terraform-compatibility.md](./docs/terraform-compatibility.md).

## Roadmap

### v0 — Queryable state

Make state queryable. No plan/apply changes yet.

- [x] HTTP backend protocol server (drop-in replacement for the `http` backend)
- [x] `.tfstate` import to normalized Postgres schema
- [x] `.tfstate` export from the database (no lock-in)
- [x] Normalization on every write (HTTP POST and CLI import)
- [x] CLI: `kl query "SELECT ..."` over the graph (table / JSON / CSV)
- [x] Canonical inventory, dependency, and blast-radius queries in [`docs/queries/`](./docs/queries/)
- [x] Real Terraform end-to-end smoke test ([`scripts/smoke.sh`](./scripts/smoke.sh)) wired into CI ([`.github/workflows/ci.yml`](./.github/workflows/ci.yml))

### v1 — Provider-aware refresh

Talk to Terraform providers directly to keep the graph fresh, out of band
from `terraform plan -refresh=false`. See [ADR 0005](./docs/adr/0005-v1-scope.md)
for the scope decision and [ADR 0006](./docs/adr/0006-refresh-implementation.md)
for the orchestrator / factory / CLI design.

- [x] tfprotov5 + tfprotov6 client wire layer (`internal/provider/`)
- [x] Provider discovery + launch + cancellable handshake
- [x] `GetSchema` + cache in `provider_schemas` (JSONB)
- [x] `Configure` + persisted blocks in `provider_configs` (`kl provider configure`)
- [x] `ReadResource` with cty/msgpack `DynamicValue` encoding
- [x] Orchestrator: bounded concurrency, group-per-provider, audit trail in `refresh_runs`
- [x] CLI: `kl refresh <state>` with `--dry-run`, `--fail-fast`, `--concurrency`, `--actor`, `--provider-search-path`
- [x] Smoke coverage (refresh round-trip in `scripts/smoke.sh`)
- [x] `UpgradeResourceState` — schema migration before refresh (v1.6.5)
- [x] Drift surfacing: per-resource `ChangedAddresses` in `refresh.Result` + CLI render (v1.7a)
- [x] Drift surfacing: `current_resource_drift` view + `Store.ListCurrentDrift` (v1.7b)
- [x] Drift demo: [`examples/big-state/drift-demo.sh`](./examples/big-state/drift-demo.sh) — refresh-style drift detection on the main big-state example (v1.7c)
- [x] Dynamic sensitivity marks (via v6 unknown-field parsing of `sensitive_paths`)
- [x] `Stop()` on cancellation + clean shutdown signal handling

### v2 — Parallel apply on shared state

The reason Kilolock exists. Two engineers updating disjoint parts of
the same state apply in parallel, with conflicts detected at the resource
level rather than at the state-file level. The open-source implementation
of the model StateGraph ships commercially as "Velocity". See
[ADR 0007](./docs/adr/0007-parallel-apply.md) for the design.

- [x] `resource_reservations` substrate + conflict matrix + `apply_runs` (v2a)
- [x] Plan introspection (`terraform show -json`) + state slicing (v2b)
- [x] Sliced `terraform apply` + row-level commit through the lifecycle path (v2c-1)
- [x] Heartbeat lease renewal + re-plan validation + plan-staleness guard (v2c-2, v2c-3)
- [x] Parallel-apply demo: two terminals, disjoint subgraphs, same state (v2d) — see [`examples/big-state/parallel-demo.sh`](./examples/big-state/parallel-demo.sh) and the [v2 parallel-apply section in its README](./examples/big-state/README.md#v2-parallel-apply-demo)
- [ ] Coexistence with vanilla `terraform apply` (preserve optimistic backend mode; add explicit whole-state serialization mode) (v2e)

**Coexistence note:** vanilla Terraform uses the HTTP backend's *whole-state*
lock (`state_locks`). Kilolock v2 apply uses row-level reservations and
intentionally bypasses `state_locks` so stale locks don’t brick v2 workflows.
If you mix vanilla `terraform apply` and `kl apply` against the same
state, treat it as an advanced workflow: prefer one toolchain, or configure
vanilla clients to serialize via the control API (see
`docs/runbooks/control-api.md`).
`kl status <state>` surfaces the lock mode, all active plain-TF locks
in optimistic mode, and a coexistence warning when v1 whole-state locks and v2
row-level reservations are active at the same time.
`kl apply --strict-coexistence` is the conservative opt-in: it fails
closed instead of merely warning when plain-TF whole-state locks are present.
For a central policy, use
`POST /api/states/{tenant}/{environment}/config` from the control API
(`docs/runbooks/control-api.md`).

**Product note:** plain Terraform remains a first-class way to use
Kilolock. The backend itself is intended to add value beyond "hosted state":
optimistic parallel plain-TF applies on disjoint write sets, queryable graph
state, append-only history, rollback, and richer conflict intelligence. The
`kl` CLI is the advanced path for explicit reservations, scoped apply
UX, and stronger operator controls — not the only way to unlock product value.

#### Apply abort (operator escape hatch)

If an `kl apply` gets stuck (provider hang, network stall, CI job killed)
it may leave an `apply_runs` row in `running` and hold `resource_reservations`
until the lease expires. `kl apply abort` is the break-glass tool to:

- mark the apply run as `aborted` (audit trail)
- release any reservations held by that apply id
- stop heartbeating so a wedged apply can't renew its lease forever

Abort is explicit — you target a specific apply run:

```bash
# preferred: abort by apply id (printed by the CLI and visible in `kl status`)
kl apply abort --apply-id <uuid> --reason "operator abort"

# convenience: abort the most recent running apply for a state
kl apply abort --state big-state --latest
```

See `docs/runbooks/apply-abort.md` for an operator playbook.

### v3 — Beyond a single state (future)

- [ ] Atomic cross-state transactions
- [ ] Cross-state output slicing (read only what's referenced)
- [ ] Resource-level RBAC (authz on who can reserve which addresses)
- [ ] Automatic retry / queueing on reservation conflict
- [ ] Drift detection as a continuous background job

## Getting started

### Fastest way to try it

If you want the shortest path from clone to `terraform init`, start here:

```sh
cp .env.example .env
docker-compose up --build -d
```

That default stack is intentionally simple:

- `kld` on `http://localhost:8080`
- one local Postgres service
- open auth for backend experiments and local demos
- no separate `klc` service
- no resource quotas by default in the OSS self-hosted path

That means the default compose is enough for:

- `terraform init` / `terraform apply` against the HTTP backend
- `kl query ...`
- `kl query resource ...`
- `kl query history ...`
- `kl rollback resource ...`

If you want the fuller self-hosted stack with control-plane bootstrap, multi-instance routing, and separate metadata/data-plane databases, use the prod-like compose:

```sh
docker-compose -f docker-compose.prodlike.yml up --build -d
docker-compose -f docker-compose.prodlike.yml exec klc klc migrate
docker-compose -f docker-compose.prodlike.yml exec klc klc init --tenant self-hosted --tenant-name "Self Hosted" --token-name operator-bootstrap
```

That stack adds:

- `kld` on `http://localhost:8080`
- `klc` on `http://localhost:8090`
- control UI on `http://localhost:8090/portal`
- metadata + shared + premium Postgres services
- explicit control-plane bootstrap and operator token flow
- no resource quotas by default unless you choose to set tenant entitlements yourself

`init` is the important one-time step. Without it, the prod-like runtime intentionally refuses to start serving requests.

After `init` succeeds:

1. Copy the bootstrap token printed by `klc init`.
2. Open the control UI at `http://localhost:8090/portal`.
3. Paste that token into the UI auth box.
4. In **Create Workspace**, enter an optional human-friendly label/name and create the workspace.
5. In **Workspaces**, copy the real `workspace_id` (`ws_...`).
6. In **Create Environment**, paste that `workspace_id`, choose an environment label such as `prod`, and create the environment.
7. In **Environments by Workspace**, load that workspace and copy the environment's `env_public_id` (`env_...`).
8. In **Create Token**, paste:
   - the same `workspace_id`
   - the `env_public_id`
   - a token name such as `terraform`
9. Create the token and copy the raw secret shown once (`kl_...`).
10. Use those values from Terraform or `kl`.

For example projects that keep the quick local backend by default (such as
`examples/big-state`), switch to the prod-like backend by copying the sample
file over the active backend config:

```sh
cp examples/local-backend/backend.tf.prodlike examples/big-state/backend.tf
rm -rf examples/big-state/.terraform examples/big-state/.terraform.lock.hcl
cd examples/big-state && terraform init
```

Then edit `examples/big-state/backend.tf` with:

- `username = "{workspace_id}"` where the workspace id looks like `ws_...`
- `password = "{token_secret}"` where the token secret looks like `kl_...`
- `env_public_id = "{env_public_id}"` from the environment row in the control UI
- backend path `http://localhost:8080/states/{workspace_id}/{env_public_id}/{state_name}`

For Terraform HTTP backend configuration, the runtime path shape is:

```text
/states/{workspace_id}/{env_public_id}/{state_name}
```

Important:

- `workspace_id` is the workspace slug like `ws_...`
- `env_public_id` is the environment public ID, not just the human label like `prod`
- the control API/UI uses the environment label for admin actions, but the runtime backend path uses `env_public_id`

### Prerequisites

- Go 1.25+
- Docker + Docker Compose
- Terraform or OpenTofu available in `PATH`

### Build the binaries

```sh
make build
```

This produces:

- `./bin/kl` — client CLI
- `./bin/kld` — backend/runtime server
- `./bin/klc` — control-plane server and operator API host

### Run the runtime locally

```sh
make db-up
cp .kl.toml.example .kl.toml
./bin/kld
```

`kl` walks up from the current working directory to find `.kl.toml` and uses it for the database URL (and any other declared keys). Environment variables (`KL_DATABASE_URL`, `DATABASE_URL`) override file values, so a committed sensible default and an ad-hoc env override coexist cleanly. See `.kl.toml.example` for the recognised keys.

### Point Terraform at it

```hcl
terraform {
  backend "http" {
    address        = "http://localhost:8080/states/example"
    lock_address   = "http://localhost:8080/states/example"
    unlock_address = "http://localhost:8080/states/example"
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
```

Then `terraform init && terraform apply` against any toy config; state ends up in Postgres, normalized into `resources` / `resource_dependencies` / `outputs` rows.

> Why `backend "http"` and not `backend "kl"`? Terraform's backend set is hardcoded into the binary — there is no plugin mechanism for backends. See ADR 0001 for the architecture decision.

### Import an existing state

```sh
./bin/kl import path/to/terraform.tfstate              # state name = "terraform"
./bin/kl import --name prod environments/prod.tfstate  # explicit name
cat backup.tfstate | ./bin/kl import --name prod -     # stdin
```

### Export a state back to a file

```sh
./bin/kl export prod -o ./prod.tfstate
./bin/kl export prod                                   # stdout
```

Export returns the bytes Terraform last wrote, byte-for-byte; the normalized rows are derived data, not authoritative.

### List managed states

```sh
./bin/kl list
```

### Query the graph

Run any read-only SQL against the normalized tables. Output as a human-readable table (default), JSON, or CSV:

```sh
./bin/kl query "SELECT type, COUNT(*) FROM resources GROUP BY type ORDER BY 2 DESC"
./bin/kl query -f docs/queries/inventory_by_type.sql --format csv
./bin/kl query -f docs/queries/blast_radius.sql --format json --timeout 10s
echo "SELECT count(*) FROM resources" | ./bin/kl query -f -
```

Queries run inside a Postgres `READ ONLY` transaction with a bounded statement timeout, so writes are rejected by the server regardless of what SQL is supplied. Curated example queries live in [`docs/queries/`](./docs/queries/).

### Query by resource address

For day-to-day operator work on one exact resource, the resource-oriented
queries are usually more convenient than raw SQL:

```sh
./bin/kl query resource --address time_sleep.slow_b
./bin/kl query history --address time_sleep.slow_b
./bin/kl rollback resource --address time_sleep.slow_b --to @1
```

Use the two query styles for different jobs:

- `kl query "SELECT ..."` when you want inventory, blast radius, drift dashboards, or cross-state/operator analysis
- `kl query resource|history --address ...` when you want one exact address, its history, or resource-level repair

### Refresh a state by talking to providers directly (v1)

Skip the slow refresh phase of `terraform plan` by letting Kilolock
read live state from providers out of band:

```sh
# Optional: persist any provider config that needs explicit attributes
# (cloud creds typically come from env vars and need no row here).
./bin/kl provider configure hashicorp/aws --alias=west <<< '{"region":"us-west-2"}'

# Preview drift without committing a new state version.
./bin/kl refresh prod --dry-run

# Live refresh: writes a new state_version with source='refresh' and an
# audit row in refresh_runs. Subsequent `terraform plan -refresh=false`
# benefits from the fresher state without paying the per-resource RPC
# cost itself.
./bin/kl refresh prod
```

Provider binaries are discovered from `--provider-search-path` (repeatable),
`KL_PROVIDER_PATH` (`:`-separated), and the default set
(`~/.terraform.d/plugin-cache`, `./.terraform/providers`, `TF_PLUGIN_CACHE_DIR`).
See [ADR 0006](./docs/adr/0006-refresh-implementation.md) for the design.

### Find drift across all of your states (v1.7)

Once `refresh` has run, "what's drifted right now?" is a single SQL
query against the `current_resource_drift` view:

```sh
./bin/kl query -f docs/queries/drift_current.sql --format json
```

The view exposes one row per currently-drifted resource (refresh
discovered it, no subsequent apply has reconciled), with the
current and previous attribute blobs side by side.

`kl refresh` itself also surfaces per-run drift addresses
in its summary output, so operators can spot the changed resources
without a follow-up query:

```
refresh succeeded for prod-vpc
  serial: 17 -> 18 (committed)
  checked: 1342  changed: 3  failed: 0
drift addresses:
  aws_instance.web[0]
  aws_s3_bucket.logs
  aws_security_group.app
```

The drift queries are available directly through the backend and CLI, but for a
single end-to-end hands-on example we recommend starting with
[`examples/big-state/`](./examples/big-state/) and its [`drift-demo.sh`](./examples/big-state/drift-demo.sh) helper.

### Run tests

```sh
make test                               # unit tests only
KL_DATABASE_URL=... make test-integration  # full DB-backed
```

### Run the end-to-end smoke

The smoke script brings up Postgres, builds the binary, runs `terraform init && apply && destroy` against the backend, and asserts on the normalized rows:

```sh
./scripts/smoke.sh                      # uses local `terraform`
TF_BIN=tofu ./scripts/smoke.sh          # against OpenTofu instead
KEEP_DB=1 KEEP_TMP=1 ./scripts/smoke.sh # leave artifacts around for debugging
```

CI runs the same script on every PR against both `terraform` (1.6 + 1.9) and `tofu latest`; see [`.github/workflows/ci.yml`](./.github/workflows/ci.yml).

### Run the big-state demo

[`examples/big-state/`](./examples/big-state/) is the main hands-on example for
this repo. It gives you one large shared state and two crisp collaboration demos:

- **same resource** → visible wait/retry behavior
- **different resources** → true parallel apply and better engineer throughput

The README in that directory also walks through:

- setup against a local `kld` runtime
- measured timings at sizes from 100 to 50,000
- query examples (inventory by type, blast radius, state size)
- one-time bootstrap and cleanup for the slow-resource demos

For a quick taste from this directory:

```sh
cd examples/big-state
terraform init
terraform apply -auto-approve -var=size=100   # ~1 second, 208 resources
```

See [`docs/protocol.md`](./docs/protocol.md) for the wire contract and [`docs/schema.md`](./docs/schema.md) for the database design.

## Design decisions

Architecture decisions are recorded as ADRs in `docs/adr/`:

- `0001-foundations.md` — language, license, compatibility target, storage, CLI strategy.
- `0002-v0-scope.md` — what "queryable state" means and the v0 acceptance criteria.
- `0003-governance-and-monetization.md` — license commitment, CLA, trademark, OSS/commercial boundary.
- `0004-resource-lifecycles.md` — content-addressable resources with lifecycle ranges (the O(N²) → O(delta) fix).
- `0005-v1-scope.md` — v1 scope decision: provider-aware refresh.
- `0006-refresh-implementation.md` — orchestrator, factory, and CLI implementation notes.
- `0007-parallel-apply.md` — v2 scope: parallel apply on shared state (OSS implementation of StateGraph Velocity's model).
- `0013-environment-isolation.md` — environment isolation boundaries and dedicated-tier deployment shape.
- `0014-file-scoped-plan-apply.md` — fast file-scoped plan/apply for large states.
- `0015-control-plane-separation.md` — separate control-plane concerns from the core backend/runtime.

Other reference docs:

- [`docs/protocol.md`](./docs/protocol.md) — HTTP backend wire contract.
- [`docs/schema.md`](./docs/schema.md) — Postgres schema rationale.

## Governance and contributing

Kilolock is and will remain open source under Apache 2.0 — that commitment is permanent and recorded in [ADR 0003](./docs/adr/0003-governance-and-monetization.md). A future commercial offering (managed service or enterprise add-ons) may exist alongside the OSS; the OSS itself is never moved behind a paywall.

### OSS vs managed service boundary

The open-source project intentionally keeps core backend capabilities in-tree,
including multi-tenant primitives, multi-instance routing primitives, and
parallel plan/apply workflows.

That includes a **backend-native product path for plain Terraform users**:
teams can keep `terraform apply` and still gain differentiated behavior from
Kilolock itself, such as optimistic parallelism, richer state history, SQL
visibility, and service-side governance controls. The `kl` CLI builds
on top of that with more explicit and safer orchestration features.

The managed-service value is expected to come from operating excellence:
reliability engineering, backup/PITR operations, restore drills, SLOs and
alerting, compliance controls, metering, and support.

- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — how to contribute.
- [`cla/icla.md`](./cla/icla.md) — Individual Contributor License Agreement (required for non-trivial contributions).
- [`MAINTAINERS.md`](./MAINTAINERS.md) — current maintainers and decision-making.
- [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md) — community expectations.
- [`TRADEMARK.md`](./TRADEMARK.md) — name usage policy.

## License

[Apache License 2.0](./LICENSE).
## Multi-instance local smoke

To validate instance-key routing locally:

```bash
make ci-multi-instance-smoke
```

This uses `docker-compose.prodlike.yml` (metadata + shared + premium Postgres) and runs
control-plane environment routing validation after provisioning an environment on the
`premium` instance.

For CI-friendly checks:

```bash
curl -sS -X POST "http://localhost:8090/api/admin/environment/validate-routing?ping=true" \
  -H "Authorization: Bearer $KL_CONTROL_TOKEN"
```

Failure-containment drill:

```bash
make ci-multi-instance-failure-drill
```

This intentionally stops `postgres-premium` and verifies:
- premium-routed requests fail (`500`/`503`)
- shared-routed requests keep working
- `/admin/routing/stats` includes the premium instance entry.

## Prod-like docker compose init (one-time)

When running `docker-compose.prodlike.yml` in **prod init mode** (`KL_INIT_MODE=prod`),
`kld` will refuse to start until the control-plane metadata DB is
initialized (it checks `system_init.status`).

By default, prod mode also expects strict transport (`KL_PROD_TLS_REQUIRED=true`):
HTTPS listeners plus `sslmode=verify-full` on database links. If you are not in
a regulated environment and want to run prod mode without TLS, explicitly set
`KL_PROD_TLS_REQUIRED=false`.

Bring up the stack, then run the control-plane init inside the compose service:

```bash
# from repo root
cp .env.example .env
docker-compose -f docker-compose.prodlike.yml up --build -d

# one-time init against the control-plane DB
docker-compose -f docker-compose.prodlike.yml exec klc klc migrate
docker-compose -f docker-compose.prodlike.yml exec klc klc init \
  --tenant self-hosted \
  --tenant-name "Self Hosted" \
  --token-name operator-bootstrap
```

After that, the runtime API should answer on `http://localhost:8080` and the
control-plane API should answer on `http://localhost:8090`.

Next, open the control UI at `http://localhost:8090/portal`, paste the
bootstrap token from `init`, then create:

- a workspace
- an environment inside that workspace
- a token for that environment

More explicitly:

1. Create the workspace and copy its `workspace_id` (`ws_...`).
2. Create an environment under that workspace.
3. Load that workspace in **Environments by Workspace** and copy the environment's `env_public_id` (`env_...`).
4. Create a token using that same `workspace_id` and `env_public_id`.
5. Copy the raw token secret (`kl_...`) when shown.

That is the normal self-hosted onboarding path before using Terraform or
`kl` against the runtime API.

For sample projects that default to the quick local backend, copy the prod-like
example backend file into place before running `terraform init`:

```bash
cp examples/local-backend/backend.tf.prodlike examples/big-state/backend.tf
rm -rf examples/big-state/.terraform examples/big-state/.terraform.lock.hcl
(cd examples/big-state && terraform init)
```

The prod-like compose expects `KL_CONTROL_TOKEN` for local runs, because the control plane refuses to start in prod mode without an operator API token.

If you previously ran this stack before the current migration baseline was
squashed, wipe volumes and start fresh:

```bash
docker-compose -f docker-compose.prodlike.yml down -v
docker-compose -f docker-compose.prodlike.yml up --build -d
```

`init` prints the bootstrap token **once**. Treat it like a secret.

When wiring Terraform to the prod-like runtime, use:

```text
http://localhost:8080/states/{workspace_id}/{env_public_id}/{state_name}
```

where:

- `workspace_id` is the workspace slug (`ws_...`)
- `env_public_id` comes from the environment row in the control UI / control API
- `state_name` is your Terraform state name (for example `big-state`)

Control-plane API reference snippets (RBAC, entitlements, onboarding) live in:
`docs/runbooks/control-api.md`.

## IaC CLI selection (Terraform / OpenTofu)

Kilolock still executes the real IaC CLI in the background; you can choose
which binary/version to use:

- Global defaults (env):
  - `KL_IAC_BIN` (for example `terraform` or `tofu`)
  - `KL_IAC_VERSION` (for version-manager style wrappers)
- Per-command override:
  - `kl plan --iac-version ...`
  - `kl apply --iac-version ...`
  - `kl provision dedicated --iac-version ...`

`--terraform-bin` remains available and takes precedence when explicitly set.

## Tenant health summary script

Use `scripts/tenant-health-summary.sh` for a compact per-environment view that
combines migration status with routing health counters.

```bash
KL_API_URL=http://localhost:8080 \
KL_AUTH_MODE=basic \
KL_PASSWORD=dev-local-token-change-me \
./scripts/tenant-health-summary.sh local
```

Bearer mode is also supported:

```bash
KL_API_URL=http://localhost:8080 \
KL_AUTH_MODE=bearer \
KL_TOKEN=<token> \
./scripts/tenant-health-summary.sh local
```
