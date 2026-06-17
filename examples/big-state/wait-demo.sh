#!/usr/bin/env bash
# wait-demo.sh — show `kl apply --wait-timeout` in action, with
# live wait-status output, across two terminal windows.
#
# The script is the conflict-variant of parallel-demo.sh: both terminals
# try to bump the SAME address (time_sleep.slow_a). The first one in
# acquires the reservation and sleeps ~30s during orchestrated terraform apply. The
# second one contends on the same address, falls into the wait loop,
# and prints the operator-facing "blocked by 1 reservation(s) on ..."
# block every 5s until terminal 1 commits. Then the second one is
# unblocked, runs its own ~30s apply, and commits.
#
# Two-terminal usage:
#
#   # Terminal 1 (the blocker):
#   examples/big-state/wait-demo.sh blocker
#
#   # Terminal 2 (the waiter), start within ~5 seconds:
#   examples/big-state/wait-demo.sh waiter
#
# Single-terminal usage (sequential, but you can watch the wait block
# stream live in the foreground):
#
#   examples/big-state/wait-demo.sh both
#
# Defaults assume `make build` has produced bin/kl, that
# `terraform init` has been run inside this directory, and that
# `kl serve` is running.

set -euo pipefail

MODE="${1:-help}"

STATE_NAME="${STATE_NAME:-}"
TF_BIN="${TF_BIN:-terraform}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-2m}"

# Repo-rooted kl binary.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
KL_BIN="${KL_BIN:-$REPO_ROOT/bin/kl}"

# Plan artifacts. The two modes share a state file but produce
# different plans (different new values for slow_a) so the second
# apply is not a no-op once it gets through the wait.
PLAN_BLOCKER_SPEC="${PLAN_BLOCKER_SPEC:-/tmp/wait-demo-blocker.json}"
PLAN_WAITER_SPEC="${PLAN_WAITER_SPEC:-/tmp/wait-demo-waiter.json}"
BLOCKER_LOG="${BLOCKER_LOG:-/tmp/wait-demo-blocker.log}"
BACKEND_ADDRESS="${TF_HTTP_ADDRESS:-}"

# ANSI niceties; degrade to plain when stdout isn't a terminal.
if [ -t 1 ] && command -v tput >/dev/null 2>&1 && [ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]; then
	BOLD="$(tput bold)"; DIM="$(tput dim)"
	GREEN="$(tput setaf 2)"; YELLOW="$(tput setaf 3)"; RED="$(tput setaf 1)"; CYAN="$(tput setaf 6)"
	RESET="$(tput sgr0)"
else
	BOLD=""; DIM=""; GREEN=""; YELLOW=""; RED=""; CYAN=""; RESET=""
fi
step()  { printf "%s==>%s %s\n" "$BOLD" "$RESET" "$*"; }
info()  { printf "%s    %s%s\n" "$DIM" "$*" "$RESET"; }
ok()    { printf "%s    ✓ %s%s\n" "$GREEN" "$*" "$RESET"; }
warn()  { printf "%s    ! %s%s\n" "$YELLOW" "$*" "$RESET"; }
fail()  { printf "%s    ✗ %s%s\n" "$RED" "$*" "$RESET"; }

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
${BOLD}wait-demo.sh — two-terminal reservation-wait demo${RESET}

Usage:
  examples/big-state/wait-demo.sh ${CYAN}blocker${RESET}
      Run in terminal 1. Acquires reservation on time_sleep.slow_a,
      runs ~30 s terraform apply, commits. Holds the apply slot long
      enough for the operator to see the wait block in terminal 2.

  examples/big-state/wait-demo.sh ${CYAN}waiter${RESET}
      Run in terminal 2 (start within ~5 s of terminal 1). Will try
      to bump the SAME address; will hit a reservation conflict;
      will print "blocked by 1 reservation(s) on \"$STATE_NAME\"" to
      stderr every 5 s; will succeed once the blocker commits.

  examples/big-state/wait-demo.sh ${CYAN}both${RESET}
      Single-terminal variant. Starts the blocker in the background
      (stdout → \$BLOCKER_LOG), then runs the waiter in the
      foreground so you can watch the wait block stream live.

  examples/big-state/wait-demo.sh ${CYAN}help${RESET}
      This message.

Environment:
  STATE_NAME       (default: $STATE_NAME)
  WAIT_TIMEOUT     (default: $WAIT_TIMEOUT) — passed to --wait-timeout
  TF_BIN           (default: $TF_BIN)
  KL_BIN   (default: \$REPO_ROOT/bin/kl)

Prereqs (same as parallel-demo.sh):
  - make build has produced bin/kl
  - terraform init has been run in $HERE
  - kld is running and reachable via the backend configured in this
    directory (or override with KL_API_URL)
  - time_sleep.slow_a exists in trunk (bootstrap with
    parallel-demo.sh's instructions if not).
EOF
}

# Sanity checks shared by all modes that actually run something.
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

# Pull the current trigger value for slow_a and slow_b. We use
# --format json + jq because CSV would need un-escaping for any
# value containing a quote, and we don't want to depend on the
# trunk being in a specific shape: each role picks a per-run
# unique target (see role_target below) and feeds slow_b's
# current value back unchanged to keep that address a no-op,
# regardless of what the previous run left behind.
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
		fail "time_sleep.slow_a or slow_b missing from trunk; see parallel-demo.sh for bootstrap instructions."
		exit 1
	fi
}

# role_target produces a target value for slow_a that is
# guaranteed to differ from the trunk's current value AND from
# the OTHER role's target, regardless of run ordering.
#
# Why per-run uniqueness:
#
# The earlier "blocker flips, waiter restores" scheme assumed
# the waiter's detect_versions ran against the pre-blocker
# trunk. That assumption broke in two cases:
#
#   - sequential ordering: if the user runs `wait-demo waiter`
#     after the blocker has already committed, the waiter sees
#     the post-blocker trunk and ends up planning a no-op
#     (target == trunk), producing an empty write_set and
#     failing the build_plan assertion.
#   - concurrent ordering: with binary v1/v2 there is no value
#     the waiter can pick that's both different from the
#     pre-blocker trunk (so the plan has a write_set) AND
#     different from the post-blocker trunk (so the apply does
#     real work). One is required for the build_plan assertion;
#     the other for a meaningful demo.
#
# A unique-per-run target — role name + $RANDOM — sidesteps
# both. Plan-time: target ≠ trunk (always a write). Apply-time:
# target ≠ whatever the other role committed (always a real
# write). Both orderings now produce a working demo; the only
# difference is whether the wait-loop UI fires (concurrent) or
# not (sequential).
role_target() {
	local role="$1"
	echo "${role}-${RANDOM}"
}

# Build a singleton-write-set file-scoped plan that bumps slow_a only.
build_plan() {
	local out_path="$1"
	local new_slow_a="$2"
	"$KL_BIN" plan \
		--no-refresh \
		--no-lock \
		--file=slow_a.tf \
		--var="slow_a_version=$new_slow_a" \
		--var="slow_b_version=$SLOW_B_CURRENT" \
		--out="$out_path" \
		"$HERE" >/dev/null
	local ws
	ws="$(jq -c .write_set "$out_path")"
	if [ "$ws" != '["time_sleep.slow_a"]' ]; then
		if jq -e '.write_set == ["time_sleep.slow_a","time_sleep.slow_c"] or .write_set == ["time_sleep.slow_c","time_sleep.slow_a"]' "$out_path" >/dev/null 2>&1; then
			fail "plan is also trying to delete stale time_sleep.slow_c from trunk: $ws"
			fail "One-time cleanup:"
			fail "  terraform apply -refresh=false -var=size=400"
			fail "This reconciles trunk with the canonical demo config, then wait-demo can run cleanly."
			exit 1
		fi
		fail "plan write set is not [\"time_sleep.slow_a\"]: $ws"
		fail "Likely causes:"
		fail "  - role_target collided with trunk (extremely unlikely; re-run)"
		fail "  - stale demo resources are still present in trunk; run a plain terraform apply once"
		fail "  - slow_b drifted between detect_versions and plan; re-run"
		fail "  - .kl-plan-*.tfplan from a previous aborted run is"
		fail "    interfering; remove $HERE/.kl-plan-*.tfplan and re-run"
		exit 1
	fi
}

run_apply_capture() {
	local log_path="$1"
	shift
	set +e
	"$@" 2>&1 | tee "$log_path"
	local rc=${PIPESTATUS[0]}
	set -e
	return "$rc"
}

is_stale_plan_log() {
	local log_path="$1"
	grep -q "Error: plan is stale:" "$log_path"
}

run_blocker() {
	preflight
	step "${BOLD}blocker${RESET}: detecting trunk versions"
	detect_versions
	local target_value
	target_value="$(role_target blocker)"
	info "state=$STATE_NAME trunk slow_a=$SLOW_A_CURRENT slow_b=$SLOW_B_CURRENT"
	info "blocker will bump slow_a → $target_value (slow_b pinned)"

	step "${BOLD}blocker${RESET}: generating file-scoped plan"
	build_plan "$PLAN_BLOCKER_SPEC" "$target_value"
	ok "plan ready: $PLAN_BLOCKER_SPEC (write_set = [time_sleep.slow_a])"

	step "${BOLD}blocker${RESET}: running apply (--orchestrated, ≈30 s due to time_sleep)"
	info "leave this terminal alone and switch to terminal 2."
	info "terminal 2 should run: examples/big-state/wait-demo.sh waiter"
	if "$KL_BIN" apply \
		--orchestrated \
		--plan-spec="$PLAN_BLOCKER_SPEC" \
		--state="$STATE_NAME" \
		--wait-timeout=0
	then
		ok "blocker committed; waiter's wait loop should now clear."
	else
		fail "blocker apply failed; see above."
		exit 1
	fi
}

run_waiter() {
	preflight
	step "${BOLD}waiter${RESET}: detecting trunk versions"
	detect_versions
	local target_value log_path
	target_value="$(role_target waiter)"
	log_path="${TMPDIR:-/tmp}/wait-demo-waiter-$$.log"
	info "state=$STATE_NAME trunk slow_a=$SLOW_A_CURRENT slow_b=$SLOW_B_CURRENT"
	info "waiter will bump slow_a → $target_value (slow_b pinned)"

	step "${BOLD}waiter${RESET}: generating file-scoped plan"
	build_plan "$PLAN_WAITER_SPEC" "$target_value"
	ok "plan ready: $PLAN_WAITER_SPEC (write_set = [time_sleep.slow_a])"

	step "${BOLD}waiter${RESET}: running apply with --orchestrated --wait-timeout=$WAIT_TIMEOUT"
	info "if a blocker is holding the reservation, you will see"
	info "  [apply: waiting Xs/Ys] blocked by N reservation(s) on \"$STATE_NAME\""
	info "every ~5 s until the blocker releases."
	if run_apply_capture "$log_path" \
		"$KL_BIN" apply \
		--orchestrated \
		--plan-spec="$PLAN_WAITER_SPEC" \
		--state="$STATE_NAME" \
		--wait-timeout="$WAIT_TIMEOUT"
	then
		ok "waiter committed (wait loop did its job)."
		return 0
	fi

	if is_stale_plan_log "$log_path"; then
		warn "waiter plan became stale after the blocker committed. Replanning against the new trunk serial and retrying once."
		step "${BOLD}waiter${RESET}: refresh trunk view after blocker commit"
		detect_versions
		info "new trunk slow_a=$SLOW_A_CURRENT slow_b=$SLOW_B_CURRENT"
		step "${BOLD}waiter${RESET}: rebuilding plan against the new trunk"
		build_plan "$PLAN_WAITER_SPEC" "$target_value"
		ok "replan ready: $PLAN_WAITER_SPEC (write_set = [time_sleep.slow_a])"
		step "${BOLD}waiter${RESET}: retry apply after replan"
		if "$KL_BIN" apply \
			--orchestrated \
			--plan-spec="$PLAN_WAITER_SPEC" \
			--state="$STATE_NAME" \
			--wait-timeout=0
		then
			ok "waiter committed after replan (expected same-resource behavior)."
			return 0
		fi
		fail "waiter retry failed after replanning; see output above."
		exit 1
	fi

	fail "waiter apply failed; see above."
	warn "if the message is \"reservation conflict\" + the wait elapsed ≈ $WAIT_TIMEOUT,"
	warn "the blocker held longer than the wait budget. Re-run with a bigger WAIT_TIMEOUT."
	exit 1
}

run_both() {
	preflight
	step "single-terminal both-modes run"
	info "Tip: for the canonical demo, use two terminals. This mode"
	info "streams the blocker's stdout to $BLOCKER_LOG so the wait"
	info "block from the waiter is visible live in this terminal."

	# Background the blocker; foreground the waiter once the
	# blocker has had ~3s to acquire its reservation.
	( "$0" blocker >"$BLOCKER_LOG" 2>&1 ) &
	local blocker_pid=$!
	info "blocker PID=$blocker_pid; log=$BLOCKER_LOG"
	info "waiting 3 s for blocker to acquire reservation..."
	sleep 3
	if ! kill -0 "$blocker_pid" 2>/dev/null; then
		fail "blocker exited before waiter started; check $BLOCKER_LOG"
		tail -30 "$BLOCKER_LOG" || true
		exit 1
	fi
	"$0" waiter
	local rc_waiter=$?

	wait "$blocker_pid" || true
	if [ $rc_waiter -ne 0 ]; then
		fail "waiter failed; blocker log tail:"
		tail -30 "$BLOCKER_LOG" || true
		exit 1
	fi
	ok "both applies committed. blocker log: $BLOCKER_LOG"
}

case "$MODE" in
	blocker) run_blocker ;;
	waiter)  run_waiter ;;
	both)    run_both ;;
	help|--help|-h) usage ;;
	*) fail "unknown mode: $MODE"; usage; exit 2 ;;
esac
