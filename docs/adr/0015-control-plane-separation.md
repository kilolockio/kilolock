# ADR 0015: Separate customer control plane from core Kilolock backend

- **Status:** Proposed
- **Date:** 2026-05-28
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0003](./0003-governance-and-monetization.md), [ADR 0013](./0013-environment-isolation.md), [ADR 0014](./0014-file-scoped-plan-apply.md)

## Context

Kilolock now includes strong backend primitives for hosted operation:
tenants, environments, API tokens, and environment-level database routing.

For a paid production service, we also need end-user flows such as:

- organization signup and lifecycle
- self-serve environment creation/deletion
- token issuance/rotation UX
- quotas, billing, and plan enforcement
- support/admin operations and audit experiences

Embedding these product-facing workflows directly into the core backend
creates avoidable coupling and risk:

- expands the security surface of the state backend
- mixes fast-moving product concerns with backend runtime concerns
- makes OSS adoption harder for operators who want different business logic

## Decision

Kilolock will keep the **backend engine** and its operator/admin primitives
in OSS, while customer onboarding and commercial lifecycle workflows are
implemented in a **separate control-plane service**.

### D1. Core backend remains capability-complete in OSS

OSS Kilolock keeps:

- Terraform/OpenTofu backend protocol handling
- state storage/query/refresh/apply features
- tenant/environment/token primitives
- multi-instance data-plane routing primitives
- operator/admin commands for provisioning and diagnostics

No core backend capability is removed to force paid adoption.

### D2. Customer-facing service is separate

A separate service (future Web UI + API) owns:

- account/org lifecycle
- self-serve tenant/environment provisioning orchestration
- policy and quota enforcement at product tier level
- billing/metering integration
- customer-facing audit/support workflows

This service talks to Kilolock through explicit admin/operator interfaces.

### D3. Publish and stabilize an integration contract

To let anyone run their own "Kilolock shop," we document a stable contract
for external control planes:

- required operations (create tenant/env/token, provision, status, rotate/revoke)
- idempotency expectations
- long-running provisioning states (`provisioning`, `ready`, `failed`)
- error classes suitable for retries vs operator action
- minimum authn/authz expectations for admin surfaces

Kilolock remains usable standalone without that service.

### D4. Security boundary

In production hosted setups:

- end users should not receive direct unrestricted access to backend admin APIs
- control-plane service holds elevated credentials and performs privileged actions
- Kilolock runtime stays focused on state protocol and execution paths

## Consequences

**Positive**

- cleaner architecture and lower blast radius
- clearer OSS/commercial boundary based on operations, not feature removal
- easier for third parties to build custom portals and business logic
- faster iteration on product UX without destabilizing backend runtime

**Negative**

- more components to operate in paid deployment
- contract/versioning work is required to avoid integration drift
- eventual consistency between systems must be handled explicitly

## Implementation path

1. Keep existing admin/operator capabilities in Kilolock OSS.
2. Document control-plane integration contract (CLI/API runbook).
3. Add contract-focused integration tests (idempotent create/provision/token flows).
4. Build separate control-plane service (API first, Web UI second).
5. Add versioned compatibility notes for control-plane to backend integration.

## Non-goals

- Building billing, signup, and storefront workflows inside `kld`.
- Hiding current tenant/environment primitives from OSS users.
- Replacing Terraform/OpenTofu workflow semantics with a proprietary runner model.

