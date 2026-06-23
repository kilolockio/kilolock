# Control API runbook

This document describes the **klc** HTTP API (control-plane).
All endpoints are rooted at `/v1/api`.

## Auth

Requests must include:

```
Authorization: Bearer $KL_CONTROL_TOKEN
```

In addition to the static control token, some endpoints also enforce RBAC
permissions for scoped principals (see the RBAC endpoints below).

## Tenant entitlements (billing caps)

Update the per-tenant entitlement metadata (billing plan + caps). This is the
control-plane way to raise limits like `max_state_resources` or
`max_environment_resources` without ad-hoc SQL.

Inspect current tenant settings:
- `GET /v1/api/tenants/{slug}`
- Permission: `tenant.read` (scoped to the tenant)

Endpoint:
- `POST /v1/api/tenants/entitlements`
- Permission: `tenant.entitlements.update`

Request body:

```json
{
  "slug": "operator",
  "billing_plan": "starter",
  "max_environments": 1,
  "max_state_resources": 100,
  "max_environment_resources": 500,
  "reason": "temporary big-state demo",
  "actor": "ops@example.com"
}
```

Example:

```bash
curl -sS -X POST "http://localhost:18082/v1/api/tenants/entitlements" \
  -H "Authorization: Bearer $KL_CONTROL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"slug":"operator","billing_plan":"starter","max_environments":1,"max_state_resources":100,"max_environment_resources":500,"reason":"demo"}'
```

## Tenant lifecycle (suspend / archive / reactivate)

Set a tenant's lifecycle status (`active`, `suspended`, `archived`).

Endpoint:
- `POST /v1/api/tenants/lifecycle`
- Permission: `tenant.lifecycle.update`

Request body:

```json
{
  "slug": "acme",
  "status": "suspended",
  "reason": "failed payment",
  "actor": "billing@example.com"
}
```

Effect:
- When a tenant is `suspended` or `archived`, the **data-plane** Terraform HTTP backend refuses mutating operations (`POST`, `DELETE`, `LOCK`, `UNLOCK`) with `403 Forbidden`.

## State concurrency / coexistence policy

Update the per-state policy that governs:

- whether plain Terraform uses optimistic whole-state locks or strict serialization (`exclusive_locks`)
- whether `kl apply` merely warns or fails closed when plain-TF locks are active (`coexistence_mode`)

- `POST /v1/api/states/{tenant}/{environment}/config`
- Permission: `state.config.update`

Request body:

```json
{
  "state": "big-state",
  "exclusive_locks": false,
  "coexistence_mode": "strict"
}
```

Notes:

- `state` is required.
- Provide at least one of `exclusive_locks` or `coexistence_mode`.
- `coexistence_mode` must be `warn` or `strict`.

Example:

```bash
curl -sS -X POST "http://localhost:18082/v1/api/states/acme/prod/config" \
  -H "Authorization: Bearer $CONTROL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "state": "big-state",
    "exclusive_locks": false,
    "coexistence_mode": "strict"
  }'
```

## Ownership transfers (operator path)

Operators can inspect and resolve customer-facing environment ownership
transfers from the control plane too.

- `GET /v1/api/ownership-transfers`
  - Permission: `tenant.read`
  - Query params:
    - `tenant` optional source/target tenant filter
    - `status` optional status filter (`pending`, `accepted`, `rejected`, `cancelled`)
- `POST /v1/api/ownership-transfers`
  - Permission: `environment.transfer.update`
- `POST /v1/api/tenants/{tenant}/environments/rename`
  - Permission: `environment.create`
- `POST /v1/api/ownership-transfers/{id}/accept`
  - Permission: `environment.transfer.update`
- `POST /v1/api/ownership-transfers/{id}/reject`
  - Permission: `environment.transfer.update`
- `POST /v1/api/ownership-transfers/{id}/cancel`
  - Permission: `environment.transfer.update`

Create example:

```bash
curl -sS -X POST "http://localhost:18082/v1/api/ownership-transfers" \
  -H "Authorization: Bearer $CONTROL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "source_tenant": "acme",
    "environment_slug": "prod",
    "target_tenant_slug": "beta",
    "actor": "operator@example.com",
    "reason": "customer requested tenant consolidation"
  }'
```

Notes:

- proposal creation chooses the **target workspace**
- final environment label is chosen during **acceptance**
- archived target environments do not block reuse of the same label

Accept example:

```bash
curl -sS -X POST "http://localhost:18082/v1/api/ownership-transfers/$PROPOSAL_ID/accept" \
  -H "Authorization: Bearer $CONTROL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"actor":"operator@example.com","target_new_slug":"prod-beta"}'
```
