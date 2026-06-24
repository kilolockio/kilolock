# State Engine Protocol v1 Draft

This document is the draft wire contract for the **state engine protocol** used by
`kl` to talk to Kilolock without being constrained to Terraform's plain HTTP
backend snapshot semantics.

It complements, rather than replaces, the Terraform-compatible
[`docs/protocol.md`](./protocol.md) contract.

## Status

- **Status:** Draft
- **Last updated:** 2026-06-24
- **Relates to:** [ADR 0028](./adr/0028-backend-enriched-graph-assisted-slice-planning.md), [ADR 0029](./adr/0029-state-engine-protocol-for-sliced-state-and-resource-locking.md), [Terraform/OpenTofu compatibility policy](./terraform-compatibility.md)

## Goals

The state engine protocol exists to support workflows that are awkward or expensive
under plain Terraform/OpenTofu HTTP backend semantics, especially on very large
states.

Primary goals:

- fetch only a relevant slice of realized state instead of always pulling the
  full trunk snapshot
- lock only the relevant resource / module scope for state engine concurrency
- make native state operations possible (`state rm`, `state mv`, repair,
  rollback, patch)
- keep the same logical state compatible with plain Terraform/OpenTofu HTTP
  backend usage
- make mixed-mode operation safe by presenting the state as locked to plain
  Terraform/OpenTofu while a state engine write is in progress

## Non-goals

- replacing the Terraform/OpenTofu HTTP backend contract
- requiring users to migrate state to a different storage model
- guaranteeing a perfect minimal slice in every edge case
- exposing CLI-specific concepts like `--file` directly as protocol primitives

## Design principles

1. **One logical state, two lanes**
   - plain Terraform/OpenTofu uses HTTP backend semantics
   - `kl` may use state engine semantics

2. **Protocol is product surface, not CLI trick**
   - `kl` is a reference client
   - future clients should be able to implement the protocol directly

3. **State identity is shared**
   - both lanes address the same workspace/environment/state
   - lineage, serial family, history, and audit trail remain unified

4. **Fail closed**
   - if slice completeness or dependency closure cannot be proven safely, the
     client should widen scope or fall back instead of guessing

5. **Mixed-mode safety first**
   - state engine write activity must appear locked to plain Terraform/OpenTofu

## Transport

v1 uses:

- HTTPS
- JSON request/response bodies
- bearer token or equivalent auth already accepted by Kilolock APIs

Base path:

```text
/v1/state-engine
```

This path is separate from:

- `/v1/states/...` for Terraform/OpenTofu HTTP backend traffic
- `/v1/admin/...` for current operator/admin APIs

## State identity

Protocol requests must identify a state using one of these equivalent forms:

### Canonical logical identity

```json
{
  "workspace_id": "ws_ab12cd34ef56",
  "env_public_id": "env_12ab34cd56ef",
  "state_name": "prod"
}
```

### Canonical combined name

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod"
}
```

### State URL

```json
{
  "state_url": "https://api.example.com/v1/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
}
```

Clients should prefer sending the combined `state` form once the identity is
resolved.

## Capability negotiation

### `GET /v1/state-engine/capabilities`

Returns protocol version and supported features.

Example response:

```json
{
  "protocol": "state-engine",
  "version": "v1",
  "capabilities": {
    "slice_fetch": true,
    "backend_closure_expansion": true,
    "resource_reservations": true,
    "terraform_visible_native_lock": true,
    "delta_commit": false,
    "native_state_rm": false,
    "native_state_mv": false
  }
}
```

### Notes

- `delta_commit=false` means the backend may still require a fuller commit path
  even when native slicing/reservations are available.
- Capability negotiation allows the backend and client to evolve without
  pretending every deployment supports every optimization.

## Client configuration

The protocol is selected by KL-owned configuration, not by introducing a new
Terraform backend type.

### Terraform/OpenTofu backend block

Users may keep a normal HTTP backend:

```hcl
terraform {
  backend "http" {
    address        = "https://api.example.com/v1/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
    lock_address   = "https://api.example.com/v1/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
    unlock_address = "https://api.example.com/v1/state-unlock/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
    lock_method    = "LOCK"
    unlock_method  = "POST"
  }
}
```

### `.kl.toml`

Example:

```toml
state_url = "https://api.example.com/v1/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
protocol = "kl"

[auth]
token_env = "KL_TOKEN"
```

### Environment variables

- `KL_STATE_URL`
- `KL_TOKEN`
- `KL_PROTOCOL`

### Precedence

Recommended precedence:

1. explicit CLI flags
2. KL environment variables
3. `.kl.toml`
4. discovered Terraform/OpenTofu backend config

## Protocol flow

The expected high-level flow for a scoped state engine write is:

1. resolve state identity
2. fetch capabilities
3. analyze local config and derive candidate scope
4. ask backend to expand that scope over realized state
5. fetch a slice
6. acquire reservations
7. mark the state as Terraform-visible locked
8. run local planning / execution / validation
9. commit mutation
10. release reservations and Terraform-visible lock

## Scope and closure primitives

The protocol intentionally accepts generic scope primitives rather than
CLI-specific flags such as `--file`.

### Resource address selector

```json
{
  "kind": "resource_address",
  "value": "aws_instance.web[0]"
}
```

### Module prefix selector

```json
{
  "kind": "module_prefix",
  "value": "module.database"
}
```

### File provenance hint

```json
{
  "kind": "file_hint",
  "value": "database.tf"
}
```

This may be used for diagnostics or server-side heuristics later, but it is not
the fundamental scope unit.

## Metadata lookup

### `POST /v1/state-engine/state/resolve`

Resolves state metadata and canonical identity.

Request:

```json
{
  "state_url": "https://api.example.com/v1/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
}
```

Response:

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "state_id": "9b25b0d1-9ea9-4cd1-8c23-4ce5a6a341f1",
  "workspace_id": "ws_ab12cd34ef56",
  "env_public_id": "env_12ab34cd56ef",
  "state_name": "prod",
  "lineage": "5734f91e-ac46-8262-b0aa-fca2549fb533",
  "serial": 47,
  "creator": "http_backend"
}
```

## Backend-assisted closure expansion

### `POST /v1/state-engine/scope/expand`

Expands client intent over the enriched realized graph.

Request:

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "selectors": [
    { "kind": "resource_address", "value": "module.database.aws_db_instance.primary" },
    { "kind": "module_prefix", "value": "module.database" }
  ],
  "client_context": {
    "explicit_write_candidates": [
      "module.database.aws_db_instance.primary"
    ],
    "explicit_read_candidates": [
      "aws_kms_key.main"
    ],
    "undeployed_config_candidates": [
      "module.database.aws_db_parameter_group.primary"
    ]
  }
}
```

Response:

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "realized_write_closure": [
    "module.database.aws_db_instance.primary"
  ],
  "realized_read_closure": [
    "aws_kms_key.main",
    "aws_security_group.db"
  ],
  "missing_from_state": [
    "module.database.aws_db_parameter_group.primary"
  ],
  "reservation_candidates": [
    { "address_glob": "module.database.aws_db_instance.primary", "mode": "write" },
    { "address_glob": "aws_kms_key.main", "mode": "read" },
    { "address_glob": "aws_security_group.db", "mode": "read" }
  ],
  "confidence": "safe",
  "notes": []
}
```

### Confidence values

- `safe` — backend can satisfy the realized closure cleanly
- `widened` — backend widened closure conservatively
- `unsafe` — backend cannot prove enough closure; client should fail closed or
  fall back

## Slice fetch

### `POST /v1/state-engine/state/slice`

Fetches a reduced realized state slice.

Request:

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "addresses": [
    "module.database.aws_db_instance.primary",
    "aws_kms_key.main",
    "aws_security_group.db"
  ],
  "base_serial": 47
}
```

Response:

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "state_id": "9b25b0d1-9ea9-4cd1-8c23-4ce5a6a341f1",
  "lineage": "5734f91e-ac46-8262-b0aa-fca2549fb533",
  "serial": 47,
  "slice": {
    "resources": [
      {
        "address": "module.database.aws_db_instance.primary",
        "provider": "registry.terraform.io/hashicorp/aws",
        "schema_version": 1,
        "attributes_json": { "identifier": "prod-db" },
        "attributes_hash": "sha256:abc",
        "resource_version": 313
      }
    ],
    "outputs": [],
    "metadata": {
      "terraform_version": "1.13.4"
    }
  }
}
```

### Notes

- v1 does not require that the wire slice be a complete Terraform state JSON
  document. It may be a backend-native shape that `kl` materializes locally
  into whatever execution format it needs.
- A future client other than `kl` may choose a different local materialization.

## Reservations

### `POST /v1/state-engine/reservations/acquire`

Request:

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "apply_id": "c106e3b2-eb7a-44f7-a2c7-c220b8d61297",
  "holder": "alice@example.com@kl",
  "lease_seconds": 900,
  "want": [
    { "address_glob": "module.database.aws_db_instance.primary", "mode": "write" },
    { "address_glob": "aws_kms_key.main", "mode": "read" }
  ]
}
```

Success response:

```json
{
  "ok": true
}
```

Conflict response:

```json
{
  "error": "reservation_conflict",
  "conflicts": [
    {
      "address_glob": "module.database.aws_db_instance.primary",
      "mode": "write",
      "holder": "bob@example.com@kl",
      "apply_id": "1f46f5f4",
      "expires_at": "2026-06-24T12:00:00Z"
    }
  ]
}
```

### `POST /v1/state-engine/reservations/renew`

Renews active reservations by `apply_id`.

### `POST /v1/state-engine/reservations/release`

Releases active reservations by `apply_id`.

## Terraform-visible coarse lock

Any state engine write must make the state appear locked to plain
Terraform/OpenTofu.

### `POST /v1/state-engine/terraform-lock/acquire`

Creates or activates a Terraform-visible coarse lock for a native writer.

Request:

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "apply_id": "c106e3b2-eb7a-44f7-a2c7-c220b8d61297",
  "holder": "alice@example.com@kl",
  "scope_summary": [
    "module.database.aws_db_instance.primary"
  ]
}
```

Response:

```json
{
  "ok": true,
  "lock_id": "state-engine-c106e3b2-eb7a-44f7-a2c7-c220b8d61297"
}
```

### `POST /v1/state-engine/terraform-lock/release`

Releases the Terraform-visible coarse lock by `apply_id` or `lock_id`.

### Required effect

While this coarse lock is active:

- `LOCK /v1/states/{name}` from plain Terraform/OpenTofu must fail as locked
- the returned lock metadata should identify the holder as a state engine
  operation

## Native commit

### `POST /v1/state-engine/state/commit`

Commits a state engine mutation.

v1 supports two commit modes:

1. `mode = "delta"` for future narrower mutation paths
2. `mode = "snapshot"` for transitional implementations that still produce a
   fuller post-mutation document locally

### Delta request shape

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "apply_id": "c106e3b2-eb7a-44f7-a2c7-c220b8d61297",
  "base_serial": 47,
  "mode": "delta",
  "changes": [
    {
      "op": "replace",
      "address": "module.database.aws_db_instance.primary",
      "before_hash": "sha256:abc",
      "after": {
        "provider": "registry.terraform.io/hashicorp/aws",
        "schema_version": 1,
        "attributes_json": { "identifier": "prod-db-v2" }
      }
    }
  ]
}
```

### Snapshot request shape

```json
{
  "state": "ws_ab12cd34ef56/env_12ab34cd56ef/prod",
  "apply_id": "c106e3b2-eb7a-44f7-a2c7-c220b8d61297",
  "base_serial": 47,
  "mode": "snapshot",
  "raw_state": "{...full state json...}",
  "write_set": [
    "module.database.aws_db_instance.primary"
  ]
}
```

Success response:

```json
{
  "ok": true,
  "committed_serial": 48,
  "new_version_id": "beae2e08-477a-4c95-b241-3e39c27c1247"
}
```

Conflict response:

```json
{
  "error": "state_serial_conflict",
  "current_serial": 48
}
```

## Native state operations

These are capability-gated and may be added incrementally.

### `POST /v1/state-engine/state/rm`

Removes one or more addresses from state.

### `POST /v1/state-engine/state/mv`

Moves one address to another address.

### Notes

- v1 should start with exact-address operations only.
- advanced module-wide or dynamically expanded semantics may be deferred.

## Error model

Protocol errors should use stable machine-readable codes.

Recommended codes:

- `unsupported_capability`
- `invalid_request`
- `unauthenticated`
- `forbidden`
- `state_not_found`
- `reservation_conflict`
- `terraform_locked`
- `state_serial_conflict`
- `unsafe_scope_closure`
- `slice_incomplete`
- `native_commit_not_supported`

Example:

```json
{
  "error": "unsafe_scope_closure",
  "message": "selected scope depends on undeployed resources outside the safe closure",
  "details": {
    "missing": [
      "module.database.aws_db_parameter_group.primary"
    ]
  }
}
```

## Lock matrix

### Native vs native

| Existing activity | New state engine read | New state engine write |
|---|---|---|
| no activity | allow | allow |
| native read on disjoint scope | allow | allow |
| native write on disjoint scope | allow | allow |
| native write on overlapping scope | block / wait | block / wait |

### Native vs plain Terraform/OpenTofu

| Existing activity | Plain Terraform/OpenTofu lock request |
|---|---|
| no native write | normal HTTP backend lock behavior |
| native read only | normal HTTP backend lock behavior |
| native write active | fail as locked |

Read-only native operations do not need to present a Terraform-visible coarse
lock unless they are participating in a mutation workflow.

## Backward compatibility

- Existing Terraform/OpenTofu HTTP backend users remain fully supported.
- Existing `kl` commands may begin by calling the state engine protocol only when:
  - explicitly configured, or
  - protocol negotiation and client policy allow it
- Falling back from state engine mode to the HTTP-compatible lane is expected in
  early implementations.

## Implementation order

Recommended order:

1. capabilities
2. state resolve
3. scope expand
4. slice fetch
5. reservations
6. Terraform-visible coarse lock
7. snapshot-mode native commit
8. exact-address `state rm`
9. exact-address `state mv`
10. delta-mode commit

## Open questions

1. Should the protocol allow the backend to return an already-materialized
   Terraform state JSON slice in addition to the backend-native slice shape?
2. Should the client send local-config closure hints as plain addresses only, or
   as richer typed graph nodes?
3. Should Terraform-visible coarse locks be explicit protocol objects, or
   derived automatically from native write reservations?
4. At what point should native commit become resource-row-authoritative by
   default rather than snapshot-mode transitional?
