#!/usr/bin/env bash
set -euo pipefail

# Quick-compose smoke: validate docker-compose.yml can start, and
# that key CLI/demo/control-plane flows work against it.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

export KL_API_PORT="${KL_API_PORT:-8080}"
export KL_QUICK_PG_PORT="${KL_QUICK_PG_PORT:-15432}"
export KL_CONTROL_PORT="${KL_CONTROL_PORT:-18082}"
export KL_DATABASE_URL="postgres://kl:kl@localhost:${KL_QUICK_PG_PORT}/kl?sslmode=disable"
export KL_CONTROL_TOKEN="${KL_CONTROL_TOKEN:-quick-compose-smoke-token}"
export KL_CONTROL_PUBLIC_API="${KL_CONTROL_PUBLIC_API:-enabled}"
export KL_CONTROL_LISTEN_ADDR=":${KL_CONTROL_PORT}"
export KL_DATA_PLANE_ADMIN_URL="${KL_DATA_PLANE_ADMIN_URL:-postgres://kl:kl@localhost:${KL_QUICK_PG_PORT}/postgres?sslmode=disable}"
export KL_DATA_PLANE_DATABASE_URL="${KL_DATA_PLANE_DATABASE_URL:-postgres://kl:kl@localhost:${KL_QUICK_PG_PORT}/postgres?sslmode=disable}"

tmp_cfg=""
control_pid=""
cleanup() {
  if [[ -n "${control_pid}" ]]; then
    kill "${control_pid}" >/dev/null 2>&1 || true
    wait "${control_pid}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${tmp_cfg}" ]]; then
    rm -rf "${tmp_cfg}" >/dev/null 2>&1 || true
  fi
  docker compose -f docker-compose.yml down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker compose -f docker-compose.yml down --remove-orphans >/dev/null 2>&1 || true
docker compose -f docker-compose.yml up -d --build

for _ in $(seq 1 60); do
  if curl -sf "http://localhost:${KL_API_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 1
done

if ! curl -sf "http://localhost:${KL_API_PORT}/healthz" >/dev/null; then
  echo "quick-compose smoke: server did not become healthy on :${KL_API_PORT}" >&2
  exit 1
fi

go build -o bin/kl ./cmd/kl
go build -o bin/kld ./cmd/kld
go build -o bin/klc ./cmd/klc

# Ensure schema is current for CLI DB callers (idempotent).
KL_DATABASE_URL="$KL_DATABASE_URL" ./bin/kld migrate >/dev/null
KL_DATABASE_URL="$KL_DATABASE_URL" ./bin/klc migrate >/dev/null

./bin/klc serve >/tmp/kl-quick-compose-control.log 2>&1 &
control_pid=$!

for _ in $(seq 1 60); do
  if curl -sf "http://localhost:${KL_CONTROL_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 1
done

if ! curl -sf "http://localhost:${KL_CONTROL_PORT}/healthz" >/dev/null; then
  echo "quick-compose smoke: control plane did not become healthy on :${KL_CONTROL_PORT}" >&2
  cat /tmp/kl-quick-compose-control.log >&2 || true
  exit 1
fi

if ! command -v terraform >/dev/null; then
  echo "terraform is required for quick-compose smoke demos" >&2
  exit 1
fi
if ! command -v jq >/dev/null; then
  echo "jq is required for quick-compose smoke demos" >&2
  exit 1
fi

terraform -chdir=examples/big-state init -input=false >/dev/null

echo "==> prepare control-plane tenant/environment for big-state demo"
CONTROL_API_URL="http://localhost:${KL_CONTROL_PORT}" \
KL_CONTROL_TOKEN="$KL_CONTROL_TOKEN" \
  ./bin/kl admin tenant create --slug vnv --name "VNV" >/dev/null || true
CONTROL_API_URL="http://localhost:${KL_CONTROL_PORT}" \
KL_CONTROL_TOKEN="$KL_CONTROL_TOKEN" \
  ./bin/kl admin environment create --tenant vnv --slug prod --provision >/dev/null || true

echo "==> control-plane coexistence helpers must flip policy cleanly"
KL_BIN="$ROOT_DIR/bin/kl" \
  KL_CONTROL_TOKEN="$KL_CONTROL_TOKEN" \
  CONTROL_API_URL="http://localhost:${KL_CONTROL_PORT}" \
  CONTROL_API_TENANT="vnv" \
  CONTROL_API_ENV="prod" \
  ./examples/big-state/parallel-demo.sh policy_show >/dev/null

KL_BIN="$ROOT_DIR/bin/kl" \
  KL_CONTROL_TOKEN="$KL_CONTROL_TOKEN" \
  CONTROL_API_URL="http://localhost:${KL_CONTROL_PORT}" \
  CONTROL_API_TENANT="vnv" \
  CONTROL_API_ENV="prod" \
  ./examples/big-state/parallel-demo.sh policy_strict >/dev/null

strict_mode="$(curl -fsS \
  -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
  "http://localhost:${KL_CONTROL_PORT}/api/states/vnv/prod" \
  | jq -r '.states[] | select(.name=="big-state") | .coexistence_mode')"
if [[ "${strict_mode}" != "strict" ]]; then
  echo "quick-compose smoke: expected big-state coexistence_mode=strict after policy_strict, got ${strict_mode:-<empty>}" >&2
  exit 1
fi

KL_BIN="$ROOT_DIR/bin/kl" \
  KL_CONTROL_TOKEN="$KL_CONTROL_TOKEN" \
  CONTROL_API_URL="http://localhost:${KL_CONTROL_PORT}" \
  CONTROL_API_TENANT="vnv" \
  CONTROL_API_ENV="prod" \
  ./examples/big-state/parallel-demo.sh policy_warn >/dev/null

warn_mode="$(curl -fsS \
  -H "Authorization: Bearer ${KL_CONTROL_TOKEN}" \
  "http://localhost:${KL_CONTROL_PORT}/api/states/vnv/prod" \
  | jq -r '.states[] | select(.name=="big-state") | .coexistence_mode')"
if [[ "${warn_mode}" != "warn" ]]; then
  echo "quick-compose smoke: expected big-state coexistence_mode=warn after policy_warn, got ${warn_mode:-<empty>}" >&2
  exit 1
fi

echo "==> confirm-scope gate must block mutating scoped apply"
tmp_cfg="$(mktemp -d)"
cat >"$tmp_cfg/main.tf" <<'EOF'
terraform {
  required_version = ">= 1.5.0"
  backend "http" {}
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
}

variable "v" {
  type    = string
  default = "v1"
}

resource "null_resource" "x" {
  triggers = {
    v = var.v
  }
}
EOF
cat >"$tmp_cfg/backend.tf" <<EOF
terraform {
  backend "http" {
    address        = "http://localhost:${KL_API_PORT}/states/vnv/prod/quick-confirm-scope"
    lock_address   = "http://localhost:${KL_API_PORT}/states/vnv/prod/quick-confirm-scope"
    unlock_address = "http://localhost:${KL_API_PORT}/states/vnv/prod/quick-confirm-scope"
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
EOF

terraform -chdir="$tmp_cfg" init -input=false >/dev/null
# Seed state quickly (null_resource has no delay).
terraform -chdir="$tmp_cfg" apply -auto-approve -input=false -refresh=false >/dev/null

# Put the resource in its own file so --file selection is clean.
mv "$tmp_cfg/main.tf" "$tmp_cfg/only.tf"

# Make a mutating change so scoped apply would run if allowed.
sed -i.bak 's/default = "v1"/default = "v2"/' "$tmp_cfg/only.tf" && rm -f "$tmp_cfg/only.tf.bak"

if (
  CONTROL_API_URL="http://localhost:${KL_CONTROL_PORT}" \
  KL_CONTROL_TOKEN="$KL_CONTROL_TOKEN" \
    "$ROOT_DIR/bin/kl" apply \
      --work-dir="$tmp_cfg" \
      --file=only.tf \
      --no-refresh \
      --wait-timeout=0 \
      --terraform-bin=terraform \
      --actor=ci \
      --allow-destructive-scoped >/dev/null 2>&1
); then
  echo "confirm-scope regression: expected scoped apply to fail without --confirm-scope" >&2
  exit 1
fi

echo "quick-compose smoke: OK"
