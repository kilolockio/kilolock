# ADR 0023: Human PATs, environment grants, and automation tokens

- **Status:** Proposed
- **Date:** 2026-06-08
- **Decision makers:** Kilolock maintainers
- **Relates to:** [ADR 0016](./0016-operator-rbac-and-delegation.md), [ADR 0022](./0022-operator-bootstrap-token-vs-data-encryption-keys.md)

## Context

Today customer backend access is primarily environment-token based.

That works for Terraform/OpenTofu automation, but it has a weak offboarding
story for humans:

- a workspace member can copy a long-lived backend token while still in the
  organization
- later, that person may be removed from the workspace or leave voluntarily
- the copied token still works until it is explicitly disabled or rotated

This is not a bug in the current model; it is the normal behavior of a shared
secret. But it is not the ideal long-term customer security model for humans.

At the same time, the product direction now clearly distinguishes:

- **user identities and memberships**
- **workspace roles**
- **operator/control roles**

This creates a natural next step:

- humans should authenticate to backend access as **themselves**
- automation should authenticate with **dedicated service credentials**

## Decision

Kilolock should evolve toward a two-track customer access model:

1. **Human Personal Access Tokens (PATs)**
   - issued to user accounts
   - authenticate a human principal
   - authorized dynamically from current membership and grants

2. **Automation tokens**
   - issued for non-human use
   - intended for CI/CD, agents, and service integrations
   - scoped explicitly and treated as service credentials

The authorization boundary for humans should move to the **environment** level,
not the state level.

This means:

- a human with access to an environment may access all states in that
  environment
- current membership and environment grants are checked at request time
- offboarding works automatically when membership or grant is removed

## Rationale

### Human access and automation access have different security needs

Human usage wants:

- automatic offboarding
- identity-bound audit trails
- simple rotation
- no dependence on copied shared workspace secrets

Automation usage wants:

- stable non-human credentials
- integration-friendly rotation workflows
- explicit environment/service scoping

Trying to satisfy both with one token type makes both stories worse.

### Membership-driven access is the right offboarding primitive

For humans, the real authority should be:

- who the person is
- which workspace memberships they currently hold
- which environments they are currently allowed to access

If that is checked dynamically, then:

- leaving a workspace removes access automatically
- removing a member removes access automatically
- role/grant changes take effect immediately

That is a much stronger offboarding model than “remember to rotate every copied
token.”

### Environment is the right granularity for MVP+ authorization

State-level authorization is possible, but it is more complex than the current
product needs.

Environment-level access is a good balance:

- simpler than per-state policy
- aligns with the existing environment model
- aligns with the likely future backend-side state migration direction
- preserves room for later state-level or resource-level restrictions if needed

## Target model

### D1. Human PATs

Human PATs are:

- issued to a user account
- stored hashed
- used for backend/API authentication by humans
- not shared between users

Recommended operational rule:

- one active PAT per user
- optionally allow one temporary replacement PAT during rotation overlap

That gives a simple mental model while still supporting safe rotation.

### D2. Human authorization after PAT authentication

Once a PAT authenticates a human principal, authorization is derived from:

- current user account status
- current workspace membership
- current role
- current environment grant

If any of those disappear, backend access disappears too.

This is the key offboarding property.

### D3. Automation tokens

Current environment/backend tokens should evolve conceptually into:

- **automation tokens**
- **service-account-like credentials**

They remain appropriate for:

- Terraform/OpenTofu automation
- CI/CD runners
- machine-to-machine integrations

They should not be treated as the long-term default for human access.

### D4. Environment grants for humans

A human backend principal should be granted access per environment.

MVP-friendly rule:

- grant to environment
- all states inside that environment become accessible

No per-state authorization is required in the first cut.

### D5. Portal and backend access stay separate

Rotating or revoking a PAT should affect:

- backend/API access using that PAT

It should **not** automatically affect:

- user login via password/OIDC/session
- workspace membership itself

Portal login is identity access.
PAT is backend access.

## Rotation policy

### R1. Human PAT rotation

Human PAT rotation should be:

- user-initiated
- explicit
- low-friction

Preferred model:

1. create replacement PAT
2. update local CLI/backend configuration
3. revoke old PAT

### R2. Automation token rotation

Automation token rotation should support:

- expiry metadata
- warning / reminder windows
- overlap rotation

But Kilolock should **not** blindly auto-rotate automation secrets by default
unless the delivery/update path is also controlled.

Important principle:

- never auto-rotate a secret unless the consumer can reliably receive and adopt
  the replacement

### R3. Weekly forced auto-rotation is not the default

A weekly auto-rotation policy sounds attractive, but for automation it can
break workloads if the secret sink is unmanaged.

So the preferred order is:

1. manual rotation with overlap
2. scheduled expiry reminders
3. provider/integration-backed rotation only when delivery is solved

## External secret-manager integration

Vault or secret-manager integrations may be valuable, but they solve a
different layer.

They can help with:

- secure storage of automation tokens
- delivery of rotated secrets
- sink/injection workflows for CI and runtime platforms

They do **not** replace the need for:

- human-vs-automation credential separation
- environment-grant authorization
- membership-driven offboarding

So Vault integration is complementary, not a substitute for this model.

## Consequences

### Positive

- stronger human offboarding story
- cleaner audit trail for backend actions by person
- clearer separation between human and automation credentials
- better long-term security posture for customer organizations

### Tradeoffs

- more schema and auth-model complexity
- another credential type to manage
- migration path needed from current shared backend tokens toward PAT usage

## Non-goals

This ADR does **not** define:

- the exact schema for environment grants
- the exact PAT issuance API/UI
- asymmetric public-key auth
- Vault integration mechanics
- per-state or per-resource human authorization

Those are follow-on design/implementation tasks.

## Recommended phased implementation

### Phase 1: model and terminology

1. rename current customer backend tokens conceptually to automation/service tokens
2. document that current workspace removal does not revoke copied shared secrets
3. introduce the human PAT concept in docs and UX direction

### Phase 2: human PAT authentication

1. add PAT issuance/revocation for user accounts
2. authenticate human PATs separately from automation tokens
3. add backend principal type for human PATs

### Phase 3: environment-grant authorization

1. add environment grants for human principals
2. enforce membership + grant checks on backend access
3. ensure member removal and leave-workspace flows revoke backend access automatically

### Phase 4: rotation maturity

1. add overlap rotation for PATs
2. add expiry / warnings for automation tokens
3. add optional scheduled rotation policies where delivery is supported

### Phase 5: secret-manager integrations

1. integrate with Vault / cloud secret managers for automation-token distribution
2. enable safer automated rotation for machine credentials

## Product principle

Humans should access customer infrastructure state as **identity-bound
principals**.

Machines should access it as **explicit automation principals**.

Authorization should be driven by current membership and environment access,
not by the historical possession of a copied shared secret.
