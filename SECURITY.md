# Security policy

## Reporting a vulnerability

Please do **not** open a public GitHub issue for suspected security vulnerabilities.

Instead, report security issues privately to the maintainers using the contact path documented in `MAINTAINERS.md`, or the repository security advisory workflow if enabled on GitHub.

When reporting, please include:

- affected component (`kl`, `kld`, `klc`, docs, compose)
- reproduction steps or proof of concept
- impact assessment
- version / commit tested
- any suggested mitigations if known

## Scope

We care especially about issues involving:

- authentication and authorization boundaries
- control-plane or runtime privilege escalation
- tenant or environment isolation failures
- direct database exposure or bypass of API-only client boundaries
- leakage of sensitive state or secrets
- unsafe rollback/query behavior that crosses expected tenant or environment scope

## Response expectations

We will try to:

- acknowledge the report promptly
- assess severity and blast radius
- coordinate a fix and disclosure plan
- credit reporters where appropriate, unless they prefer not to be named

## Supported release posture

This project is still evolving quickly. In general, the `main` branch and the most recent tagged release should be treated as the supported baseline unless stated otherwise.
