#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

require docker
require psql
require go
require curl
require awk

LATEST_VERSION="$(ls -1 pkg/migrate/migrations/*.sql | sed -E 's#.*/([0-9]+)_.*#\1#' | sort -n | tail -1)"
if [[ -z "${LATEST_VERSION}" ]]; then
  echo "failed to detect latest migration version" >&2
  exit 1
fi
# After squashing, we may only have a single baseline migration. In that case
# the only meaningful upgrade scenario is "baseline -> latest".
if (( 10#$LATEST_VERSION <= 1 )); then
  SCENARIOS=("0001")
else
  N_MINUS_1_VERSION="$(printf '%04d' "$((10#$LATEST_VERSION - 1))")"
  SCENARIOS=("${N_MINUS_1_VERSION}")
fi

export KL_DATABASE_URL="${KL_DATABASE_URL:-postgres://kl:kl@localhost:15432/kl?sslmode=disable}"
export KL_LISTEN_ADDR="${KL_LISTEN_ADDR:-:18081}"
export KL_CONTROL_LISTEN_ADDR="${KL_CONTROL_LISTEN_ADDR:-:18082}"
export KL_CONTROL_TOKEN="${KL_CONTROL_TOKEN:-migration-smoke-token}"
export KL_INIT_MODE="${KL_INIT_MODE:-prod}"
export KL_AUTH_MODE="${KL_AUTH_MODE:-database}"

cleanup() {
  docker compose -f docker-compose.yml down -v >/dev/null 2>&1 || true
  if [[ -n "${SERVE_PID:-}" ]]; then
    kill "$SERVE_PID" >/dev/null 2>&1 || true
  fi
  if [[ -n "${CONTROL_PID:-}" ]]; then
    kill "$CONTROL_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "==> start quick postgres"
docker compose -f docker-compose.yml down --remove-orphans >/dev/null 2>&1 || true
docker compose -f docker-compose.yml up -d postgres

for _ in $(seq 1 60); do
  if psql "$KL_DATABASE_URL" -c 'select 1' >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
psql "$KL_DATABASE_URL" -c 'select 1' >/dev/null

echo "==> build binaries"
go build -o bin/kl ./cmd/kl
go build -o bin/klc ./cmd/klc

seed_schema_to() {
  local target="$1"
  echo "==> reset schema"
  psql "$KL_DATABASE_URL" -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null
  echo "==> apply baseline + migrations up to ${target}"
  psql "$KL_DATABASE_URL" -f pkg/migrate/migrations/0001_baseline.sql >/dev/null
  for f in pkg/migrate/migrations/*.sql; do
    local base ver
    base="$(basename "$f")"
    ver="$(echo "$base" | sed -E 's/^([0-9]+)_.*$/\1/')"
    if [[ "$ver" == "0001" ]]; then
      continue
    fi
    if (( 10#$ver <= 10#$target )); then
      psql "$KL_DATABASE_URL" -f "$f" >/dev/null
    fi
  done
}

wait_healthz() {
  local url="$1"
  for _ in $(seq 1 30); do
    if curl -sf "$url" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  curl -sf "$url" >/dev/null
}

assert_control_authz() {
  local code
  local readonly_secret
  readonly_secret="$(./bin/kl admin token create --tenant operator --environment default --name readonly-check | awk -F': ' '/^secret \(save now, shown once\): /{print $2}')"
  if [[ -z "${readonly_secret}" ]]; then
    echo "failed to create readonly-check token" >&2
    exit 1
  fi
  local readonly_id
  readonly_id="$(psql "$KL_DATABASE_URL" -tA -c "SELECT tok.id::text FROM api_tokens tok JOIN tenants t ON t.id = tok.tenant_id JOIN environments e ON e.id = tok.environment_id WHERE t.slug='operator' AND e.slug='default' AND tok.name='readonly-check' ORDER BY tok.created_at DESC LIMIT 1")"
  if [[ -z "${readonly_id}" ]]; then
    echo "failed to resolve readonly-check token id" >&2
    exit 1
  fi
  curl -sf \
    -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/rbac/grants" \
    -d "{\"subject_kind\":\"api_token\",\"subject_id\":\"${readonly_id}\",\"role_key\":\"support_readonly\",\"scope_kind\":\"tenant\",\"scope_ref\":\"operator\",\"granted_by\":\"migration-smoke\"}" \
    >/dev/null

  # tenant-scoped readonly token can read environments in its tenant
  curl -sf -H "Authorization: Bearer ${readonly_secret}" "http://localhost:18082/api/tenants/operator/environments" >/dev/null
  # but cannot access global tenant list
  code="$(curl -s -o /tmp/kl-migration-smoke-authz-global.out -w '%{http_code}' \
    -H "Authorization: Bearer ${readonly_secret}" \
    "http://localhost:18082/api/tenants")"
  if [[ "$code" != "403" ]]; then
    echo "expected 403 for readonly global tenant list, got ${code}" >&2
    cat /tmp/kl-migration-smoke-authz-global.out >&2 || true
    exit 1
  fi
  # readonly token cannot create tenant
  code="$(curl -s -o /tmp/kl-migration-smoke-authz.out -w '%{http_code}' \
    -H "Authorization: Bearer ${readonly_secret}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/tenants" \
    -d '{"slug":"forbidden","name":"Forbidden"}')"
  if [[ "$code" != "403" ]]; then
    echo "expected 403 for readonly tenant create, got ${code}" >&2
    cat /tmp/kl-migration-smoke-authz.out >&2 || true
    exit 1
  fi
}

assert_control_audit_events() {
  local tenant_slug="audit-smoke"
  local env_slug="prod"
  local token_name="deploy"
  local token_id
  local code

  curl -sf \
    -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/tenants" \
    -d "{\"slug\":\"${tenant_slug}\",\"name\":\"Audit Smoke\"}" \
    >/dev/null
  curl -sf \
    -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/tenants/${tenant_slug}/environments" \
    -d "{\"slug\":\"${env_slug}\",\"tier\":\"shared\"}" \
    >/dev/null
  curl -sf \
    -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/tokens" \
    -d "{\"tenant\":\"${tenant_slug}\",\"environment\":\"${env_slug}\",\"name\":\"${token_name}\"}" \
    >/dev/null

  token_id="$(psql "$KL_DATABASE_URL" -tA -c "SELECT tok.id::text FROM api_tokens tok JOIN tenants t ON t.id = tok.tenant_id JOIN environments e ON e.id = tok.environment_id WHERE t.slug='${tenant_slug}' AND e.slug='${env_slug}' AND tok.name='${token_name}' ORDER BY tok.created_at DESC LIMIT 1")"
  if [[ -z "${token_id}" ]]; then
    echo "failed to resolve audit token id" >&2
    exit 1
  fi

  curl -sf \
    -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/tenants/lifecycle" \
    -d "{\"slug\":\"${tenant_slug}\",\"status\":\"suspended\",\"reason\":\"migration-smoke\"}" \
    >/dev/null
  curl -sf \
    -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/tenants/${tenant_slug}/environments/lifecycle" \
    -d "{\"environment\":\"${env_slug}\",\"status\":\"suspended\",\"reason\":\"migration-smoke\"}" \
    >/dev/null
  curl -sf \
    -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "http://localhost:18082/api/tokens/lifecycle" \
    -d "{\"id\":\"${token_id}\",\"status\":\"suspended\",\"reason\":\"migration-smoke\"}" \
    >/dev/null

  code="$(psql "$KL_DATABASE_URL" -tA -c "SELECT count(*) FROM events WHERE kind='tenant_lifecycle_update' AND payload->>'to'='suspended' AND payload->>'slug'='${tenant_slug}'")"
  if [[ "$code" != "1" ]]; then
    echo "expected tenant_lifecycle_update audit event count=1, got ${code}" >&2
    exit 1
  fi
  code="$(psql "$KL_DATABASE_URL" -tA -c "SELECT count(*) FROM events WHERE kind='environment_lifecycle_update' AND payload->>'to'='suspended' AND payload->>'tenant_slug'='${tenant_slug}' AND payload->>'env_slug'='${env_slug}'")"
  if [[ "$code" != "1" ]]; then
    echo "expected environment_lifecycle_update audit event count=1, got ${code}" >&2
    exit 1
  fi
  code="$(psql "$KL_DATABASE_URL" -tA -c "SELECT count(*) FROM events WHERE kind='api_token_lifecycle_update' AND payload->>'to'='suspended' AND payload->>'token_id'='${token_id}'")"
  if [[ "$code" != "1" ]]; then
    echo "expected api_token_lifecycle_update audit event count=1, got ${code}" >&2
    exit 1
  fi
}

run_upgrade_scenario() {
  local from="$1"
  echo "==> scenario: upgrade from schema ${from} -> latest ${LATEST_VERSION}"
  seed_schema_to "$from"

  echo "==> migrate ${from} -> latest via kl migrate"
  ./bin/kl migrate >/dev/null

  applied_latest="$(psql "$KL_DATABASE_URL" -tA -c 'select max(version) from schema_migrations')"
  if [[ "$applied_latest" != "$LATEST_VERSION" ]]; then
    echo "expected latest schema version $LATEST_VERSION, got $applied_latest" >&2
    exit 1
  fi
  echo "ok: schema_migrations max(version)=$applied_latest"

  echo "==> bootstrap metadata for prod serve sanity"
  ./bin/kl operator init --tenant operator --tenant-name Operator --token-name bootstrap --token "$KL_CONTROL_TOKEN" >/dev/null

  echo "==> serve startup sanity"
  KL_LOG_FORMAT=text KL_LOG_LEVEL=info ./bin/kl serve --skip-migrate >/tmp/kl-migration-smoke-serve.log 2>&1 &
  SERVE_PID=$!
  wait_healthz "http://localhost:18081/healthz"
  echo "ok: kl serve healthz"
  kill "$SERVE_PID" >/dev/null 2>&1 || true
  unset SERVE_PID

  echo "==> control startup + authz sanity"
  KL_LOG_FORMAT=text KL_LOG_LEVEL=info ./bin/klc serve >/tmp/kl-migration-smoke-control.log 2>&1 &
  CONTROL_PID=$!
  wait_healthz "http://localhost:18082/healthz"
  assert_control_authz
  assert_control_audit_events
  echo "ok: klc healthz + authz + lifecycle audit"
  kill "$CONTROL_PID" >/dev/null 2>&1 || true
  unset CONTROL_PID
}

for v in "${SCENARIOS[@]}"; do
  run_upgrade_scenario "$v"
done

echo "==> migration upgrade smoke passed (${SCENARIOS[*]} -> latest + boot/authz sanity)"
