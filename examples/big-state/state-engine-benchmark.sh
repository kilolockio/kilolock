#!/usr/bin/env bash
# state-engine-benchmark.sh — repeatable large-state benchmark for the
# state-engine planning/apply path on the big-state fixture.
#
# Default flow:
#   1. ensure the slow_a / slow_b demo resources exist
#   2. measure one cold scoped plan after the graph-cache TTL expires
#   3. measure one immediate warm scoped plan against the same file
#
# Optional:
#   4. run one real scoped apply from a generated plan spec
#
# The goal is not to produce perfect scientific numbers. The goal is to make it
# easy to compare "before vs after" after changes to slice expansion, cache
# behavior, or state materialization.

set -euo pipefail

MODE="${1:-plans}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
KL_BIN="${KL_BIN:-$REPO_ROOT/bin/kl}"
TF_BIN="${TF_BIN:-terraform}"

STATE_NAME="${STATE_NAME:-}"
BACKEND_ADDRESS="${TF_HTTP_ADDRESS:-}"
BACKEND_USERNAME="${KL_USERNAME:-${TF_HTTP_USERNAME:-${TF_HTTP_USER:-}}}"
BACKEND_PASSWORD="${KL_PASSWORD:-${TF_HTTP_PASSWORD:-}}"
KL_PROTOCOL_VALUE="${KL_PROTOCOL_VALUE:-state-engine}"

SCOPE_FILE="${SCOPE_FILE:-slow_a.tf}"
SCOPE_ADDR="${SCOPE_ADDR:-time_sleep.slow_a}"
PIN_ADDR="${PIN_ADDR:-time_sleep.slow_b}"

CACHE_EXPIRE_WAIT="${CACHE_EXPIRE_WAIT:-6}"
KEEP_ARTIFACTS="${KEEP_ARTIFACTS:-}"

PLAN_COLD_SPEC="${PLAN_COLD_SPEC:-/tmp/kl-state-engine-bench-cold.json}"
PLAN_WARM_SPEC="${PLAN_WARM_SPEC:-/tmp/kl-state-engine-bench-warm.json}"
PLAN_APPLY_SPEC="${PLAN_APPLY_SPEC:-/tmp/kl-state-engine-bench-apply.json}"
PLAN_BACKEND_SPEC="${PLAN_BACKEND_SPEC:-/tmp/kl-state-engine-bench-backend.json}"
PLAN_TERRAFORM_OUT="${PLAN_TERRAFORM_OUT:-/tmp/kl-state-engine-bench-terraform.tfplan}"
PLAN_COLD_LOG="${PLAN_COLD_LOG:-/tmp/kl-state-engine-bench-cold.log}"
PLAN_WARM_LOG="${PLAN_WARM_LOG:-/tmp/kl-state-engine-bench-warm.log}"
APPLY_LOG="${APPLY_LOG:-/tmp/kl-state-engine-bench-apply.log}"
PLAN_BACKEND_LOG="${PLAN_BACKEND_LOG:-/tmp/kl-state-engine-bench-backend.log}"
PLAN_TERRAFORM_LOG="${PLAN_TERRAFORM_LOG:-/tmp/kl-state-engine-bench-terraform.log}"
FULL_STATE_PATH="${FULL_STATE_PATH:-/tmp/kl-state-engine-bench-full.tfstate}"

if [ -t 1 ] && command -v tput >/dev/null 2>&1 && [ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]; then
	BOLD="$(tput bold)"; DIM="$(tput dim)"
	GREEN="$(tput setaf 2)"; YELLOW="$(tput setaf 3)"; RED="$(tput setaf 1)"; CYAN="$(tput setaf 6)"
	RESET="$(tput sgr0)"
else
	BOLD=""; DIM=""; GREEN=""; YELLOW=""; RED=""; CYAN=""; RESET=""
fi

step() { printf "%s==>%s %s\n" "$BOLD" "$RESET" "$*"; }
info() { printf "%s    %s%s\n" "$DIM" "$*" "$RESET"; }
ok()   { printf "%s    ✓ %s%s\n" "$GREEN" "$*" "$RESET"; }
warn() { printf "%s    ! %s%s\n" "$YELLOW" "$*" "$RESET"; }
fail() { printf "%s    ✗ %s%s\n" "$RED" "$*" "$RESET"; exit 1; }

cleanup() {
	if [ -n "$KEEP_ARTIFACTS" ]; then
		return
	fi
	rm -f "$PLAN_COLD_SPEC" "$PLAN_WARM_SPEC" "$PLAN_APPLY_SPEC" \
		"$PLAN_BACKEND_SPEC" "$PLAN_TERRAFORM_OUT" \
		"$PLAN_COLD_LOG" "$PLAN_WARM_LOG" "$APPLY_LOG" \
		"$PLAN_BACKEND_LOG" "$PLAN_TERRAFORM_LOG" "$FULL_STATE_PATH"
}
trap cleanup EXIT

now_ms() {
	python3 - <<'PY'
import time
print(int(time.time() * 1000))
PY
}

discover_backend() {
	local backend_state="$HERE/.terraform/terraform.tfstate"
	[ -f "$backend_state" ] || return 0
	local discovered
	discovered="$(python3 - "$backend_state" <<'PY'
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    data = json.load(fh)
cfg = ((data.get("backend") or {}).get("config") or {})
for key in ("address", "username", "password"):
    print(cfg.get(key) or "")
PY
)"
	local discovered_address discovered_username discovered_password
	discovered_address="$(printf '%s\n' "$discovered" | sed -n '1p')"
	discovered_username="$(printf '%s\n' "$discovered" | sed -n '2p')"
	discovered_password="$(printf '%s\n' "$discovered" | sed -n '3p')"
	if [ -z "$BACKEND_ADDRESS" ] && [ -n "$discovered_address" ]; then
		BACKEND_ADDRESS="$discovered_address"
	fi
	if [ -z "$BACKEND_USERNAME" ] && [ -n "$discovered_username" ]; then
		BACKEND_USERNAME="$discovered_username"
	fi
	if [ -z "$BACKEND_PASSWORD" ] && [ -n "$discovered_password" ]; then
		BACKEND_PASSWORD="$discovered_password"
	fi
	if [ -z "$STATE_NAME" ] && [ -n "$BACKEND_ADDRESS" ]; then
		STATE_NAME="$(python3 - "$BACKEND_ADDRESS" <<'PY'
from urllib.parse import urlparse
import sys
u = urlparse(sys.argv[1].strip())
path = u.path.strip("/")
prefix = "v1/states/"
print(path[len(prefix):] if path.startswith(prefix) else "")
PY
)"
	fi
	if [ -z "$STATE_NAME" ]; then
		STATE_NAME="big-state"
	fi
}

preflight() {
	[ -x "$KL_BIN" ] || fail "kl binary not at $KL_BIN (run \`make build\`)"
	command -v "$TF_BIN" >/dev/null || fail "terraform binary '$TF_BIN' not on PATH"
	command -v jq >/dev/null || fail "jq is required"
	command -v curl >/dev/null || fail "curl is required"
	command -v python3 >/dev/null || fail "python3 is required"
	[ -d "$HERE/.terraform" ] || fail "run \`terraform init\` in $HERE first"
	discover_backend
	if ! "$KL_BIN" list >/dev/null 2>&1; then
		fail "kl cannot reach the runtime API. Run \`terraform init\` here and make sure the backend is reachable."
	fi
}

run_kl() {
	KL_PROTOCOL="$KL_PROTOCOL_VALUE" \
	"$KL_BIN" "$@"
}

run_kl_backend() {
	KL_PROTOCOL="terraform-http" \
	"$KL_BIN" "$@"
}

detect_versions() {
	local state_sql state_json
	state_sql="
SELECT address, COALESCE(attributes->'triggers'->>'version', '') AS version
FROM current_resources
WHERE state_name = '$STATE_NAME'
  AND address IN ('$SCOPE_ADDR', '$PIN_ADDR')
ORDER BY address;
"
	state_json="$("$KL_BIN" query --format json "$state_sql")"
	SCOPE_CURRENT="$(jq -r --arg a "$SCOPE_ADDR" '.[] | select(.address==$a) | .version' <<<"$state_json")"
	PIN_CURRENT="$(jq -r --arg a "$PIN_ADDR" '.[] | select(.address==$a) | .version' <<<"$state_json")"
}

bootstrap_scope_fixture_if_needed() {
	detect_versions
	if [ -n "$SCOPE_CURRENT" ] && [ -n "$PIN_CURRENT" ]; then
		ok "benchmark resources already exist in trunk: $SCOPE_ADDR=$SCOPE_CURRENT, $PIN_ADDR=$PIN_CURRENT"
		return
	fi

	local bootstrap_scope bootstrap_pin
	bootstrap_scope="${SCOPE_CURRENT:-v1}"
	bootstrap_pin="${PIN_CURRENT:-v1}"

	step "bootstrap benchmark resources"
	info "bootstrapping $SCOPE_ADDR=$bootstrap_scope and $PIN_ADDR=$bootstrap_pin"
	(
		cd "$HERE"
		"$TF_BIN" apply \
			-auto-approve \
			-input=false \
			-refresh=false \
			-target="$SCOPE_ADDR" \
			-target="$PIN_ADDR" \
			-var="slow_a_version=$bootstrap_scope" \
			-var="slow_b_version=$bootstrap_pin"
	)
	detect_versions
	[ -n "$SCOPE_CURRENT" ] && [ -n "$PIN_CURRENT" ] || fail "failed to bootstrap benchmark resources"
	ok "bootstrapped benchmark resources: $SCOPE_ADDR=$SCOPE_CURRENT, $PIN_ADDR=$PIN_CURRENT"
}

fetch_full_state() {
	local url="${BACKEND_ADDRESS}"
	if [ -z "$url" ]; then
		return 0
	fi
	if [ -n "$BACKEND_USERNAME" ] || [ -n "$BACKEND_PASSWORD" ]; then
		curl -fsS -u "$BACKEND_USERNAME:$BACKEND_PASSWORD" "$url" -o "$FULL_STATE_PATH"
	else
		curl -fsS "$url" -o "$FULL_STATE_PATH"
	fi
}

bytes_of() {
	wc -c <"$1" | tr -d '[:space:]'
}

target_value() {
	local prefix="$1"
	echo "${prefix}-${RANDOM}"
}

render_case_summary() {
	local label="$1" spec_path="$2" wall_ms="$3"
	local mode cache_hit confidence resolve_ms expand_ms fetch_ms server_expand_ms server_fetch_ms
	local slice_bytes slice_resources full_bytes realized edges scanned walked config_nodes module_selectors
	local fetch_count write_count read_count slice_requested slice_materialized

	mode="$(jq -r '.state_engine.mode // ""' "$spec_path")"
	cache_hit="$(jq -r '.state_engine.graph_cache_hit // false' "$spec_path")"
	confidence="$(jq -r '.state_engine.confidence // ""' "$spec_path")"
	resolve_ms="$(jq -r '.state_engine.resolve_duration_ms // 0' "$spec_path")"
	expand_ms="$(jq -r '.state_engine.expand_duration_ms // 0' "$spec_path")"
	fetch_ms="$(jq -r '.state_engine.slice_fetch_duration_ms // 0' "$spec_path")"
	server_expand_ms="$(jq -r '.state_engine.server_expand_duration_ms // 0' "$spec_path")"
	server_fetch_ms="$(jq -r '.state_engine.server_slice_duration_ms // 0' "$spec_path")"
	slice_bytes="$(jq -r '.state_engine.slice_bytes // 0' "$spec_path")"
	slice_resources="$(jq -r '.state_engine.slice_resource_count // 0' "$spec_path")"
	realized="$(jq -r '.state_engine.realized_resource_count // 0' "$spec_path")"
	edges="$(jq -r '.state_engine.dependency_edge_count // 0' "$spec_path")"
	scanned="$(jq -r '.state_engine.inventory_scan_count // 0' "$spec_path")"
	walked="$(jq -r '.state_engine.walked_node_count // 0' "$spec_path")"
	config_nodes="$(jq -r '.state_engine.config_node_count // 0' "$spec_path")"
	module_selectors="$(jq -r '.state_engine.module_selector_count // 0' "$spec_path")"
	fetch_count="$(jq -r '.state_engine.fetch_address_count // 0' "$spec_path")"
	write_count="$(jq -r '.state_engine.write_address_count // 0' "$spec_path")"
	read_count="$(jq -r '.state_engine.read_address_count // 0' "$spec_path")"
	slice_requested="$(jq -r '.state_engine.slice_requested_count // 0' "$spec_path")"
	slice_materialized="$(jq -r '.state_engine.slice_materialized_count // 0' "$spec_path")"
	if [ -f "$FULL_STATE_PATH" ]; then
		full_bytes="$(bytes_of "$FULL_STATE_PATH")"
	else
		full_bytes="0"
	fi

	printf "%s%s%s\n" "$BOLD" "$label" "$RESET"
	info "mode=$mode confidence=${confidence:-n/a} cache_hit=$cache_hit"
	info "wall=${wall_ms}ms client(resolve=${resolve_ms}ms expand=${expand_ms}ms fetch=${fetch_ms}ms)"
	info "server(expand=${server_expand_ms}ms fetch=${server_fetch_ms}ms)"
	info "graph(realized=$realized edges=$edges scanned=$scanned walked=$walked config=$config_nodes modules=$module_selectors)"
	info "scope(fetch=$fetch_count write=$write_count read=$read_count)"
	info "slice(requested=$slice_requested materialized=$slice_materialized resources=$slice_resources bytes=$slice_bytes full_bytes=$full_bytes)"
}

render_backend_case_summary() {
	local label="$1" spec_path="$2" wall_ms="$3" full_bytes
	if [ -f "$FULL_STATE_PATH" ]; then
		full_bytes="$(bytes_of "$FULL_STATE_PATH")"
	else
		full_bytes="0"
	fi

	printf "%s%s%s\n" "$BOLD" "$label" "$RESET"
	info "mode=terraform-http full-trunk"
	info "wall=${wall_ms}ms"
	info "write_set=$(jq -r '.write_set | length' "$spec_path") read_set=$(jq -r '.read_set | length' "$spec_path") reservations=$(jq -r '.reservations | length' "$spec_path")"
	info "full_state_bytes=$full_bytes"
}

render_terraform_case_summary() {
	local label="$1" wall_ms="$2" full_bytes
	if [ -f "$FULL_STATE_PATH" ]; then
		full_bytes="$(bytes_of "$FULL_STATE_PATH")"
	else
		full_bytes="0"
	fi

	printf "%s%s%s\n" "$BOLD" "$label" "$RESET"
	info "mode=terraform target over full trunk"
	info "wall=${wall_ms}ms"
	info "full_state_bytes=$full_bytes"
}

run_backend_plan_case() {
	local label="$1" spec_path="$2" log_path="$3" new_scope="$4"
	local started ended wall_ms
	started="$(now_ms)"
	(
		cd "$HERE"
		run_kl_backend plan \
			--no-refresh \
			--no-lock \
			--file="$SCOPE_FILE" \
			--var="slow_a_version=$new_scope" \
			--var="slow_b_version=$PIN_CURRENT" \
			--out="$spec_path" \
			"$HERE"
	) >"$log_path" 2>&1
	ended="$(now_ms)"
	wall_ms=$(( ended - started ))

	if ! jq -e --arg addr "$SCOPE_ADDR" '.write_set == [$addr]' "$spec_path" >/dev/null; then
		fail "$label: expected write_set to be exactly [$SCOPE_ADDR]"
	fi
	if jq -e '.state_engine != null' "$spec_path" >/dev/null; then
		fail "$label: expected no state_engine metadata in terraform-http mode"
	fi

	render_backend_case_summary "$label" "$spec_path" "$wall_ms"
}

run_terraform_target_case() {
	local label="$1" log_path="$2" new_scope="$3"
	local started ended wall_ms
	started="$(now_ms)"
	(
		cd "$HERE"
		"$TF_BIN" plan \
			-input=false \
			-lock=false \
			-refresh=false \
			-target="$SCOPE_ADDR" \
			-var="slow_a_version=$new_scope" \
			-var="slow_b_version=$PIN_CURRENT" \
			-out="$PLAN_TERRAFORM_OUT"
	) >"$log_path" 2>&1
	ended="$(now_ms)"
	wall_ms=$(( ended - started ))

	if ! grep -q "Saved the plan to:" "$log_path"; then
		fail "$label: terraform did not produce a saved plan"
	fi

	render_terraform_case_summary "$label" "$wall_ms"
}

run_plan_case() {
	local label="$1" spec_path="$2" log_path="$3" new_scope="$4"
	local started ended wall_ms
	started="$(now_ms)"
	(
		cd "$HERE"
		run_kl plan \
			--no-refresh \
			--no-lock \
			--file="$SCOPE_FILE" \
			--var="slow_a_version=$new_scope" \
			--var="slow_b_version=$PIN_CURRENT" \
			--out="$spec_path" \
			"$HERE"
	) >"$log_path" 2>&1
	ended="$(now_ms)"
	wall_ms=$(( ended - started ))

	if ! jq -e --arg addr "$SCOPE_ADDR" '.write_set == [$addr]' "$spec_path" >/dev/null; then
		fail "$label: expected write_set to be exactly [$SCOPE_ADDR]"
	fi
	if ! jq -e '.state_engine.confidence == "safe"' "$spec_path" >/dev/null; then
		fail "$label: expected state-engine confidence=safe"
	fi

	render_case_summary "$label" "$spec_path" "$wall_ms"
}

run_apply_case() {
	local new_scope started ended wall_ms
	new_scope="$(target_value bench-apply)"

	step "build apply benchmark spec"
	(
		cd "$HERE"
		run_kl plan \
			--no-refresh \
			--no-lock \
			--file="$SCOPE_FILE" \
			--var="slow_a_version=$new_scope" \
			--var="slow_b_version=$PIN_CURRENT" \
			--out="$PLAN_APPLY_SPEC" \
			"$HERE"
	) >"$APPLY_LOG" 2>&1

	if ! jq -e --arg addr "$SCOPE_ADDR" '.write_set == [$addr]' "$PLAN_APPLY_SPEC" >/dev/null; then
		fail "apply benchmark: expected write_set to be exactly [$SCOPE_ADDR]"
	fi

	step "run scoped apply benchmark"
	started="$(now_ms)"
	(
		cd "$HERE"
		run_kl apply --plan-spec "$PLAN_APPLY_SPEC"
	) >>"$APPLY_LOG" 2>&1
	ended="$(now_ms)"
	wall_ms=$(( ended - started ))

	if ! grep -q 'commit mode:[[:space:]]*state-engine delta' "$APPLY_LOG"; then
		fail "apply benchmark: expected state-engine delta commit"
	fi
	if ! grep -q 'Native intent source: terraform validation replan' "$APPLY_LOG"; then
		fail "apply benchmark: expected native intent source"
	fi

	printf "%s%s%s\n" "$BOLD" "apply" "$RESET"
	info "wall=${wall_ms}ms"
	info "artifacts: spec=$PLAN_APPLY_SPEC log=$APPLY_LOG"
	ok "scoped apply committed through the trusted state-engine delta lane"
}

usage() {
	cat <<EOF
${BOLD}state-engine-benchmark.sh — repeatable ADR29 benchmark${RESET}

Usage:
  examples/big-state/state-engine-benchmark.sh ${CYAN}plans${RESET}
      Compare three planning lanes on the same scoped change:
      1. plain Terraform target over full trunk
      2. KL scoped plan over terraform-http full trunk
      3. KL state-engine scoped plan (cold + warm). Default mode.

  examples/big-state/state-engine-benchmark.sh ${CYAN}apply${RESET}
      Build one scoped plan and measure one real trusted-lane apply.

  examples/big-state/state-engine-benchmark.sh ${CYAN}all${RESET}
      Run both plan and apply measurements.

What it measures:
  - end-to-end wall clock
  - full-trunk versus native-slice payload context
  - client timings: resolve / expand / fetch
  - server timings: expand / slice materialization
  - graph diagnostics: realized rows / dependency edges / walked nodes / scans
  - scope and slice counts

Useful env vars:
  KL_BIN               default: \$REPO_ROOT/bin/kl
  TF_BIN               default: terraform
  STATE_NAME           override discovered state name
  CACHE_EXPIRE_WAIT    seconds to wait so the graph cache goes cold (default: $CACHE_EXPIRE_WAIT)
  KEEP_ARTIFACTS=1     keep generated specs/logs under /tmp
EOF
}

main() {
	case "$MODE" in
		help|-h|--help)
			usage
			exit 0
			;;
		plans|apply|all)
			;;
		*)
			fail "unknown mode: $MODE"
			;;
	esac

	preflight
	step "checking benchmark environment"
	info "state=$STATE_NAME"
	info "backend=${BACKEND_ADDRESS:-<discovered by terraform init>}"
	info "protocol=$KL_PROTOCOL_VALUE"
	info "scope file=$SCOPE_FILE"

	bootstrap_scope_fixture_if_needed
	fetch_full_state || warn "could not fetch full backend state payload for byte comparison"

	if [ "$MODE" = "plans" ] || [ "$MODE" = "all" ]; then
		detect_versions
		run_terraform_target_case "terraform target lane" "$PLAN_TERRAFORM_LOG" "$(target_value bench-tf)"
		run_backend_plan_case "kl scoped full-trunk lane" "$PLAN_BACKEND_SPEC" "$PLAN_BACKEND_LOG" "$(target_value bench-backend)"
		step "wait for graph cache expiry"
		info "sleeping ${CACHE_EXPIRE_WAIT}s so the next scoped plan should be cold"
		sleep "$CACHE_EXPIRE_WAIT"
		detect_versions
		run_plan_case "cold plan" "$PLAN_COLD_SPEC" "$PLAN_COLD_LOG" "$(target_value bench-cold)"
		run_plan_case "warm plan" "$PLAN_WARM_SPEC" "$PLAN_WARM_LOG" "$(target_value bench-warm)"
		ok "lane comparison benchmark complete"
		info "artifacts: $PLAN_TERRAFORM_OUT $PLAN_BACKEND_SPEC $PLAN_COLD_SPEC $PLAN_WARM_SPEC"
	fi

	if [ "$MODE" = "apply" ] || [ "$MODE" = "all" ]; then
		detect_versions
		run_apply_case
	fi
}

main "$@"
