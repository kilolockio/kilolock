# Apply abort runbook

`kl apply abort` is the operator escape hatch for stuck or unwanted
applies. It marks a specific `apply_runs` row as `aborted` and releases any
`resource_reservations` held by that apply id.

This is intentionally explicit: you must target the apply you want to abort.

## When to use it

- Terraform/provider is hung (network stall, API timeout, provider deadlock).
- A CI job running `kl apply` was killed and left reservations behind.
- You started an apply with the wrong targets/vars and want to unblock others.

## Identify the apply run

Use `kl status <state>` to find in-flight applies and their ids.

## Abort by apply id (preferred)

```bash
kl apply abort --apply-id <uuid> --reason "operator abort"
```

## Abort the most recent running apply for a state

Convenience mode when you don’t have the id handy:

```bash
kl apply abort --state <state-name> --latest
```

If multiple people run applies on the same state, filter by actor:

```bash
kl apply abort --state <state-name> --latest --actor "alice@ci"
```

## What abort does (and does not) do

Does:
- sets `apply_runs.status = aborted` (+ stamps `finished_at`)
- releases reservations for that `apply_id`
- prevents the orchestrator from committing if it observes the abort

Does not:
- guarantee that a separate already-running `terraform` process stops instantly
  (it is canceled via context and should terminate, but provider hangs can still
  delay shutdown)

