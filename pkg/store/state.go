package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/auth"
)

// ErrStateNotFound is returned when a state with the given name does not
// exist or has no versions yet. Callers translate this to HTTP 404.
var ErrStateNotFound = errors.New("state not found")

// ErrLockMismatch is returned when a write or delete is attempted with a
// lock ID that does not match the currently held lock (or when no lock is
// held but one was expected). Callers translate this to HTTP 409.
var ErrLockMismatch = errors.New("lock id mismatch")

// ErrStateLocked is returned when a write is attempted while another lock
// is held and the caller supplied no lock ID. Callers translate this to
// HTTP 423.
var ErrStateLocked = errors.New("state is locked")

// ErrInvalidState is returned when the supplied rawState is not a valid
// Terraform v4 state document. Callers translate this to HTTP 400.
var ErrInvalidState = errors.New("invalid state document")

// ErrSerialConflict is returned when a write tries to insert a state
// version whose Terraform serial collides with one already stored for
// this state. Callers translate this to HTTP 409.
var ErrSerialConflict = errors.New("state serial conflict")

// ErrLineageMismatch is returned when a POST presents a state whose
// `lineage` field differs from the trunk's. Lineage is Terraform's
// identity-of-state UUID; a mismatch means the operator is pushing a
// different state entirely. We never merge across lineages — that
// would conflate independent Terraform configurations.
//
// Callers translate this to HTTP 409.
var ErrLineageMismatch = errors.New("state lineage mismatch")

// ErrEntitlementExceeded is returned when a write violates tenant-level
// plan limits (e.g. max_state_resources).
var ErrEntitlementExceeded = errors.New("entitlement exceeded")

// StateNotActiveError is returned when a caller tries to use a state
// that still exists but is no longer active (for example, archived via
// the customer portal and awaiting support-led restore).
type StateNotActiveError struct {
	Status LifecycleStatus
}

func (e *StateNotActiveError) Error() string {
	if e == nil || e.Status == "" {
		return "state is not active"
	}
	return fmt.Sprintf("state is %s", e.Status)
}

// WriteSetConflictError is returned from WriteState when the
// optimistic-merge path detects that addresses the operator tried to
// write also moved in trunk between their lock acquisition and their
// commit attempt.
//
// The error carries the list of conflicting addresses and a snapshot
// of the offending commit's metadata so the HTTP layer can surface a
// human-readable 409 body that points operators at the exact rows
// they need to re-plan against.
type WriteSetConflictError struct {
	// Addresses is the sorted intersection of "what I tried to write"
	// and "what got committed between my source serial and now."
	Addresses []string

	// LatestSerial is trunk's serial at the moment we detected the
	// conflict — what the operator's next plan should start from.
	LatestSerial int64

	// LatestActor is who committed the conflicting change (best-effort,
	// from state_versions.created_by; may be empty for older rows).
	LatestActor string
}

// Error renders the conflict as a single-line message suitable for
// logs. The HTTP handler produces a multi-line operator-facing
// version separately.
func (e *WriteSetConflictError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Addresses) == 0 {
		return "state write-set conflict"
	}
	return fmt.Sprintf("state write-set conflict on %d address(es): %v", len(e.Addresses), e.Addresses)
}

// CurrentStateInfo bundles the current-state metadata the refresh
// orchestrator needs in one consistent snapshot. The four fields are
// always read together because separate queries would be racey
// against concurrent writers.
//
// Raw is exactly what was last written and may be large; callers are
// expected to feed it directly to tfstate.Parse.
type CurrentStateInfo struct {
	StateID   string
	VersionID string
	Serial    int64
	Raw       []byte
}

// GetCurrentStateInfo returns enough metadata to begin a refresh run
// for the named state in one round trip: the state's row id, the
// current state_version's row id, that version's serial, and the
// raw JSON.
//
// Returns ErrStateNotFound when the state does not exist or has no
// versions yet — both of which should preempt any refresh attempt.
//
// The state lookup is scoped by the caller's tenant via the
// principal in ctx (see internal/auth). Cross-tenant access is
// impossible by construction: the (tenant_id, name) uniqueness
// added in migration 0009 means another tenant with the same state
// name is a different row entirely.
func (s *Store) GetCurrentStateInfo(ctx context.Context, name string) (*CurrentStateInfo, error) {
	where, args := s.stateByNameWhere(ctx, name)
	q := `
		SELECT s.id, sv.id, sv.serial, sv.raw_state::text
		FROM   states s
		JOIN   state_versions sv ON sv.id = s.current_version_id
		WHERE  ` + where
	var (
		stateID, versionID string
		serial             int64
		raw                string
	)
	err := s.pool.QueryRow(ctx, q, args...).Scan(&stateID, &versionID, &serial, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query current state info: %w", err)
	}
	return &CurrentStateInfo{
		StateID:   stateID,
		VersionID: versionID,
		Serial:    serial,
		Raw:       []byte(raw),
	}, nil
}

// EnsureCurrentStateInfo returns the current state info, creating a
// serial-0 empty genesis version when the state does not exist yet.
func (s *Store) EnsureCurrentStateInfo(ctx context.Context, name string) (*CurrentStateInfo, error) {
	info, err := s.GetCurrentStateInfo(ctx, name)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, ErrStateNotFound) {
		return nil, err
	}

	tenantID := auth.TenantFromContext(ctx)
	emptyParsed := tfstate.EmptyState("")
	emptyRaw, err := tfstate.EmptyStateBytes("")
	if err != nil {
		return nil, err
	}
	var out CurrentStateInfo
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		stateID, err := upsertState(ctx, tx, tenantID, name, "")
		if err != nil {
			return err
		}

		var (
			gotStateID string
			versionID  string
			serial     int64
			raw        string
		)
		q := `
			SELECT s.id, sv.id, sv.serial, sv.raw_state::text
			FROM   states s
			JOIN   state_versions sv ON sv.id = s.current_version_id
			WHERE  s.id = $1`
		switch err := tx.QueryRow(ctx, q, stateID).Scan(&gotStateID, &versionID, &serial, &raw); {
		case err == nil:
			out = CurrentStateInfo{StateID: gotStateID, VersionID: versionID, Serial: serial, Raw: []byte(raw)}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("query current genesis state info: %w", err)
		}

		if err := tx.QueryRow(ctx,
			`INSERT INTO state_versions
			 	(state_id, tenant_id, serial, terraform_version, raw_state, source, created_by)
			 VALUES ($1, $2, 0, $3, $4::jsonb, 'genesis', 'system')
			 RETURNING id`,
			stateID, tenantID, emptyParsed.TerraformVersion, string(emptyRaw),
		).Scan(&versionID); err != nil {
			if isUniqueViolation(err, "state_versions_state_id_serial_key") {
				// Another bootstrap transaction won. Read it back.
				if err := tx.QueryRow(ctx, q, stateID).Scan(&out.StateID, &out.VersionID, &out.Serial, &raw); err != nil {
					return fmt.Errorf("read concurrent genesis winner: %w", err)
				}
				out.Raw = []byte(raw)
				return nil
			}
			return fmt.Errorf("insert genesis state_version: %w", err)
		}
		if err := normalize(ctx, tx, tenantID, stateID, versionID, 0, emptyParsed); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE states
			 SET current_version_id = $1, updated_at = now()
			 WHERE id = $2`,
			versionID, stateID,
		); err != nil {
			return fmt.Errorf("update genesis current_version_id: %w", err)
		}
		out = CurrentStateInfo{
			StateID:   stateID,
			VersionID: versionID,
			Serial:    0,
			Raw:       emptyRaw,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetStateRawAtSerial returns the raw state JSON bytes for the named state
// at the requested serial.
func (s *Store) GetStateRawAtSerial(ctx context.Context, name string, serial int64) ([]byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, ErrStateNotFound
	}
	if serial <= 0 {
		return nil, ErrStateNotFound
	}
	where, args := s.stateByNameWhere(ctx, name)
	args = append(args, serial)
	serialParam := len(args)
	q := `
		SELECT sv.raw_state::text
		FROM   states s
		JOIN   state_versions sv ON sv.state_id = s.id
		WHERE  ` + where + `
		  AND  sv.serial = $` + fmt.Sprint(serialParam)
	var raw string
	err := s.pool.QueryRow(ctx, q, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// LookupStateVersionID returns the state_version row id for the
// (state_id, serial) pair. Used by the refresh orchestrator after
// WriteState to recover the new version's id without racing against
// other writers (the (state_id, serial) tuple is unique by schema
// constraint).
//
// Returns ErrStateNotFound when no row matches.
func (s *Store) LookupStateVersionID(ctx context.Context, stateID string, serial int64) (string, error) {
	if stateID == "" {
		return "", errors.New("LookupStateVersionID: stateID must not be empty")
	}
	const q = `
		SELECT id FROM state_versions
		WHERE  state_id = $1 AND serial = $2
	`
	var versionID string
	err := s.pool.QueryRow(ctx, q, stateID, serial).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrStateNotFound
	}
	if err != nil {
		return "", fmt.Errorf("query state_version by serial: %w", err)
	}
	return versionID, nil
}

// GetCurrentState returns the raw JSON of the latest state_version for
// the named state, or ErrStateNotFound when the state has no versions.
//
// The returned bytes are exactly what was last written; callers are not
// expected to parse them. Scoped by the caller's tenant; cross-tenant
// reads return ErrStateNotFound rather than the other tenant's data.
func (s *Store) GetCurrentState(ctx context.Context, name string) ([]byte, error) {
	where, args := s.stateByNameWhere(ctx, name)
	q := `
		SELECT sv.raw_state::text
		FROM   states s
		JOIN   state_versions sv ON sv.id = s.current_version_id
		WHERE  ` + where
	var raw string
	err := s.pool.QueryRow(ctx, q, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// WriteState parses, stores, and normalizes a new state version for the
// named state. It is the single write entry point for both the HTTP
// backend POST and the `kl import` CLI.
//
// Lock semantics:
//
//   - If a lock is currently held, lockID must match the lock's ID.
//   - If no lock is held and lockID is non-empty, the caller's lock view
//     is stale; the write is rejected with ErrLockMismatch.
//   - If no lock is held and lockID is empty, the write is allowed
//     (Terraform with locking disabled, or out-of-band imports).
//
// Serial handling:
//
//   - The serial encoded in rawState is preserved as state_versions.serial.
//   - If the same serial already exists for this state, ErrSerialConflict
//     is returned (Terraform's monotonic-serial invariant).
//   - If rawState carries serial 0 or omits it entirely (rare), the next
//     greater serial for the state is computed.
//
// Normalization:
//
//   - Resources, dependencies, and outputs are projected into their
//     respective tables in the same transaction. raw_state remains the
//     source of truth for byte-identical export.
func (s *Store) WriteState(ctx context.Context, name, lockID string, rawState []byte, source, actor string) error {
	return s.writeStateInternal(ctx, name, lockID, rawState, source, actor, false)
}

// WriteStateForApply commits a state version on behalf of the v2 apply
// orchestrator. It is identical to WriteState except that it does NOT
// consult state_locks before writing.
//
// Rationale: the v1 HTTP-backend whole-state lock (state_locks) exists
// to serialize vanilla terraform clients, which have no other way to
// avoid clobbering each other. The v2 apply orchestrator already holds
// row-level reservations on every address in its write set (see
// pkg/store/reservations.go) and re-fetches trunk just before
// commit; the only remaining concurrency invariant — the
// state_versions.serial uniqueness — is enforced by ErrSerialConflict
// from the INSERT below.
//
// Subjecting the orchestrator to state_locks would mean any leaked
// terraform-side lock (a SIGKILLed `terraform plan`, for example)
// bricks every kl apply until an operator manually
// DELETEs the row. That's the bug this method exists to fix.
//
// Coexistence with vanilla terraform (v2e): once vanilla terraform
// clients are modeled as *-glob write reservations on acquire, the
// HTTP-lock and the reservation matrix collapse into the same check
// and this method's "skip lock" carve-out becomes redundant. Until
// then, kl apply may race with an in-flight vanilla terraform
// apply on commit; the race resolves via ErrSerialConflict on
// state_versions, which is safe (no torn writes) but visible as a
// conflict to the operator. Documented in ADR 0007 and the v2e
// roadmap.
func (s *Store) WriteStateForApply(ctx context.Context, name string, rawState []byte, source, actor string) error {
	return s.writeStateInternal(ctx, name, "", rawState, source, actor, true)
}

// writeStateInternal is the shared body of WriteState and
// WriteStateForApply.
//
// Three commit paths live here, selected per-call:
//
//   - bypassLock=true                       → apply-orchestrator path.
//     The lock check is skipped entirely (see WriteStateForApply doc),
//     and rawState is committed verbatim with serial = MAX+1.
//
//   - bypassLock=false, exclusive_locks=true → legacy exclusive path.
//     state_locks must list a row with id == lockID. rawState is
//     committed verbatim with serial = MAX+1.
//
//   - bypassLock=false, exclusive_locks=false → optimistic-merge path.
//     If the lock row carries a source_serial, the incoming state is
//     3-way-merged against trunk (base = state at source_serial,
//     trunk = current). If the operator's write set is disjoint from
//     what committed since source_serial, the merge succeeds and the
//     merged bytes are stored. Otherwise we return WriteSetConflictError
//     and the operator must re-plan.
//
// The lock row that authorized this write is left in place across
// all paths: Terraform's HTTP backend always pairs a LOCK with an
// UNLOCK, even in the optimistic case, and consuming the lock on
// commit would turn the operator's UNLOCK into a "lock not found"
// 409. The lifecycle is: AcquireLock → WriteState → ReleaseLock.
func (s *Store) writeStateInternal(ctx context.Context, name, lockID string, rawState []byte, source, actor string, bypassLock bool) error {
	parsed, err := tfstate.Parse(rawState)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	}

	tenantID := auth.TenantFromContext(ctx)

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		stateID, err := upsertState(ctx, tx, tenantID, name, parsed.Lineage)
		if err != nil {
			return err
		}
		if err := enforceTenantStateLimits(ctx, tx, tenantID, stateID, name, parsed); err != nil {
			return err
		}

		// Branch routing: bypass / exclusive / optimistic.
		exclusive, err := readExclusiveLocksFlag(ctx, tx, stateID)
		if err != nil {
			return err
		}

		var (
			storeBytes  = rawState
			storeParsed = parsed
		)

		switch {
		case bypassLock:
			// Apply-orchestrator path: no lock interaction. Commit
			// the bytes verbatim.

		case exclusive:
			// Legacy exclusive path: verify the caller holds the
			// one lock that exists on this state, then commit
			// verbatim.
			if err := checkLockForWrite(ctx, tx, stateID, lockID); err != nil {
				return err
			}

		default:
			// Optimistic-merge path.
			//
			// First validate the lock state using the same
			// checkLockForWrite semantics vanilla terraform expects:
			//
			//   - no locks held + no lockID         → OK (out-of-band write).
			//   - any locks held + no lockID        → ErrStateLocked.
			//   - lockID present + no matching row  → ErrLockMismatch.
			//
			// In optimistic mode there may be MULTIPLE concurrent
			// lock rows; we therefore do the lookup by exact
			// (state_id, lock_id) rather than QueryRow over the
			// state_id alone (which would arbitrarily pick one).
			sourceSerial, lockExists, err := readLockSourceSerial(ctx, tx, stateID, lockID)
			if err != nil {
				return err
			}
			if !lockExists {
				// No row matches this lockID. Either nobody is
				// locked (lockID may be "" or stale), or another
				// operator holds a lock and we don't have a
				// matching row. Defer to the canonical lock
				// check, which preserves the legacy error shape.
				if err := checkAnyLockBlocks(ctx, tx, stateID, lockID); err != nil {
					return err
				}
				// Fall through: commit verbatim (no merge).
			} else if sourceSerial > 0 {
				// We hold a lock with a known baseline; check
				// whether trunk has advanced past it and, if so,
				// merge.
				trunkSerial, trunkRaw, err := readCurrentVersionRaw(ctx, tx, stateID)
				if err != nil && !errors.Is(err, ErrStateNotFound) {
					return err
				}
				if trunkSerial > sourceSerial {
					_, baseRaw, err := readVersionRawAtSerial(ctx, tx, stateID, sourceSerial)
					if err != nil {
						return fmt.Errorf("read base version: %w", err)
					}
					baseParsed, err := tfstate.Parse(baseRaw)
					if err != nil {
						return fmt.Errorf("parse base state: %w", err)
					}
					trunkParsed, err := tfstate.Parse(trunkRaw)
					if err != nil {
						return fmt.Errorf("parse trunk state: %w", err)
					}
					merged, err := MergeStates(baseParsed, trunkParsed, parsed)
					if err != nil {
						// Surface WriteSetConflictError /
						// ErrLineageMismatch verbatim; the
						// HTTP layer needs the typed value.
						return err
					}
					mergedBytes, err := json.Marshal(merged)
					if err != nil {
						return fmt.Errorf("encode merged state: %w", err)
					}
					storeBytes = mergedBytes
					storeParsed = merged
				}
			}
		}

		// Assign the next serial.
		//
		// Two contracts coexist here:
		//
		//   bypassLock=true (apply orchestrator) — preserve the
		//   legacy "trust the incoming serial" behavior. The
		//   orchestrator's own retry/serial-bump logic relies on
		//   ErrSerialConflict to detect concurrent committers; if
		//   we silently advanced past a collision the orchestrator
		//   would never learn.
		//
		//   bypassLock=false (HTTP backend, both exclusive and
		//   optimistic) — recompute MAX+1. The incoming proposed
		//   state's serial is computed from the operator's stale
		//   trunk read; trusting it would let two concurrent
		//   committers both produce a state at the same number.
		//   The unique constraint would still catch the second
		//   one, but it'd be a confusing 500 from the operator's
		//   point of view — much cleaner to assign a fresh
		//   monotone serial here.
		var nextSerial int64
		if bypassLock {
			nextSerial = storeParsed.Serial
			if nextSerial <= 0 {
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(MAX(serial), 0) + 1
					 FROM   state_versions
					 WHERE  state_id = $1`,
					stateID,
				).Scan(&nextSerial); err != nil {
					return fmt.Errorf("compute next serial: %w", err)
				}
			}
		} else {
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(MAX(serial), 0) + 1
				 FROM   state_versions
				 WHERE  state_id = $1`,
				stateID,
			).Scan(&nextSerial); err != nil {
				return fmt.Errorf("compute next serial: %w", err)
			}
		}

		// Rewrite the JSON's embedded serial to match the row's
		// serial. Required so Terraform's next GET returns a
		// document whose internal serial matches what the lock
		// response advertised.
		//
		// In the bypassLock path with the original incoming
		// serial preserved, this is a no-op rewrite — but doing
		// it unconditionally keeps the optimistic-merge case
		// safe without a second branch.
		storeBytes, err = rewriteStateSerial(storeBytes, nextSerial)
		if err != nil {
			return fmt.Errorf("rewrite serial: %w", err)
		}
		storeParsed.Serial = nextSerial

		var versionID string
		err = tx.QueryRow(ctx,
			`INSERT INTO state_versions
			 	(state_id, tenant_id, serial, terraform_version, raw_state, source, created_by)
			 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
			 RETURNING id`,
			stateID, tenantID, nextSerial, storeParsed.TerraformVersion, string(storeBytes), source, actor,
		).Scan(&versionID)
		if err != nil {
			if isUniqueViolation(err, "state_versions_state_id_serial_key") {
				return ErrSerialConflict
			}
			return fmt.Errorf("insert state_version: %w", err)
		}

		if err := normalize(ctx, tx, tenantID, stateID, versionID, nextSerial, storeParsed); err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`UPDATE states
			 SET    current_version_id = $1, updated_at = now()
			 WHERE  id = $2`,
			versionID, stateID,
		)
		if err != nil {
			return fmt.Errorf("update current_version_id: %w", err)
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, state_version_id, actor, payload)
			 VALUES ('state_write', $1, $2, $3, $4, jsonb_build_object('source', $5::text, 'serial', $6::bigint))`,
			tenantID, stateID, versionID, actor, source, nextSerial,
		)
		if err != nil {
			return err
		}
		if !bypassLock && strings.TrimSpace(lockID) != "" {
			if _, err := tx.Exec(ctx,
				`UPDATE state_locks
				 SET source_serial = $3
				 WHERE state_id = $1 AND lock_id = $2`,
				stateID, strings.TrimSpace(lockID), nextSerial,
			); err != nil {
				return fmt.Errorf("advance lock source_serial: %w", err)
			}
		}
		return nil
	})
}

func enforceTenantStateLimits(ctx context.Context, tx pgx.Tx, tenantID, stateID, stateName string, st *tfstate.State) error {
	if strings.TrimSpace(tenantID) == "" || st == nil {
		return nil
	}
	var maxResources, maxEnvironmentResources int
	if err := tx.QueryRow(ctx, `SELECT max_state_resources, max_environment_resources FROM tenants WHERE id = $1`, tenantID).Scan(&maxResources, &maxEnvironmentResources); err != nil {
		// If the tenant row is missing (self-hosted legacy), fail open.
		return nil
	}
	count := countManagedStateResourceInstances(st)
	hardStateResources := hardQuotaFromSoft(maxResources)
	if hardStateResources <= 0 {
	} else if count > hardStateResources {
		return fmt.Errorf("%w: state quota exceeded: resulting state would have %d resources (soft=%d, hard=%d)", ErrEntitlementExceeded, count, maxResources, hardStateResources)
	}
	hardEnvironmentResources := hardQuotaFromSoft(maxEnvironmentResources)
	if hardEnvironmentResources > 0 {
		envPrefix := environmentStatePrefix(ctx, stateName)
		var otherResources int
		if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM resources r
JOIN states s ON s.id = r.state_id
WHERE s.tenant_id = $1
  AND s.lifecycle_status = 'active'
  AND r.mode = 'managed'
  AND r.delete_serial IS NULL
  AND (NULLIF($2, '')::uuid IS NULL OR r.state_id <> NULLIF($2, '')::uuid)
  AND ($3 = '' OR s.name LIKE $3)`, tenantID, strings.TrimSpace(stateID), envPrefix+"%").Scan(&otherResources); err != nil {
			return err
		}
		total := otherResources + count
		if total > hardEnvironmentResources {
			return fmt.Errorf("%w: environment quota exceeded: resulting environment would have %d resources (soft=%d, hard=%d)", ErrEntitlementExceeded, total, maxEnvironmentResources, hardEnvironmentResources)
		}
	}
	return nil
}

func hardQuotaFromSoft(soft int) int {
	if soft <= 0 {
		return 0
	}
	return soft + (soft+1)/2
}

func environmentStatePrefix(ctx context.Context, stateName string) string {
	if p, ok := auth.FromContext(ctx); ok {
		workspaceID := strings.TrimSpace(p.WorkspaceID)
		environmentPublicID := strings.TrimSpace(p.EnvironmentPublicID)
		if workspaceID != "" && environmentPublicID != "" {
			return workspaceID + "/" + environmentPublicID + "/"
		}
	}
	parts := strings.Split(strings.Trim(strings.TrimSpace(stateName), "/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return ""
	}
	return parts[0] + "/" + parts[1] + "/"
}

func countManagedStateResourceInstances(st *tfstate.State) int {
	if st == nil {
		return 0
	}
	seen := make(map[string]struct{})
	count := 0
	for _, r := range st.Resources {
		if strings.TrimSpace(r.Mode) != "managed" {
			continue
		}
		for _, inst := range r.Instances {
			if len(inst.IndexKey) == 0 || string(inst.IndexKey) == "null" {
				count++
				continue
			}
			addr, err := tfstate.InstanceAddress(r, inst)
			if err != nil {
				count++
				continue
			}
			if _, ok := seen[addr]; ok {
				if strings.EqualFold(strings.TrimSpace(inst.Status), "deposed") {
					continue
				}
				count++
				continue
			}
			seen[addr] = struct{}{}
			count++
		}
	}
	return count
}

// readExclusiveLocksFlag returns the per-state exclusive_locks toggle.
// Defaults to false on rows predating migration 0011 (NOT NULL with
// DEFAULT false guarantees there are no NULLs in practice; the
// defensive nil-check is purely documentation).
func readExclusiveLocksFlag(ctx context.Context, tx pgx.Tx, stateID string) (bool, error) {
	var exclusive bool
	if err := tx.QueryRow(ctx,
		`SELECT exclusive_locks FROM states WHERE id = $1`,
		stateID,
	).Scan(&exclusive); err != nil {
		return false, fmt.Errorf("read exclusive_locks: %w", err)
	}
	return exclusive, nil
}

// readLockSourceSerial returns the source_serial recorded when this
// lock_id was acquired. lockExists is false when no row matches —
// the caller is expected to fall through to checkLockForWrite, which
// produces the same errors the legacy path would have.
//
// A NULL source_serial (lock row predating migration 0011) reports
// as 0; the optimistic path treats it as "no merge baseline known"
// and commits the operator's bytes verbatim, matching the legacy
// behavior exactly.
func readLockSourceSerial(ctx context.Context, tx pgx.Tx, stateID, lockID string) (int64, bool, error) {
	if lockID == "" {
		return 0, false, nil
	}
	var sourceSerial *int64
	err := tx.QueryRow(ctx,
		`SELECT source_serial
		 FROM   state_locks
		 WHERE  state_id = $1 AND lock_id = $2`,
		stateID, lockID,
	).Scan(&sourceSerial)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read lock source_serial: %w", err)
	}
	if sourceSerial == nil {
		return 0, true, nil
	}
	return *sourceSerial, true, nil
}

// readCurrentVersionRaw returns the trunk version's serial and raw
// bytes. Returns ErrStateNotFound when the state has no versions
// (first-ever write).
func readCurrentVersionRaw(ctx context.Context, tx pgx.Tx, stateID string) (int64, []byte, error) {
	const q = `
		SELECT sv.serial, sv.raw_state::text
		FROM   states s
		JOIN   state_versions sv ON sv.id = s.current_version_id
		WHERE  s.id = $1
	`
	var (
		serial int64
		raw    string
	)
	err := tx.QueryRow(ctx, q, stateID).Scan(&serial, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, ErrStateNotFound
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read current version: %w", err)
	}
	return serial, []byte(raw), nil
}

// readVersionRawAtSerial returns the version_id and raw bytes of
// the state_version with the given (state_id, serial). Used by the
// optimistic-merge path to read the "base" state — what was trunk
// at the moment the operator acquired their lock. Returns
// ErrStateNotFound when no such row exists (lock-holder's source
// serial was wiped, e.g. by a destructive operator override).
func readVersionRawAtSerial(ctx context.Context, tx pgx.Tx, stateID string, serial int64) (string, []byte, error) {
	var (
		id  string
		raw string
	)
	err := tx.QueryRow(ctx,
		`SELECT id, raw_state::text
		 FROM   state_versions
		 WHERE  state_id = $1 AND serial = $2`,
		stateID, serial,
	).Scan(&id, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrStateNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("read version at serial %d: %w", serial, err)
	}
	return id, []byte(raw), nil
}

// DeleteState removes a state and (via cascade) all of its versions and
// lock rows. Same lock semantics as WriteState. Scoped by the caller's
// tenant; another tenant's state with the same name is invisible.
func (s *Store) DeleteState(ctx context.Context, name, lockID, actor string) error {
	tenantID := auth.TenantFromContext(ctx)

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		where, args := s.statesByNameWhereAnyLifecycle(name, tenantID)
		var stateID string
		err := tx.QueryRow(ctx,
			`SELECT id
			 FROM states
			 WHERE `+where+`
			 ORDER BY CASE WHEN name = $1 THEN 0 ELSE 1 END
			 LIMIT 1`,
			args...,
		).Scan(&stateID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStateNotFound
		}
		if err != nil {
			return err
		}

		if err := checkLockForWrite(ctx, tx, stateID, lockID); err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, actor) VALUES ('state_delete', $1, $2, $3)`,
			tenantID, stateID, actor,
		)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `DELETE FROM states WHERE id = $1`, stateID)
		return err
	})
}

// upsertState creates a state row by (tenant_id, name) if it does not
// exist and returns its id. If lineage is non-empty and the state
// currently has a NULL lineage, the column is populated; an existing
// lineage is never overwritten (Terraform's lineage is meant to
// survive state moves).
//
// The tenant id is passed explicitly rather than read from ctx here:
// this function runs inside a transaction body where its caller has
// already extracted the tenant and is responsible for keeping it
// constant across the whole write. The explicit parameter keeps that
// invariant visible in the signature.
func upsertState(ctx context.Context, tx pgx.Tx, tenantID, name, lineage string) (string, error) {
	var existing struct {
		ID      string
		Name    string
		Status  string
		Lineage string
	}
	err := tx.QueryRow(ctx,
		`SELECT id::text, name, lifecycle_status, COALESCE(lineage::text, '')
		 FROM states
		 WHERE tenant_id = $1
		   AND (
		     name = $2
		     OR (lifecycle_status = 'archived' AND name LIKE $2 || '--archived-%')
		   )
		 ORDER BY CASE WHEN name = $2 THEN 0 ELSE 1 END
		 LIMIT 1`,
		tenantID, name,
	).Scan(&existing.ID, &existing.Name, &existing.Status, &existing.Lineage)
	switch {
	case err == nil:
		status, parseErr := ParseLifecycleStatus(existing.Status)
		if parseErr != nil {
			return "", parseErr
		}
		if existing.Name == name && status != LifecycleStatusActive {
			return "", &StateNotActiveError{Status: status}
		}
		if existing.Name != name && status != LifecycleStatusActive &&
			(strings.TrimSpace(lineage) == "" || strings.TrimSpace(existing.Lineage) == "" || strings.TrimSpace(existing.Lineage) == strings.TrimSpace(lineage)) {
			return "", &StateNotActiveError{Status: status}
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}

	var id string
	var lineageArg any
	if lineage != "" {
		lineageArg = lineage
	}
	// ON CONFLICT targets the named (tenant_id, name) constraint
	// added by migration 0009. The constraint-name form (vs
	// `ON CONFLICT (tenant_id, name)`) is more self-documenting
	// and survives changes to the column order in the unique
	// index.
	err = tx.QueryRow(ctx,
		`INSERT INTO states (tenant_id, name, lineage) VALUES ($1, $2, $3)
		 ON CONFLICT ON CONSTRAINT states_tenant_name_key DO UPDATE
		     SET lineage = COALESCE(states.lineage, EXCLUDED.lineage)
		 RETURNING id`,
		tenantID, name, lineageArg,
	).Scan(&id)
	return id, err
}

// checkAnyLockBlocks is the optimistic-mode equivalent of
// checkLockForWrite. The legacy implementation reads ONE row by
// state_id (assuming a single-writer model) and compares it to the
// caller's lockID. In optimistic mode multiple rows can coexist, so
// we instead count how many rows are present and special-case:
//
//   - 0 rows + any lockID  → OK if lockID="" (out-of-band), ErrLockMismatch
//     otherwise.
//   - 0 rows + lockID=""   → OK.
//   - >=1 row + lockID=""  → ErrStateLocked (vanilla "no lock id supplied").
//   - >=1 row + lockID set → ErrLockMismatch (no row matches our id).
//
// This routine is called by the optimistic path AFTER we've already
// confirmed that NO row matches our lockID (or that our lockID is
// empty). It's purely the "fall-back to the legacy error shape"
// step — we don't make any new policy decisions here.
func checkAnyLockBlocks(ctx context.Context, tx pgx.Tx, stateID, lockID string) error {
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM state_locks WHERE state_id = $1`,
		stateID,
	).Scan(&count); err != nil {
		return fmt.Errorf("count state_locks: %w", err)
	}
	switch {
	case count == 0 && lockID == "":
		return nil
	case count == 0 && lockID != "":
		return ErrLockMismatch
	case count > 0 && lockID == "":
		return ErrStateLocked
	default:
		return ErrLockMismatch
	}
}

// checkLockForWrite enforces the write-time lock semantics documented on
// WriteState. It does not acquire any new lock.
func checkLockForWrite(ctx context.Context, tx pgx.Tx, stateID, lockID string) error {
	var heldLockID *string
	err := tx.QueryRow(ctx,
		`SELECT lock_id FROM state_locks WHERE state_id = $1`,
		stateID,
	).Scan(&heldLockID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if lockID == "" {
			return nil
		}
		return ErrLockMismatch
	case err != nil:
		return err
	}

	if lockID == "" {
		return ErrStateLocked
	}
	if heldLockID == nil || *heldLockID != lockID {
		return ErrLockMismatch
	}
	return nil
}
