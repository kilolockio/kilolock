#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "==> prod-critical package gate"
go test ./cmd/kl ./cmd/klc ./pkg/store ./internal/backend
echo "==> prod-critical gate passed"
