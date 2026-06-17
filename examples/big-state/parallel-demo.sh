#!/usr/bin/env bash
# parallel-demo.sh — show two engineers applying disjoint changes to the same
# state at the same time.
#
# This is the happy-path counterpart to wait-demo.sh:
#
#   - wait-demo.sh     => SAME resource, reservation conflict, visible waiting UI
#   - parallel-demo.sh => DIFFERENT resources, no conflict, narrow file-scoped plans, ~30s total wall-clock
#
# Two-terminal usage:
#
#   # Terminal 1                        # Terminal 2
#   examples/big-state/parallel-demo.sh slow_a
#   examples/big-state/parallel-demo.sh slow_b
#
# Single-terminal usage:
#
#   examples/big-state/parallel-demo.sh both
#
# The demo bumps `time_sleep.slow_a` and `time_sleep.slow_b` independently via narrow file-scoped plans and then applies them with explicit `--orchestrated`.
# Each resource sleeps for ~30s during apply, so:
#
#   - serial applies would take ~60s total
#   - parallel applies should finish in ~30s total
#
# Requirements:
#   - `make build` has produced `bin/kl`
#   - `terraform init` has been run in this directory
#   - `kld` is running and reachable
#   - `jq` is installed

set -euo pipefail

MODE="${1:-help}"
STATE_NAME="${STATE_NAME:-}"
TF_BIN="${TF_BIN:-terraform}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
KL_BIN="${KL_BIN:-$REPO_ROOT/bin/kl}"

PLAN_A_SPEC="${PLAN_A_SPEC:-/tmp/parallel-plan-slow_a.json}"
PLAN_B_SPEC="${PLAN_B_SPEC:-/tmp/parallel-plan-slow_b.json}"
APPLY_A_LOG="${APPLY_A_LOG:-/tmp/parallel-apply-slow_a.log}"
APPLY_B_LOG="${APPLY_B_LOG:-/tmp/parallel-apply-slow_b.log}"
BACKEND_ADDRESS="${TF_HTTP_ADDRESS:-}"

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

discover_backend() {
	local backend_state="$HERE/.terraform/terraform.tfstate"
	[ -f "$backend_state" ] || return 0
	local discovered
	discovered="$(python3 - "$backend_state" <<'PY'
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    data = json.load(fh)
cfg = ((data.get("backend") or {}).get("config") or {})
print(cfg.get("address") or "")
PY
)"
	if [ -z "$BACKEND_ADDRESS" ] && [ -n "$discovered" ]; then
		BACKEND_ADDRESS="$discovered"
	fi
	if [ -z "$STATE_NAME" ] && [ -n "$BACKEND_ADDRESS" ]; then
		STATE_NAME="$(python3 - "$BACKEND_ADDRESS" <<'PY'
from urllib.parse import urlparse
import sys
u = urlparse(sys.argv[1].strip())
path = u.path.strip("/")
prefix = "states/"
if path.startswith(prefix):
    print(path[len(prefix):])
else:
    print("")
PY
)"
	fi
	if [ -z "$STATE_NAME" ]; then
		STATE_NAME="big-state"
	fi
}

usage() {
	cat <<EOF
${BOLD}parallel-demo.sh — disjoint parallel apply demo${RESET}

Usage:
  examples/big-state/parallel-demo.sh ${CYAN}slow_a${RESET}
      Terminal 1. Bumps only time_sleep.slow_a.

  examples/big-state/parallel-demo.sh ${CYAN}slow_b${RESET}
      Terminal 2. Bumps only time_sleep.slow_b.

  examples/big-state/parallel-demo.sh ${CYAN}both${RESET}
      Single-terminal helper. Runs both applies in parallel and shows
      where each side's output was logged.

Why this matters:
  Same state, different resources. Kilolock only reserves the affected
  branch of the state graph, so both engineers can keep moving and the
  total wall-clock stays close to one 30-second sleep instead of two.
EOF
}

preflight() {
	[ -x "$KL_BIN" ] || { fail "kl binary not at $KL_BIN (run \`make build\`)"; exit 1; }
	command -v "$TF_BIN" >/dev/null || { fail "terraform binary '$TF_BIN' not on PATH"; exit 1; }
	command -v jq >/dev/null || { fail "jq is required"; exit 1; }
	[ -d "$HERE/.terraform" ] || { fail "run \`terraform init\` in $HERE first"; exit 1; }
	discover_backend
	if ! "$KL_BIN" list >/dev/null 2>&1; then
		fail "kl cannot reach the runtime API. Run \`terraform init\` here and make sure kld is reachable via the configured backend."
		exit 1
	fi
}

detect_versions() {
	local state_sql state_json
	state_sql="
SELECT address, COALESCE(attributes->'triggers'->>'version', '') AS version
FROM current_resources
WHERE state_name = '$STATE_NAME'
  AND address IN ('time_sleep.slow_a', 'time_sleep.slow_b')
ORDER BY address;
"
	state_json="$("$KL_BIN" query --format json "$state_sql")"
	SLOW_A_CURRENT="$(jq -r --arg a 'time_sleep.slow_a' '.[] | select(.address==$a) | .version' <<<"$state_json")"
	SLOW_B_CURRENT="$(jq -r --arg a 'time_sleep.slow_b' '.[] | select(.address==$a) | .version' <<<"$state_json")"
	if [ -z "$SLOW_A_CURRENT" ] || [ -z "$SLOW_B_CURRENT" ]; then
		fail "time_sleep.slow_a or slow_b missing from trunk."
		fail "Bootstrap once with: terraform apply -refresh=false -target=time_sleep.slow_a -target=time_sleep.slow_b -var=slow_a_version=v1 -var=slow_b_version=v1"
		exit 1
	fi
}

role_target() {
	local role="$1"
	echo "${role}-${RANDOM}"
}

build_plan() {
	local target_addr="$1" out_path="$2"
	local new_a="$SLOW_A_CURRENT" new_b="$SLOW_B_CURRENT" expected_ws=""
	case "$target_addr" in
		slow_a)
			new_a="$(role_target slow_a)"
			expected_ws='["time_sleep.slow_a"]'
			;;
		slow_b)
			new_b="$(role_target slow_b)"
			expected_ws='["time_sleep.slow_b"]'
			;;
		*)
			fail "unknown target address: $target_addr"
			exit 2
			;;
	esac

	local scope_file=""
	if [ "$target_addr" = "slow_a" ]; then
		scope_file="slow_a.tf"
	else
		scope_file="slow_b.tf"
	fi

	"$KL_BIN" plan \
		--no-refresh \
		--no-lock \
		--file="$scope_file" \
		--var="slow_a_version=$new_a" \
		--var="slow_b_version=$new_b" \
		--out="$out_path" \
		"$HERE" >/dev/null

	local ws
	ws="$(jq -c .write_set "$out_path")"
	if [ "$ws" != "$expected_ws" ]; then
		fail "plan write set is not $expected_ws: $ws"
		fail "Likely causes:"
		fail "  - stale demo resources are still present in trunk; run a plain terraform apply once"
		fail "  - one of the slow_* resources drifted between detect_versions and plan; re-run"
		fail "  - .kl-plan-*.tfplan from a previous aborted run is interfering; remove $HERE/.kl-plan-*.tfplan and re-run"
		exit 1
	fi

	if [ "$target_addr" = "slow_a" ]; then
		ok "plan ready: $out_path (write_set = [time_sleep.slow_a]; slow_a $SLOW_A_CURRENT → $new_a)"
	else
		ok "plan ready: $out_path (write_set = [time_sleep.slow_b]; slow_b $SLOW_B_CURRENT → $new_b)"
	fi
}

run_one() {
	local target_addr="$1" out_path="$2"
	preflight
	step "${BOLD}$target_addr${RESET}: detecting trunk versions"
	detect_versions
	info "state=$STATE_NAME trunk slow_a=$SLOW_A_CURRENT slow_b=$SLOW_B_CURRENT"

	step "${BOLD}$target_addr${RESET}: generating file-scoped plan"
	build_plan "$target_addr" "$out_path"

	step "${BOLD}$target_addr${RESET}: running apply (--orchestrated)"
	if "$KL_BIN" apply --orchestrated --plan-spec="$out_path" --state="$STATE_NAME" --wait-timeout=0; then
		ok "$target_addr committed without waiting."
	else
		fail "$target_addr apply failed; see above."
		exit 1
	fi
}

run_both() {
	preflight
	step "single-terminal disjoint parallel run"
	info "This starts both applies in parallel."
	info "Expected wall-clock: usually ~35–55 s total, not ~60 s serial."
	detect_versions
	ok "state=$STATE_NAME trunk has slow_a=$SLOW_A_CURRENT slow_b=$SLOW_B_CURRENT"

	step "build both file-scoped plans"
	( build_plan slow_a "$PLAN_A_SPEC" ) &
	local pid_plan_a=$!
	( build_plan slow_b "$PLAN_B_SPEC" ) &
	local pid_plan_b=$!
	local plan_failed=0
	if ! wait "$pid_plan_a"; then
		plan_failed=1
	fi
	if ! wait "$pid_plan_b"; then
		plan_failed=1
	fi
	if [ "$plan_failed" -ne 0 ]; then
		fail "one or both plan builds failed"
		exit 1
	fi

	step "run both applies in parallel"
	local started elapsed
	started="$(date +%s)"
	(
		"$KL_BIN" apply --orchestrated --plan-spec="$PLAN_A_SPEC" --state="$STATE_NAME" --wait-timeout=0
	) >"$APPLY_A_LOG" 2>&1 &
	local pid_a=$!
	(
		"$KL_BIN" apply --orchestrated --plan-spec="$PLAN_B_SPEC" --state="$STATE_NAME" --wait-timeout=0
	) >"$APPLY_B_LOG" 2>&1 &
	local pid_b=$!
	local apply_failed=0
	if ! wait "$pid_a"; then
		apply_failed=1
	fi
	if ! wait "$pid_b"; then
		apply_failed=1
	fi
	elapsed="$(( $(date +%s) - started ))"

	if [ "$apply_failed" -ne 0 ]; then
		fail "one or both applies failed"
		info "slow_a log: $APPLY_A_LOG"
		info "slow_b log: $APPLY_B_LOG"
		exit 1
	fi

	ok "both applies finished"
	ok "total wall-clock: ${elapsed}s"
	info "slow_a log: $APPLY_A_LOG"
	info "slow_b log: $APPLY_B_LOG"
	if [ "$elapsed" -le 55 ]; then
		ok "parallel apply saved time: both changes finished in roughly one slow sleep plus normal Terraform overhead"
	elif [ "$elapsed" -le 75 ]; then
		warn "parallel apply still saved time, but overhead was noticeable (${elapsed}s). This can happen from provider startup, plan replay, or local machine load."
	else
		warn "elapsed time is suspiciously high (${elapsed}s); check the logs above for possible serialization or local slowdown"
	fi
}

case "$MODE" in
	slow_a) run_one slow_a "$PLAN_A_SPEC" ;;
	slow_b) run_one slow_b "$PLAN_B_SPEC" ;;
	both)   run_both ;;
	help|-h|--help) usage ;;
	*) fail "unknown mode: $MODE"; usage; exit 2 ;;
esac
