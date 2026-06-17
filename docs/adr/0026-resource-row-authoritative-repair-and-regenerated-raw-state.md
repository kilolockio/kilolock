# ADR 0026: Resource-row-authoritative repair with regenerated raw state

Date: 2026-06-11

## Status

Proposed

## Context

Kilolock currently stores state in two forms:

- `state_versions.raw_state` as the authoritative Terraform-compatible blob
- normalized `resources`, dependencies, and outputs as derived projections

That model is safe and simple, but it makes resource-level repair heavier than
it needs to be:

- preview has historically drifted toward loading and parsing large historical
  state blobs
- apply still patches full state JSON and writes a brand-new full state version
  even when the operator only wants to repair a single resource address

For large states, the operator expectation is different:

- resource history should feel interactive
- rollback preview should feel interactive
- resource repair apply should be narrowly scoped in its mutation path even if a
  full Terraform-compatible state blob must still exist afterwards

## Decision

For the next evolution of resource repair, we will move toward a
resource-row-authoritative mutation model.

The target shape is:

1. normalized `resources` rows become the authoritative mutation surface for
   resource-level repair operations
2. dependency rows and related projections are updated from that resource-level
   mutation
3. a full Terraform-compatible `raw_state` document is regenerated from the
   normalized rows and written as the next `state_version`

This preserves Terraform/backend compatibility while making resource repair much
closer to a narrow database mutation rather than a blob-patch workflow.

## Consequences

### Positive

- resource rollback preview and apply become easier to scale to very large
  states
- operator mental model improves: small repair should behave like a small
  mutation
- backend-native query and repair features become easier to present as
  low-latency tools for large-state engineering workflows

### Negative

- the storage contract becomes more complex because state regeneration must stay
  byte-coherent enough for Terraform compatibility
- regeneration bugs would be dangerous because `raw_state` remains the object
  consumed by Terraform clients
- we need careful validation that regenerated state preserves provider-facing
  semantics and instance addressing

## Near-term rule

This ADR does **not** block the current POC:

- heavy rollback apply is acceptable for now
- fast query and fast rollback preview remain the immediate priority

The current implementation can survive with a heavier apply path as long as the
operator-facing read paths are responsive.

## Follow-on work

- make rollback preview entirely SQL/index-backed
- identify the remaining slow paths in resource preview for large states
- design state regeneration from normalized rows
- add validation tests that compare regenerated `raw_state` against expected
  Terraform semantics
