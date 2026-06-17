#!/usr/bin/env bash
set -euo pipefail

TENANT="${1:-}"
if [[ -z "$TENANT" ]]; then
  echo "usage: $0 <tenant-slug>" >&2
  exit 2
fi

API_URL="${KL_API_URL:-http://localhost:8080}"
AUTH_MODE="${KL_AUTH_MODE:-basic}"
USERNAME="${KL_USERNAME:-$TENANT}"
PASSWORD="${KL_PASSWORD:-}"
TOKEN="${KL_TOKEN:-}"

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 2
fi

auth_args=()
case "$AUTH_MODE" in
  bearer)
    if [[ -z "$TOKEN" ]]; then
      echo "KL_TOKEN is required for bearer mode" >&2
      exit 2
    fi
    auth_args=(-H "Authorization: Bearer ${TOKEN}")
    ;;
  basic)
    if [[ -z "$PASSWORD" ]]; then
      echo "KL_PASSWORD is required for basic mode" >&2
      exit 2
    fi
    auth_args=(-u "${USERNAME}:${PASSWORD}")
    ;;
  *)
    echo "unsupported KL_AUTH_MODE=$AUTH_MODE (use basic|bearer)" >&2
    exit 2
    ;;
esac

migration_json="$(./bin/kl admin environment migration-status --tenant "$TENANT" --json)"
routing_json="$(curl -sf "${auth_args[@]}" "${API_URL}/admin/routing/stats")"

echo "$migration_json" | jq -r --argjson routing "$routing_json" --arg tenant "$TENANT" '
  .environments[]
  | . as $env
  | ($routing.routing_instances[$env.instance].status // "n/a") as $routing_status
  | ($routing.routing_instances[$env.instance].connect_failures // 0) as $connect_failures
  | "tenant=\($tenant) env=\($env.environment_slug) instance=\($env.instance) env_status=\($env.status) mig_ver=\($env.last_migration_version) mig_err=\($env.last_migration_error // "-") routing=\($routing_status) connect_failures=\($connect_failures)"
'
