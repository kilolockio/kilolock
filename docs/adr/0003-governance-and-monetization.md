# ADR 0003: Governance and monetization strategy

- **Status:** Accepted
- **Date:** 2026-05-12
- **Decider(s):** @davesade (David Kubec)
- **Relates to:** [ADR 0001](./0001-foundations.md)

## Context

Kilolock is currently a solo open-source project under Apache 2.0.
Before accepting external contributions and before making strategic
decisions about the project's future, the project needs to be explicit
about four things:

1. The license commitment to users and contributors over time.
2. The legal terms under which contributions are accepted.
3. The trademark policy for the project name.
4. Whether — and how — a future commercial offering may exist alongside
   the open-source project.

This ADR records those decisions. It is intended as the trust contract
between the project and its users / contributors. Future contributors
should be able to read this document and understand exactly what kind
of project they are contributing to.

This ADR is not legal advice. Where contractual force is required
(CLA enforcement, trademark registration, corporate transfer) external
legal review is required. The decisions here describe intent.

## Decisions

### D1. The OSS license is and remains Apache 2.0

The OSS portion of Kilolock will not be relicensed away from a
permissive OSI-approved license. In particular, it will not be moved
to BSL, SSPL, Elastic License, or any other "source available, not
open source" license. The Apache 2.0 license that applies to the
current repository at HEAD applies to all future versions of the OSS.

**Rationale.** A project's trust with users and contributors requires
a permanent license commitment. The HashiCorp BSL pivot in 2023 is the
cautionary tale: once trust is eroded by a "we reserve the right to
change this later" stance, it is not easily restored. The cost of
permanently committing to Apache 2.0 — that we can never extract
margin by closing the OSS — is the correct cost to pay.

### D2. Contributions are accepted under an Individual CLA

Non-trivial contributions require the contributor to sign the project's
Individual Contributor License Agreement, [`cla/icla.md`](../../cla/icla.md).
The ICLA grants the project's maintainer(s) — and any future legal
entity that becomes the Project Steward — the right to redistribute
the contribution under any OSI-approved OSS license **and** under
proprietary commercial terms.

Trivial contributions (typo fixes, comment clarifications, ≤10 line
changes that have no copyright-worthy creativity) may be merged
without an ICLA at the maintainers' discretion, treated as inbound =
outbound under the existing Apache 2.0 license.

**Rationale.** The ICLA is the mechanism that keeps a future commercial
offering possible without requiring sign-off from every past
contributor. Without it, the project is permanently boxed into
Apache 2.0-only distribution. The CLA's friction cost is real but
small with modern tooling (CLA Assistant / EasyCLA), and it is the
single most important governance decision that is harder to add
retroactively than up front.

### D3. "Kilolock" is a reserved trademark

The name **Kilolock** and any associated logos and marks are
trademarks of David Kubec, with intent to register and (at the
appropriate time) transfer to a successor entity. Use of the name in
product or service names by third parties requires written permission.
Acceptable nominative use ("compatible with Kilolock", "an
Kilolock plugin") is allowed without permission. See
[`TRADEMARK.md`](../../TRADEMARK.md) for the policy in full.

**Rationale.** Trademarks and copyright are distinct. The OSS license
grants permission to use the code, not permission to use the name.
Without an explicit trademark policy, a third party could (legally and
in good faith) ship a confusing "Acme Kilolock" service. Protecting
the name now keeps the door open for a future commercial product
without requiring litigation.

### D4. A future commercial offering is explicitly allowed

A commercial entity (referred to here as "Kilolock Inc." pending
actual incorporation) may be formed in the future to operate one or
more of:

- A managed/hosted Kilolock service ("Kilolock Cloud").
- Enterprise add-ons sold as separate products.
- Paid support, consulting, and training.

The trademark, copyright on maintainer-authored code, project
stewardship role, and ICLA assignment rights may be transferred to
this entity. The OSS license commitment from D1 survives any such
transfer.

### D5. Feature boundary between OSS and commercial

This boundary will evolve, but the principles are fixed:

**The OSS includes the full backend capability set** required to run
Kilolock in production if the operator is willing to own operations.
Specifically:

- HTTP backend protocol server.
- State storage, versioning, locking, audit trail.
- Normalization (resources, dependencies, outputs).
- SQL query CLI and basic inventory queries.
- Migration tools, import/export.
- Multi-tenant primitives (tenants, tokens, environment isolation).
- Multi-instance data-plane routing primitives.
- Basic web UI for browsing state (if/when built).
- Documentation, examples, and reference deployments.

The intended product story explicitly includes **plain Terraform users**.
Operators should be able to keep their existing Terraform/OpenTofu CLI and
still get differentiated value from Kilolock as the backend/data plane:
queryable graph state, append-only history, rollback, richer conflict
intelligence, and backend-native concurrency modes that go beyond a generic
blob store.

**Commercial offerings focus on operating the system at scale (not
withholding core backend capabilities), and may include:**

- Enterprise authentication (SSO/SAML, advanced OIDC policies).
- Resource-level RBAC and per-tenant authorization.
- Audit log retention beyond a default OSS retention window.
- Metering, billing, quotas, and tenant lifecycle automation.
- Hosted cost-attribution dashboards.
- Advanced graph visualization and blast-radius UI.
- Continuous drift-detection daemon with notifications.
- Cross-state policy engine.
- SLA-backed support and incident response.

The `kl` CLI may continue to expose more advanced orchestration and
operator UX than plain Terraform can express directly, but that is an
**additional power path**, not the sole source of product differentiation.
The backend itself should remain meaningfully better than "an expensive
hosted state file."

A commercial feature can be moved into the OSS in the future; an OSS
feature is never moved out.

### D6. Maintainership is recorded in `MAINTAINERS.md`

The current list of maintainers and their scope of authority lives in
[`MAINTAINERS.md`](../../MAINTAINERS.md). v0 maintainership is the
solo founder. Governance will be revisited when the project gains
additional active maintainers.

## Consequences

**Positive.**

- The "no BSL" commitment is a meaningful trust signal in 2026, when
  several prominent projects have made the opposite choice.
- The ICLA preserves commercial flexibility without forcing the OSS
  itself to be encumbered.
- The trademark policy keeps the door open for a future commercial
  offering with the same name.
- Contributors know what they're signing up for. Users know what
  they're depending on.

**Negative.**

- The ICLA creates contributor friction. Some casual contributors
  will not sign. This is acceptable cost for the optionality it buys.
- The OSS / commercial feature boundary in D5 will be tested. Some
  proposals from the community will sit awkwardly on the line.
  Maintainers must be willing to say "no, this stays out of OSS"
  with a clear rationale.
- Holding "Kilolock" as a personal trademark (until incorporation)
  exposes one individual to defense costs in the unlikely event of
  challenge. Acceptable for v0 stage.

## Revisit conditions

This ADR should be revised — or superseded — when:

- A corporate entity is formed and the trademark / project stewardship
  is transferred. The new entity becomes the named party in
  references to "the Project Steward" or "Kilolock Inc.".
- A contributor friction problem with the ICLA becomes material
  enough that switching to DCO is warranted. (This would imply
  giving up some commercial flexibility.)
- The OSS / commercial feature boundary needs material adjustment
  (e.g., something previously commercial-only is moved into OSS).
- A second maintainer is added; governance shifts from "BDFL" to
  some kind of voting or consensus model.
