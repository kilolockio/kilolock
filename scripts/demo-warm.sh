#!/usr/bin/env bash
# Restore the big-state demo fixture from
# examples/big-state/big-state.dump into the local kl
# database. Designed for the few-seconds-between-demo-takes
# turnaround that re-bootstrapping via `terraform apply` against
# 10k resources doesn't give you.
#
# DESTRUCTIVE: wipes the entire target database before restoring.
# This is intentional — pg_restore --clean drops every table it
# created and recreates from the dump, so any state besides what's
# in the dump goes away. If you have additional fixtures you want
# to keep, snapshot them first (`make demo-snapshot`) so they end
# up in the same dump file, or use a separate database for demo
# work and an env-var-switch for the URL.
#
# Usage:
#
#   KL_DATABASE_URL=postgres://... \
#     scripts/demo-warm.sh
#
# Exit codes:
#   0  restored, schema reconciled, big-state present
#   1  pg_restore failed, schema-migration drift detected,
#      or post-restore sanity check failed
#   2  bad invocation

set -euo pipefail

DUMP_PATH="${DUMP_PATH:-examples/big-state/big-state.dump}"

if [[ -z "${KL_DATABASE_URL:-}" ]]; then
  echo "demo-warm: KL_DATABASE_URL is not set" >&2
  exit 2
fi

if ! command -v pg_restore >/dev/null 2>&1; then
  echo "demo-warm: pg_restore not found on PATH" >&2
  exit 2
fi

if [[ ! -f "$DUMP_PATH" ]]; then
  cat >&2 <<EOF
demo-warm: $DUMP_PATH does not exist.

You need to snapshot a working big-state first:

  1. Bootstrap big-state the slow way once:
       cd examples/big-state
       terraform init
       TF_VAR_size=10000 terraform apply

  2. Capture it:
       make demo-snapshot

After that, \`make demo-warm\` will restore in seconds.
EOF
  exit 2
fi

echo "demo-warm: restoring from $DUMP_PATH (DESTRUCTIVE)..."
echo "demo-warm: this will wipe every table in the target database first."

# Disable set -e for the pg_restore call. pg_restore exits non-zero
# when it encounters errors mid-stream and continues — for example,
# a dump produced against Postgres 17 contains `SET
# transaction_timeout = 0` which Postgres 14 doesn't recognize, but
# the rest of the restore proceeds and the data ends up correctly
# in place. We let the sanity check below decide success rather
# than failing here on a benign forward-compatibility warning.
#
# --clean / --if-exists: drop existing objects before recreating.
# --no-owner / --no-acl: portable across DB roles.
# --jobs=4: parallel restore — biggest single speedup for big-state.
set +e
pg_restore \
  --clean \
  --if-exists \
  --no-owner \
  --no-acl \
  --jobs=4 \
  --dbname="$KL_DATABASE_URL" \
  "$DUMP_PATH"
RESTORE_EXIT=$?
set -e
if [[ $RESTORE_EXIT -ne 0 ]]; then
  echo "demo-warm: pg_restore exited $RESTORE_EXIT — verifying via sanity check..." >&2
fi

# Drift check: reconcile schema_migrations with the embedded migration
# set. The dump may have been captured before some migrations existed,
# in which case `schema_migrations` is behind reality. Running
# `kl migrate` here is idempotent and fast — it either:
#   - no-ops, when the dump matches the binary;
#   - applies missing migrations, when the dump is older but the binary
#     is newer and the new migrations don't collide with restored
#     objects;
#   - fails with a copy-pasteable INSERT, when an object exists but its
#     migration row was wiped by the restore (the common failure mode
#     we want serve to never have to surface on its own).
#
# Doing this in demo-warm — instead of waiting for `kl serve`
# to refuse to start — keeps the demo loop honest: every successful
# `make demo-warm` returns a database in a fully migrated state.
KL_BIN="${KL_BIN:-./bin/kl}"
if [[ -x "$KL_BIN" ]]; then
  echo "demo-warm: reconciling schema_migrations via $KL_BIN migrate..."
  if ! "$KL_BIN" migrate; then
    echo "demo-warm: ERROR: schema migration after restore failed" >&2
    echo "demo-warm: see the error above for the suggested fix" >&2
    exit 1
  fi
else
  echo "demo-warm: WARNING: $KL_BIN not found — skipping schema drift check" >&2
  echo "demo-warm: build the binary (\`make build\`) and rerun to verify the schema" >&2
fi

# Sanity check: did big-state come back?
# psql -tA strips column names and trims whitespace from output so
# the value is directly usable in shell tests.
COUNT="$(psql -tA "$KL_DATABASE_URL" -c \
  "SELECT count(*) FROM current_resources WHERE state_name = 'big-state';")"

if [[ "$COUNT" -gt 0 ]]; then
  echo "demo-warm: done — big-state restored with $COUNT current resources"
  exit 0
fi

echo "demo-warm: ERROR: restore completed but big-state has no current resources" >&2
echo "demo-warm: the dump may be empty or from before the big-state bootstrap" >&2
exit 1
