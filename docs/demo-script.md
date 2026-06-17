# Kilolock 8-minute demo script

The narrative is one sentence: *parallel apply on shared state, with
history, rollback, and zero lock-in.* Everything below supports that
sentence; cut whatever doesn't.

Target length: **8 minutes**. Each section's headline number is the
approximate elapsed-time mark. Times include narration, not just
the command. The commands themselves are short by design — every
command on screen should be a one-liner an operator can read at a
glance.

The script is written to be recorded with `asciinema`. Suggested
record command:

```bash
asciinema rec \
  --title 'Kilolock: parallel apply, history, rollback' \
  --idle-time-limit 2 \
  --command 'bash' \
  kl-demo.cast
```

Output that gets too noisy for a recording (Terraform's progress
spam, in particular) is intentionally not narrated below. Pipe
through `grep -E '^(Plan|Apply complete|Error)'` when desired.

## 0:00 — Setup (do this BEFORE you hit record)

1. `make demo-warm` to restore the pre-baked `big-state` fixture
   (10k resources, ~5 seconds).
2. `make demo-status` to confirm it's there.
3. Open two terminals side-by-side. Set `KL_DATABASE_URL`
   in both, plus `KL_LOG_LEVEL=warn` so the recording
   isn't drowned in info logs.
4. `cd examples/big-state` in both.
5. Have `bin/kl` in `$PATH` (or use `make build` and
   `./bin/kl`).

The recording starts AFTER the warm restore. The audience should
not see a 30-second pg_restore.

## 0:00 — The problem (45s)

Narrate over an empty terminal:

> "This is what a Terraform module with 10,000 resources looks
> like in our state. Vanilla `terraform apply` on this state
> takes about 45 minutes — even though I only want to change two
> of those resources. And while it's running, *no other engineer
> on my team can run apply* on this module, because Terraform
> holds a whole-state lock for the entire duration."

Run, in one terminal only:

```bash
cat examples/big-state/main.tf | head -30
```

Just enough to show the `time_sleep.slow_a` / `slow_b` resources.

## 0:45 — Parallel apply, the elevator pitch (2:00)

The headline beat: **plain `terraform apply` runs concurrently on
the same state**. No kl CLI involvement on the operator
side — the backend's optimistic commit-time merge is what makes
the two POSTs succeed.

Before recording, make sure the demo state is in optimistic lock mode:

```bash
curl -sS -X POST "http://localhost:8090/api/states/operator/default/config" \
  -H "Authorization: Bearer $KL_CONTROL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"state":"big-state","exclusive_locks":false}'
```

Run, simultaneously (start them within ~2 seconds of each other
so they overlap; each `time_sleep` is 30s, which is the visible
overlap window):

```bash
# Terminal 1
make parallel-tf-a

# Terminal 2
make parallel-tf-b
```

Each is shorthand for:

```bash
terraform apply -auto-approve -refresh=false \
    -var=slow_a_version=$NEW -var=slow_b_version=$CURRENT
```

There is **no `-target=` flag**: each terminal runs from its
own working directory (the make targets prepare
`/tmp/parallel-tf-{a,b}/` on first invocation) and passes
both vars — its own as the new value, the other pinned to
the trunk value observed at startup. Terraform's plan in each
directory naturally produces a single-resource write set, and
the backend's optimistic merge stitches the two POSTs into
one new state version.

Quick FAQ if someone in the audience asks "why two
directories":

> "In a real workflow, two engineers have two checkouts of
> the module on different branches; the OS naturally gives
> each one its own `.terraform/` cache. We're mimicking that
> here so the two `terraform` processes don't fight over
> local state files in a single directory. The shared piece
> — and the whole point of this demo — is the backend
> state: both terminals POST to the same `big-state` and
> the backend merges them."

While the applies run, narrate:

> "Two engineers, same state, different resources. Both running
> vanilla `terraform apply` — no kl CLI, no plan-spec
> file, no reservations. Kilolock's backend lets multiple
> operators hold the HTTP-backend lock at the same time, then
> arbitrates at commit time: if the two write sets are disjoint,
> both POSTs merge and both commits succeed. If they touch the
> same address, the second one gets a 409 with the exact address
> in conflict. Vanilla Terraform on this same 10,000-resource
> state with the default S3 backend would have made me wait for
> my colleague's whole-state lock to release."

Both finish in ~30s total wall clock (vs ~60s serialized). Show
the elapsed times.

If the audience asks how the backend knew which addresses each
operator wrote, point at:

```bash
kl history big-state --limit=5
```

> "Each lock records the trunk serial the operator read at lock
> time. At POST the backend diffs `state at source_serial` vs
> the incoming state — that's the write set. If that intersects
> the diff of `state at source_serial` vs current trunk, conflict.
> Otherwise we 3-way-merge and commit."

### Advanced beat — explicit reservations (optional, swap out
### the plain-terraform beat above with this if the audience is
### infra-platform-engineer-y and cares about the wait UI)

```bash
# Terminal 1
make parallel-demo-a

# Terminal 2
make parallel-demo-b
```

Same disjoint-write-set outcome, but goes through `kl
apply` and the pessimistic reservation matrix instead of the
optimistic POST merge. The reservation matrix is what makes the
wait-feedback demo (bonus beat below) work.

## 2:45 — History (1:00)

```bash
kl history big-state --limit=5
```

Point out:
- The `*` marker on the latest version.
- Two new versions appeared, source=`apply`, different actors.
- The serial monotonically increased; no serial conflict.

> "Every write goes through the same append-only history. Nothing
> ever gets overwritten. Every state version is recoverable, by
> any operator, at any time."

## 3:45 — Rollback (2:00)

```bash
kl rollback big-state --to=@2
```

Pause on the output. Walk through it:

- "It's a dry run by default. It hasn't changed anything yet."
- "It tells me which addresses would be added back, removed,
  or have their attributes change."
- "Most importantly: it warns me that rolling back the *state*
  is not the same as rolling back the *infrastructure*. If I
  rolled this back without reverting the HCL, I'd orphan
  resources in the cloud."

Now actually do it:

```bash
kl rollback big-state --to=@2 --apply --yes
```

Run `history` again to show the new `source=rollback` row at the
top.

```bash
kl history big-state --limit=3
```

> "The rollback is itself just another append. The old current
> version is still in history. Nothing destructive."

## 5:45 — Coexistence with vanilla Terraform (1:30)

This is the "no lock-in" beat. Critical for the trust story.

```bash
# Show the backend config — it's a plain HTTP backend.
cat examples/big-state/backend.tf
```

```bash
# A vanilla terraform plan still works.
terraform plan -refresh=false | tail -5
```

> "Kilolock exposes Terraform's standard HTTP backend protocol.
> Vanilla `terraform plan` and `terraform apply` work against it
> with zero changes. You're not locked in — the day you decide
> Kilolock isn't for you, point the backend back at S3 with
> `terraform init -migrate-state` and you're out. Your state is
> still vanilla Terraform JSON, exactly the way Terraform wrote
> it."

## 7:15 — Close (45s)

> "What you saw: parallel apply on shared state, append-only
> history, dry-run rollback, full compatibility with vanilla
> Terraform. Built on Postgres so you can SQL-query your
> infrastructure the same way you query everything else.
> Open source. Self-hosted today. Hosted offering coming.
> Try it: `git clone github.com/davesade/kilolock && make
> db-up && make build && make demo-warm`."

## Bonus beat — reservation wait feedback (optional, ~60 s)

If the parallel-apply demo lands and you have time, follow it
with the wait-feedback variant: two terminals, both bumping the
SAME address. The second terminal will not fail — it will sit in
a wait loop and stream a live "blocked by 1 reservation(s)" block
until the first one commits, then proceed.

```bash
# Terminal 1 (the blocker):
make wait-demo-blocker

# Terminal 2 (the waiter), within ~5 seconds:
make wait-demo-waiter
```

What the audience sees on terminal 2:

```text
[apply: waiting 4s/2m] blocked by 1 reservation(s) on "big-state":
  time_sleep.slow_a  write  held by davidkubec@cli (apply 7c3a91…, lease 14m23s)
  retrying in 2s
```

The block refreshes every ~5 s (and immediately whenever the
conflict set CHANGES — e.g. when the blocker releases). Once the
blocker commits, terminal 2 logs "reservation conflict cleared"
and runs its own apply.

If you want the wait visible in a single terminal (e.g. for an
asciinema cut), use `make wait-demo` instead — it backgrounds the
blocker to `/tmp/wait-demo-blocker.log` and foregrounds the
waiter so the wait block streams live.

The same `--wait-timeout` flag accepts `0` for fail-fast (CI's
preference). Mention this if the audience asks "what about
non-interactive runs".

## Recovery moves

If parallel apply demos hang or conflict (state drift, leaked
locks, etc.):

```bash
# Drop the v1 HTTP-backend lock if a previous terraform run
# leaked it.
psql "$KL_DATABASE_URL" \
  -c "DELETE FROM state_locks WHERE state_id = (SELECT id FROM states WHERE name = 'big-state');"

# Drop any leaked reservations / apply runs.
psql "$KL_DATABASE_URL" \
  -c "DELETE FROM resource_reservations rr USING apply_runs ar
      WHERE rr.apply_run_id = ar.id AND ar.state_id = (SELECT id FROM states WHERE name = 'big-state');"
psql "$KL_DATABASE_URL" \
  -c "DELETE FROM apply_runs WHERE state_id = (SELECT id FROM states WHERE name = 'big-state');"
```

If `big-state` itself gets wiped, restore with `make demo-warm`.
The integration test suite preserves it by default
(`KL_TEST_PROTECT_STATES=big-state`) but a manual
`db-reset` or `DROP SCHEMA` will still take it out.

## Cuts (if you're over time)

In priority order — cut from the top:

1. The `cat backend.tf` and `terraform plan` segment (saves 90s).
   Move the lock-in promise into the close.
2. The history walk (saves 60s). Move it into a single "and
   yes there's history too" beat between parallel apply and
   rollback.
3. The setup narration (saves 45s). Walk in mid-state.

## Cuts you cannot make

The orphan warning in the rollback dry-run. That single
sentence is the most-likely-misused feature of the whole
project surface; if you cut it, somebody will deploy rollback
to prod without reading the manual.
