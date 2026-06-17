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

docker-compose -f docker-compose.prodlike.yml down --remove-orphans >/dev/null 2>&1 || true
docker-compose -f docker-compose.prodlike.yml up -d --build

for _ in $(seq 1 60); do
  if curl -sf "http://localhost:${KL_API_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 1
done
docker compose exec -T kl /usr/local/bin/kld migrate >/dev/null

docker compose exec -T kl kl admin tenant create --slug smoke --name "Smoke Tenant" >/dev/null || true
docker compose exec -T kl kl admin environment create --tenant smoke --slug premium --instance premium --provision
docker compose exec -T kl kl admin environment validate-routing
docker compose exec -T kl kl admin environment list --tenant smoke

if ! command -v terraform >/dev/null; then
  echo "terraform is required for big-state smoke checks" >&2
  exit 1
fi
if ! command -v jq >/dev/null; then
  echo "jq is required for big-state smoke checks" >&2
  exit 1
fi

go build -o bin/kl ./cmd/kl
terraform -chdir=examples/big-state init -input=false >/dev/null
echo "==> strict targeted preflight must block high-fanout apply --target"
if (
  cd "$ROOT_DIR/examples/big-state"
    "$ROOT_DIR/bin/kl" apply \
      --target=module.primary_herd \
      --allow-unsafe-target \
      --strict-target-preflight \
      --dry-run
); then
  echo "strict-target-preflight regression: expected failure for high-fanout target, got success" >&2
  exit 1
fi

echo "multi-instance smoke: OK"
