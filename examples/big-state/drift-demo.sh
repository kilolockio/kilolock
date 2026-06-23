#!/usr/bin/env bash
# drift-demo.sh — simulate refresh-discovered drift on the big-state example.
#
# This keeps the public demo story in one place:
#   - wait-demo.sh     => same resource, visible reservation waiting
#   - parallel-demo.sh => different resources, parallel apply saves time
#   - drift-demo.sh    => out-of-band drift becomes a cheap backend query

set -euo pipefail

STATE_NAME="${STATE_NAME:-}"
SERVER_URL="${KL_SERVER_URL:-http://localhost:8080}"
TF_BIN="${TF_BIN:-terraform}"
SUMMARY_ADDRESS="${SUMMARY_ADDRESS:-null_resource.summary}"
KEEP_DRIFT="${KEEP_DRIFT:-}"
BACKEND_ADDRESS="${TF_HTTP_ADDRESS:-}"
BACKEND_USERNAME="${KL_USERNAME:-${TF_HTTP_USERNAME:-${TF_HTTP_USER:-}}}"
BACKEND_PASSWORD="${KL_PASSWORD:-${TF_HTTP_PASSWORD:-}}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
KL_BIN="${KL_BIN:-$REPO_ROOT/bin/kl}"
TMP_DIR="$(mktemp -d -t kl-big-state-drift.XXXXXX)"

cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT

if [ -t 1 ] && command -v tput >/dev/null 2>&1 && [ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]; then
	BOLD="$(tput bold)"; DIM="$(tput dim)"
	GREEN="$(tput setaf 2)"; YELLOW="$(tput setaf 3)"; RED="$(tput setaf 1)"
	RESET="$(tput sgr0)"
else
	BOLD=""; DIM=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi
step() { printf "%s==>%s %s\n" "$BOLD" "$RESET" "$*"; }
info() { printf "%s    %s%s\n" "$DIM" "$*" "$RESET"; }
ok()   { printf "%s    ✓ %s%s\n" "$GREEN" "$*" "$RESET"; }
warn() { printf "%s    ! %s%s\n" "$YELLOW" "$*" "$RESET"; }
fail() { printf "%s    ✗ %s%s\n" "$RED" "$*" "$RESET"; exit 1; }

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
    value = cfg.get(key) or ""
    print(value)
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
	if [ -n "$BACKEND_ADDRESS" ] && [ -z "${KL_SERVER_URL:-}" ]; then
		SERVER_URL="$(python3 - "$BACKEND_ADDRESS" <<'PY'
from urllib.parse import urlparse
import sys
u = urlparse(sys.argv[1].strip())
print(f"{u.scheme}://{u.netloc}")
PY
)"
	fi
	if [ -z "$STATE_NAME" ] && [ -n "$BACKEND_ADDRESS" ]; then
		STATE_NAME="$(python3 - "$BACKEND_ADDRESS" <<'PY'
from urllib.parse import urlparse
import sys
u = urlparse(sys.argv[1].strip())
path = u.path.strip("/")
prefix = "v1/states/"
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

fetch_state() {
	local out="$1"
	local url="${BACKEND_ADDRESS:-$SERVER_URL/states/$STATE_NAME}"
	if [ -n "$BACKEND_USERNAME" ] || [ -n "$BACKEND_PASSWORD" ]; then
		curl -fsS -u "$BACKEND_USERNAME:$BACKEND_PASSWORD" "$url" -o "$out"
	else
		curl -fsS "$url" -o "$out"
	fi
}

usage() {
	cat <<USAGE
${BOLD}drift-demo.sh — drift detection on big-state${RESET}

Usage:
  examples/big-state/drift-demo.sh

What it does:
  1. Fetch the current big-state snapshot from kld.
  2. Pin the byte form once so only the intentionally-mutated resource differs.
  3. Simulate out-of-band drift on ${SUMMARY_ADDRESS} via
     kl import --source=refresh.
  4. Query current_resource_drift to show the changed note value.
  5. Restore the canonical state unless KEEP_DRIFT=1 is set.

Prereqs:
  - make build has produced bin/kl
  - terraform init has been run in $HERE
  - the state already exists (bootstrap with terraform apply -refresh=false)
  - kld is reachable at ${SERVER_URL} (or override KL_SERVER_URL)
USAGE
}

preflight() {
	[ -x "$KL_BIN" ] || fail "kl binary not at $KL_BIN (run make build)"
	command -v "$TF_BIN" >/dev/null || fail "terraform binary '$TF_BIN' not on PATH"
	command -v jq >/dev/null || fail "jq is required"
	command -v curl >/dev/null || fail "curl is required"
	command -v python3 >/dev/null || fail "python3 is required"
	[ -d "$HERE/.terraform" ] || fail "run terraform init in $HERE first"
	discover_backend
	curl -fsS "$SERVER_URL/healthz" >/dev/null 2>&1 || fail "kld is not responding at $SERVER_URL"
	"$KL_BIN" list >/dev/null 2>&1 || fail "kl cannot reach the runtime API from this directory"
}

bump_serial() {
	local src="$1" dst="$2"
	python3 - "$src" "$dst" <<'PY'
import re, sys
src = open(sys.argv[1]).read()
match = re.search(r'"serial"\s*:\s*(\d+)', src)
assert match, 'could not find top-level serial'
serial = int(match.group(1)) + 1
out, count = re.subn(r'"serial"\s*:\s*\d+', f'"serial":{serial}', src, count=1)
assert count == 1, 'could not bump top-level serial'
open(sys.argv[2], 'w').write(out)
PY
}

mutate_note() {
	local src="$1" dst="$2" current_note="$3" new_note="$4"
	python3 - "$src" "$dst" "$current_note" "$new_note" <<'PY'
import re, sys
src = open(sys.argv[1]).read()
current = sys.argv[3]
new = sys.argv[4]
match = re.search(r'"serial"\s*:\s*(\d+)', src)
assert match, 'could not find top-level serial'
serial = int(match.group(1)) + 1
out, count = re.subn(r'"serial"\s*:\s*\d+', f'"serial":{serial}', src, count=1)
assert count == 1, 'could not bump top-level serial'
pattern = re.compile(r'("note"\s*:\s*")' + re.escape(current) + r'(")')
out, count = pattern.subn(r'\1' + new + r'\2', out, count=1)
assert count == 1, 'could not find summary note to mutate'
open(sys.argv[2], 'w').write(out)
PY
}

run_query() {
	local format="$1" sql="$2"
	"$KL_BIN" query --format "$format" "$sql"
}

main() {
	case "${1:-}" in
		help|--help|-h)
			usage
			exit 0
			;;
	esac

	preflight

	local current_raw pinned_next pinned_raw drifted_raw restore_raw current_note drifted_note
	current_raw="$TMP_DIR/state-current.tfstate"
	pinned_next="$TMP_DIR/state-pinned-next.tfstate"
	pinned_raw="$TMP_DIR/state-pinned.tfstate"
	drifted_raw="$TMP_DIR/state-drifted.tfstate"
	restore_raw="$TMP_DIR/state-restored.tfstate"

	step "fetch current state"
	fetch_state "$current_raw" || fail "state '$STATE_NAME' not found or backend auth/path is wrong; bootstrap once with terraform apply -refresh=false"
	current_note="$(jq -r '.resources[] | select(.type=="null_resource" and .name=="summary") | .instances[0].attributes.triggers.note' "$current_raw")"
	[ -n "$current_note" ] && [ "$current_note" != "null" ] || fail "could not find ${SUMMARY_ADDRESS} note in current state"
	info "current ${SUMMARY_ADDRESS} note = $current_note"

	step "pin canonical byte form"
	bump_serial "$current_raw" "$pinned_next"
	"$KL_BIN" import --name="$STATE_NAME" --source=apply "$pinned_next" >/dev/null
	fetch_state "$pinned_raw"
	ok "canonical state pinned through source='apply'"

	drifted_note="drifted-via-console-$RANDOM"
	step "simulate out-of-band drift"
	mutate_note "$pinned_raw" "$drifted_raw" "$current_note" "$drifted_note"
	"$KL_BIN" import --name="$STATE_NAME" --source=refresh "$drifted_raw" >/dev/null
	ok "refresh-style drift recorded for ${SUMMARY_ADDRESS}"

	step "query current drift"
	local drift_sql drift_json
	read -r -d '' drift_sql <<SQL || true
SELECT address,
       previous_attributes->'triggers'->>'note' AS previous_note,
       current_attributes->'triggers'->>'note'  AS current_note,
       detected_at_serial
FROM current_resource_drift
WHERE state_name = '$STATE_NAME'
  AND address = '$SUMMARY_ADDRESS'
ORDER BY detected_at_serial DESC
LIMIT 1
SQL
	drift_json="$(run_query json "$drift_sql")"
	if [ "$(jq 'length' <<<"$drift_json")" -eq 0 ]; then
		fail "no drift row found for ${SUMMARY_ADDRESS}"
	fi
	run_query table "$drift_sql"
	ok "drift is now queryable without a full terraform plan"

	if [ -n "$KEEP_DRIFT" ]; then
		warn "KEEP_DRIFT is set; leaving the refresh-discovered drift in place"
		warn "Restore later with: terraform apply -refresh=false"
		exit 0
	fi

	step "restore canonical state"
	bump_serial "$pinned_raw" "$restore_raw"
	"$KL_BIN" import --name="$STATE_NAME" --source=apply "$restore_raw" >/dev/null
	local post_restore_json
	post_restore_json="$(run_query json "$drift_sql")"
	if [ "$(jq 'length' <<<"$post_restore_json")" -ne 0 ]; then
		fail "drift row still present after restore"
	fi
	ok "drift cleared; state is back to the checked-in big-state defaults"
}

main "$@"
