# Contributing to Kilolock

Thanks for considering a contribution. This document covers what you
need to know to get your change merged.

## TL;DR

1. Open an issue first for anything non-trivial. Discussion is cheaper
   than a rejected PR.
2. Sign the [ICLA](./cla/icla.md) on your first non-trivial Pull
   Request. Trivial fixes (typos, comments, ≤10 lines) don't need a
   signature.
3. Make sure `make fmt vet test` is clean.
4. Open a PR against `main` with a clear description.
5. Maintainers respond within a few days. Drive-by reminders are fine
   if it's been longer than a week.

## What this project is and isn't

Kilolock is a graph-native control plane for Terraform / OpenTofu.
See the [README](./README.md) and [ADRs](./docs/adr/) for the design
direction. Contributions that fit that direction — and that the
maintainers agree fit the [v0 scope](./docs/adr/0002-v0-scope.md) —
are welcome.

Contributions that are explicitly out of scope:

- New plan/apply engine features. v0 is read-mostly with respect to
  cloud providers. The plan/apply work is v1+ territory and architected
  separately.
- Web UI work. v0 is CLI-first. UI may come later but is not yet
  scoped.
- Replacing Postgres with another storage backend. See
  [ADR 0001 §D5](./docs/adr/0001-foundations.md). The internal
  interfaces are designed for cleanliness, not pluggability.

If your idea doesn't obviously fit, open a discussion issue first. We
can usually find a workable scope.

## Development environment

You need:

- Go 1.25+
- Docker and Docker Compose (v2 standalone `docker-compose` or the
  `docker compose` plugin)
- A Unix-like shell (the Makefile assumes `bash`)

```sh
git clone https://github.com/kilolockio/kilolock.git
cd kl
make db-up
KL_DATABASE_URL='postgres://kl:kl@localhost:5432/kl?sslmode=disable' \
  make build test test-integration
```

## Code style

- Run `make fmt` before submitting. CI rejects unformatted code.
- Run `make vet` and `make vet -tags=integration` (or equivalent).
- Run `make test` and, if your change touches DB-backed code,
  `KL_DATABASE_URL=... make test-integration`.
- Follow the existing patterns. Where in doubt, read the file you're
  editing — its style is the style.

A few style notes that come up often:

- **No narrative comments.** Don't add comments that just restate
  what the code does (`// Open the database`). Comments should
  explain non-obvious *intent*, trade-offs, or constraints.
- **Sentinel errors for known failure modes.** New error categories
  should follow the `store.ErrSomething` pattern with explicit `errors.Is`
  checks at the handler/CLI boundary.
- **SQL lives next to the Go that runs it.** Inline `const querySQL = ...`
  is preferred over a separate `.sql` file for query-time SQL.
  Migration files are the exception — they live in
  `pkg/migrate/migrations/`.
- **Tests next to code, integration tests `//go:build integration`
  gated.** See `internal/backend/server_integration_test.go` for the
  pattern.

## Pull request expectations

- One logical change per PR. Refactors and feature work in separate
  PRs.
- Describe *why*, not just *what*, in the PR body.
- Update relevant docs (`README.md`, `docs/`) in the same PR.
- Add or update tests for any behavior change.
- Don't squash before review; we'll handle final history when merging.

## Contributor License Agreement

Kilolock is Apache 2.0 licensed and intends to remain so permanently.
However, non-trivial contributions require signing the project's
[Individual Contributor License Agreement](./cla/icla.md).

The ICLA grants the project's maintainer(s) — and any future legal
entity that takes over project stewardship — the rights needed to:

1. Keep distributing the OSS under Apache 2.0 (or, in the future,
   another OSI-approved permissive license).
2. Use contributed code in a future commercial offering (managed
   service or enterprise add-ons), without having to track down each
   past contributor for permission.

The rationale is recorded in
[ADR 0003](./docs/adr/0003-governance-and-monetization.md).

If you cannot sign the ICLA (because of employer restrictions, for
example), please open an issue rather than a PR. Sometimes the path
forward is for someone else to re-implement the idea independently, or
for your employer to sign a corporate variant. We'll work with you.

## Code of conduct

This project follows the [Code of Conduct](./CODE_OF_CONDUCT.md).
Be kind. Disagreements about technical decisions are expected; personal
attacks are not.

## Security issues

For security-sensitive reports (a way to exfiltrate state, a way to
bypass locking, an authentication bypass against the HTTP backend),
**please do not open a public issue**. Email the maintainer directly
(address in [`MAINTAINERS.md`](./MAINTAINERS.md)) and we will respond
within a few business days.

## Questions

Open a GitHub Discussion (when the repo grows enough to enable them)
or a regular issue with the `question` label.
