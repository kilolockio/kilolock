#!/usr/bin/env bash
# End-to-end smoke test for Kilolock.
#
# Pipeline (all from one command):
#
#   1. Ensure Postgres is running (docker-compose up -d).
#   2. Build ./bin/kl fresh.
#   3. Apply migrations.
#   4. Start the HTTP backend server in the background.
#   5. Poll /healthz until ready (10s timeout).
#   6. Copy the testdata/smoke fixture into a temp dir.
#   7. terraform init + apply against the backend.
#   8. Assert the normalized rows are what we expect.
#   9. Force a second apply with a changed variable to exercise the
#      lock + serial path again.
#  10. terraform destroy.
#  11. Assert the state is gone from the server.
#  12. Stop the server, optionally tear down Postgres.
#
# Env vars:
#   TF_BIN                       Terraform CLI to drive (default: terraform).
#                                Set to "tofu" to run against OpenTofu instead.
#   KEEP_DB                      If non-empty, leave Postgres running on exit.
#   KEEP_TMP                     If non-empty, leave the temp Terraform dir on
#                                disk and print its path. Useful for debugging.
#   KL_LISTEN            Listen address for the backend (default :8181).
#   KL_USE_EXISTING_DB   If set, assume Postgres is already up at
#                                KL_DATABASE_URL (e.g. a CI service
#                                container) and don't run docker-compose.
#   KL_DATABASE_URL      Override the database connection string.
#   KL_AUTH_MODE         Backend auth mode for the smoke server
#                                (default: open, because this smoke focuses
#                                on state/read-write behavior rather than
#                                token bootstrap flows).
#
# Exit code is non-zero on any failure. The script is safe to re-run.

set -euo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TF_BIN="${TF_BIN:-terraform}"
LISTEN_ADDR="${KL_LISTEN:-:8181}"
SERVER_HOST="${LISTEN_ADDR#:}"
SERVER_URL="http://localhost:${SERVER_HOST}"
STATE_NAME="smoke-$(date +%s)-$$"
DB_URL="${KL_DATABASE_URL:-postgres://kl:kl@localhost:5432/kl?sslmode=disable}"
USE_EXISTING_DB="${KL_USE_EXISTING_DB:-}"
AUTH_MODE="${KL_AUTH_MODE:-open}"

# Resolve docker-compose vs docker compose only when we're going to use
# it (the CI path uses a service container and doesn't have docker in
# the runner's job container).
COMPOSE=""
if [[ -z "$USE_EXISTING_DB" ]]; then
    if command -v docker-compose >/dev/null 2>&1; then
        COMPOSE="docker-compose"
    elif docker compose version >/dev/null 2>&1; then
        COMPOSE="docker compose"
    else
        echo "smoke: neither docker-compose nor 'docker compose' is available" >&2
        exit 2
    fi
fi

TMP_DIR=""
SERVER_PID=""
SERVER_LOG=""

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------

log()   { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
fatal() { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Cleanup: runs on any exit, success or failure.
# ---------------------------------------------------------------------------

cleanup() {
    local rc=$?
    set +e

    if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        log "stopping server (pid=$SERVER_PID)"
        kill "$SERVER_PID" 2>/dev/null
        wait "$SERVER_PID" 2>/dev/null
    fi

    if [[ $rc -ne 0 && -n "$SERVER_LOG" && -f "$SERVER_LOG" ]]; then
        warn "server log (last 50 lines):"
        tail -n 50 "$SERVER_LOG" >&2
    fi

    if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
        if [[ -n "${KEEP_TMP:-}" ]]; then
            warn "leaving temp dir at $TMP_DIR (KEEP_TMP set)"
        else
            rm -rf "$TMP_DIR"
        fi
    fi

    if [[ -z "$USE_EXISTING_DB" && -z "${KEEP_DB:-}" ]]; then
        log "stopping postgres"
        $COMPOSE down --remove-orphans >/dev/null 2>&1
    elif [[ -n "$USE_EXISTING_DB" ]]; then
        : # caller manages Postgres lifecycle
    else
        warn "leaving postgres running (KEEP_DB set)"
    fi

    exit $rc
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# Pre-flight tool checks
# ---------------------------------------------------------------------------

require_bin() {
    command -v "$1" >/dev/null 2>&1 || fatal "$1 not found in PATH"
}

require_bin go
require_bin "$TF_BIN"
require_bin jq
require_bin curl
if [[ -z "$USE_EXISTING_DB" ]]; then
    require_bin docker
fi

log "tooling: $($TF_BIN version | head -n1), $(go version), $(jq --version)"

# ---------------------------------------------------------------------------
# 1. Postgres
# ---------------------------------------------------------------------------

wait_for_postgres() {
    # Probe the *host-side* TCP endpoint. The docker-compose healthcheck
    # turns green from inside the container slightly before the published
    # port is reachable; we care about the latter.
    local host=localhost port=5432
    for i in $(seq 1 60); do  # 60 * 0.5s = 30s budget
        if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
            exec 3<&- 3>&-
            # One more round-trip via pg_isready inside the container if
            # we have compose available, to make sure auth init has run.
            if [[ -n "$COMPOSE" ]] \
               && $COMPOSE exec -T postgres pg_isready -U kl >/dev/null 2>&1; then
                return 0
            fi
            # No compose (CI path): fall back to actually attempting a
            # connection from the binary later. Returning here is fine
            # because the migration step retries on transient failures.
            [[ -z "$COMPOSE" ]] && return 0
        fi
        if [[ $i -eq 60 ]]; then
            fatal "postgres did not accept connections within 30s"
        fi
        sleep 0.5
    done
}

if [[ -n "$USE_EXISTING_DB" ]]; then
    log "using existing postgres at $DB_URL"
    log "waiting for postgres to accept connections"
    wait_for_postgres
else
    log "bringing up postgres"
    $COMPOSE up -d >/dev/null

    log "waiting for postgres to accept connections"
    wait_for_postgres
fi

# ---------------------------------------------------------------------------
# 2. Build the binary fresh
# ---------------------------------------------------------------------------

log "building ./bin/kl and ./bin/kld"
mkdir -p bin
go build -o bin/kl ./cmd/kl
go build -o bin/kld ./cmd/kld

# ---------------------------------------------------------------------------
# 3. Migrate
# ---------------------------------------------------------------------------

log "applying migrations"
KL_DATABASE_URL="$DB_URL" ./bin/kld migrate >/dev/null

# ---------------------------------------------------------------------------
# 4. Start the server in the background
# ---------------------------------------------------------------------------

SERVER_LOG="${TMPDIR:-/tmp}/kl-smoke-server.$$.log"
: >"$SERVER_LOG"

log "starting server on $LISTEN_ADDR (log: $SERVER_LOG)"
KL_DATABASE_URL="$DB_URL" \
KL_AUTH_MODE="$AUTH_MODE" \
KL_LISTEN_ADDR="$LISTEN_ADDR" \
KL_LOG_FORMAT=text \
KL_LOG_LEVEL=info \
  ./bin/kld serve >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

# ---------------------------------------------------------------------------
# 5. Poll /healthz
# ---------------------------------------------------------------------------

log "waiting for $SERVER_URL/healthz"
for i in $(seq 1 50); do  # 50 * 0.2s = 10s budget
    if curl -fsS "$SERVER_URL/healthz" >/dev/null 2>&1; then
        log "server ready"
        break
    fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
        fatal "server exited before becoming healthy; see log above"
    fi
    if [[ $i -eq 50 ]]; then
        fatal "server did not become healthy within 10s"
    fi
    sleep 0.2
done

# ---------------------------------------------------------------------------
# 6. Stage the Terraform fixture in a temp dir
# ---------------------------------------------------------------------------

TMP_DIR="$(mktemp -d -t kl-smoke.XXXXXX)"
log "staging fixture into $TMP_DIR"
cp -R testdata/smoke/. "$TMP_DIR/"

cat >"$TMP_DIR/backend.tf" <<EOF
terraform {
  backend "http" {
    address        = "$SERVER_URL/states/$STATE_NAME"
    lock_address   = "$SERVER_URL/states/$STATE_NAME"
    unlock_address = "$SERVER_URL/states/$STATE_NAME"
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
EOF

# Honor the user's plugin cache if they have one configured; otherwise
# pin a local one for this run so repeated invocations don't re-download.
if [[ -z "${TF_PLUGIN_CACHE_DIR:-}" ]]; then
    export TF_PLUGIN_CACHE_DIR="$REPO_ROOT/.terraform-plugin-cache"
fi
mkdir -p "$TF_PLUGIN_CACHE_DIR"

# Less noise from terraform; we still capture full output via tee on failure.
export TF_IN_AUTOMATION=1

# ---------------------------------------------------------------------------
# 7. First apply
# ---------------------------------------------------------------------------

pushd "$TMP_DIR" >/dev/null

log "$TF_BIN init"
"$TF_BIN" init -input=false -no-color >/dev/null

log "$TF_BIN apply (first)"
"$TF_BIN" apply -input=false -auto-approve -no-color >/dev/null

popd >/dev/null

# ---------------------------------------------------------------------------
# 8. Assertions against the normalized rows
# ---------------------------------------------------------------------------

query() {
    local sql=$1
    KL_API_URL="$SERVER_URL" \
        ./bin/kl query --format json "$sql"
}

assert_count() {
    local what=$1 sql=$2 op=$3 expected=$4
    local got
    got=$(query "$sql" | jq -r '.[0] | (.count // .n // (to_entries[0].value))')
    case "$op" in
        eq) [[ "$got" -eq "$expected" ]] ;;
        ge) [[ "$got" -ge "$expected" ]] ;;
        *)  fatal "assert_count: unknown op $op" ;;
    esac
    if [[ $? -ne 0 ]]; then
        fatal "$what: expected $op $expected, got $got"
    fi
    log "  ✓ $what = $got"
}

log "asserting normalized rows after first apply"

assert_count "states.count" \
    "SELECT count(*)::int AS count FROM states WHERE name = '$STATE_NAME'" \
    eq 1

# Expected resources:
#   random_pet.name, null_resource.marker, null_resource.stamp,
#   module.tag.random_id.this = 4 managed resources
assert_count "resources.count" \
    "SELECT count(*)::int AS count FROM current_resources WHERE state_name = '$STATE_NAME' AND mode = 'managed'" \
    eq 4

assert_count "module-resources.count" \
    "SELECT count(*)::int AS count FROM current_resources WHERE state_name = '$STATE_NAME' AND mode = 'managed' AND module_path <> ''" \
    eq 1

assert_count "edges.count" \
    "SELECT count(*)::int AS count FROM current_resource_dependencies rd JOIN current_resources r ON r.id = rd.from_resource_id WHERE r.state_name = '$STATE_NAME'" \
    ge 1

assert_count "outputs.count" \
    "SELECT count(*)::int AS count FROM outputs o JOIN states s ON s.current_version_id = o.state_version_id WHERE s.name = '$STATE_NAME'" \
    eq 2

log "asserting read-only enforcement"
if KL_API_URL="$SERVER_URL" \
   ./bin/kl query "DELETE FROM resources" >/dev/null 2>&1; then
    fatal "DELETE was accepted by kl query; read-only enforcement broken"
fi
log "  ✓ writes rejected by server"

# ---------------------------------------------------------------------------
# 8a. Provider-aware refresh round-trip (v1.6c)
#
# We exercise the path `kl refresh` will take in production:
#
#   1. Discover the provider binaries from the temp dir's
#      .terraform/providers tree (populated by `terraform init`
#      a few steps ago, optionally backed by TF_PLUGIN_CACHE_DIR).
#   2. Launch each provider, fetch + cache its schema in
#      provider_schemas, and Configure with an empty config block
#      (these providers need none).
#   3. ReadResource for every managed instance and either commit
#      a new state_version with source='refresh' or, for --dry-run,
#      report counters only.
#
# Two assertions:
#   * --dry-run does not bump the serial; an audit row lands with
#     status='succeeded'.
#   * Live refresh writes a new state_version whose source='refresh'.
#
# Refresh emits hclog lines from each provider during launch/shutdown
# — that's not noise we control. We capture stderr to a file so the
# smoke log stays focused on assertions; the file is shown on failure
# by the cleanup trap (set via SERVER_LOG; refresh uses its own).
# ---------------------------------------------------------------------------

REFRESH_LOG="${TMPDIR:-/tmp}/kl-smoke-refresh.$$.log"
: >"$REFRESH_LOG"

refresh() {
    (
        cd "$TMP_DIR"
        KL_DATABASE_URL="$DB_URL" \
        KL_LOG_LEVEL=warn \
            "$REPO_ROOT/bin/kl" refresh \
                --provider-search-path="$TMP_DIR/.terraform/providers" \
                "$@"
    ) 2>>"$REFRESH_LOG"
}

log "refresh --dry-run (provider-aware, no commit)"
if ! refresh --dry-run "$STATE_NAME" >/dev/null; then
    warn "refresh --dry-run stderr (last 30 lines):"
    tail -n 30 "$REFRESH_LOG" >&2
    fatal "refresh --dry-run failed; see log above"
fi

assert_count "refresh-runs.count (after dry-run)" \
    "SELECT count(*)::int AS count FROM refresh_runs rr JOIN states s ON s.id = rr.state_id WHERE s.name = '$STATE_NAME' AND rr.status = 'succeeded'" \
    eq 1

# Dry-run must not touch state_versions. We snapshot the pre-refresh
# count and assert equality after.
SV_BEFORE=$(query "SELECT count(*)::int AS count FROM state_versions sv JOIN states s ON s.id = sv.state_id WHERE s.name = '$STATE_NAME'" | jq -r '.[0].count')

log "refresh (live, commits source='refresh')"
if ! refresh "$STATE_NAME" >/dev/null; then
    warn "refresh stderr (last 30 lines):"
    tail -n 30 "$REFRESH_LOG" >&2
    fatal "refresh failed; see log above"
fi

assert_count "refresh-runs.count (after live)" \
    "SELECT count(*)::int AS count FROM refresh_runs rr JOIN states s ON s.id = rr.state_id WHERE s.name = '$STATE_NAME' AND rr.status = 'succeeded'" \
    eq 2

# A live refresh must always commit a new version (always-write-on-
# no-drift is part of v1.6c's contract — it keeps the version chain
# honest about when refresh ran).
SV_AFTER=$(query "SELECT count(*)::int AS count FROM state_versions sv JOIN states s ON s.id = sv.state_id WHERE s.name = '$STATE_NAME'" | jq -r '.[0].count')
if [[ "$SV_AFTER" -le "$SV_BEFORE" ]]; then
    fatal "live refresh did not bump state_versions: before=$SV_BEFORE after=$SV_AFTER"
fi
log "  ✓ state_versions bumped: $SV_BEFORE → $SV_AFTER"

# The new version must be tagged with source='refresh'. This is
# important: downstream tools (drift dashboards, audit pipelines)
# differentiate refresh-driven versions from apply-driven ones.
assert_count "refresh-sourced versions" \
    "SELECT count(*)::int AS count FROM state_versions sv JOIN states s ON s.id = sv.state_id WHERE s.name = '$STATE_NAME' AND sv.source = 'refresh'" \
    ge 1

# Resource shape and counts must be preserved across a refresh
# (these providers report no drift, so the new version is byte-for-
# byte equivalent post-normalization).
assert_count "resources.count (after refresh)" \
    "SELECT count(*)::int AS count FROM current_resources WHERE state_name = '$STATE_NAME' AND mode = 'managed'" \
    eq 4

if [[ -z "${KEEP_TMP:-}" ]]; then
    rm -f "$REFRESH_LOG"
fi

# ---------------------------------------------------------------------------
# 9. Second apply (changed var) to exercise serial bump + lock round-trip
# ---------------------------------------------------------------------------

pushd "$TMP_DIR" >/dev/null

log "$TF_BIN apply (second, name_length=3)"
"$TF_BIN" apply -input=false -auto-approve -no-color -var=name_length=3 >/dev/null

popd >/dev/null

assert_count "state-versions.count" \
    "SELECT count(*)::int AS count FROM state_versions sv JOIN states s ON s.id = sv.state_id WHERE s.name = '$STATE_NAME'" \
    ge 2

# ---------------------------------------------------------------------------
# 10. Destroy
# ---------------------------------------------------------------------------

pushd "$TMP_DIR" >/dev/null

log "$TF_BIN destroy"
"$TF_BIN" destroy -input=false -auto-approve -no-color -var=name_length=3 >/dev/null

popd >/dev/null

# After destroy, the state row still exists but the current_version has
# no resources. (Terraform writes an empty-resource state, then issues a
# DELETE on the state path on `terraform workspace delete`, which we
# don't run here. That matches every other http-backend deployment.)
assert_count "resources-after-destroy.count" \
    "SELECT count(*)::int AS count FROM current_resources WHERE state_name = '$STATE_NAME' AND mode = 'managed'" \
    eq 0

log "smoke succeeded"
