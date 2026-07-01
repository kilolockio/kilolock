# State Engine Local Smoke

This runbook exercises the first OSS implementation slice of the
state-engine protocol against the quick local Docker Compose stack.

## Prerequisites

- quick local stack running:

```sh
docker compose up --build
```

- API available at `http://localhost:8080`

The quick stack uses open auth, so the examples below do not need credentials.

## 1. Confirm capabilities

```sh
curl -s http://localhost:8080/v1/state-engine/capabilities | jq
```

Expected highlights:

- `protocol = "state-engine"`
- `version = "v1"`
- `slice_fetch = true`
- `resource_reservations = true`
- `terraform_visible_native_lock = true`
- `delta_commit = true`
- `native_state_rm = true`
- `native_state_mv = true`
- `native_resource_rollback = true`

## 2. Seed a small state

Use any existing Terraform config against the local HTTP backend, or write a
small synthetic state through the standard backend API.

Once a state exists, the state-engine endpoints can resolve and slice it.

## 3. Resolve state metadata

```sh
curl -s -X POST http://localhost:8080/v1/state-engine/state/resolve \
  -H 'Content-Type: application/json' \
  -d '{"state":"example"}' | jq
```

Expected highlights:

- `state_id`
- `lineage`
- `serial`

## 4. Ask backend to expand scope

```sh
curl -s -X POST http://localhost:8080/v1/state-engine/scope/expand \
  -H 'Content-Type: application/json' \
  -d '{
    "state":"example",
    "selectors":[
      {"kind":"resource_address","value":"null_resource.x"}
    ],
    "client_context":{
      "explicit_write_candidates":["null_resource.x"],
      "explicit_read_candidates":[],
      "undeployed_config_candidates":[]
    }
  }' | jq
```

Expected highlights:

- `realized_write_closure`
- `realized_read_closure`
- `reservation_candidates`
- `confidence`

## 5. Fetch a slice

```sh
curl -s -X POST http://localhost:8080/v1/state-engine/state/slice \
  -H 'Content-Type: application/json' \
  -d '{
    "state":"example",
    "addresses":["null_resource.x"]
  }' | jq
```

Expected highlights:

- `serial`
- `slice.resources[]`
- `slice.resources[].attributes_hash`

## 6. Acquire Terraform-visible coarse lock

```sh
curl -s -X POST http://localhost:8080/v1/state-engine/terraform-lock/acquire \
  -H 'Content-Type: application/json' \
  -d '{
    "state":"example",
    "apply_id":"demo-1",
    "holder":"local-smoke",
    "scope_summary":["null_resource.x"]
  }' | jq
```

Expected highlights:

- `ok = true`
- `lock_id = "state-engine-demo-1"`

## 7. Verify plain Terraform lock is blocked

```sh
curl -i -X LOCK http://localhost:8080/v1/states/example \
  -H 'Content-Type: application/json' \
  -d '{
    "ID":"tf-lock-1",
    "Operation":"OperationTypeApply",
    "Info":"",
    "Who":"terraform-cli",
    "Version":"1.13.4",
    "Created":"2026-06-24T10:00:00Z",
    "Path":"http://localhost:8080/v1/states/example"
  }'
```

Expected:

- HTTP `423 Locked`
- body contains the state-engine sentinel lock metadata

## 8. Confirm force-unlock does not clear the state-engine lock

```sh
curl -i -X UNLOCK http://localhost:8080/v1/states/example
```

Expected:

- HTTP `200 OK`
- a subsequent `LOCK` attempt is still blocked while the state-engine lock is
  active

## 9. Release the coarse lock

```sh
curl -s -X POST http://localhost:8080/v1/state-engine/terraform-lock/release \
  -H 'Content-Type: application/json' \
  -d '{
    "state":"example",
    "apply_id":"demo-1",
    "actor":"local-smoke"
  }' | jq
```

## 10. Exercise native exact-address state operations

The quickest end-to-end path uses the existing big-state fixture:

```sh
cd examples/big-state
./state-engine-demo.sh
```

Expected highlights:

- native move preview/apply succeeds
- moved address is verified through `kl query resource`
- the resource is moved back to its canonical address
- native remove preview/apply succeeds
- native rollback preview/apply restores the removed address from prior serial
- the removed address is restored from prior history

## 11. Exercise native snapshot commit API directly

This uses the protocol commit endpoint rather than the older admin write-apply
shim.

```sh
curl -s -X POST http://localhost:8080/v1/state-engine/state/commit \
  -H 'Content-Type: application/json' \
  -d '{
    "state":"example",
    "apply_id":"demo-commit-1",
    "base_serial":1,
    "mode":"snapshot",
    "raw_state":"{\"version\":4,\"terraform_version\":\"1.13.4\",\"serial\":2,\"lineage\":\"9b39e2c0-1111-2222-3333-444455556666\",\"outputs\":{},\"resources\":[]}",
    "write_set":[]
  }' | jq
```

Expected highlights:

- `ok = true`
- `committed_serial` advances
- `new_version_id` is returned

## 12. Exercise native apply-run lifecycle directly

```sh
curl -s -X POST http://localhost:8080/v1/state-engine/apply-runs/begin \
  -H 'Content-Type: application/json' \
  -d '{
    "state":"example",
    "actor":"local-smoke",
    "source_serial":1
  }' | jq
```

Capture the returned `id`, then:

```sh
curl -s http://localhost:8080/v1/state-engine/apply-runs/APPLY_ID/status | jq

curl -s -X POST http://localhost:8080/v1/state-engine/apply-runs/APPLY_ID/finish \
  -H 'Content-Type: application/json' \
  -d '{
    "status":"committed",
    "committed_serial":2,
    "resources_planned":1,
    "resources_applied":1
  }' | jq
```

Expected highlights:

- begin returns an `id`
- status reports `running` before finish
- finish returns `ok = true`

## Current scope of the implementation

This OSS slice now implements:

- capabilities endpoint
- state resolve
- backend-assisted scope expansion
- slice fetch
- state-engine reservation endpoints
- state-engine apply-run lifecycle endpoints
- Terraform-visible coarse lock acquire/release
- state-engine snapshot commit endpoint
- native exact-address `kl state rm`
- native exact-address `kl state mv`
- native exact-address `kl rollback resource`
- native `kl apply` path that uses state-engine apply-run + reservations +
  coarse lock + snapshot commit

It does **not** yet implement:

- delta-mode wire commit
- a backend-executed apply engine (the current `kl apply` still runs Terraform locally, then commits through the state-engine protocol)
