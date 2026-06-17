#!/usr/bin/env bash
# Snapshot the operator's local kl database — schema + data —
# into examples/big-state/big-state.dump so `make demo-warm` can
# restore it in seconds instead of re-running the multi-minute
# terraform bootstrap.
#
# Usage:
#
#   KL_DATABASE_URL=postgres://... \
#     scripts/demo-snapshot.sh
#
# The output is pg_dump's binary "custom" format so pg_restore can
# do parallel restore + selective restore later. The file is
# .gitignored (it's large — typical big-state with 10k resources is
# 8-15 MB compressed) but lives under examples/ so operators find
# it next to the demo it supports.
#
# Exit codes:
#   0  snapshot written
#   1  pg_dump failed
#   2  bad invocation

set -euo pipefail

DUMP_PATH="${DUMP_PATH:-examples/big-state/big-state.dump}"

if [[ -z "${KL_DATABASE_URL:-}" ]]; then
  echo "demo-snapshot: KL_DATABASE_URL is not set" >&2
  exit 2
fi

if ! command -v pg_dump >/dev/null 2>&1; then
  echo "demo-snapshot: pg_dump not found on PATH" >&2
  exit 2
fi

mkdir -p "$(dirname "$DUMP_PATH")"

# --format=custom: parallelisable, compressed, selective restore.
# --no-owner / --no-privileges: portable across DB owners.
# --no-acl: avoid GRANTs that the restore-side role may not have.
echo "demo-snapshot: writing $DUMP_PATH..."
pg_dump \
  --format=custom \
  --no-owner \
  --no-privileges \
  --no-acl \
  --file="$DUMP_PATH" \
  "$KL_DATABASE_URL"

SIZE_HUMAN=$(du -h "$DUMP_PATH" | cut -f1)
echo "demo-snapshot: done ($SIZE_HUMAN)"
echo "demo-snapshot: restore with \`make demo-warm\`"
