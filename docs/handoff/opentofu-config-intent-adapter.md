# OpenTofu Config Intent Adapter

This note captures the next implementation step after ADR 0029's current OSS
slice.

## What exists now

`kl` now asks an internal package for backend expansion intent:

- `internal/configscope`

That package returns a stable model:

- planning targets
- selectors
- explicit write candidates
- explicit read candidates
- undeployed config candidates

Today:

- discovery is selected by `KL_CONFIG_DISCOVERY`
- supported values are:
  - `heuristic` (default)
  - `opentofu`
- `heuristic` stays backed by the lightweight extractor from `internal/plan`
- `opentofu` now uses an HCL AST walk that is closer to OpenTofu-style config
  analysis, but it is still **not** a direct import of OpenTofu internal
  packages because those are not publicly importable

## Why this seam matters

The next hard problem is no longer "how do we store realized state?".

It is:

- how to understand undeployed resources correctly from local config
- how to derive dependency closure before touching the backend
- how to tell safe undeployed candidates from unsafe/ambiguous ones

That is the point where reusing OpenTofu components becomes attractive.

## Intended next swap

Replace the internal implementation behind `internal/configscope` with an
OpenTofu-backed extractor that can:

1. load root module + child modules
2. resolve addresses with better parity to Terraform/OpenTofu
3. build richer dependency information for undeployed resources
4. emit the same `Intent` model already used by `cmd/kl`

## Constraint

Do not leak OpenTofu-native types outside `internal/configscope`.

The rest of Kilolock should continue to depend only on the stable local model,
so the integration remains:

- replaceable
- testable
- portable to other upstreams later if needed

## Practical coding plan

1. Improve the `opentofu` engine implementation under `internal/configscope`.
2. Extend read-only extraction:
   - selectors
   - undeployed candidates
   - direct references
3. Compare output against current heuristic tests.
4. Evaluate whether selected pieces of OpenTofu source should be vendored into
   Kilolock under an internal compatibility package, since upstream `internal/*`
   packages cannot be imported directly.
5. Later, when confidence is high enough, consider switching the default
   discovery engine to the OpenTofu-backed path and keep heuristic mode as
   fallback for a while.
