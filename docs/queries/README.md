# Canonical example queries

This directory contains hand-curated SQL queries demonstrating what the
Kilolock data model makes easy. Each file is a standalone query that
can be passed to `kl query -f` directly.

```sh
./bin/kl query -f docs/queries/inventory_by_type.sql
./bin/kl query -f docs/queries/blast_radius.sql --format json
```

Queries that need parameters are written with example literals inlined
(e.g. `WHERE address = 'aws_vpc.main'`). Edit the file before running
or pipe through `sed` to substitute values:

```sh
sed 's/PLACEHOLDER/aws_vpc.main/' docs/queries/blast_radius.sql \
  | ./bin/kl query -f -
```

## Index

| File | What it answers |
|---|---|
| [`inventory_by_type.sql`](./inventory_by_type.sql) | Across all states, how many resources of each type do I have? |
| [`states_overview.sql`](./states_overview.sql) | What's the size of each managed state, and is anything locked right now? |
| [`resources_in_state.sql`](./resources_in_state.sql) | List every resource in a given state with its module path. |
| [`dependencies_of.sql`](./dependencies_of.sql) | What does this resource depend on (direct edges)? |
| [`dependents_of.sql`](./dependents_of.sql) | What depends on this resource (direct reverse edges)? |
| [`blast_radius.sql`](./blast_radius.sql) | What does this resource transitively reach? (Recursive CTE.) |
| [`unencrypted_s3_buckets.sql`](./unencrypted_s3_buckets.sql) | Find any aws_s3_bucket missing server-side encryption. (JSONB attribute query.) |
| [`recent_writes.sql`](./recent_writes.sql) | Which states were written in the last 24 hours, and by whom? |
| [`orphaned_data_sources.sql`](./orphaned_data_sources.sql) | Data sources that no managed resource depends on. |
| [`drift_current.sql`](./drift_current.sql) | What resources are currently drifted (refresh-detected, not yet reconciled)? Includes the set of changed attribute keys. |

## Notes

- Every query in this directory is **read-only**. The query command
  refuses any non-read transaction at the Postgres level, so even if
  you write a destructive query it will be rejected.
- Most queries target the `current_resources`,
  `current_resource_dependencies`, and `current_resource_drift`
  views, which expose only the currently-alive resources of each
  state. To ask point-in-time
  questions ("what did state X look like at serial 17?") query the
  underlying `resources` table directly with a
  `r.create_serial <= S AND (r.delete_serial IS NULL OR r.delete_serial > S)`
  predicate, or join `state_versions` to compute S.
- The schema these queries target is defined in
  [`pkg/migrate/migrations/`](../../pkg/migrate/migrations/)
  and rationalized in [`docs/schema.md`](../schema.md).
