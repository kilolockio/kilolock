# `big-state` — Kilolock at scale

A Terraform configuration sized to make the Kilolock value proposition concrete: tens to hundreds of thousands of resources, a state file in the megabytes, and queries that still answer in milliseconds.

Everything here uses **credential-free providers** (`random`, `null`). No cloud account, no IAM, no network calls past the initial provider download.

---

## What it produces

| Variable | Default | Resources | Edges | State size (raw JSON) |
|---|---|---|---|---|
| `size = 100` | — | 208 | 415 | ~100 KB |
| `size = 1000` | — | 2,006 | ~4,100 | ~1 MB |
| `size = 5000` | — | 10,006 | ~20,500 | ~5 MB |
| `size = 50000` | **default** | 100,006 | ~205,000 | ~50 MB |

Topology: one root deployment (random_pet + random_id + null_resource + summary), one `primary_herd` module with `var.size` (tag, label) pairs anchored by a leader, and one `shadow_herd` instance sized 1 that depends on the primary herd's leader. Resource count is **linear** in `size` (the herd module is structured to avoid quadratic dependency-edge blowup — see the comment at the top of `modules/herd/main.tf`).

## Expected apply times

Measured on Apple M1 / Colima Postgres, single-threaded apply:

| `size` | Apply time | Notes |
|---|---|---|
| `100` | ~1 s | Sanity check; always run this first. |
| `1000` | ~10–15 s | Comfortable for iterating on queries. |
| `5000` | ~1.5 min | Representative of "big" production states. |
| `50000` | ~10–15 min | The "ouch, that's why we built Kilolock" experience. |

Apply time is **almost entirely Terraform itself** — graph build, per-resource provider RPCs, state serialization. The Kilolock backend handles each POST quickly; the bottleneck is Terraform's own per-resource overhead.

---

## Prerequisites

- `kld` reachable on `http://localhost:8080` (the default local compose does this out of the box).
- `terraform init` run in this directory so the CLI can discover the backend/runtime endpoint automatically.

## Backend modes

This example keeps the quick local backend active in:

- `backend.tf`

That file is meant for the default OSS quick-start stack: one runtime on
`http://localhost:8080`, open auth, and no separate control plane.

The backend method shape used in this repo is intentionally the same one we
recommend for hosted/cloud deployments:

- state reads/writes: `GET` / `POST` on `/v1/states/...`
- lock acquire: `LOCK` on `/v1/states/...`
- lock release: `POST` on `/v1/state-unlock/...`

This means the default OSS `docker-compose` flow can stay exactly as it is.
You do **not** need one backend block for local compose and another one for
cloud just because of unlock behavior.

If you want to run `big-state` against the prod-like compose instead:

1. Copy the prod-like sample over `backend.tf`:

```sh
cp ../local-backend/backend.tf.prodlike ./backend.tf
```

2. Edit the copied `backend.tf` with your real:
   - `workspace_id`
   - `env_public_id`
   - token secret

3. Re-initialize Terraform:

```sh
rm -rf .terraform .terraform.lock.hcl
terraform init
```

Only one backend block may be active at a time, so avoid keeping two backend
`.tf` files enabled in this directory.

### Option A: local Postgres + local `kld`

One-shot setup from the repo root:

```sh
make db-up
make build
DB='postgres://kl:kl@localhost:5432/kl?sslmode=disable'
KL_DATABASE_URL=$DB ./bin/kld migrate
KL_DATABASE_URL=$DB ./bin/kld serve &
```

### Option B: Docker Compose (default OSS path)

From the repo root:

```sh
cp .env.example .env
docker-compose up -d --build
```

This is the same default stack described in the top-level README: one Postgres,
open auth, runtime on `http://localhost:8080`, and no separate control service.
After `terraform init`, the `kl` CLI discovers that backend/runtime
endpoint from this directory automatically.

### Option C: prod-like compose + control plane

From the repo root:

```sh
cp .env.example .env
docker-compose -f docker-compose.prodlike.yml up -d --build
docker-compose -f docker-compose.prodlike.yml exec klc klc migrate
docker-compose -f docker-compose.prodlike.yml exec klc klc init \
  --tenant self-hosted \
  --tenant-name "Self Hosted" \
  --token-name operator-bootstrap
```

Then:

1. Copy the bootstrap token printed by `init`.
2. Open `http://localhost:8090/portal`.
3. Paste the token into the auth box.
4. In **Create Workspace**, enter an optional human-friendly label/name, then create the workspace.
5. In **Workspaces**, copy the real `workspace_id` (`ws_...`).
6. In **Create Environment**, paste that `workspace_id`, choose an environment label such as `prod`, then create the environment.
7. In **Environments by Workspace**, load that workspace and copy the environment's `env_public_id` (`env_...`).
8. In **Create Token**, paste:
   - the same `workspace_id`
   - the environment `env_public_id`
   - a token name such as `terraform`
9. Create the token and copy the raw secret shown once (`kl_...`).
10. Copy `examples/local-backend/backend.tf.prodlike` over `backend.tf`.
11. Fill in:
   - `username = "ws_..."`
   - `password = "kl_..."`
   - backend path `.../v1/states/{workspace_id}/{env_public_id}/big-state`
12. Run:

```sh
rm -rf .terraform .terraform.lock.hcl
terraform init
```

The demo scripts in this directory will then follow the active backend
configuration automatically.

## Lock/unlock methods

Why does this example use:

```hcl
lock_method    = "LOCK"
unlock_method  = "POST"
unlock_address = ".../v1/state-unlock/..."
```

Because some managed edges and load balancers are happy to pass through
nonstandard `LOCK` but reject `UNLOCK` requests with a body before they ever
reach Kilolock. The dedicated `POST /v1/state-unlock/...` route avoids that issue
while keeping the backend behavior the same.

Today Kilolock does **not** expose a matching `POST /state-lock/...` alias, so
do not change `lock_method` to `POST` unless we implement that route in the
backend first.

## Run the demo

From this directory:

```sh
# 1. Sanity-check the demo at small scale (~1 second).
terraform init
terraform apply -auto-approve -var=size=100

# 2. Step up.
terraform apply -auto-approve -var=size=1000

# 3. The "real" demo (~10–15 minutes).
terraform apply -auto-approve   # uses the default size=50000

# 4. Tear down whenever (also slow at high sizes).
terraform destroy -auto-approve -var=size=50000
```

## Quota-aware workflow

When this demo points at a hosted or entitlement-limited deployment, prefer:

```sh
./bin/kl quota remaining
terraform plan -out=plan.tfplan
./bin/kl quota check --tf-plan plan.tfplan
./bin/kl plan -f slow_a.tf -o slow-a.plan.json
```

What to expect:

- `kl quota remaining` shows current headroom for the active state and its environment
- `kl quota check` uses Terraform's own plan output and asks the backend whether the projected resource count still fits
- `kl plan` performs the same style of quota preflight automatically when it can discover the HTTP backend state
- soft-limit overages warn but still exit `0`
- hard-limit overages fail before `kl apply`

This is intentionally stronger than plain `terraform apply` against the HTTP
backend alone. Plain Terraform can still create infrastructure and only learn
about quota rejection when the final state write is refused; Kilolock CLI
preflight exists to catch that earlier.

To point at a different `kld` instance, override at init time:

```sh
terraform init -reconfigure \
  -backend-config="address=http://kl.example.internal/v1/states/big-state" \
  -backend-config="lock_address=http://kl.example.internal/v1/states/big-state" \
  -backend-config="unlock_address=http://kl.example.internal/v1/state-unlock/big-state"
```

---

## Big-state demos

This directory intentionally carries exactly three supported public demos:

- `./wait-demo.sh` for same-resource contention and visible waiting
- `./parallel-demo.sh` for different-resource parallelism on one shared state
- `./drift-demo.sh` for refresh-style drift detection on the same large-state fixture

Everything else in this directory exists only to support those demos or the Terraform fixture itself.

`time_sleep.slow_a` and `time_sleep.slow_b` are the two root-scope slow
resources that make the collaboration story tangible. Each one sleeps for
~30 seconds during apply and each one is driven by its own version variable:

- `slow_a_version` only replaces `time_sleep.slow_a`
- `slow_b_version` only replaces `time_sleep.slow_b`

That gives us two collaboration demos plus one drift demo. The collaboration scripts use narrow file-scoped plans under the hood so the demo spends its time showing reservation behavior, not replanning the entire state.

If you are deciding which demo to run, use this quick guide:

| Demo | Script | Purpose | What to watch for |
|---|---|---|---|
| Same-resource contention | `./wait-demo.sh blocker` + `./wait-demo.sh waiter` | Show that Kilolock does not let two engineers mutate the same branch blindly. | The second apply waits on the reservation and then proceeds after the first commit. |
| Different-resource parallelism | `./parallel-demo.sh slow_a` + `./parallel-demo.sh slow_b` | Show that two engineers can keep moving on one large shared state when their write sets are disjoint. | Total wall-clock stays close to one 30-second apply, not two serial ones. |
| Drift detection | `./drift-demo.sh` | Show that refresh-discovered drift becomes a cheap backend query, not a custom state-file diff workflow. | `current_resource_drift` immediately shows the changed resource and the before/after note value. |

### 1. Same resource → visible waiting

Use `wait-demo.sh` when you want to show what happens when two engineers try
to change the **same** resource.

```sh
# Terminal 1                      # Terminal 2
./wait-demo.sh blocker           ./wait-demo.sh waiter
```

Or single-terminal:

```sh
./wait-demo.sh both
```

What this demonstrates:

- terminal 1 acquires the orchestrated reservation on `time_sleep.slow_a`
- terminal 2 detects the orchestrated conflict on the same address
- terminal 2 prints the wait-status block every few seconds
- once terminal 1 commits, terminal 2 proceeds

This is the “same branch of state” story. Internally the script scopes planning to `slow_a.tf`, applies with explicit `--orchestrated`, and if the blocker advanced trunk in the meantime the waiter automatically replans once before retrying. That keeps the demo aligned with the real stale-plan safety model rather than whole-state backend locking.

### 2. Different resources → parallel apply saves time

Use `parallel-demo.sh` when you want to show what happens when two engineers
change **different** resources in the same state.

```sh
# Terminal 1                      # Terminal 2
./parallel-demo.sh slow_a       ./parallel-demo.sh slow_b
```

Or single-terminal:

```sh
./parallel-demo.sh both
```

What this demonstrates:

- one engineer changes `time_sleep.slow_a`
- the other changes `time_sleep.slow_b`
- the write sets are disjoint
- both orchestrated applies run at the same time
- total wall-clock stays close to one 30-second sleep instead of two

This is the “we reserve only the affected branch, not the whole state” story. Internally each side plans only its own file (`slow_a.tf` or `slow_b.tf`) and then applies with explicit `--orchestrated`, which keeps the demo honest and snappy even when the rest of the state is large.

### 3. Drift detection → inspect one changed resource immediately

Use `drift-demo.sh` when you want to show that once refresh-discovered drift
has been written, answering *"what drifted?"* is a cheap backend query instead
of a custom state-file diff.

```sh
./drift-demo.sh
```

What this demonstrates:

- the demo simulates out-of-band drift on `null_resource.summary`
- it records that drift through `source='refresh'`
- `current_resource_drift` answers the question directly
- the script restores the canonical state afterwards unless `KEEP_DRIFT=1`

This keeps the drift story in the same large-state example as the parallel
collaboration demos.

### One-time bootstrap on a fresh state

Before running the collaboration demos, make sure the state reflects the
**default values currently committed in this directory**. The simplest safe
bootstrap is to apply the example once with its defaults:

```sh
terraform apply -refresh=false
```

That seeds the canonical trunk shape the demo scripts expect.

If you want the smallest possible bootstrap for just the two slow demo
resources, run:

```sh
terraform apply -refresh=false \
  -target=time_sleep.slow_a -target=time_sleep.slow_b \
  -var=slow_a_version=v1 -var=slow_b_version=v1
```

Use the targeted bootstrap only if those values still match the defaults in the
checked-in HCL. If you changed the code locally, prefer the plain default apply
above so the demo starts from exactly what is in the files.

### One-time cleanup after old demo drift

If you previously experimented with extra slow resources and one of the demos
complains about stale trunk state, reconcile once with:

```sh
terraform apply -refresh=false -var=size=400
```

That brings the state back to the canonical demo shape.

## What to look at while it's applying

Even mid-apply, the backend is queryable. The state-versions table records every POST, so you can watch the import progress in real time from another terminal:

```sh
watch -n 1 'kl query "SELECT serial, terraform_version, created_at FROM state_versions sv JOIN states s ON s.id=sv.state_id WHERE s.name='\''big-state'\'' ORDER BY serial DESC LIMIT 5"'
```

## Queries to demonstrate the value

Measured below against a ~6,000-resource state (partial apply of `size=5000`):

### Inventory by type — **22 ms**

```sh
kl query "SELECT type, count(*)::int AS instances
                  FROM resources r JOIN states s ON s.current_version_id = r.state_version_id
                  WHERE s.name='big-state'
                  GROUP BY type ORDER BY instances DESC"
```

```
type           instances
random_id      3132
random_string  2827
null_resource  1
random_pet     1
```

### Inventory by module — **22 ms**

```sh
kl query "SELECT COALESCE(NULLIF(module_path,''), '(root)') AS module,
                         count(*)::int AS resources
                  FROM resources r JOIN states s ON s.current_version_id = r.state_version_id
                  WHERE s.name='big-state'
                  GROUP BY module ORDER BY resources DESC"
```

### Transitive blast radius from the herd leader — **133 ms**

```sh
kl query "WITH RECURSIVE reach AS (
    SELECT r.id, r.address, 0 AS depth
    FROM   resources r JOIN states s ON s.current_version_id = r.state_version_id
    WHERE  s.name='big-state' AND r.address = 'module.primary_herd.random_id.leader'
    UNION
    SELECT r.id, r.address, reach.depth + 1
    FROM   reach
    JOIN   resource_dependencies rd ON rd.to_resource_id = reach.id
    JOIN   resources r ON r.id = rd.from_resource_id
  )
  SELECT count(*)::int AS reachable_resources FROM reach"
```

A recursive descent that reaches every resource transitively depending on a single anchor — on 6k nodes — in 133 ms. The flat `.tfstate` equivalent is "parse the whole file into memory, build an in-memory graph, BFS, count." Trivially correct on small states; intolerable on big ones.

### State size on disk

```sh
kl query "SELECT
  pg_size_pretty(octet_length(sv.raw_state::text)::bigint) AS raw_state_size,
  pg_size_pretty(pg_total_relation_size('resources'))      AS resources_table,
  pg_size_pretty(pg_total_relation_size('resource_dependencies')) AS deps_table
  FROM states s JOIN state_versions sv ON sv.id = s.current_version_id
  WHERE s.name='big-state'"
```

The normalized representation is competitive with the raw JSON in size, but indexed: arbitrary `WHERE address LIKE 'module.primary_herd.random_string.%'`-style queries are sub-millisecond instead of streaming gigabytes through `jq`.

---

## Compare to the flat-state baseline

Two equivalents you might reach for in a vanilla Terraform setup, both for a "show me every resource of type X" query:

| Approach | What it does | Cost at 100k resources |
|---|---|---|
| `terraform state list \| grep 'random_id\.'` | Acquires the workspace lock, downloads the full state, deserializes it, prints every resource address, then greps. | Seconds to minutes; serializes against any concurrent operation. |
| `jq '.resources[] \| select(.type=="random_id")' terraform.tfstate` | Streams the entire JSON blob through `jq`. | Hundreds of MB through CPU; many seconds. |
| `kl query "SELECT … WHERE type='random_id'"` | One indexed SQL query against the running database. | Tens of milliseconds; no lock. |

This is the headline. Inventory, dependency, and compliance questions stop being "schedule a job to dump state to S3 every 6 hours and ingest into something else" and start being one-line queries against live data.

---

## Known quirks

- **`terraform destroy` at large `size` takes about as long as the corresponding `apply`.** Same per-resource overhead in reverse. If you abort an apply mid-flight and need a clean slate, `TRUNCATE` on the schema is faster than `destroy`.
- **Apply at `size=50000` will use several GB of memory in Terraform itself**, mostly for the in-process graph. Kilolock's server-side footprint is modest by comparison.
- **If an apply is killed mid-flight, the lock survives.** Use `terraform force-unlock <id>` (works as of v0); the lock id is visible via `kl query "SELECT lock_id, who, created FROM state_locks"`.

## Reference: the topology

```
random_pet.deployment_name              ← deployment identifier (root)
  └── random_id.deployment_id           ← depends on deployment_name
        └── null_resource.deployment_marker

module "primary_herd"  (size = var.size)
  └── random_id.leader                  ← anchor for the herd
        ├── random_id.tag[count=size]   ← fan-out (size edges)
        └── random_string.label[count=size]  ← fan-out (size edges)

module "shadow_herd"   (size = 1)
  └── random_id.leader                  ← seeded from primary_herd.leader

null_resource.summary                   ← references both herds
```
