# ADR 0022: Operator bootstrap credentials are not data-encryption keys

- **Status:** Proposed
- **Date:** 2026-06-08
- **Decision makers:** Kilolock maintainers
- **Relates to:** [ADR 0015](./0015-control-plane-separation.md), [ADR 0016](./0016-operator-rbac-and-delegation.md), [ADR 0021](./0021-large-state-payloads-and-object-storage.md)

## Context

Kilolock bootstrap uses an initial control-plane token created by:

- `klc init`
- or `kl operator init`

That token is shown once, stored by the operator, and auto-granted
`platform_admin`.

It is the first credential that can:

- access the control plane
- create additional operator tokens
- manage tenant lifecycle, entitlements, and recovery actions
- recover the system when normal workflows are unavailable

At the same time, Kilolock stores sensitive data:

- token hashes and metadata
- raw state snapshots
- future tenant metadata
- potentially provider-derived secrets or sensitive values inside state payloads

This raises an important design question:

- should the initial operator bootstrap token also be used as a root key for
  encrypting sensitive data in the database?

## Decision

Kilolock will **not** use the operator bootstrap token as a database
encryption key or as general-purpose cryptographic root material.

Instead, Kilolock separates two responsibilities:

1. **Bootstrap/operator credentials**
   - identify who may operate the platform
   - authorize privileged control-plane actions
2. **Data-encryption keys**
   - protect sensitive stored data
   - are managed independently from operator identities and sessions

If field-level or payload-level encryption is introduced later, it must use a
separate key-management mechanism, such as:

- cloud KMS
- Vault transit / seal-backed key material
- HSM-backed key services
- another dedicated secret-management path

## Rationale

### Authentication credentials and encryption keys have different lifecycles

Bootstrap tokens are expected to be:

- issued once
- stored by operators
- rotated or replaced when needed
- revoked if compromised
- scoped by RBAC over time

Encryption keys should instead be:

- stable enough to preserve data readability
- independently rotatable
- recoverable without depending on one operator identity
- managed with explicit key hierarchy and audit

Binding both concerns to the same secret creates unnecessary coupling.

### Rotation would become unsafe and awkward

If the bootstrap token also encrypted stored data, then:

- rotating the token would imply re-encrypting data
- deleting or replacing the token could orphan ciphertext
- compromise of one operator credential would escalate directly into decryption
  authority

That is the wrong failure mode.

### Control-plane break-glass is not the same as cryptographic root

The bootstrap token is intentionally a **break-glass operator credential**.
Its purpose is administrative recovery and initial RBAC bootstrap.

That is closer to:

- a superadmin or root auth credential

and not to:

- a storage-encryption master key

### Vault follows a similar separation

HashiCorp Vault does not use its root token as the storage-encryption key.

The closest conceptual split is:

- **Vault root token** → privileged authentication and administration
- **Vault seal / master key material** → protection of encrypted storage

Kilolock should follow the same class of separation:

- operator bootstrap token for authn/authz bootstrap
- separate data-encryption material for protecting persisted sensitive values

## Consequences

### Positive

- cleaner security boundary between operator identity and data protection
- safer token rotation and recovery
- easier future integration with KMS / Vault / HSM-backed encryption
- lower risk that one leaked operator token becomes universal decryption power

### Tradeoffs

- encryption-at-rest strategy must be designed explicitly instead of “reusing”
  the bootstrap credential
- field-level encryption, if added later, will require dedicated configuration,
  key loading, rotation, and recovery procedures

## Non-goals

This ADR does **not** decide:

- the exact field-level encryption scheme
- whether raw state should be encrypted at the application layer
- whether encryption should be handled by cloud KMS, Vault, or another provider
- how customer-managed keys should work in BYODB or dedicated-host offerings

Those are future design decisions.

## Practical guidance for the current MVP

Until a dedicated application-level encryption model exists:

- treat the bootstrap token as an operator-only credential
- store it in a secret manager
- do not reuse it for any encryption purpose
- rely on standard infrastructure controls for stored data, such as:
  - database encryption at rest
  - disk / volume encryption
  - transport security
  - hashing of secrets where plaintext is not required

## Future direction

If Kilolock introduces encryption for sensitive stored values, the likely
shape should be:

1. introduce a dedicated data-key provider abstraction
2. keep operator RBAC completely separate from key material
3. support managed-service KMS integration first
4. later consider Vault transit or customer-managed key options for advanced
   deployments

This keeps the security model legible:

- **bootstrap token** answers: “who may operate the system?”
- **data-encryption key** answers: “how is sensitive persisted data protected?”
