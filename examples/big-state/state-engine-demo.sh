#!/usr/bin/env bash
# state-engine-demo.sh — local POC demo for the state-engine protocol.
#
# Supported demos:
#   1. scope  - proves backend-assisted mixed graph closure for `kl plan -f`
#               when the selected file depends on an undeployed support node
#               in another file, and that support node depends on realized state
#               plus removed-config classification for addresses no longer present,
#               that a selected local module file widens to `module.*` writes,
#               and that a plain `kl apply -f ...` can commit via the trusted
#               state-engine delta lane
#   2. lanes  - proves the runtime lane split:
#               proven-safe native slice => trusted state-engine delta lane
#               full-trunk-fallback     => broader backend/full-trunk lane
#   2. native - proves native `kl state mv` / `kl state rm` /
#               `kl rollback resource`
#   4. all    - runs all demos (default)
#
# Requirements:
#   - `make build` has produced `bin/kl`
#   - `terraform init` has been run in this directory
#   - the backend/runtime is reachable
#   - `jq` is installed

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
KL_BIN="${KL_BIN:-$REPO_ROOT/bin/kl}"
TERRAFORM_BIN="${TERRAFORM_BIN:-terraform}"
PLAN_FILE="${PLAN_FILE:-/tmp/kl-state-engine-scope.plan.json}"
TRUSTED_PLAN_FILE="${TRUSTED_PLAN_FILE:-/tmp/kl-state-engine-trusted.plan.json}"
FALLBACK_PLAN_FILE="${FALLBACK_PLAN_FILE:-/tmp/kl-state-engine-fallback.plan.json}"
UPDATE_PLAN_FILE="${UPDATE_PLAN_FILE:-/tmp/kl-state-engine-update.plan.json}"
SCOPE_EXPAND_JSON="${SCOPE_EXPAND_JSON:-/tmp/kl-state-engine-scope-expand.json}"
SCOPE_SLICE_JSON="${SCOPE_SLICE_JSON:-/tmp/kl-state-engine-scope-slice.json}"
FULL_STATE_JSON="${FULL_STATE_JSON:-/tmp/kl-state-engine-full-state.json}"
STATE_NAME="${STATE_NAME:-}"
BACKEND_ADDRESS="${TF_HTTP_ADDRESS:-}"
API_TOKEN="${API_TOKEN:-${KL_TOKEN:-${TF_HTTP_PASSWORD:-}}}"

if [ "${MOVE_SOURCE+x}" = "x" ]; then
	MOVE_SOURCE_EXPLICIT=1
else
	MOVE_SOURCE_EXPLICIT=0
fi
if [ "${REMOVE_ADDRESS+x}" = "x" ]; then
	REMOVE_ADDRESS_EXPLICIT=1
else
	REMOVE_ADDRESS_EXPLICIT=0
fi
MOVE_SOURCE="${MOVE_SOURCE:-time_sleep.slow_a}"
MOVE_TARGET="${MOVE_TARGET:-module.native_demo.time_sleep.slow_a}"
REMOVE_ADDRESS="${REMOVE_ADDRESS:-time_sleep.slow_b}"

SCOPE_LEAF_FILE="${SCOPE_LEAF_FILE:-state_engine_leaf_demo.tf}"
SCOPE_SUPPORT_FILE="${SCOPE_SUPPORT_FILE:-state_engine_support_demo.tf}"
SCOPE_BRIDGE_FILE="${SCOPE_BRIDGE_FILE:-state_engine_bridge_demo.tf}"
SCOPE_REMOVED_FILE="${SCOPE_REMOVED_FILE:-state_engine_removed_demo.tf}"
MODULE_SCOPE_FILE="${MODULE_SCOPE_FILE:-state_engine_module_scope_demo.tf}"
MODULE_SCOPE_DIR="${MODULE_SCOPE_DIR:-state_engine_module_scope_demo_module}"
NATIVE_DEMO_FILE="${NATIVE_DEMO_FILE:-state_engine_native_demo.tf}"
SCOPE_LEAF_ADDR="${SCOPE_LEAF_ADDR:-null_resource.state_engine_leaf_demo}"
SCOPE_SUPPORT_ADDR="${SCOPE_SUPPORT_ADDR:-null_resource.state_engine_support_demo}"
SCOPE_BRIDGE_ADDR="${SCOPE_BRIDGE_ADDR:-null_resource.state_engine_bridge_demo}"
SCOPE_REALIZED_DEP="${SCOPE_REALIZED_DEP:-random_pet.deployment_name}"
SCOPE_REMOVED_ADDR="${SCOPE_REMOVED_ADDR:-null_resource.state_engine_removed_demo}"
MODULE_SCOPE_ADDR="${MODULE_SCOPE_ADDR:-module.state_engine_demo.null_resource.member}"
SCOPE_LEAF_MARKER="${SCOPE_LEAF_MARKER:-leaf-demo-$RANDOM}"
NATIVE_DEMO_ADDR="${NATIVE_DEMO_ADDR:-null_resource.state_engine_native_demo}"
NATIVE_DEMO_TARGET="${NATIVE_DEMO_TARGET:-module.native_demo.null_resource.state_engine_native_demo}"
SLOW_A_SCOPE_FILE="${SLOW_A_SCOPE_FILE:-slow_a.tf}"
SLOW_A_SCOPE_ADDR="${SLOW_A_SCOPE_ADDR:-time_sleep.slow_a}"
SLOW_B_SCOPE_ADDR="${SLOW_B_SCOPE_ADDR:-time_sleep.slow_b}"

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
fail() { printf "%s    ✗ %s%s\n" "$RED" "$*" "$RESET"; }

cleanup_paths=()
cleanup() {
	local path
	for path in "${cleanup_paths[@]:-}"; do
		rm -rf "$path"
	done
}
trap cleanup EXIT

add_cleanup() {
	cleanup_paths+=("$1")
}

bytes_of() {
	wc -c <"$1" | tr -d '[:space:]'
}

print_size_comparison() {
	local full_file="$1"
	local slice_file="$2"
	local full_bytes slice_bytes saved pct
	full_bytes="$(bytes_of "$full_file")"
	slice_bytes="$(bytes_of "$slice_file")"
	saved=$(( full_bytes - slice_bytes ))
	if [ "$full_bytes" -gt 0 ]; then
		pct=$(( (slice_bytes * 100) / full_bytes ))
	else
		pct=0
	fi
	info "vanilla TF full state payload: ${full_bytes} bytes"
	info "KL state-engine slice JSON:   ${slice_bytes} bytes"
	if [ "$saved" -ge 0 ]; then
		ok "slice is smaller by ${saved} bytes (${pct}% of full payload kept)"
	else
		warn "slice payload is larger than full state by $(( -saved )) bytes"
	fi
}

run_kl() {
	KL_PROTOCOL=state-engine \
	KL_CONFIG_DISCOVERY="${KL_CONFIG_DISCOVERY:-opentofu}" \
	"$KL_BIN" "$@"
}

api_base() {
	printf '%s\n' "${BACKEND_ADDRESS%/v1/states/*}"
}

api_json() {
	local method="$1"
	local url="$2"
	local body="${3:-}"
	shift 3 || true
	local -a curl_args
	curl_args=(-fsS -X "$method" "$url" -H "Content-Type: application/json")
	if [ -n "$API_TOKEN" ]; then
		curl_args+=(-H "Authorization: Bearer $API_TOKEN")
	fi
	if [ -n "$body" ]; then
		curl_args+=(--data "$body")
	fi
	curl "${curl_args[@]}"
}

discover_backend() {
	local backend_state="$HERE/.terraform/terraform.tfstate"
	[ -f "$backend_state" ] || return 0
	local discovered=""
	discovered="$(jq -r '(.backend.config.address // "")' "$backend_state" 2>/dev/null || true)"
	if [ -z "$BACKEND_ADDRESS" ] && [ -n "$discovered" ] && [ "$discovered" != "null" ]; then
		BACKEND_ADDRESS="$discovered"
	fi
	if [ -z "$STATE_NAME" ] && [ -n "$BACKEND_ADDRESS" ]; then
		STATE_NAME="$(printf '%s\n' "$BACKEND_ADDRESS" | sed -E 's#^.*/v1/states/##')"
	fi
	if [ -z "$STATE_NAME" ]; then
		STATE_NAME="big-state"
	fi
}

preflight() {
	[ -x "$KL_BIN" ] || { fail "kl binary not at $KL_BIN (run \`make build\`)"; exit 1; }
	command -v jq >/dev/null || { fail "jq is required"; exit 1; }
	[ -d "$HERE/.terraform" ] || { fail "run \`terraform init\` in $HERE first"; exit 1; }
	discover_backend
	if ! "$KL_BIN" list >/dev/null 2>&1; then
		fail "kl cannot reach the runtime API. Make sure the backend is reachable."
		exit 1
	fi
}

query_exists() {
	local address="$1"
	if run_kl query resource --format json --address "$address" "$STATE_NAME" >/tmp/kl-state-engine-demo-resource.json 2>/dev/null; then
		return 0
	fi
	return 1
}

detect_slow_versions() {
	local state_sql state_json
	state_sql="
SELECT address, COALESCE(attributes->'triggers'->>'version', '') AS version
FROM current_resources
WHERE state_name = '$STATE_NAME'
  AND address IN ('$SLOW_A_SCOPE_ADDR', '$SLOW_B_SCOPE_ADDR')
ORDER BY address;
"
	state_json="$("$KL_BIN" query --format json "$state_sql")"
	SLOW_A_CURRENT="$(jq -r --arg a "$SLOW_A_SCOPE_ADDR" '.[] | select(.address==$a) | .version' <<<"$state_json")"
	SLOW_B_CURRENT="$(jq -r --arg a "$SLOW_B_SCOPE_ADDR" '.[] | select(.address==$a) | .version' <<<"$state_json")"
}

scoped_update_target() {
	echo "state-engine-$RANDOM"
}

bootstrap_slow_scope_fixture_if_needed() {
	detect_slow_versions
	if [ -n "$SLOW_A_CURRENT" ] && [ -n "$SLOW_B_CURRENT" ]; then
		ok "slow scoped demo resources already exist in trunk: $SLOW_A_SCOPE_ADDR, $SLOW_B_SCOPE_ADDR"
		return
	fi

	local bootstrap_a bootstrap_b
	bootstrap_a="${SLOW_A_CURRENT:-v1}"
	bootstrap_b="${SLOW_B_CURRENT:-v1}"

	step "bootstrap slow scoped demo resources"
	info "bootstrapping $SLOW_A_SCOPE_ADDR=$bootstrap_a and $SLOW_B_SCOPE_ADDR=$bootstrap_b"
	(
		cd "$HERE"
		"$TERRAFORM_BIN" apply \
			-auto-approve \
			-input=false \
			-refresh=false \
			-target="$SLOW_A_SCOPE_ADDR" \
			-target="$SLOW_B_SCOPE_ADDR" \
			-var="slow_a_version=$bootstrap_a" \
			-var="slow_b_version=$bootstrap_b"
	)
	detect_slow_versions
	if [ -z "$SLOW_A_CURRENT" ] || [ -z "$SLOW_B_CURRENT" ]; then
		fail "failed to bootstrap slow scoped demo resources"
		exit 1
	fi
	ok "bootstrapped slow scoped demo resources: $SLOW_A_SCOPE_ADDR=$SLOW_A_CURRENT, $SLOW_B_SCOPE_ADDR=$SLOW_B_CURRENT"
}

current_serial() {
	local serial
	serial="$(run_kl status --format json "$STATE_NAME" | jq -r '(.CurrentSerial // .current_serial // .serial // empty)')"
	if [ -z "$serial" ] || [ "$serial" = "null" ]; then
		printf '    ✗ unable to determine current serial from `kl status --format json`\n' >&2
		return 1
	fi
	printf '%s\n' "$serial"
}

resolve_native_fixture_addresses() {
	if [ "$MOVE_SOURCE_EXPLICIT" != "1" ] && ! query_exists "$MOVE_SOURCE"; then
		if query_exists "time_sleep.slow_b"; then
			MOVE_SOURCE="time_sleep.slow_b"
			MOVE_TARGET="module.native_demo.time_sleep.slow_b"
		elif query_exists "$NATIVE_DEMO_ADDR"; then
			MOVE_SOURCE="$NATIVE_DEMO_ADDR"
			MOVE_TARGET="$NATIVE_DEMO_TARGET"
		fi
	fi
	if [ "$REMOVE_ADDRESS_EXPLICIT" != "1" ] && ! query_exists "$REMOVE_ADDRESS"; then
		local candidate
		for candidate in time_sleep.slow_b time_sleep.slow_a "$MOVE_SOURCE" "$NATIVE_DEMO_ADDR"; do
			if query_exists "$candidate"; then
				REMOVE_ADDRESS="$candidate"
				break
			fi
		done
	fi
}

write_native_demo_resource_file() {
	cat >"$HERE/$NATIVE_DEMO_FILE" <<EOF
resource "null_resource" "state_engine_native_demo" {
  triggers = {
    marker = "state-engine-native-demo"
  }
}
EOF
}

bootstrap_native_demo_resource() {
	local native_path="$HERE/$NATIVE_DEMO_FILE"
	if [ -e "$native_path" ]; then
		fail "temporary native demo file already exists; remove or rename:"
		info "$native_path"
		exit 1
	fi
	add_cleanup "$native_path"
	write_native_demo_resource_file
	step "bootstrap native mutation demo resource"
	(
		cd "$HERE"
		"$TERRAFORM_BIN" apply -auto-approve -input=false -target="$NATIVE_DEMO_ADDR"
	)
	if ! query_exists "$NATIVE_DEMO_ADDR"; then
		fail "failed to bootstrap native demo resource $NATIVE_DEMO_ADDR"
		exit 1
	fi
	ok "bootstrapped native demo resource: $NATIVE_DEMO_ADDR"
}

ensure_native_fixture_present() {
	resolve_native_fixture_addresses
	if [ "$MOVE_SOURCE_EXPLICIT" != "1" ] && [ "$REMOVE_ADDRESS_EXPLICIT" != "1" ] && ! query_exists "$MOVE_SOURCE" && ! query_exists "$REMOVE_ADDRESS"; then
		bootstrap_native_demo_resource
		MOVE_SOURCE="$NATIVE_DEMO_ADDR"
		MOVE_TARGET="$NATIVE_DEMO_TARGET"
		REMOVE_ADDRESS="$NATIVE_DEMO_ADDR"
	fi
	resolve_native_fixture_addresses
	if ! query_exists "$MOVE_SOURCE"; then
		fail "$MOVE_SOURCE is missing from trunk"
		exit 1
	fi
	if ! query_exists "$REMOVE_ADDRESS"; then
		fail "$REMOVE_ADDRESS is missing from trunk"
		exit 1
	fi
	ok "verified native move source exists: $MOVE_SOURCE"
	ok "verified native remove target exists: $REMOVE_ADDRESS"
}

ensure_scope_fixture_present() {
	if ! query_exists "$SCOPE_REALIZED_DEP"; then
		fail "$SCOPE_REALIZED_DEP is missing from trunk"
		exit 1
	fi
	ok "verified realized dependency exists: $SCOPE_REALIZED_DEP"
}

looks_like_scope_demo_file() {
	local path="$1"
	[ -f "$path" ] || return 1
	case "$(basename "$path")" in
		"$SCOPE_LEAF_FILE")
			grep -q "$SCOPE_SUPPORT_ADDR" "$path" && grep -q 'null_resource" "state_engine_leaf_demo"' "$path"
			;;
		"$SCOPE_SUPPORT_FILE")
			grep -q "$SCOPE_REALIZED_DEP" "$path" && grep -q 'null_resource" "state_engine_support_demo"' "$path"
			;;
		"$SCOPE_BRIDGE_FILE")
			grep -q 'null_resource" "state_engine_bridge_demo"' "$path"
			;;
		"$SCOPE_REMOVED_FILE")
			grep -q "$SCOPE_REMOVED_ADDR" "$path"
			;;
		"$MODULE_SCOPE_FILE")
			grep -q 'module "state_engine_demo"' "$path" && grep -q "$MODULE_SCOPE_DIR" "$path"
			;;
		*)
			return 1
			;;
	esac
}

cleanup_stale_scope_demo_files_if_safe() {
	local leaf_path="$HERE/$SCOPE_LEAF_FILE"
	local support_path="$HERE/$SCOPE_SUPPORT_FILE"
	local bridge_path="$HERE/$SCOPE_BRIDGE_FILE"
	local removed_path="$HERE/$SCOPE_REMOVED_FILE"
	local module_file_path="$HERE/$MODULE_SCOPE_FILE"
	local module_dir_path="$HERE/$MODULE_SCOPE_DIR"
	local existing=()
	local path

	for path in "$leaf_path" "$support_path" "$bridge_path" "$removed_path" "$module_file_path" "$module_dir_path"; do
		if [ -e "$path" ]; then
			existing+=("$path")
		fi
	done
	[ "${#existing[@]}" -gt 0 ] || return 0

	for path in "${existing[@]}"; do
		if [ "$path" = "$module_dir_path" ]; then
			if [ ! -f "$module_dir_path/main.tf" ] || ! grep -q 'null_resource" "member"' "$module_dir_path/main.tf"; then
				fail "temporary scope demo files already exist and do not look like safe demo leftovers:"
				info "$path"
				exit 1
			fi
			continue
		fi
		if ! looks_like_scope_demo_file "$path"; then
			fail "temporary scope demo files already exist and do not look like safe demo leftovers:"
			info "$path"
			exit 1
		fi
	done

	warn "found stale scope demo files from a previous run; cleaning them up automatically"
	for path in "${existing[@]}"; do
		rm -rf "$path"
		ok "removed stale demo path: $(basename "$path")"
	done
}

create_scope_demo_files() {
	local leaf_path="$HERE/$SCOPE_LEAF_FILE"
	local support_path="$HERE/$SCOPE_SUPPORT_FILE"
	local bridge_path="$HERE/$SCOPE_BRIDGE_FILE"
	local removed_path="$HERE/$SCOPE_REMOVED_FILE"
	local module_file_path="$HERE/$MODULE_SCOPE_FILE"
	local module_dir_path="$HERE/$MODULE_SCOPE_DIR"
	cleanup_stale_scope_demo_files_if_safe
	if [ -e "$leaf_path" ] || [ -e "$support_path" ] || [ -e "$bridge_path" ] || [ -e "$removed_path" ] || [ -e "$module_file_path" ] || [ -e "$module_dir_path" ]; then
		fail "temporary scope demo files already exist; remove or rename:"
		info "$leaf_path"
		info "$support_path"
		info "$bridge_path"
		info "$removed_path"
		info "$module_file_path"
		info "$module_dir_path"
		exit 1
	fi
	add_cleanup "$leaf_path"
	add_cleanup "$support_path"
	add_cleanup "$bridge_path"
	add_cleanup "$removed_path"
	add_cleanup "$module_file_path"
	add_cleanup "$module_dir_path"
	add_cleanup "$PLAN_FILE"
	add_cleanup "$TRUSTED_PLAN_FILE"
	add_cleanup "$FALLBACK_PLAN_FILE"
	add_cleanup "$UPDATE_PLAN_FILE"
	add_cleanup "$SCOPE_EXPAND_JSON"
	add_cleanup "$SCOPE_SLICE_JSON"
	add_cleanup "$FULL_STATE_JSON"

	cat >"$support_path" <<EOF
resource "null_resource" "state_engine_support_demo" {
  triggers = {
    deployment = ${SCOPE_REALIZED_DEP}.id
  }
}
EOF

	cat >"$bridge_path" <<EOF
resource "null_resource" "state_engine_bridge_demo" {
  triggers = {
    marker = "state-engine-bridge-demo"
  }
}
EOF

	write_scope_leaf_file

	ok "wrote temporary support file: $SCOPE_SUPPORT_FILE"
	ok "wrote temporary bridge file: $SCOPE_BRIDGE_FILE"
	ok "wrote temporary selected file: $SCOPE_LEAF_FILE"
	ok "reserved temporary removed file: $SCOPE_REMOVED_FILE"

	mkdir -p "$module_dir_path"
	cat >"$module_dir_path/main.tf" <<EOF
variable "value" {
  type = string
}

resource "null_resource" "member" {
  triggers = {
    value = var.value
  }
}
EOF

	cat >"$module_file_path" <<EOF
module "state_engine_demo" {
  source = "./$MODULE_SCOPE_DIR"
  value  = "module-demo-v1"
}
EOF

	ok "wrote temporary module scope file: $MODULE_SCOPE_FILE"
	ok "wrote temporary module source: $MODULE_SCOPE_DIR/main.tf"

	(
		cd "$HERE"
		"$TERRAFORM_BIN" init -input=false >/dev/null
	)
	ok "refreshed terraform init for temporary demo modules"
}

write_scope_support_file_with_bridge_dep() {
	cat >"$HERE/$SCOPE_SUPPORT_FILE" <<EOF
resource "null_resource" "state_engine_support_demo" {
  depends_on = [${SCOPE_BRIDGE_ADDR}]

  triggers = {
    deployment = ${SCOPE_REALIZED_DEP}.id
  }
}
EOF
}

write_scope_support_file_with_bridge_trigger() {
	cat >"$HERE/$SCOPE_SUPPORT_FILE" <<EOF
resource "null_resource" "state_engine_support_demo" {
  triggers = {
    deployment = ${SCOPE_REALIZED_DEP}.id
    bridge     = ${SCOPE_BRIDGE_ADDR}.id
  }
}
EOF
}

write_scope_leaf_file() {
	cat >"$HERE/$SCOPE_LEAF_FILE" <<EOF
resource "null_resource" "state_engine_leaf_demo" {
  triggers = {
    support = ${SCOPE_SUPPORT_ADDR}.id
    marker  = "$SCOPE_LEAF_MARKER"
  }
}
EOF
}

bootstrap_scope_support_resource() {
	if query_exists "$SCOPE_SUPPORT_ADDR"; then
		ok "scope support resource already exists in trunk: $SCOPE_SUPPORT_ADDR"
		return
	fi

	step "bootstrap realized support resource"
	(
		cd "$HERE"
		"$TERRAFORM_BIN" apply -auto-approve -input=false -target="$SCOPE_SUPPORT_ADDR"
	)
	if ! query_exists "$SCOPE_SUPPORT_ADDR"; then
		fail "failed to bootstrap support resource $SCOPE_SUPPORT_ADDR"
		exit 1
	fi
	ok "bootstrapped realized support resource: $SCOPE_SUPPORT_ADDR"
}

write_removed_demo_resource_file() {
	cat >"$HERE/$SCOPE_REMOVED_FILE" <<EOF
resource "null_resource" "state_engine_removed_demo" {
  triggers = {
    marker = "state-engine-demo"
  }
}
EOF
}

write_removed_demo_removed_block() {
	cat >"$HERE/$SCOPE_REMOVED_FILE" <<EOF
removed {
  from = ${SCOPE_REMOVED_ADDR}
}
EOF
}

bootstrap_removed_demo_resource() {
	if query_exists "$SCOPE_REMOVED_ADDR"; then
		ok "removed-config demo resource already exists in trunk: $SCOPE_REMOVED_ADDR"
		write_removed_demo_removed_block
		return
	fi

	step "bootstrap removable demo resource"
	write_removed_demo_resource_file
	(
		cd "$HERE"
		"$TERRAFORM_BIN" apply -auto-approve -input=false -target="$SCOPE_REMOVED_ADDR"
	)
	if ! query_exists "$SCOPE_REMOVED_ADDR"; then
		fail "failed to bootstrap removable demo resource $SCOPE_REMOVED_ADDR"
		exit 1
	fi
	ok "bootstrapped removable demo resource: $SCOPE_REMOVED_ADDR"
	write_removed_demo_removed_block
	ok "rewrote temporary removed file to a removed block"
}

write_module_scope_file() {
	local value="$1"
	cat >"$HERE/$MODULE_SCOPE_FILE" <<EOF
module "state_engine_demo" {
  source = "./$MODULE_SCOPE_DIR"
  value  = "$value"
}
EOF
}

bootstrap_module_scope_resource() {
	if query_exists "$MODULE_SCOPE_ADDR"; then
		ok "module scope demo resource already exists in trunk: $MODULE_SCOPE_ADDR"
		return
	fi

	step "bootstrap module-scoped demo resource"
	write_module_scope_file "module-demo-v1"
	(
		cd "$HERE"
		"$TERRAFORM_BIN" init -input=false >/dev/null
		"$TERRAFORM_BIN" apply -auto-approve -input=false -target=module.state_engine_demo
	)
	if ! query_exists "$MODULE_SCOPE_ADDR"; then
		fail "failed to bootstrap module-scoped demo resource $MODULE_SCOPE_ADDR"
		exit 1
	fi
	ok "bootstrapped module-scoped demo resource: $MODULE_SCOPE_ADDR"
}

capture_scope_payload_sizes() {
	local base serial expand_body fetch_addresses_json slice_body
	base="$(api_base)"
	api_json GET "$BACKEND_ADDRESS" "" >"$FULL_STATE_JSON"
	serial="$(api_json POST "$base/v1/state-engine/state/resolve" "{\"state\":\"$STATE_NAME\"}" | jq -r '.serial')"
	expand_body="$(cat <<EOF
{"state":"$STATE_NAME","selectors":[{"kind":"resource_address","value":"$SCOPE_LEAF_ADDR"}],"client_context":{"explicit_write_candidates":["$SCOPE_LEAF_ADDR"],"explicit_read_candidates":["$SCOPE_SUPPORT_ADDR","$SCOPE_REALIZED_DEP"],"undeployed_config_candidates":["$SCOPE_LEAF_ADDR","$SCOPE_SUPPORT_ADDR"],"config_nodes":[{"address":"$SCOPE_LEAF_ADDR","dependencies":["$SCOPE_SUPPORT_ADDR"]},{"address":"$SCOPE_SUPPORT_ADDR","dependencies":["$SCOPE_REALIZED_DEP"]}]}}
EOF
)"
	api_json POST "$base/v1/state-engine/scope/expand" "$expand_body" >"$SCOPE_EXPAND_JSON"
	fetch_addresses_json="$(jq -c '.scope_contract.fetch_addresses' "$SCOPE_EXPAND_JSON")"
	slice_body="{\"state\":\"$STATE_NAME\",\"base_serial\":$serial,\"addresses\":$fetch_addresses_json}"
	api_json POST "$base/v1/state-engine/state/slice" "$slice_body" >"$SCOPE_SLICE_JSON"
}

run_scope_demo() {
	local direct_apply_out
	local support_was_realized=0
	direct_apply_out="$(mktemp /tmp/kl-state-engine-scope-apply.XXXXXX.log)"
	add_cleanup "$direct_apply_out"

	step "scope demo: mixed graph closure plus removed-config deletion preview"
	ensure_scope_fixture_present
	create_scope_demo_files
	bootstrap_removed_demo_resource
	if query_exists "$SCOPE_SUPPORT_ADDR"; then
		support_was_realized=1
		warn "support demo resource already exists in trunk; initial scope plan may keep it as a realized read dependency or widen it into an immediate support mutation"
	else
		info "support demo resource is not yet realized; initial scope plan should create it as part of the widened trusted write set"
	fi

	info "selected file: $SCOPE_LEAF_FILE"
	info "support file:  $SCOPE_SUPPORT_FILE"
	info "bridge file:   $SCOPE_BRIDGE_FILE"
	info "removed file:  $SCOPE_REMOVED_FILE"
	info "leaf address:  $SCOPE_LEAF_ADDR"
	info "support addr:  $SCOPE_SUPPORT_ADDR"
	info "bridge addr:   $SCOPE_BRIDGE_ADDR"
	info "removed addr:  $SCOPE_REMOVED_ADDR"
	info "realized dep:  $SCOPE_REALIZED_DEP"

	step "run file-scoped state-engine plan"
	(
		cd "$HERE"
		run_kl plan -f "$SCOPE_LEAF_FILE" -o "$PLAN_FILE"
	)
	ok "plan succeeded and spec written to $PLAN_FILE"

	step "compare full backend state vs state-engine slice payload"
	capture_scope_payload_sizes
	print_size_comparison "$FULL_STATE_JSON" "$SCOPE_SLICE_JSON"

	step "inspect resulting plan spec"
	local write_set read_set
	write_set="$(jq -r '.write_set[]?' "$PLAN_FILE" | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
	read_set="$(jq -r '.read_set[]?' "$PLAN_FILE" | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
	info "write_set: ${write_set:-<empty>}"
	info "read_set:  ${read_set:-<empty>}"

	if ! jq -e --arg addr "$SCOPE_LEAF_ADDR" '.write_set | index($addr) != null' "$PLAN_FILE" >/dev/null; then
		fail "plan write_set does not contain $SCOPE_LEAF_ADDR"
		exit 1
	fi
	ok "write_set contains selected undeployed leaf"

	if [ "$support_was_realized" -eq 0 ]; then
		if ! jq -e --arg addr "$SCOPE_SUPPORT_ADDR" '.write_set | index($addr) != null' "$PLAN_FILE" >/dev/null; then
			fail "plan write_set does not contain backend-proven support write $SCOPE_SUPPORT_ADDR"
			exit 1
		fi
		ok "write_set also keeps the config-required support write"
	else
		if jq -e --arg addr "$SCOPE_SUPPORT_ADDR" '.write_set | index($addr) != null' "$PLAN_FILE" >/dev/null; then
			ok "already-realized support resource widened immediately into a trusted support mutation"
		elif jq -e --arg addr "$SCOPE_SUPPORT_ADDR" '.read_set | index($addr) != null' "$PLAN_FILE" >/dev/null; then
			ok "already-realized support resource remains a read dependency at this stage"
		else
			fail "plan contains neither a trusted widened write nor a read dependency for already-realized support resource $SCOPE_SUPPORT_ADDR"
			exit 1
		fi
	fi

	if ! jq -e --arg addr "$SCOPE_REALIZED_DEP" '.read_set | index($addr) != null' "$PLAN_FILE" >/dev/null; then
		fail "plan read_set does not contain realized dependency $SCOPE_REALIZED_DEP"
		exit 1
	fi
	ok "read_set contains realized dependency discovered through support file"

	if ! jq -e '(.plan_summary.create + .plan_summary.update + .plan_summary.replace + .plan_summary.delete + .plan_summary.forget) >= 1' "$PLAN_FILE" >/dev/null; then
		fail "expected at least one real scoped mutation in scope demo plan"
		exit 1
	fi
	ok "plan summary reports a real scoped mutation"

	bootstrap_scope_support_resource
	write_scope_support_file_with_bridge_dep
	ok "rewrote support file so realized support now graph-depends on undeployed bridge helper"

	step "prove backend keeps the scope native when a realized support node now points at a new helper"
	(
		cd "$HERE"
		run_kl plan -f "$SCOPE_LEAF_FILE" -o "$PLAN_FILE"
	)
	ok "bridge-aware scope plan written to $PLAN_FILE"

	if ! jq -e '.state_engine.mode == "native-slice"' "$PLAN_FILE" >/dev/null; then
		fail "expected bridge-aware scope to remain native-slice"
		exit 1
	fi
	ok "bridge-aware scope remains native-slice"

	if ! jq -e '.state_engine.confidence == "safe"' "$PLAN_FILE" >/dev/null; then
		fail "expected bridge-aware scope to remain safe"
		exit 1
	fi
	ok "bridge-aware scope remains safe"

	if jq -e --arg addr "$SCOPE_BRIDGE_ADDR" '.state_engine.config_required_nodes | index($addr) != null' "$PLAN_FILE" >/dev/null; then
		ok "bridge helper is preserved as a config-required node"
	elif jq -e --arg addr "$SCOPE_BRIDGE_ADDR" '(.write_set | index($addr) != null) or (.state_engine.fetch_addresses | index($addr) != null)' "$PLAN_FILE" >/dev/null; then
		ok "bridge helper is preserved in the trusted native scope even when promoted beyond config-only metadata"
	else
		fail "expected bridge helper to remain visible in the trusted native scope"
		exit 1
	fi

	step "prove trusted native apply can widen to support mutation plus undeployed helper"
	write_scope_support_file_with_bridge_trigger
	ok "rewrote support file so realized support now mutates from undeployed bridge helper output"
	(
		cd "$HERE"
		run_kl plan -f "$SCOPE_LEAF_FILE" -o "$TRUSTED_PLAN_FILE"
	)
	ok "trusted mixed-graph apply plan written to $TRUSTED_PLAN_FILE"

	if ! jq -e --arg addr "$SCOPE_LEAF_ADDR" '.write_set | index($addr) != null' "$TRUSTED_PLAN_FILE" >/dev/null; then
		fail "expected trusted mixed-graph write_set to contain $SCOPE_LEAF_ADDR"
		exit 1
	fi
	ok "trusted mixed-graph write_set contains the selected leaf write"

	if jq -e --arg addr "$SCOPE_BRIDGE_ADDR" '.write_set | index($addr) != null' "$TRUSTED_PLAN_FILE" >/dev/null; then
		ok "trusted mixed-graph plan keeps bridge helper as a direct write"
	elif jq -e --arg addr "$SCOPE_BRIDGE_ADDR" '(.state_engine.config_required_nodes | index($addr) != null) or (.read_set | index($addr) != null) or (.state_engine.fetch_addresses | index($addr) != null)' "$TRUSTED_PLAN_FILE" >/dev/null; then
		ok "trusted mixed-graph plan keeps bridge helper visible as a preserved dependency/config node"
	else
		fail "expected trusted mixed-graph plan to keep $SCOPE_BRIDGE_ADDR visible as either a write or a preserved dependency"
		exit 1
	fi

	if jq -e --arg addr "$SCOPE_SUPPORT_ADDR" '.write_set | index($addr) != null' "$TRUSTED_PLAN_FILE" >/dev/null; then
		ok "trusted mixed-graph plan keeps support as a direct mutation"
	elif jq -e --arg addr "$SCOPE_SUPPORT_ADDR" '(.read_set | index($addr) != null) or (.state_engine.fetch_addresses | index($addr) != null)' "$TRUSTED_PLAN_FILE" >/dev/null; then
		ok "trusted mixed-graph plan keeps support visible as a preserved dependency"
	else
		fail "expected trusted mixed-graph plan to keep $SCOPE_SUPPORT_ADDR visible as either a write or a preserved dependency"
		exit 1
	fi

	if ! jq -e '.state_engine.mode == "native-slice" or .state_engine.mode == "native-slice-with-discovery-fallback"' "$TRUSTED_PLAN_FILE" >/dev/null; then
		fail "expected trusted mixed-graph plan to stay on a native slice mode"
		exit 1
	fi
	if ! jq -e '.state_engine.confidence == "safe"' "$TRUSTED_PLAN_FILE" >/dev/null; then
		fail "expected trusted mixed-graph plan to remain safe"
		exit 1
	fi
	ok "trusted mixed-graph plan remains safe for native apply"

	(
		cd "$HERE"
		run_kl apply --plan-spec "$TRUSTED_PLAN_FILE"
	) 2>&1 | tee "$direct_apply_out" >/dev/null
	if ! grep -q 'commit mode:[[:space:]]*state-engine delta' "$direct_apply_out"; then
		fail "trusted mixed-graph apply did not use state-engine delta commit"
		exit 1
	fi
	if ! grep -q 'Native intent source: terraform validation replan' "$direct_apply_out"; then
		fail "trusted mixed-graph apply did not surface native intent source"
		exit 1
	fi
	if ! grep -q "$SCOPE_LEAF_ADDR" "$direct_apply_out"; then
		fail "trusted mixed-graph apply output did not mention $SCOPE_LEAF_ADDR"
		exit 1
	fi
	ok "trusted mixed-graph apply output mentions the selected leaf write"

	if jq -e --arg addr "$SCOPE_BRIDGE_ADDR" '.write_set | index($addr) != null' "$TRUSTED_PLAN_FILE" >/dev/null; then
		if ! grep -q "$SCOPE_BRIDGE_ADDR" "$direct_apply_out"; then
			fail "trusted mixed-graph apply output did not mention direct bridge write $SCOPE_BRIDGE_ADDR"
			exit 1
		fi
		ok "trusted mixed-graph apply output mentions the direct bridge write"
	fi

	if jq -e --arg addr "$SCOPE_SUPPORT_ADDR" '.write_set | index($addr) != null' "$TRUSTED_PLAN_FILE" >/dev/null; then
		if ! grep -q "$SCOPE_SUPPORT_ADDR" "$direct_apply_out"; then
			fail "trusted mixed-graph apply output did not mention direct support write $SCOPE_SUPPORT_ADDR"
			exit 1
		fi
		ok "trusted mixed-graph apply output mentions the direct support write"
	fi

	ok "trusted mixed-graph apply widened safely and committed through the native delta lane"

	step "run a normal scoped update through plan -> apply on the trusted lane"
	bootstrap_slow_scope_fixture_if_needed
	local new_slow_a direct_update_out
	new_slow_a="$(scoped_update_target)"
	direct_update_out="$(mktemp /tmp/kl-state-engine-update-apply.XXXXXX.log)"
	add_cleanup "$direct_update_out"
	info "state=$STATE_NAME trunk slow_a=$SLOW_A_CURRENT slow_b=$SLOW_B_CURRENT"
	info "scoped update will bump $SLOW_A_SCOPE_ADDR -> $new_slow_a while pinning $SLOW_B_SCOPE_ADDR"

	(
		cd "$HERE"
		run_kl plan \
			--no-refresh \
			--no-lock \
			--file="$SLOW_A_SCOPE_FILE" \
			--var="slow_a_version=$new_slow_a" \
			--var="slow_b_version=$SLOW_B_CURRENT" \
			--out="$UPDATE_PLAN_FILE" \
			"$HERE"
	)
	ok "trusted scoped update plan written to $UPDATE_PLAN_FILE"

	if ! jq -e --arg addr "$SLOW_A_SCOPE_ADDR" '.write_set == [$addr]' "$UPDATE_PLAN_FILE" >/dev/null; then
		fail "expected scoped update write_set to be exactly [$SLOW_A_SCOPE_ADDR]"
		exit 1
	fi
	ok "scoped update plan narrows to $SLOW_A_SCOPE_ADDR only"

	if ! jq -e '.state_engine.mode == "native-slice" or .state_engine.mode == "native-slice-with-discovery-fallback"' "$UPDATE_PLAN_FILE" >/dev/null; then
		fail "expected scoped update plan to stay on a native slice mode"
		exit 1
	fi
	if ! jq -e '.state_engine.confidence == "safe"' "$UPDATE_PLAN_FILE" >/dev/null; then
		fail "expected scoped update plan to remain safe"
		exit 1
	fi
	ok "scoped update plan remains trusted for native apply"

	(
		cd "$HERE"
		run_kl apply --plan-spec "$UPDATE_PLAN_FILE"
	) 2>&1 | tee "$direct_update_out" >/dev/null
	if ! grep -q 'commit mode:[[:space:]]*state-engine delta' "$direct_update_out"; then
		fail "scoped update apply did not use state-engine delta commit"
		exit 1
	fi
	if ! grep -q 'Native intent source: terraform validation replan' "$direct_update_out"; then
		fail "scoped update apply did not surface native intent source"
		exit 1
	fi
	if ! grep -q "$SLOW_A_SCOPE_ADDR" "$direct_update_out"; then
		fail "scoped update apply did not report $SLOW_A_SCOPE_ADDR in output"
		exit 1
	fi
	ok "normal scoped update used the trusted state-engine delta lane"

	step "run a module-scoped update through plan -> apply on the trusted lane"
	bootstrap_module_scope_resource
	local module_demo_value module_apply_out
	module_demo_value="module-demo-$RANDOM"
	module_apply_out="$(mktemp /tmp/kl-state-engine-module-apply.XXXXXX.log)"
	add_cleanup "$module_apply_out"
	write_module_scope_file "$module_demo_value"
	info "module file: $MODULE_SCOPE_FILE"
	info "module addr: $MODULE_SCOPE_ADDR"
	info "new value:  $module_demo_value"

	(
		cd "$HERE"
		run_kl plan -f "$MODULE_SCOPE_FILE" -o "$UPDATE_PLAN_FILE"
	)
	ok "module-scoped plan written to $UPDATE_PLAN_FILE"

	if ! jq -e --arg addr "$MODULE_SCOPE_ADDR" '.write_set | index($addr) != null' "$UPDATE_PLAN_FILE" >/dev/null; then
		fail "expected module-scoped write_set to contain $MODULE_SCOPE_ADDR"
		exit 1
	fi
	ok "module-scoped plan widens selected module file to realized module resource writes"

	if ! jq -e '.state_engine.mode == "native-slice" or .state_engine.mode == "native-slice-with-discovery-fallback"' "$UPDATE_PLAN_FILE" >/dev/null; then
		fail "expected module-scoped plan to stay on a native slice mode"
		exit 1
	fi
	if ! jq -e '.state_engine.confidence == "safe"' "$UPDATE_PLAN_FILE" >/dev/null; then
		fail "expected module-scoped plan to remain safe"
		exit 1
	fi
	ok "module-scoped plan remains trusted for native apply"

	(
		cd "$HERE"
		run_kl apply --plan-spec "$UPDATE_PLAN_FILE"
	) 2>&1 | tee "$module_apply_out" >/dev/null
	if ! grep -q 'commit mode:[[:space:]]*state-engine delta' "$module_apply_out"; then
		fail "module-scoped apply did not use state-engine delta commit"
		exit 1
	fi
	if ! grep -q 'Native intent source: terraform validation replan' "$module_apply_out"; then
		fail "module-scoped apply did not surface native intent source"
		exit 1
	fi
	if ! grep -q "$MODULE_SCOPE_ADDR" "$module_apply_out"; then
		fail "module-scoped apply did not report $MODULE_SCOPE_ADDR in output"
		exit 1
	fi
	ok "module-scoped update used the trusted state-engine delta lane"

	step "run removed-config preview plan"
	(
		cd "$HERE"
		run_kl plan -f "$SCOPE_REMOVED_FILE" -o "$PLAN_FILE"
	)
	ok "removed-config preview written to $PLAN_FILE"

	local removed_nodes removed_notes
	removed_nodes="$(jq -r '.state_engine.removed_config_nodes[]?' "$PLAN_FILE" | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
	removed_notes="$(jq -r '.state_engine.notes[]?' "$PLAN_FILE" | tr '\n' '\n')"
	info "removed_config_nodes: ${removed_nodes:-<empty>}"

	if ! jq -e --arg addr "$SCOPE_REMOVED_ADDR" '.state_engine.removed_config_nodes | index($addr) != null' "$PLAN_FILE" >/dev/null; then
		fail "plan metadata does not contain removed_config_nodes entry for $SCOPE_REMOVED_ADDR"
		exit 1
	fi
	ok "plan metadata records removed-config intent"

	if ! jq -e '.state_engine.confidence == "safe"' "$PLAN_FILE" >/dev/null; then
		fail "expected removed-config preview to remain safe"
		exit 1
	fi
	ok "backend classified removed-config preview as safe"

	if ! jq -e '.plan_summary.delete >= 1 or .plan_summary.forget >= 1' "$PLAN_FILE" >/dev/null; then
		fail "expected removed-config preview to produce a delete/forget action"
		exit 1
	fi
	ok "removed-config preview produces a real scoped mutation"

	if ! jq -e '.state_engine.notes | map(test("removed config node\\(s\\) still exist in realized state")) | any' "$PLAN_FILE" >/dev/null; then
		fail "expected backend note describing realized removed node"
		info "$removed_notes"
		exit 1
	fi
	ok "backend explains that the removed node is still realized and must be deleted"

	step "run native scoped delete apply"
	(
		cd "$HERE"
		run_kl apply -f "$SCOPE_REMOVED_FILE" --confirm-scope --allow-destructive-scoped
	) 2>&1 | tee "$direct_apply_out" >/dev/null
	if ! grep -q 'commit mode:[[:space:]]*state-engine delta' "$direct_apply_out"; then
		fail "direct file-scoped apply did not use state-engine delta commit"
		exit 1
	fi
	if ! grep -q 'Native intent source: terraform validation replan' "$direct_apply_out"; then
		fail "direct file-scoped apply did not surface native intent source"
		exit 1
	fi
	ok "direct file-scoped apply used the trusted state-engine delta lane"
	if query_exists "$SCOPE_REMOVED_ADDR"; then
		fail "expected scoped delete apply to remove $SCOPE_REMOVED_ADDR"
		exit 1
	fi
	ok "native scoped apply removed $SCOPE_REMOVED_ADDR"

	warn "temporary demo files will be removed on exit"
}

run_move_demo() {
	step "preview native move"
	run_kl state mv --from "$MOVE_SOURCE" --to "$MOVE_TARGET" "$STATE_NAME"

	step "apply native move"
	run_kl state mv --from "$MOVE_SOURCE" --to "$MOVE_TARGET" --apply --yes "$STATE_NAME" >/tmp/kl-state-engine-move.log
	ok "moved $MOVE_SOURCE -> $MOVE_TARGET"

	if ! query_exists "$MOVE_TARGET"; then
		fail "moved address $MOVE_TARGET not found after apply"
		exit 1
	fi
	ok "verified moved address exists"

	step "move resource back to canonical address"
	run_kl state mv --from "$MOVE_TARGET" --to "$MOVE_SOURCE" --apply --yes "$STATE_NAME" >/tmp/kl-state-engine-move-back.log
	if ! query_exists "$MOVE_SOURCE"; then
		fail "restored address $MOVE_SOURCE not found after move-back"
		exit 1
	fi
	ok "restored $MOVE_SOURCE"
}

run_remove_demo() {
	local serial_before_remove
	serial_before_remove="$(current_serial)"
	step "preview native remove"
	run_kl state rm --address "$REMOVE_ADDRESS" "$STATE_NAME"

	step "apply native remove"
	run_kl state rm --address "$REMOVE_ADDRESS" --apply --yes "$STATE_NAME" >/tmp/kl-state-engine-remove.log
	ok "removed $REMOVE_ADDRESS from current state"

	if query_exists "$REMOVE_ADDRESS"; then
		fail "$REMOVE_ADDRESS still exists after remove apply"
		exit 1
	fi
	ok "verified removed address is absent"

	step "preview native rollback/restore from prior serial"
	run_kl rollback resource --address "$REMOVE_ADDRESS" --to "$serial_before_remove" "$STATE_NAME"

	step "apply native rollback/restore from prior serial"
	run_kl rollback resource --address "$REMOVE_ADDRESS" --to "$serial_before_remove" --apply --yes "$STATE_NAME" >/tmp/kl-state-engine-restore.log
	if ! query_exists "$REMOVE_ADDRESS"; then
		fail "$REMOVE_ADDRESS missing after restore rollback"
		exit 1
	fi
	ok "restored $REMOVE_ADDRESS from serial $serial_before_remove"
}

run_native_demo() {
	step "native state-engine mutation demo"
	ensure_native_fixture_present
	run_move_demo
	run_remove_demo
}

bootstrap_removed_demo_for_lane_split() {
	create_scope_demo_files
	bootstrap_removed_demo_resource
}

run_lane_split_demo() {
	local trusted_out fallback_preflight_out fallback_apply_out
	trusted_out="$(mktemp /tmp/kl-state-engine-trusted-apply.XXXXXX.log)"
	fallback_preflight_out="$(mktemp /tmp/kl-state-engine-fallback-preflight.XXXXXX.log)"
	fallback_apply_out="$(mktemp /tmp/kl-state-engine-fallback-apply.XXXXXX.log)"
	add_cleanup "$trusted_out"
	add_cleanup "$fallback_preflight_out"
	add_cleanup "$fallback_apply_out"

	step "lane split demo: trusted native slice versus explicit fallback"
	ensure_scope_fixture_present
	bootstrap_removed_demo_for_lane_split

	step "build trusted removed-config spec"
	(
		cd "$HERE"
		run_kl plan -f "$SCOPE_REMOVED_FILE" -o "$TRUSTED_PLAN_FILE"
	)
	ok "trusted spec written to $TRUSTED_PLAN_FILE"

	if ! jq -e '.state_engine.mode == "native-slice" or .state_engine.mode == "native-slice-with-discovery-fallback"' "$TRUSTED_PLAN_FILE" >/dev/null; then
		fail "trusted spec did not resolve to a native slice mode"
		exit 1
	fi
	if ! jq -e '.state_engine.confidence == "safe"' "$TRUSTED_PLAN_FILE" >/dev/null; then
		fail "trusted spec is not marked safe"
		exit 1
	fi
	ok "trusted spec is native and safe"

	step "trusted lane preflight"
	(
		cd "$HERE"
		run_kl apply --plan-spec "$TRUSTED_PLAN_FILE" --dry-run
	) 2>&1 | tee "$trusted_out" >/dev/null
	if ! grep -q 'native apply safety:[[:space:]]*proven-safe' "$trusted_out"; then
		fail "trusted preflight did not report proven-safe native apply"
		exit 1
	fi
	ok "trusted preflight reports proven-safe native apply"

	step "trusted lane apply"
	(
		cd "$HERE"
		run_kl apply --plan-spec "$TRUSTED_PLAN_FILE"
	) 2>&1 | tee "$trusted_out" >/dev/null
	if ! grep -q 'commit mode:[[:space:]]*state-engine delta' "$trusted_out"; then
		fail "trusted apply did not use state-engine delta commit"
		exit 1
	fi
	if ! grep -q 'Native intent source: terraform validation replan' "$trusted_out"; then
		fail "trusted apply did not surface native intent source"
		exit 1
	fi
	ok "trusted apply used the trusted state-engine delta lane"
	if query_exists "$SCOPE_REMOVED_ADDR"; then
		fail "trusted lane should have removed $SCOPE_REMOVED_ADDR"
		exit 1
	fi
	ok "trusted lane removed the temporary resource"

	step "re-bootstrap removable demo resource for fallback lane"
	bootstrap_removed_demo_resource

	step "forge explicit fallback spec from the trusted spec"
	jq '
		.state_engine.mode = "full-trunk-fallback"
		| .state_engine.fallback_reason = "demo: forcing runtime fallback lane"
		| .state_engine.notes = ((.state_engine.notes // []) + ["demo: forcing runtime fallback lane"])
	' "$TRUSTED_PLAN_FILE" >"$FALLBACK_PLAN_FILE"
	ok "fallback spec written to $FALLBACK_PLAN_FILE"

	step "fallback lane preflight"
	(
		cd "$HERE"
		run_kl apply --plan-spec "$FALLBACK_PLAN_FILE" --dry-run
	) 2>&1 | tee "$fallback_preflight_out" >/dev/null
	if ! grep -q 'native apply safety:[[:space:]]*fallback-required' "$fallback_preflight_out"; then
		fail "fallback preflight did not report fallback-required"
		exit 1
	fi
	ok "fallback preflight reports fallback-required"

	step "fallback lane apply via broader backend/full-trunk runtime"
	(
		cd "$HERE"
		run_kl apply --plan-spec "$FALLBACK_PLAN_FILE"
	) 2>&1 | tee "$fallback_apply_out" >/dev/null
	if ! grep -q 'apply succeeded (state=' "$fallback_apply_out"; then
		fail "fallback apply did not complete on the broader lane"
		exit 1
	fi
	if grep -q 'Native intent source:' "$fallback_apply_out"; then
		fail "fallback apply unexpectedly surfaced native intent"
		exit 1
	fi
	if grep -q 'commit mode:[[:space:]]*state-engine delta' "$fallback_apply_out"; then
		fail "fallback apply unexpectedly used the trusted state-engine delta lane"
		exit 1
	fi
	ok "fallback apply stayed off the trusted state-engine lane"
	if query_exists "$SCOPE_REMOVED_ADDR"; then
		fail "fallback lane should have removed $SCOPE_REMOVED_ADDR"
		exit 1
	fi
	ok "fallback lane also removed the temporary resource via the broader path"
}

usage() {
	cat <<EOF
${BOLD}state-engine-demo.sh — state-engine POC demo${RESET}

Usage:
  examples/big-state/state-engine-demo.sh [scope|native|all]

Modes:
  scope   prove mixed-graph closure and removed-config classification
  lanes   prove trusted native lane vs fallback runtime lane
  native  prove native state mv / state rm / rollback resource
  all     run all demos (default)

Scope demo shape:
  selected file: ${CYAN}$SCOPE_LEAF_FILE${RESET}
  support file:  ${CYAN}$SCOPE_SUPPORT_FILE${RESET}
  removed file:  ${CYAN}$SCOPE_REMOVED_FILE${RESET}

  ${CYAN}$SCOPE_LEAF_ADDR${RESET}
    depends on ${CYAN}$SCOPE_SUPPORT_ADDR${RESET}
    (undeployed, lives in non-selected support file)

  ${CYAN}$SCOPE_SUPPORT_ADDR${RESET}
    depends on realized ${CYAN}$SCOPE_REALIZED_DEP${RESET}

  ${CYAN}$SCOPE_REMOVED_ADDR${RESET}
    is bootstrapped as a temporary resource, then replaced by a
    ${CYAN}removed { from = ... }${RESET} block and deleted via native scoped apply

Environment:
  KL_BIN                 default: \$REPO_ROOT/bin/kl
  KL_CONFIG_DISCOVERY    default: opentofu
  PLAN_FILE              default: $PLAN_FILE
  TRUSTED_PLAN_FILE      default: $TRUSTED_PLAN_FILE
  FALLBACK_PLAN_FILE     default: $FALLBACK_PLAN_FILE
  STATE_NAME             override discovered state name
  MOVE_SOURCE            default: $MOVE_SOURCE
  MOVE_TARGET            default: $MOVE_TARGET
  REMOVE_ADDRESS         default: $REMOVE_ADDRESS
  SCOPE_REMOVED_ADDR     default: $SCOPE_REMOVED_ADDR
  TERRAFORM_BIN          default: $TERRAFORM_BIN
EOF
}

main() {
	local mode="${1:-all}"
	case "$mode" in
		help|-h|--help)
			usage
			exit 0
			;;
		scope|lanes|native|all)
			;;
		*)
			fail "unknown mode: $mode"
			usage
			exit 2
			;;
	esac

	preflight
	step "checking demo environment"
	info "state=$STATE_NAME"
	info "backend=${BACKEND_ADDRESS:-<discovered by terraform init>}"
	info "protocol=state-engine"
	info "config discovery=${KL_CONFIG_DISCOVERY:-opentofu}"

	case "$mode" in
		scope)
			run_scope_demo
			;;
		lanes)
			run_lane_split_demo
			;;
		native)
			run_native_demo
			;;
		all)
			run_scope_demo
			run_lane_split_demo
			run_native_demo
			;;
	esac

	ok "state-engine demo finished cleanly"
}

main "$@"
