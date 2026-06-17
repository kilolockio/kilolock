#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

export KL_META_PORT="${KL_META_PORT:-15432}"
export KL_SHARED_PORT="${KL_SHARED_PORT:-15433}"
export KL_PREMIUM_PORT="${KL_PREMIUM_PORT:-15434}"
export KL_API_PORT="${KL_API_PORT:-8080}"
export KL_BOOTSTRAP_TENANT_SLUG=" "

cleanup() {
  docker-compose -f docker-compose.prodlike.yml down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

assert_ok() {
  local code="$1"
  local msg="$2"
  if [[ "$code" -ne 0 ]]; then
    echo "FAIL: ${msg}" >&2
    exit 1
  fi
}

assert_nonzero() {
  local code="$1"
  local msg="$2"
  if [[ "$code" -eq 0 ]]; then
    echo "FAIL: ${msg}" >&2
    exit 1
  fi
}

docker-compose -f docker-compose.prodlike.yml down --remove-orphans >/dev/null 2>&1 || true
docker-compose -f docker-compose.prodlike.yml up -d --build

for _ in $(seq 1 60); do
  if curl -sf "http://localhost:${KL_API_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 1
done
docker compose exec -T kl kl migrate >/dev/null

docker compose exec -T kl kl admin tenant create --slug drill --name "Failure Drill" >/dev/null || true
docker compose exec -T kl kl admin environment create --tenant drill --slug shared --instance shared --provision >/dev/null
docker compose exec -T kl kl admin environment create --tenant drill --slug premium --instance premium --provision >/dev/null

SHARED_TOKEN="$(docker compose exec -T kl kl admin token create --tenant drill --environment shared --name ci-shared | awk -F': ' '/^secret \(save now, shown once\): /{print $2}')"
PREMIUM_TOKEN="$(docker compose exec -T kl kl admin token create --tenant drill --environment premium --name ci-premium | awk -F': ' '/^secret \(save now, shown once\): /{print $2}')"

STATE_PAYLOAD='{"version":4,"terraform_version":"1.9.5","serial":1,"lineage":"00000000-0000-0000-0000-000000000001","outputs":{},"resources":[]}'

# Prime both paths while premium DB is healthy.
curl -sf -u "drill:${SHARED_TOKEN}" -H "Content-Type: application/json" -X POST --data "$STATE_PAYLOAD" "http://localhost:${KL_API_PORT}/states/smoke-shared" >/dev/null
curl -sf -u "drill:${PREMIUM_TOKEN}" -H "Content-Type: application/json" -X POST --data "$STATE_PAYLOAD" "http://localhost:${KL_API_PORT}/states/smoke-premium" >/dev/null

docker-compose -f docker-compose.prodlike.yml stop postgres-premium >/dev/null

# Premium must fail (503), shared must continue to work.
set +e
PREMIUM_CODE="$(curl -s -o /tmp/kl-premium-out.txt -w '%{http_code}' -u "drill:${PREMIUM_TOKEN}" "http://localhost:${KL_API_PORT}/states/smoke-premium")"
CURL_EXIT="$?"
set -e
assert_ok "$CURL_EXIT" "curl should reach kl endpoint for premium request"
if [[ "$PREMIUM_CODE" != "503" ]]; then
  echo "FAIL: expected premium request HTTP 503 after premium DB stop, got ${PREMIUM_CODE}" >&2
  exit 1
fi

set +e
curl -sf -u "drill:${SHARED_TOKEN}" "http://localhost:${KL_API_PORT}/states/smoke-shared" >/dev/null
SHARED_EXIT="$?"
set -e
assert_ok "$SHARED_EXIT" "shared request should succeed while premium DB is down"

STATS_JSON="$(curl -sf -u "drill:${SHARED_TOKEN}" "http://localhost:${KL_API_PORT}/admin/routing/stats")"
echo "$STATS_JSON" | grep -q '"premium"' || {
  echo "FAIL: routing stats missing premium instance key" >&2
  exit 1
}

echo "multi-instance failure drill: OK"
