# Execution-plane audit checklist

Use this runbook to audit the Terraform execution plane step by step.

The goal is not to “be clever” while testing. The goal is to answer, for each
scenario:

- what did we do?
- what did the backend do?
- was the result predictable?
- was the error message understandable?

If you are unsure whether something is “good enough”, mark it as:

- `pass`
- `fail`
- `unclear`

That is enough. We can interpret the results together afterward.

## Scope

This checklist focuses on:

- backend auth behavior
- state path validation
- PAT and automation-token behavior
- optimistic conflicts
- long-running apply/destroy checkpoint behavior
- quota enforcement behavior
- lifecycle-state write blocking
- operator-visible diagnostics

It does **not** try to verify hosted-product UX such as customer UI screens, billing checkout, or invitation flows.

## Prerequisites

- local stack is already running
- you have at least:
  - one personal workspace
  - one organization workspace
  - one environment with a state
- you can use both:
  - a normal environment token
  - a personal access token (PAT)

Helpful terminals to keep open:

```bash
docker compose logs -f kl
```

Optional DB inspection:

```bash
docker compose exec postgres-meta psql -U kl -d kl
```

## Record template

For each step, record:

- `result`: pass / fail / unclear
- `terraform output`: short summary
- `backend log`: short summary
- `notes`: anything surprising

## 1. Happy-path backend auth

### 1A. Automation token works

Use a normal environment token in Terraform HTTP backend config.

Expected:

- `terraform init` succeeds
- `terraform plan` can read state
- no auth error in Terraform

Mark: pass

- if it fails, capture the exact Terraform error
- note whether backend logs show `401`, `403`, or something else

### 1B. PAT works when environment grant exists

Use:

- `username = workspace_id`
- `password = PAT`

Only do this after granting PAT access to that environment through your control workflow.

Expected:

- `terraform init` succeeds
- `terraform plan` can read state
- backend logs identify the request as PAT-backed auth

If this fails, capture: pass

- exact backend path
- whether the workspace is personal or organization
- whether the grant was recorded as active

## 2. Negative auth behavior

### 2A. PAT without environment grant

In an organization workspace:

- revoke PAT access to the target environment
- retry `terraform init`

Expected:

- Terraform fails
- failure is clearly auth-related
- backend does **not** silently allow access

Desired outcome: pass

- the failure should be understandable, not mysterious

### 2B. Wrong workspace id

Use the right PAT or token, but intentionally set the wrong `workspace_id`.

Expected:

- request fails
- failure should be clearly scoped/path related

### 2C. Wrong environment id

Use the right credential, but intentionally set the wrong `env_public_id`.

Expected:

- request fails
- failure should be clearly scoped/path related

### 2D. Revoked PAT

- create PAT
- verify backend access works
- delete/revoke PAT
- retry backend access

Expected:

- backend access stops immediately
- control/API access remains unaffected

## 3. Membership-driven offboarding

Organization workspace only.

### 3A. Member leaves workspace

- grant PAT access to member
- verify member PAT works against one environment
- remove or let the member leave the workspace
- retry backend access with the same PAT

Expected:

- backend access stops
- no manual token rotation is needed for the human PAT path

### 3B. PAT delete cleans up grants

- create PAT
- grant environment access
- delete PAT
- reopen PAT access modal

Expected:

- member shows `No PAT created yet`
- grant count no longer includes that deleted PAT access

## 4. State-path semantics

Confirm the state path format used in backend snippets is:

```text
states/{workspace_id}/{env_public_id}/{state_name}
```

### 4A. Correct path works

Expected:

- normal backend behavior

### 4B. Environment slug instead of env_public_id

Intentionally try:

```text
states/{workspace_id}/{env_slug}/{state_name}
```

Expected:

- request fails
- failure is understandable

If the failure looks like generic “requires auth”, note that as a diagnostics gap.

## 5. Long-running apply checkpoint behavior

Use `examples/big-state`.

### 5A. Large apply

Run a large apply and observe:

- whether resource counts update only on checkpoints
- whether long runs complete without self-conflicting

Expected:

- Terraform may print many resource completions before visible count changes
- backend should not fail against its own earlier checkpoints in the same run

If it fails:

- save `errored.tfstate`
- capture backend logs around the failure

### 5B. Large destroy

Run a large destroy on a populated state.

Expected:

- destroy may checkpoint multiple times
- backend should not return `409` because of the same lock/run conflicting with itself

If it fails:

- record whether failure is `409`, `403`, or other
- capture the backend log snippet with `latest_serial` if present

## 6. Optimistic coexistence behavior

This section verifies the product promise around disjoint concurrent changes.

### 6A. Same state, disjoint resources

Use two independent Terraform configurations or operators against the same state
with disjoint resource addresses.

Expected:

- both runs can succeed under optimistic coexistence
- backend should not reject disjoint writes as conflicts

### 6B. Same state, overlapping resources

Run two operators against the same addresses.

Expected:

- one run succeeds
- one run fails with `409`
- response/log should include:
  - conflicting addresses
  - latest serial
  - actionable next step

Mark `unclear` if the behavior is correct but the message is too difficult to
act on quickly.

## 7. Quota behavior

Before testing final write rejection, also test the CLI-side preflight path:

- `kl quota remaining`
- `terraform plan -out=plan.tfplan`
- `kl quota check --tf-plan plan.tfplan`
- `kl plan ...`

Expected:

- `kl quota remaining` reports current headroom for state and environment
- `kl quota check` warns on soft-limit breaches
- `kl quota check` fails on hard-limit breaches
- `kl plan` mirrors the same hard-fail / soft-warn behavior when backend discovery works

### 7A. Exact boundary

Test a state at exactly the hard boundary for the current plan.

Expected:

- exact boundary should succeed
- only `>` hard limit should fail

### 7B. Temporary spike during apply

Run a replacement-heavy apply that may temporarily grow the state.

Expected:

- if it fails, backend should clearly say quota/entitlement was the reason
- operator should be able to distinguish this from DB or auth failure

Record whether the error message explains:

- resulting state count
- soft limit
- hard limit

### 7C. Environment aggregate quota

Use multiple states inside the same environment.

Expected:

- environment quota is scoped to that environment only
- archived states do not count toward active environment total

## 8. Lifecycle-state blocking

### 8A. Suspended workspace

Suspend a workspace through control or another lifecycle-management path.

Expected:

- backend writes fail
- failure clearly indicates a lifecycle hold

### 8B. Archived state

Archive one state and retry backend write.

Expected:

- backend write fails
- failure should clearly identify archived/inactive state

### 8C. Restored state

Restore the same state through control and retry.

Expected:

- backend access works again

## 9. Error-surface review

For every failure you hit during this audit, classify it:

- `auth`
- `scope/path`
- `quota`
- `conflict`
- `lifecycle`
- `environment unavailable`
- `unknown`

Then answer two questions:

### 9A. Was the HTTP behavior correct?

Examples:

- `401` for missing/invalid auth
- `403` for entitlement/lifecycle denial
- `409` for optimistic conflict

### 9B. Was the Terraform-visible message understandable?

Use this rubric:

- `clear`: I know what to do next
- `partial`: I know roughly what failed, but not the fix
- `poor`: message is too vague to act on

## 10. End-of-audit summary

At the end, summarize in four buckets:

### Working as intended

- scenarios that clearly passed

### Behavior correct, messaging weak

- backend did the right thing
- message/log/operator UX should improve

### Behavior surprising

- technically explainable
- still not what a Terraform user would expect

### Likely bugs

- reproducible incorrect behavior
- inconsistent state
- wrong status code
- missing cleanup

## Recommended artifacts to save

When something fails, save as much of this as practical:

- Terraform error output
- `errored.tfstate`
- backend log snippet
- relevant control/API evidence
- workspace id / environment id / state name
- whether you used:
  - automation token
  - PAT
  - personal workspace
  - organization workspace

## What “good enough” looks like

For this audit stream, “good enough” means:

- happy-path auth works for automation tokens and PATs
- revoked membership/PAT/grants stop access predictably
- disjoint concurrent writes can succeed
- overlapping writes fail predictably and explain what to do next
- overlapping writes fail with actionable conflict info
- long-running apply/destroy no longer self-conflicts
- quota failures are clearly recognizable as quota failures
- lifecycle blocking is clear and intentional

If that is true, we can be pretty confident the execution plane is in healthy
MVP territory.

## Current findings report

This section captures concrete observations from local execution-plane testing
as of June 11, 2026. Treat it as a living report, not a final guarantee.

### Confirmed working

- `kl apply --orchestrated` in parallel on different resources works as
  expected when using scoped execution (`--file` / `--target`).
- The scoped orchestrated path now behaves like a real reservation-aware apply
  flow rather than silently falling back to plain Terraform semantics.

### Confirmed expected failures

- Plain `terraform apply` in parallel against the same targeted resource fails
  on the losing writer with backend `409` and produces `errored.tfstate`.
- Plain `kl apply` without `--orchestrated` behaves the same way as
  Terraform in that same-resource parallel case.
- This is expected because the non-orchestrated path is still backend-scoped
  Terraform execution, not reservation-aware orchestration.

### Important product distinction

- The concurrency value of Kilolock lives in the backend/orchestrated model:
  scoped graph execution plus scoped coordination.
- This is different from whole-state backends such as S3-style state storage,
  where locking usually serializes the entire state object.

### Operator note

- If a reservation conflict happens during orchestrated execution, prefer
  `kl apply abort --apply-id <id>` over waiting for the full lease to
  expire when the blocked run is clearly stale or interrupted.

## Current product boundaries

### Queryable backend

- The current `kl query` command is an operator/database tool.
- It runs read-only SQL directly against the configured database.
- It is **not** a customer-safe, state-scoped query API over backend auth.
- Possession of database access is therefore the security boundary today, not
  workspace or environment membership.

### Rollback granularity

- The current `kl rollback` command operates at whole-state-version
  granularity.
- It replays a historical `raw_state` as a new current state version.
- It does **not** perform rollback at individual resource level.
- It also rewinds state bookkeeping only; it does not directly roll back cloud
  resources.
