package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/auth"
)

// StateVersionInfo is the operator-facing metadata for one row of
// state_versions. The raw_state payload is intentionally NOT carried
// here — it can be tens of megabytes; callers that want the bytes
// (export, rollback) read it separately via GetVersionRaw.
//
// IsCurrent is true iff this row is what `states.current_version_id`
// points to right now. It lets `kl history` mark the head of
// the chain in the table output without a second round-trip.
type StateVersionInfo struct {
	ID               string
	StateID          string
	StateName        string
	Serial           int64
	TerraformVersion string
	Source           string
	CreatedAt        time.Time
	CreatedBy        string
	SizeBytes        int
	IsCurrent        bool
}

// ListVersions returns recent versions of the named state, newest
// first. limit caps the number of rows; 0 or negative means "no
// cap" (use with care on long-lived states — pagination via offset
// is recommended for any UI).
//
// Returns ErrStateNotFound if the state doesn't exist. An existing
// state with no versions yet (theoretical edge case: row created
// but first WriteState rolled back) returns an empty slice.
func (s *Store) ListVersions(ctx context.Context, name string, limit, offset int) ([]StateVersionInfo, error) {
	where, lookupArgs := s.statesByNameWhere(ctx, name)
	var stateID, currentID string
	err := s.pool.QueryRow(ctx,
		`SELECT id, COALESCE(current_version_id::text, '')
		 FROM   states
		 WHERE  `+where,
		lookupArgs...,
	).Scan(&stateID, &currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup state: %w", err)
	}

	// octet_length on jsonb gives the canonical text-rendered byte
	// count — useful for "how big is this version" without dragging
	// the full payload across the wire. The cost is one toast-table
	// fetch per row which is well-amortised by the OFFSET / LIMIT
	// the caller asked for.
	q := `
		SELECT id,
		       serial,
		       COALESCE(terraform_version, ''),
		       source,
		       created_at,
		       COALESCE(created_by, ''),
		       octet_length(raw_state::text)
		FROM   state_versions
		WHERE  state_id = $1
		ORDER  BY serial DESC
	`
	args := []any{stateID}
	if limit > 0 {
		q += " LIMIT $2"
		args = append(args, limit)
		if offset > 0 {
			q += " OFFSET $3"
			args = append(args, offset)
		}
	} else if offset > 0 {
		q += " OFFSET $2"
		args = append(args, offset)
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list state_versions: %w", err)
	}
	defer rows.Close()

	var out []StateVersionInfo
	for rows.Next() {
		var v StateVersionInfo
		if err := rows.Scan(&v.ID, &v.Serial, &v.TerraformVersion, &v.Source, &v.CreatedAt, &v.CreatedBy, &v.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan state_version: %w", err)
		}
		v.StateID = stateID
		v.StateName = name
		v.IsCurrent = (v.ID == currentID)
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersionRaw returns the raw .tfstate bytes plus metadata for a
// specific historical version, identified by ref. Acceptable ref
// shapes:
//
//   - "" or "current" or "@0"         → states.current_version_id
//   - "@<n>" with n > 0               → n-th version back from current
//     (@1 = previous, @2 = two back, …)
//   - <decimal>                       → state_versions.serial = <decimal>
//   - <uuid>                          → state_versions.id = <uuid>
//
// Returns ErrStateNotFound when the state or the specific version
// is not found. Disambiguation is intentionally conservative:
// purely numeric refs are treated as serials, not as version
// counts, because operators reason about serials (terraform shows
// them) more reliably than version-ids.
func (s *Store) GetVersionRaw(ctx context.Context, name, ref string) (*StateVersionInfo, []byte, error) {
	stateID, currentID, err := s.lookupStateIDAndCurrent(ctx, name)
	if err != nil {
		return nil, nil, err
	}

	info, raw, err := s.resolveVersion(ctx, stateID, currentID, ref)
	if err != nil {
		return nil, nil, err
	}
	info.StateName = name
	return info, raw, nil
}

// ReplayVersion is the rollback primitive: it copies the raw_state
// of `ref` into a new state_versions row at serial MAX(serial)+1,
// flips states.current_version_id to point at the new row, and
// projects the resources/outputs/dependencies through normalize().
//
// The raw_state's embedded "serial" JSON field is rewritten in
// place to the new serial. Without that rewrite Terraform's
// next read would see a state file whose internal serial is
// older than what the backend advertised in the lock response,
// which produces a "state serial mismatch" panic on the client
// side. The rewrite is the only mutation; all other top-level
// fields (including unknown ones we don't model) round-trip
// byte-identically through json.RawMessage so future Terraform
// format additions can't silently corrupt rollbacks.
//
// The whole operation runs in one transaction:
//
//  1. Resolve `ref` to a (version_id, raw_state) pair.
//  2. Compute MAX(serial)+1 for the state.
//  3. Rewrite the JSON's serial field.
//  4. INSERT the new state_versions row with source='rollback'.
//  5. UPDATE states.current_version_id.
//  6. normalize() — re-projects resources/outputs/dependencies.
//  7. INSERT an events row.
//
// Lock semantics deliberately match WriteStateForApply: the v1
// state_locks table is NOT consulted. Rollback is an operator
// action against the bookkeeping; the v1 HTTP-backend lock is
// for serializing concurrent vanilla-terraform clients, none of
// whom are involved here.
//
// Returns the new version's info on success.
func (s *Store) ReplayVersion(ctx context.Context, name, ref, actor string) (*StateVersionInfo, error) {
	tenantID := auth.TenantFromContext(ctx)
	var newInfo *StateVersionInfo

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		stateID, currentID, err := s.lookupStateIDAndCurrentTx(ctx, tx, name)
		if err != nil {
			return err
		}

		srcInfo, srcRaw, err := s.resolveVersionTx(ctx, tx, stateID, currentID, ref)
		if err != nil {
			return err
		}

		var nextSerial int64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(serial), 0) + 1 FROM state_versions WHERE state_id = $1`,
			stateID,
		).Scan(&nextSerial); err != nil {
			return fmt.Errorf("compute next serial: %w", err)
		}

		rewritten, err := rewriteStateSerial(srcRaw, nextSerial)
		if err != nil {
			return fmt.Errorf("rewrite serial: %w", err)
		}

		parsed, err := tfstate.Parse(rewritten)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidState, err)
		}

		// Audit payload captures the rollback's provenance: which
		// version this row was cloned from, what serial it had,
		// who triggered the operation. The same shape lets
		// `kl history` surface "rolled back from serial
		// X" without joining out to event payloads at render
		// time (we'd add a `replays` table later if rollback
		// chains get common enough to query).
		auditPayload, err := json.Marshal(map[string]any{
			"replayed_from_version": srcInfo.ID,
			"replayed_from_serial":  srcInfo.Serial,
		})
		if err != nil {
			return fmt.Errorf("encode audit payload: %w", err)
		}

		var newVersionID string
		var newCreatedAt time.Time
		err = tx.QueryRow(ctx,
			`INSERT INTO state_versions
			   (tenant_id, state_id, serial, terraform_version, raw_state, source, created_by)
			 VALUES ($1, $2, $3, $4, $5::jsonb, 'rollback', $6)
			 RETURNING id, created_at`,
			tenantID, stateID, nextSerial, parsed.TerraformVersion, string(rewritten), actor,
		).Scan(&newVersionID, &newCreatedAt)
		if err != nil {
			if isUniqueViolation(err, "state_versions_state_id_serial_key") {
				return ErrSerialConflict
			}
			return fmt.Errorf("insert state_version: %w", err)
		}

		if err := normalize(ctx, tx, tenantID, stateID, newVersionID, nextSerial, parsed); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE states SET current_version_id = $1, updated_at = now() WHERE id = $2`,
			newVersionID, stateID,
		); err != nil {
			return fmt.Errorf("update current_version_id: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, state_version_id, actor, payload)
			 VALUES ('state_rollback', $1, $2, $3, $4, $5::jsonb)`,
			tenantID, stateID, newVersionID, actor, string(auditPayload),
		); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}

		newInfo = &StateVersionInfo{
			ID:               newVersionID,
			StateID:          stateID,
			StateName:        name,
			Serial:           nextSerial,
			TerraformVersion: parsed.TerraformVersion,
			Source:           "rollback",
			CreatedAt:        newCreatedAt,
			CreatedBy:        actor,
			SizeBytes:        len(rewritten),
			IsCurrent:        true,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return newInfo, nil
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (s *Store) lookupStateIDAndCurrent(ctx context.Context, name string) (stateID, currentID string, err error) {
	where, args := s.statesByNameWhere(ctx, name)
	err = s.pool.QueryRow(ctx,
		`SELECT id, COALESCE(current_version_id::text, '')
		 FROM   states
		 WHERE  `+where,
		args...,
	).Scan(&stateID, &currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrStateNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("lookup state: %w", err)
	}
	return stateID, currentID, nil
}

func (s *Store) lookupStateIDAndCurrentTx(ctx context.Context, tx pgx.Tx, name string) (stateID, currentID string, err error) {
	where, args := s.statesByNameWhere(ctx, name)
	err = tx.QueryRow(ctx,
		`SELECT id, COALESCE(current_version_id::text, '')
		 FROM   states
		 WHERE  `+where,
		args...,
	).Scan(&stateID, &currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrStateNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("lookup state: %w", err)
	}
	return stateID, currentID, nil
}

// resolveVersion is the pool-level form; resolveVersionTx is the
// tx-bound form used inside ReplayVersion. The two share parsing
// logic but use different connection objects so we can't share
// the row-fetch.
func (s *Store) resolveVersion(ctx context.Context, stateID, currentID, ref string) (*StateVersionInfo, []byte, error) {
	resolved, err := s.maybeResolveTagRef(ctx, stateID, ref)
	if err != nil {
		return nil, nil, err
	}
	q, args, err := versionLookupQuery(stateID, resolved, true, true)
	if err != nil {
		return nil, nil, err
	}
	row := s.pool.QueryRow(ctx, q, args...)
	return scanVersionLookup(row, stateID, currentID)
}

func (s *Store) resolveVersionTx(ctx context.Context, tx pgx.Tx, stateID, currentID, ref string) (*StateVersionInfo, []byte, error) {
	resolved, err := s.maybeResolveTagRefTx(ctx, tx, stateID, ref)
	if err != nil {
		return nil, nil, err
	}
	q, args, err := versionLookupQuery(stateID, resolved, true, true)
	if err != nil {
		return nil, nil, err
	}
	row := tx.QueryRow(ctx, q, args...)
	return scanVersionLookup(row, stateID, currentID)
}

func (s *Store) resolveVersionInfo(ctx context.Context, stateID, currentID, ref string) (*StateVersionInfo, error) {
	resolved, err := s.maybeResolveTagRef(ctx, stateID, ref)
	if err != nil {
		return nil, err
	}
	q, args, err := versionLookupQuery(stateID, resolved, false, false)
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, q, args...)
	info, err := scanVersionLookupInfo(row, stateID, currentID)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// maybeResolveTagRef short-circuits a tag-name ref into the
// underlying state_version UUID before the rest of the lookup
// machinery runs. Returns the original ref unchanged when the input
// is one of the OTHER documented shapes (current / @N / decimal /
// UUID); only refs that "look like a tag name" are passed through
// the tag table.
//
// "Look like a tag name" is: non-empty, not "current", doesn't start
// with '@', isn't all digits, isn't a UUID. The CHECK constraint in
// migration 0010 guarantees no row in state_version_tags can take a
// shape that matches one of the other classes, so the predicate
// never produces a false-positive on a real ref.
//
// On ErrTagNotFound we DO NOT swap to the original ref: an operator
// who typed a tag-shaped string clearly meant a tag, and a generic
// "ref not found" three layers down is less helpful than the
// specific "tag X not found in state Y" we surface here.
func (s *Store) maybeResolveTagRef(ctx context.Context, stateID, ref string) (string, error) {
	if !refLooksLikeTag(ref) {
		return ref, nil
	}
	tenantID := auth.TenantFromContext(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return s.resolveTagToVersionID(ctx, tx, tenantID, stateID, ref)
}

func (s *Store) maybeResolveTagRefTx(ctx context.Context, tx pgx.Tx, stateID, ref string) (string, error) {
	if !refLooksLikeTag(ref) {
		return ref, nil
	}
	tenantID := auth.TenantFromContext(ctx)
	return s.resolveTagToVersionID(ctx, tx, tenantID, stateID, ref)
}

// refLooksLikeTag is the predicate that decides whether to consult
// state_version_tags. We keep the test classification-by-shape so
// the tag lookup is skipped (zero extra round-trip) for the common
// case of a serial / UUID / @N ref.
func refLooksLikeTag(ref string) bool {
	if ref == "" || ref == "current" {
		return false
	}
	if strings.HasPrefix(ref, "@") {
		return false
	}
	if looksLikeUUID(ref) {
		return false
	}
	// All-digit refs are serials. Anything else is candidate tag.
	for _, c := range ref {
		if c < '0' || c > '9' {
			return true
		}
	}
	return false
}

// versionLookupQuery builds the SQL + arg list that resolves a
// version reference to a state_versions row. The four shapes are
// mutually exclusive and chosen on the ref string:
//
//   - ""/"current"/"@0"     →  WHERE id = states.current_version_id
//   - "@<n>"                →  ORDER BY serial DESC OFFSET n LIMIT 1
//   - integer literal       →  WHERE serial = $2
//   - UUID                  →  WHERE id = $2
func versionLookupQuery(stateID, ref string, includeRaw, includeSize bool) (string, []any, error) {
	sel := `
		SELECT id,
		       serial,
		       COALESCE(terraform_version, ''),
		       source,
		       created_at,
		       COALESCE(created_by, '')
	`
	if includeSize {
		sel += `,
		       octet_length(raw_state::text)
	`
	} else {
		sel += `,
		       0
	`
	}
	if includeRaw {
		sel += `,
		       raw_state::text
	`
	}
	sel += `
		FROM   state_versions
	`
	switch {
	case ref == "" || ref == "current" || ref == "@0":
		q := sel + `
			WHERE state_id = $1
			  AND id = (SELECT current_version_id FROM states WHERE id = $1)
		`
		return q, []any{stateID}, nil

	case strings.HasPrefix(ref, "@"):
		n, err := strconv.Atoi(strings.TrimPrefix(ref, "@"))
		if err != nil || n < 0 {
			return "", nil, fmt.Errorf("invalid version reference %q: expected @N with N >= 0", ref)
		}
		q := sel + `
			WHERE state_id = $1
			ORDER BY serial DESC
			OFFSET $2 LIMIT 1
		`
		return q, []any{stateID, n}, nil

	case looksLikeUUID(ref):
		q := sel + `WHERE state_id = $1 AND id = $2`
		return q, []any{stateID, ref}, nil

	default:
		if serial, err := strconv.ParseInt(ref, 10, 64); err == nil {
			q := sel + `WHERE state_id = $1 AND serial = $2`
			return q, []any{stateID, serial}, nil
		}
		return "", nil, fmt.Errorf("invalid version reference %q: expected serial, @N, UUID, or \"current\"", ref)
	}
}

func scanVersionLookup(row pgx.Row, stateID, currentID string) (*StateVersionInfo, []byte, error) {
	var (
		info StateVersionInfo
		raw  string
	)
	err := row.Scan(
		&info.ID,
		&info.Serial,
		&info.TerraformVersion,
		&info.Source,
		&info.CreatedAt,
		&info.CreatedBy,
		&info.SizeBytes,
		&raw,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrStateNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("scan state_version: %w", err)
	}
	info.StateID = stateID
	info.IsCurrent = (info.ID == currentID)
	return &info, []byte(raw), nil
}

func scanVersionLookupInfo(row pgx.Row, stateID, currentID string) (*StateVersionInfo, error) {
	var info StateVersionInfo
	err := row.Scan(
		&info.ID,
		&info.Serial,
		&info.TerraformVersion,
		&info.Source,
		&info.CreatedAt,
		&info.CreatedBy,
		&info.SizeBytes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan state_version: %w", err)
	}
	info.StateID = stateID
	info.IsCurrent = (info.ID == currentID)
	return &info, nil
}

// rewriteStateSerial replaces the top-level "serial" field of a
// Terraform v4 .tfstate JSON document. All other top-level keys
// (including ones kl doesn't model) round-trip
// byte-identically through json.RawMessage. The reordering /
// reflowing of keys in the resulting bytes is irrelevant —
// Terraform parses the file as JSON and never compares it byte
// for byte to anything that cares about ordering.
func rewriteStateSerial(raw []byte, newSerial int64) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode state json: %w", err)
	}
	serialBytes, err := json.Marshal(newSerial)
	if err != nil {
		return nil, fmt.Errorf("encode new serial: %w", err)
	}
	doc["serial"] = serialBytes
	return json.Marshal(doc)
}

// looksLikeUUID is the cheapest possible UUID-shape check that
// rejects integers: 36 chars, four dashes at the canonical
// positions. We don't validate hex-ness because the SQL layer
// will reject malformed UUIDs with a clear error anyway, and we
// don't want to drag a uuid library into the store just for
// reference parsing.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	return s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}
