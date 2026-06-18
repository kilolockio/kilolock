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

## Requirement

Terraform must be installed on the machine where you run the examples and CLI
workflows in this repo. Kilolock wraps Terraform-compatible workflows; it does
not replace the Terraform CLI itself.

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

- `docs/runbooks/self-hosted-bootstrap.md`

## First Useful Workflow

### 1. Point Terraform at Kilolock

```hcl
terraform {
  backend "http" {
    address        = "http://localhost:8080/states/example"
    lock_address   = "http://localhost:8080/states/example"
    unlock_address = "http://localhost:8080/state-unlock/example"
    lock_method    = "LOCK"
    unlock_method  = "POST"
  }
}
```

For the default OSS quick-start and local `docker-compose`, keep this shape as-is:

- `lock_method = "LOCK"`
- `unlock_method = "POST"`
- `unlock_address = ".../state-unlock/..."`

That local stack already supports the cloud-compatible unlock route, so there is
no need to maintain a separate local-only backend pattern.

Then:

```sh
terraform init
terraform apply
```

### 2. Query what Terraform wrote

```sh
./bin/kl query "SELECT type, COUNT(*) FROM resources GROUP BY type ORDER BY 2 DESC"
./bin/kl query -f docs/queries/inventory_by_type.sql --format csv
./bin/kl query resource --address time_sleep.slow_a
./bin/kl query history --address time_sleep.slow_a
```

### 3. Use file-scoped planning/apply

```sh
./bin/kl plan -f slow_a.tf -o slow-a.plan.json
./bin/kl apply -f slow_a.tf --confirm-scope
```

Useful notes:

- `-f` / `--file` scopes to resources declared in selected file(s)
- `-o` / `--out` writes the plan spec to a chosen path
- `kl plan` now performs a backend quota preflight when it can discover an HTTP backend state
- hard quota overages fail the plan early; soft quota overages print a warning
- destructive file-scoped applies require explicit acknowledgement

### 4. Check quota headroom before apply

```sh
./bin/kl quota remaining
terraform plan -out=plan.tfplan
./bin/kl quota check --tf-plan plan.tfplan
```

Useful notes:

- `kl quota remaining` shows current state and environment headroom
- `kl quota check` consumes a Terraform plan and evaluates the projected resource count against backend quota
- soft limit breaches return success with a warning
- hard limit breaches return a non-zero exit code before `kl apply`

### 5. Refresh without paying full Terraform refresh every time

```sh
./bin/kl refresh example --dry-run
./bin/kl refresh example
./bin/kl query -f docs/queries/drift_current.sql --format json
```

### 6. Inspect, repair, or roll back

```sh
./bin/kl status example
./bin/kl history example
./bin/kl rollback resource --address time_sleep.slow_a --to @1
./bin/kl apply abort --state example --latest
```

Runbook for stuck applies:

- `docs/runbooks/apply-abort.md`

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
./bin/kld
```

Environment variables such as `KL_DATABASE_URL` and `DATABASE_URL` override
values from `.kl.toml`.

## Docs Map

### Start here

- `docs/runbooks/self-hosted-bootstrap.md`
- `examples/big-state/README.md`
- `docs/runbooks/control-api.md`

### Operator runbooks

- `docs/runbooks/apply-abort.md`
- `docs/runbooks/resource-level-emergency-repair.md`
- `docs/runbooks/execution-plane-audit-checklist.md`
- `docs/runbooks/oss-first-release-checklist.md`

### Architecture and reference

- `docs/protocol.md`
- `docs/schema.md`
- `docs/terraform-compatibility.md`
- `docs/adr/`

## Roadmap

Current OSS shape:

- v0: queryable state
- v1: provider-aware refresh and drift surfacing
- v2: scoped/orchestrated apply on shared state
- v3: cross-state transactions and deeper automation later

If you want the detailed implementation history and design rationale, start
with:

- `docs/adr/0006-refresh-implementation.md`
- `docs/adr/0007-parallel-apply.md`
- `docs/adr/0014-file-scoped-plan-apply.md`
- `docs/adr/0015-control-plane-separation.md`

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
