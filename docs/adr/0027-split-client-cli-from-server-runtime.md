# ADR 0027: Split client CLI from server/runtime binary

## Status

Accepted

## Context

Kilolock started with a single `kl` binary that mixed:

- client/operator workflows
- server runtime
- database migrations
- provisioning helpers

That shape was convenient early on, but it blurred an important boundary:

- **client CLI** should talk only to server APIs
- **server/runtime tooling** may talk directly to the database

We have already migrated normal CLI workflows (`apply`, `rollback`, `query`,
`admin`, `operator`, `provider`, `refresh`, `import`, `tag`) behind backend or
control APIs. The remaining direct database access in `cmd/kl` is now
limited to runtime/deployment commands such as:

- `serve`
- `migrate`
- `provision`

For first public deployment we want the public contract to be simple and
defensible:

- `kl` is a **client CLI**
- it does **not** require direct database connectivity
- server-side binaries own database access

## Decision

We split the mixed binary into two roles:

- `kl`
  - client/operator CLI
  - API-driven
  - no direct DB requirement for normal usage
- `kld`
  - server/runtime/deployment binary
  - owns:
    - `serve`
    - `migrate`
    - `provision`
  - may connect directly to the database(s)

`klc` already fits the server/runtime
side of this boundary and continue to talk directly to the control/data plane
as services.

## Consequences

### Positive

- Clear public security boundary
- Cleaner OSS story
- Easier packaging and docs
- Fewer surprises about which commands need private network access
- Lets us state plainly:
  - `kl` is API-only
  - `kld` is infrastructure-side

### Tradeoffs

- More than one binary to package
- Some deployment docs/scripts must switch from `kld serve|migrate|provision`
  to `kld serve|migrate|provision`
- Short-term code duplication is acceptable while the split settles

## Follow-up

- Keep moving docs/scripts to the new binary names
- Consider extracting shared runtime helpers if duplication becomes noisy
- Keep client-facing commands out of `kld`
