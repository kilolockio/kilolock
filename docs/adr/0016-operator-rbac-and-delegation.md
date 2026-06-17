# ADR 0016: Operator RBAC and delegated control-plane permissions

- **Status:** Proposed
- **Date:** 2026-05-29
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0003](./0003-governance-and-monetization.md), [ADR 0013](./0013-environment-isolation.md), [ADR 0015](./0015-control-plane-separation.md)

## Context

Today the control plane starts from a single bootstrap operator token. This is
good for bring-up, but not enough for real production operations where we need:

- multiple operator identities
- least-privilege access by function (support, SRE, billing, platform admin)
- auditable permission boundaries
- safe delegation to customer-facing control workflows

Without explicit roles and permissions, every elevated token is effectively
super-admin, which increases blast radius and weakens audit quality.

## Decision

Kilolock will add **RBAC for operator/control-plane actions** with scoped
permissions and role bindings. Bootstrap token remains for initial setup, then
operators should create least-privilege principals.

### D1. Principals, roles, permissions

Add explicit control-plane auth model:

- **Principal**: human, service account, automation client
- **Role**: named permission bundle
- **Permission**: atomic action (for example `tenant.create`, `token.rotate`)
- **Binding**: principal -> role with optional scope

### D2. Scoped authorization

Permissions are evaluated with scope:

- global (all tenants)
- tenant-scoped
- environment-scoped

This allows support/billing/operator personas without global superuser access.

### D3. Bootstrap remains one-time break-glass

`klc init` still issues the first high-privilege token.
After initialization:

- bootstrap token is intended for emergency use only
- day-to-day operations should use role-bound principals
- bootstrap rotation/revocation is supported operationally

### D4. Default role set

Ship opinionated defaults:

- `platform_admin` (full control-plane permissions)
- `tenant_admin` (manage one tenant and its environments/tokens)
- `support_readonly` (read metadata/state inventory only)
- `provisioner` (run provisioning workflows, no tenant billing/admin changes)

### D5. Enforcement point

Authorization is enforced in `klc` API/CLI handlers (policy check
before store mutation). Data-plane backend auth remains environment token based.

## Consequences

**Positive**

- reduced blast radius
- clear separation of duties
- stronger enterprise posture for paid service
- cleaner external control-plane integration story

**Negative**

- added schema and policy complexity
- migration/bootstrapping path must be clear to avoid lockout
- more test surface (authz matrix)

## Implementation path (phased)

1. **Schema + policy primitives**
   - tables for principals, roles, permissions, bindings, auth tokens
   - seed default roles/permissions
2. **Read-only enforcement first**
   - require authn principal resolution for control-plane API
   - enforce permissions for list/get endpoints
3. **Write-path enforcement**
   - enforce create/update/delete/suspend/archive/provision actions
   - deny by default when permission missing
4. **Scoped bindings**
   - tenant/environment-scoped role bindings
   - scope-aware evaluator
5. **Operator UX**
   - CLI/API for principal create, role binding, token rotation/revocation
   - web UI role management screen
6. **Audit hardening**
   - log principal ID, action, scope, request ID, decision (allow/deny)
   - include role source in decision trace

## Compatibility and migration

- Existing bootstrap token flow remains functional.
- Initial rollout can map bootstrap token to `platform_admin`.
- If RBAC config is absent, startup behavior should fail closed in production
  mode and fail open only in explicit development mode.

## Non-goals

- Full enterprise IdP/SSO in first RBAC iteration.
- Resource-level Terraform object authorization in backend runtime.
- Replacing customer environment tokens for Terraform/OpenTofu backend access.

