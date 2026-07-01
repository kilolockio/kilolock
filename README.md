# Kilolock

[![ci](https://github.com/kilolockio/kilolock/actions/workflows/ci.yml/badge.svg)](https://github.com/kilolockio/kilolock/actions/workflows/ci.yml)
[![lint](https://github.com/kilolockio/kilolock/actions/workflows/lint.yml/badge.svg)](https://github.com/kilolockio/kilolock/actions/workflows/lint.yml)
[![go](https://img.shields.io/github/go-mod/go-version/kilolockio/kilolock)](./go.mod)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](./LICENSE)

Project site: [docs/index.html](./docs/index.html)

**A self-hosted graph-native control plane for Terraform and OpenTofu.**

Kilolock stores Terraform/OpenTofu state in PostgreSQL as a normalized graph
instead of a flat `.tfstate` blob. That gives you a drop-in HTTP backend,
queryable state, provider-aware refresh, resource-level history/repair, and
scoped/orchestrated workflows for large states.

## Two Usage Modes

Kilolock currently has two distinct lanes:

- **Terraform/OpenTofu HTTP backend lane**
  This is the default, stable path. Terraform or OpenTofu talks to
  `/v1/states/...` with normal HTTP-backend semantics and Kilolock stays
  wire-compatible.
- **KL state-engine lane**
  This is the experimental power-user path for sliced state fetch, narrower
  reservations, and native state operations such as `kl state rm` and
  `kl state mv`. It is the point where Kilolock starts to differ sharply from
  plain Terraform backend behavior.

If you are new to the repo, start with the HTTP backend lane first. Move to the
state-engine lane when you specifically want large-state collaboration or native
exact-address state mutations.

### Why the native lane matters

Terraform's HTTP backend model treats the state as one snapshot that must be
pulled, locked, and written back as a whole. That is simple and compatible, but
it becomes painful when a company keeps a lot of infrastructure in one shared
state.

Kilolock's native state-engine lane is meant for that situation:

- `kl` can ask the backend for only the realized slice that matters for a
  selected file, target, or module
- the backend can reason about the dependency graph before the client runs
  Terraform locally
- the trusted native lane can reserve only the relevant branch of the graph
  instead of serializing unrelated work
- exact-address native operations such as `kl state rm`, `kl state mv`, and
  resource rollback stop being "whole-state" workflows

The long-term implication is important: a team should be able to keep a very
large monolithic state when that matches the real architecture, while still
letting different engineers or teams update their own graph branches without
blocking everybody else. In the future, that same model can support finer
resource- or branch-level permissions on top of one logical state.

## Requirement

Terraform must be installed on the machine where you run the examples and CLI
workflows in this repo. Kilolock wraps Terraform-compatible workflows; it does
not replace the Terraform CLI itself.

Build the Kilolock binaries before using the CLI examples:

```sh
make build
export PATH="$(pwd)/bin:$PATH"
```

That puts `kl`, `kld`, and `klc` on your shell `PATH` for the current repo
checkout. If you prefer a system-wide install, copy the binaries from `./bin/`
into a directory that is already on your `PATH`.

## Versioning

Kilolock derives its build version automatically from Git tags that match
`vX.Y.Z`.

- exact tag on `HEAD` -> `X.Y.Z`
- commits after a tag -> next patch prerelease like `X.Y.(Z+1)-dev.N`
- no tag yet -> `0.1.0-dev.N`

Useful checks:

```sh
kl version
kl version --json
kld version --json
curl -s http://localhost:8080/versionz
```

`/versionz` is the machine-readable runtime endpoint for the backend service.

## Quick Start

### Fastest local stack

```sh
cp .env.example .env
docker-compose up --build -d
```

This starts the simplest OSS stack on `http://localhost:8080`:

- `kld` runtime/backend
- one local Postgres
- open auth for quick backend experiments

That is enough for:

- `terraform init` / `terraform apply` against the HTTP backend
- `kl query`
- `kl query resource`
- `kl query history`
- `kl rollback resource`
- `kl state rm`
- `kl state mv`

The local OSS stack supports both lanes, but the Terraform/OpenTofu HTTP backend
path is still the baseline behavior to learn first.

If you want to see the state-engine lane distinction explicitly, run:

```sh
examples/big-state/state-engine-demo.sh lanes
```

That demo shows both outcomes:

- a proven-safe native slice that commits through `state-engine delta`
- a fallback-classified spec that stays off the trusted state-engine lane

It also shows the core trade-off of the native lane:

- when the backend can prove a safe narrow slice, `kl` works on only the
  relevant part of the state
- when it cannot prove that safely, Kilolock fails closed or falls back rather
  than guessing

### Self-hosted prod-like stack

If you want the fuller self-hosted setup with `klc`, explicit bootstrap,
control UI, and multi-instance routing:

```sh
cp .env.example .env
docker-compose -f docker-compose.prodlike.yml up --build -d
docker-compose -f docker-compose.prodlike.yml exec klc klc migrate
docker-compose -f docker-compose.prodlike.yml exec klc klc init \
  --tenant self-hosted \
  --tenant-name "Self Hosted" \
  --token-name operator-bootstrap
```

Primary runbook:

- [docs/runbooks/self-hosted-bootstrap.md](./docs/runbooks/self-hosted-bootstrap.md)

## First Useful Workflow

### 1. Point Terraform at Kilolock

```hcl
terraform {
  backend "http" {
    address        = "http://localhost:8080/v1/states/example"
    lock_address   = "http://localhost:8080/v1/states/example"
    unlock_address = "http://localhost:8080/v1/state-unlock/example"
    lock_method    = "LOCK"
    unlock_method  = "POST"
  }
}
```

For the default OSS quick-start and local `docker-compose`, use the standard
Terraform HTTP backend lock flow:

- `lock_method = "LOCK"`
- `unlock_method = "POST"`
- `unlock_address = ".../v1/state-unlock/..."`

Then:

```sh
terraform init
terraform apply
```

### 2. Query what Terraform wrote

```sh
kl query "SELECT type, COUNT(*) FROM resources GROUP BY type ORDER BY 2 DESC"
kl query -f docs/queries/inventory_by_type.sql --format csv
kl query resource --address time_sleep.slow_a
kl query history --address time_sleep.slow_a
```

### 3. Use file-scoped planning/apply

```sh
kl plan -f slow_a.tf -o slow-a.plan.json
kl apply -f slow_a.tf --confirm-scope
```

Useful notes:

- `-f` / `--file` scopes to resources declared in selected file(s)
- `-o` / `--out` writes the plan spec to a chosen path
- `kl plan` now performs a backend quota preflight when it can discover an HTTP backend state
- hard quota overages fail the plan early; soft quota overages print a warning
- destructive file-scoped applies require explicit acknowledgement

### 4. Check quota headroom before apply

```sh
kl quota remaining
terraform plan -out=plan.tfplan
kl quota check --tf-plan plan.tfplan
```

Useful notes:

- `kl quota remaining` shows current state and environment headroom
- `kl quota check` consumes a Terraform plan and evaluates the projected resource count against backend quota
- soft limit breaches return success with a warning
- hard limit breaches return a non-zero exit code before `kl apply`

### 5. Refresh state without a full Terraform plan cycle

```sh
kl refresh example --dry-run
kl refresh example
kl query -f docs/queries/drift_current.sql --format json
```

This refresh path still talks to providers, but it avoids rerunning the full
Terraform config/plan flow when you mainly want updated backend state and drift
visibility.

### 6. Inspect, repair, or roll back

```sh
kl status example
kl history example
kl rollback resource --address time_sleep.slow_a --to @1
kl apply abort --state example --latest
```

Experimental native state-engine operations:

```sh
KL_PROTOCOL=state-engine kl state rm example --address time_sleep.slow_b
KL_PROTOCOL=state-engine kl state mv example --from time_sleep.slow_a --to module.demo.time_sleep.slow_a
```

Those state-engine commands are aimed at large-state and repair workflows. They
use Kilolock's native protocol rather than Terraform's plain HTTP backend wire
contract.

That native protocol is the reason Kilolock can eventually support "one giant
state, many independent teams" much better than a plain HTTP backend ever can:
the backend understands graph branches and slice boundaries instead of seeing
every operation as one whole-state blob exchange.

Runbook for stuck applies:

- [docs/runbooks/apply-abort.md](./docs/runbooks/apply-abort.md)

## Examples

### Main hands-on demo

The main example is:

- [`examples/big-state/`](./examples/big-state/)

It covers:

- local backend setup
- large shared-state behavior
- drift walkthrough
- parallel apply demo
- useful SQL queries

Quick taste:

```sh
cd examples/big-state
terraform init
terraform apply -auto-approve -var=size=100
```

### Smoke tests

```sh
./scripts/smoke.sh
TF_BIN=tofu ./scripts/smoke.sh
make ci-multi-instance-smoke
make ci-multi-instance-failure-drill
```

## What You Get Today

- drop-in Terraform/OpenTofu HTTP backend
- normalized PostgreSQL state graph
- SQL querying across resources and states
- provider-aware refresh and drift surfacing
- file-scoped and targeted plan/apply helpers
- backend-driven quota preview and plan admission checks
- orchestrated apply foundations with reservations
- per-resource history and emergency repair
- self-hosted control plane for operator workflows

## Binaries

```sh
make build
```

This produces:

- `./bin/kl` - client CLI
- `./bin/kld` - backend/runtime server
- `./bin/klc` - control-plane server

### Run locally without Docker Compose

```sh
make db-up
cp .kl.toml.example .kl.toml
kld
```

Environment variables such as `KL_DATABASE_URL` and `DATABASE_URL` override
values from `.kl.toml`.

## Docs Map

### Start here

- [docs/runbooks/self-hosted-bootstrap.md](./docs/runbooks/self-hosted-bootstrap.md)
- [examples/big-state/README.md](./examples/big-state/README.md)
- [docs/runbooks/control-api.md](./docs/runbooks/control-api.md)

### State-engine docs

- [docs/state-engine-protocol-v1.md](./docs/state-engine-protocol-v1.md)
- [docs/runbooks/state-engine-local-smoke.md](./docs/runbooks/state-engine-local-smoke.md)
- [docs/adr/0029-state-engine-protocol-for-sliced-state-and-resource-locking.md](./docs/adr/0029-state-engine-protocol-for-sliced-state-and-resource-locking.md)

### Operator runbooks

- [docs/runbooks/apply-abort.md](./docs/runbooks/apply-abort.md)
- [docs/runbooks/resource-level-emergency-repair.md](./docs/runbooks/resource-level-emergency-repair.md)
- [docs/runbooks/execution-plane-audit-checklist.md](./docs/runbooks/execution-plane-audit-checklist.md)
- [docs/runbooks/oss-first-release-checklist.md](./docs/runbooks/oss-first-release-checklist.md)

### Architecture and reference

- [docs/protocol.md](./docs/protocol.md)
- [docs/schema.md](./docs/schema.md)
- [docs/terraform-compatibility.md](./docs/terraform-compatibility.md)
- [docs/adr/](./docs/adr/)

## Roadmap

Current OSS shape:

- v0: queryable state
- v1: provider-aware refresh and drift surfacing
- v2: scoped/orchestrated apply on shared state
- v3: state-engine protocol for sliced state, narrower locking, and native state operations

If you want the detailed implementation history and design rationale, start
with:

- [docs/adr/0006-refresh-implementation.md](./docs/adr/0006-refresh-implementation.md)
- [docs/adr/0007-parallel-apply.md](./docs/adr/0007-parallel-apply.md)
- [docs/adr/0014-file-scoped-plan-apply.md](./docs/adr/0014-file-scoped-plan-apply.md)
- [docs/adr/0015-control-plane-separation.md](./docs/adr/0015-control-plane-separation.md)
- [docs/adr/0029-state-engine-protocol-for-sliced-state-and-resource-locking.md](./docs/adr/0029-state-engine-protocol-for-sliced-state-and-resource-locking.md)

## Scope

This repo is the OSS/self-hosted Kilolock stack:

- `kl`
- `kld`
- `klc`
- local Docker Compose flows
- backend/query/repair/operator workflows

Hosted-business features such as customer portal UX, billing/signup glue, and
cloud-specific deployment packaging live outside this OSS repo.

## IaC CLI Selection

Kilolock still executes the real IaC CLI in the background. You can control
that with:

- `KL_IAC_BIN`
- `KL_IAC_VERSION`
- `kl plan --iac-version ...`
- `kl apply --iac-version ...`
- `kl provision dedicated --iac-version ...`

`--terraform-bin` remains available and takes precedence when set explicitly.

## Governance And Contributing

Kilolock is Apache 2.0 open source. The OSS project remains self-hostable and
auditable; a future managed service does not change that.

- [`CONTRIBUTING.md`](./CONTRIBUTING.md)
- [`cla/icla.md`](./cla/icla.md)
- [`MAINTAINERS.md`](./MAINTAINERS.md)
- [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md)
- [`TRADEMARK.md`](./TRADEMARK.md)

## License

[Apache License 2.0](./LICENSE)
