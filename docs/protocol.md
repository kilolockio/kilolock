# HTTP backend protocol

This document is the source of truth for the wire contract Kilolock
exposes to Terraform and OpenTofu in v0. It mirrors the documented
behavior of Terraform's [`http` backend][tf-http] and lists every
intentional deviation explicitly.

[tf-http]: https://developer.hashicorp.com/terraform/language/backend/http

## Address

A state is addressed by name at the URL path:

```
<scheme>://<host>:<port>/states/<state_name>
```

The `<state_name>` segment is URL-decoded and must be non-empty. State
names that contain `/` are not supported in v0.

A sample Terraform backend block:

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

## Operations

### `GET /states/{name}`

Returns the current (most recent) state for the named state.

- **200** with `Content-Type: application/json` and the state body when a
  state exists.
- **404** with no body when the state has never been written.

### `POST /states/{name}?ID=<lock_id>`

Atomically writes a new state version. The request body is the full
`.tfstate` JSON; Kilolock stores it byte-for-byte in
`state_versions.raw_state` *and* projects it into the normalized
tables (`resources`, `resource_dependencies`, `outputs`) in the same
transaction. The raw bytes remain the source of truth for export; the
normalized rows are derived data used by SQL queries and the future
graph-scoped engine.

Lock semantics:

| Lock currently held? | `?ID=` supplied? | Behavior |
|---|---|---|
| no | no | accepted; written under no lock |
| no | yes | **409 Conflict** — caller's lock view is stale |
| yes | no | **423 Locked** — supply the lock id |
| yes | matching | accepted |
| yes | mismatching | **409 Conflict** |

- **200** on success, empty body.
- **400** when the request body is not valid JSON or not a Terraform v4 state.
- **409 Conflict** also returned if the supplied state's `serial` matches a serial already stored for this state. Bump the serial and retry, or use `kl` CLI tooling to inspect history.
- **423 Locked** per the lock matrix above.
- **500** on internal errors.

### `DELETE /states/{name}?ID=<lock_id>`

Removes a state and (via `ON DELETE CASCADE`) all of its versions,
resources, dependencies, outputs, locks, and audit events. Same lock
semantics as `POST`.

- **200** on success, empty body.
- **404** if the state does not exist.
- **409 / 423** per the lock matrix.

### `LOCK /states/{name}`

Acquires a lock on the named state, creating the state row on first use.
The request body is the JSON `LockInfo` Terraform sends:

```json
{
  "ID":        "lock-uuid",
  "Operation": "OperationTypeApply",
  "Info":      "",
  "Who":       "alice@laptop",
  "Version":   "1.13.4",
  "Created":   "2026-05-12T11:30:00.000Z",
  "Path":      "http://localhost:8080/states/example"
}
```

- **200** when the lock is acquired, empty body.
- **400** when the body is not valid JSON or `ID` is missing.
- **423 Locked** when the state is already locked; the response body is
  the existing `LockInfo` JSON so Terraform can show the holder.

### `UNLOCK /states/{name}`

Releases a held lock. Two shapes are accepted:

1. **Owner release.** Body is the same `LockInfo` JSON used by `LOCK`,
   with a non-empty `ID`. The lock is released only when `ID` matches
   the one currently held.

2. **Force release.** Body is empty, or JSON with an empty `ID`. The
   lock is released unconditionally and the operation is logged as
   `lock_force_release` in the audit trail. This branch exists because
   `terraform force-unlock` against an `http` backend transmits an
   empty body and never sends the user-supplied lock ID over the wire
   (see [Terraform's `httpClient.Unlock`][tf-unlock]: it sends
   `c.jsonLockInfo`, which is nil outside the process that acquired
   the lock).

[tf-unlock]: https://github.com/hashicorp/terraform/blob/v1.13.4/internal/backend/remote-state/http/client.go

- **200** when released, or on a force release against a state with no
  lock currently held (idempotent — `terraform force-unlock` is safe to
  re-run).
- **400** when the body is non-empty and not valid JSON.
- **409 Conflict** when an owner-release `ID` does not match the held
  lock.

## Authentication

### Multi-customer (hosted) — `KL_AUTH_MODE=database`

Each **customer** is a row in `tenants` (unique `slug`). Each **API token**
belongs to one tenant and is stored as a SHA-256 hash.

Create customers and tokens with the control API:

```sh
curl -sS -X POST "http://localhost:8090/api/tenants" \
  -H "Authorization: Bearer $KL_CONTROL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"slug":"acme","name":"Acme Corp"}'

curl -sS -X POST "http://localhost:8090/api/tenants/acme/tokens" \
  -H "Authorization: Bearer $KL_CONTROL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"terraform-ci","environment":"default"}'
# token_secret is returned once — save it
```

**Terraform `backend "http"`** (recommended):

```hcl
terraform {
  backend "http" {
    address        = "https://api.example.com/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
    lock_address   = "https://api.example.com/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
    unlock_address = "https://api.example.com/states/ws_ab12cd34ef56/env_12ab34cd56ef/prod"
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
    username       = "ws_ab12cd34ef56"   # workspace_id
    password       = "kl_…"             # environment token secret
  }
}
```

The server validates:

- `hash(password)` matches an active environment token
- `username` matches that token's `workspace_id`
- the state path starts with that token's `{workspace_id}/{env_public_id}/...`

Wrong pairing or wrong path → auth/path failure.

**Bearer** (no tenant in header): `Authorization: Bearer kl_…` — the
token alone identifies the tenant.

`/healthz` is always unauthenticated.

### Single-tenant (legacy) — `KL_AUTH_MODE=static`

One shared `KL_AUTH_TOKEN` for the built-in self-hosted singleton tenant.

### Open (development only) — `KL_AUTH_MODE=open`

No HTTP authentication.

### Isolation

Every store query filters by `tenant_id` from the authenticated
Principal. Two customers can each have a state named `prod`; they never
see each other's rows. In the workspace/environment-aware runtime path,
the backend address is:

```text
/states/{workspace_id}/{env_public_id}/{state_name}
```

## Audit trail

Every operation that mutates state writes a row to `events`:

| `events.kind` | Triggered by |
|---|---|
| `state_write` | `POST /states/{name}` |
| `state_delete` | `DELETE /states/{name}` |
| `lock_acquire` | `LOCK /states/{name}` |
| `lock_release` | `UNLOCK /states/{name}` with matching `ID` |
| `lock_force_release` | `UNLOCK /states/{name}` with empty body / empty `ID` (`terraform force-unlock`) |

Reads do not produce events in v0.

## Limits

| Knob | Default | Why |
|---|---|---|
| State body size | 512 MiB | Real-world large states have been observed in the 300 MiB range; the cap is set above that with comfortable headroom. |
| Lock body size | 64 KiB | Lock info is small; anything larger is suspicious. |
| `ReadHeaderTimeout` | 15 s | Guards against slow-loris clients. |
| Migrate-on-startup timeout | 30 s | Bounds initial schema work; relax if needed. |

## Deviations from the Terraform `http` backend

- **The HTTP method names for lock/unlock are fixed at `LOCK` / `UNLOCK`.**
  Terraform's `http` backend lets you configure other methods. v0
  Kilolock accepts only the defaults.
- **No `update_method` distinction.** Terraform supports configuring an
  alternate method (e.g. `PUT`) for state writes; v0 implements `POST`
  only, which is the default.
- **Response on `POST` is always 200 (or an error).** Some backends use
  201 on first write; v0 does not distinguish.
- **No retry-related headers (`Retry-After`).** Clients are expected to
  honor 423/409 by re-fetching the lock or retrying after backoff.
