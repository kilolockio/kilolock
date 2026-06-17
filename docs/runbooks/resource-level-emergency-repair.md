# Resource-level emergency repair runbook

This runbook covers the new backend-native resource inspection and
resource-level state repair flow.

Use it when:

- one specific Terraform resource address needs investigation
- a whole-state rollback would be too blunt
- you want to restore/remove/replace a single resource in state bookkeeping

This is an **emergency state repair** workflow.

It does:

- inspect current live resource rows through backend auth
- inspect per-resource history through backend auth
- write a new state version by replaying one exact historical resource address

It does **not**:

- directly roll back cloud resources
- guarantee the next `terraform plan` is empty
- replace normal Terraform workflows

## Prerequisites

- local stack is running
- the target state already exists
- you can authenticate either with:
  - environment automation token, or
  - PAT + environment grant

If you are using PAT auth in Terraform-style backend terms:

- `username = workspace_id`
- `password = PAT`

## 1. Inspect the current resource

Use the exact Terraform address:

```bash
kl query resource --address aws_instance.web
```

Expected:

- current state name resolves from backend config if omitted
- one live resource is returned
- attributes are shown from normalized backend state

Useful for:

- confirming you are targeting the right address
- seeing provider/type/module metadata
- confirming current attribute values

## 2. Inspect resource history

```bash
kl query history --address aws_instance.web
```

Expected:

- rows are ordered newest-first
- status column helps you reason about lifecycle:
  - `current`
  - `superseded`
  - `restored-current`
  - `restored-old`

Use this to identify the version/serial you want to replay from.

## 3. Preview resource repair

Always preview first:

```bash
kl rollback resource --address aws_instance.web --to @1
```

Expected preview includes:

- current version serial/id/source
- target version serial/id/source
- action:
  - `replace`
  - `restore`
  - `remove`
  - `no-op`
- dependency list from the historical/current instance
- dependents in current state
- warnings

Interpretation:

- `replace`
  - current address exists and target address exists
  - current state instance will be replaced with historical bookkeeping
- `restore`
  - current address is missing, target address exists
  - address will be restored into current state
- `remove`
  - current address exists, target address is absent
  - address will be removed from current state
- `no-op`
  - current and target resource content already match

## 4. Apply resource repair

If the preview looks correct:

```bash
kl rollback resource --address aws_instance.web --to @1 --apply
```

For non-interactive execution:

```bash
kl rollback resource --address aws_instance.web --to @1 --apply --yes
```

Expected:

- the command writes a **new current state version**
- it does not mutate prior history
- it does not directly touch cloud resources

After apply:

- run `terraform plan`
- inspect whether the cloud and HCL now diverge from repaired state

## 5. Recommended follow-up after repair

After a resource-level repair:

1. run `terraform plan`
2. decide whether HCL must be reverted too
3. decide whether cloud resources need manual/operator action
4. keep the new state serial/version id in the incident notes

## 6. Example scenarios

### Restore one accidentally removed resource from state

```bash
kl query history --address aws_instance.web
kl rollback resource --address aws_instance.web --to 42
kl rollback resource --address aws_instance.web --to 42 --apply
terraform plan
```

### Remove one bad resource from current state

Pick a target version where the address did not yet exist:

```bash
kl rollback resource --address aws_instance.web --to 7
```

If preview action is `remove`, apply only after confirming this is really
intended.

### Replace current resource bookkeeping with older known-good metadata

```bash
kl rollback resource --address aws_instance.web --to @2
```

Use this when the address still exists but the current state payload is wrong.

## 7. Warnings and limits

- exact-address only for now
- state repair only; cloud rollback is out of scope
- dependency warnings are advisory, not a full graph solver
- follow-up `terraform plan` is mandatory for safe operator judgment

## 8. Suggested manual validation checklist

- `query resource` returns the expected current address
- `query history` shows the expected serial progression
- dry-run preview action matches operator intent
- dependency/dependent hints are understandable
- apply writes one new current version
- unrelated resource addresses remain unchanged
- next `terraform plan` is explainable
