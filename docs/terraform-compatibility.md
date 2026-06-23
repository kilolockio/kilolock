# Terraform/OpenTofu Compatibility Policy

This document defines how Kilolock handles IaC CLI version compatibility for OSS and managed deployments.

## Customer Configuration

Customers choose their Terraform/OpenTofu version in their own runtime:

1. Native Terraform/OpenTofu CLI usage:
   - Version comes from the user’s local machine or CI image.
2. Kilolock CLI scoped/targeted flows (`kl plan/apply`):
   - `--terraform-bin` to select binary family/path.
   - `--iac-version` to select a versioned binary (`terraform-1.9.8`, etc).
   - Optional config defaults from `.kl.toml` / env (`KL_IAC_BIN`, `KL_IAC_VERSION`).

The Kilolock server does not execute customer Terraform plans in normal backend mode; it serves HTTP backend protocol and persists state metadata.

## Quota Enforcement Model

Quota behavior differs depending on which client path the operator uses:

1. Native Terraform/OpenTofu HTTP backend usage:
   - Reads and writes go through the standard backend protocol.
   - Hard quota is still enforced by the backend on the final state write.
   - This means plain Terraform may discover quota failure late, after provider-side infrastructure work already happened.
2. Kilolock CLI plan-driven usage:
   - `kl plan` performs a backend quota preflight from the plan's projected managed-resource delta.
   - `kl quota remaining` shows current headroom.
   - `kl quota check --tf-plan ...` lets CI or operators evaluate a Terraform plan explicitly before apply.
   - Hard quota overages fail early.
   - Soft quota overages warn but still succeed.

Quota admission is based on the **projected final state shape**, not on a
temporary in-flight peak during provider execution order. In practical terms:

- a plan that deletes `1000` managed resources and creates `100` managed
  resources is evaluated as a net `-900` change
- if the resulting final state is within quota, the quota check passes
- Kilolock does **not** currently reject a plan solely because a
  create-before-destroy or replacement sequence might temporarily have old and
  new infrastructure alive at the same time before the final state settles

This applies both to:

- CLI-side preflight (`kl plan`, `kl quota check`)
- backend write-time hard enforcement on the final stored state

This is why plan admission is documented as a Kilolock CLI guarantee, not a
generic Terraform HTTP backend guarantee.

## Support Levels

1. **Supported**:
   - Versions covered by CI smoke matrix in `.github/workflows/ci.yml`.
   - Current default matrix includes recent Terraform lines and OpenTofu.
2. **Best effort**:
   - Nearby patch/minor releases not explicitly in matrix.
3. **Unsupported**:
   - Very old releases with known backend/protocol incompatibilities.

## Operational Impact on Server

Different IaC client versions can affect:

1. HTTP lock/retry behavior.
2. State payload shape/size and `terraform_version` values recorded in `state_versions`.
3. Provider-driven plan/apply behavior on client side (not server execution path).

## Observability

Control-plane API exposes observed version distribution:

- `GET /v1/api/platform/iac-versions?limit=50`

This aggregates `state_versions.terraform_version` so operators can:

1. See real version mix across tenants.
2. Plan deprecation windows.
3. Detect outlier clients after upgrades.

## Recommended Practice

1. Pin IaC versions per repo (CI image/toolchain).
2. Upgrade in staged waves.
3. Monitor observed versions in control API before removing support for older lines.
