# ADR 0024: Unified apply builds a spec

- **Status:** Accepted
- **Date:** 2026-06-11
- **Decision makers:** Kilolock maintainers
- **Relates to:** [ADR 0014](./0014-file-scoped-plan-apply.md), [ADR 0017](./0017-state-first-targeted-plan-apply.md), [`docs/runbooks/execution-plane-audit-checklist.md`](../runbooks/execution-plane-audit-checklist.md)

## Context

`kl apply` currently exposes different mental models depending on the
mode:

- unscoped apply can run without first producing an Kilolock plan spec
- `apply --file ...` builds a scoped spec implicitly
- `apply --target ...` builds a targeted spec implicitly
- `apply --orchestrated` has historically depended on a previously generated
  `kl-plan.json`

This split is defensible from an implementation-history perspective, but it is
not a good product model:

- users reasonably expect one apply contract
- execution semantics become harder to explain and document
- scoped and unscoped paths can drift in safety properties
- OSS users should not need to learn which apply path “really” planned first

At the same time, Kilolock’s execution-plane features depend on plan-derived
metadata:

- `write_set`
- `read_set`
- reservations
- pinned variables
- source serial / staleness checks

So the right answer is not “drop the spec”, but “make spec generation a normal
part of apply.”

## Decision

All `kl apply` modes should operate from an Kilolock plan spec.

This means:

- plain `kl apply` builds a fresh full spec implicitly when
  `--plan-spec` is not supplied
- `apply --file ...` builds a fresh file-scoped spec implicitly
- `apply --target ...` builds a fresh targeted spec implicitly
- explicit `--plan-spec` remains supported for review, CI, approvals, and
  reproducibility

The difference between apply modes is therefore:

- **how the spec is produced**
- not **whether a spec exists**

## Rationale

### One execution contract is easier to trust

If every apply mode starts from a spec, then every apply mode can share:

- the same scope model
- the same reservation model
- the same staleness model
- the same preflight and diagnostics structure

That reduces surprises and makes the OSS story cleaner.

### Explicit planning still matters

This ADR does **not** remove `kl plan`.

Explicit planning remains valuable for:

- code review
- CI artifacts
- approvals
- reproducibility
- debugging

But it becomes optional for interactive apply, not mandatory.

### Scoped apply remains first-class

This decision does not weaken scoped apply.

It preserves the original reason explicit specs existed in the first place:
scoped execution needs plan-derived metadata. The change is simply that
unscoped apply joins the same contract instead of remaining an exception.

## Consequences

### Positive

- simpler user mental model
- more uniform apply safety properties
- fewer mode-specific bugs
- better fit for public OSS usage

### Negative

- unscoped apply now pays the cost of an internal planning step
- apply implementation becomes more opinionated about Terraform planning before
  execution

This is an acceptable tradeoff because correctness and consistency are more
important than shaving a planning phase off one mode while the others still
need it.

## Implementation direction

Near-term implementation:

- when `--plan-spec` is omitted, `kl apply` generates a spec internally
  before execution
- scoped and targeted apply continue generating scoped specs internally
- explicit `--plan-spec` remains supported unchanged

Future refinements may add:

- clearer “implicit plan” progress output
- optional persistence of the generated spec for debugging
- richer freshness/approval policies around saved specs
