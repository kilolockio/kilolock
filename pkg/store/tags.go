package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/pkg/auth"
)

// VersionTag is the operator-facing projection of a state_version_tags
// row. Carries enough to render the `tag list` output and to power
// the version-ref resolver — specifically, both the tag's own
// metadata and the serial of the version it currently points at, so
// the CLI never has to do a second round-trip just to display the
// serial alongside the tag name.
type VersionTag struct {
	Tag         string
	StateName   string
	StateID     string
	VersionID   string
	Serial      int64
	Description string
	Actor       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErrTagReservedName is returned when the operator tries to name a
// tag in a way that would collide with the version-ref resolver
// (e.g. "current", "@1", a pure number). The SQL CHECK constraint
// rejects these too, but catching them in Go lets us return a
// purpose-built error instead of leaking SQLSTATE 23514.
var ErrTagReservedName = errors.New("tag name is reserved")

// ErrTagNotFound is returned by ListTagsForVersion / UnsetTag when
// the addressed tag doesn't exist for this (tenant, state) pair.
// Callers map it to a user-facing "tag X not found in state Y".
var ErrTagNotFound = errors.New("tag not found")

// SetTag creates or moves the named tag to point at the version
// identified by versionRef. ALL the ref shapes accepted by
// GetVersionRaw are accepted here (serial, @N, uuid, "current") so
// the operator can say e.g. `kl tag prod-2026-05 current`
// or `kl tag pre-mig 41` interchangeably.
//
// Returns the resulting row so the CLI can show the operator the
// resolved serial in the confirmation message (catches typos: "tag
// pre-mig @1 set on serial 41" → "wait, I meant 42").
//
// Behaviour for an existing tag:
//
//   - If the tag already exists and points at the same version,
//     the row's metadata (description/actor) is REFRESHED and
//     updated_at advances. This is the "I'm pinning my work but
//     adding a note" case.
//   - If the tag exists and points at a different version, the
//     pointer moves. The audit trail of where it was before lives
//     in the events table (a `tag.move` row is written).
//
// The function is transactional: the events row and the tag UPDATE
// (or INSERT) commit together, so a partial write can never produce
// "tag moved but no audit trail" or vice versa.
func (s *Store) SetTag(ctx context.Context, name, versionRef, tag, description, actor string) (*VersionTag, error) {
	if err := ValidateTagName(tag); err != nil {
		return nil, err
	}
	tenantID := auth.TenantFromContext(ctx)

	var out *VersionTag
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		stateID, currentID, err := s.lookupStateIDAndCurrentTx(ctx, tx, name)
		if err != nil {
			return err
		}
		info, _, err := s.resolveVersionTx(ctx, tx, stateID, currentID, versionRef)
		if err != nil {
			return err
		}
		info.StateName = name

		// Upsert by (tenant, state, tag). Conflict target is the
		// UNIQUE index we built in 0010, hence "ON CONSTRAINT".
		// ON CONFLICT on (tenant_id, state_id, tag) rather than
		// "ON CONSTRAINT <unique_idx_name>" because postgres only
		// accepts CONSTRAINT names there, not UNIQUE INDEX names —
		// and we deliberately built 0010's uniqueness as an index
		// (cheaper to drop/recreate during schema rolls than a
		// table constraint). The inference target form gives us
		// the same upsert semantics without that coupling.
		const upsert = `
			INSERT INTO state_version_tags
			    (tenant_id, state_id, state_version_id, tag, description, actor)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, state_id, tag)
			DO UPDATE SET
			    state_version_id = EXCLUDED.state_version_id,
			    description       = EXCLUDED.description,
			    actor             = EXCLUDED.actor,
			    updated_at        = now()
			RETURNING state_version_id, COALESCE(description, ''), actor, created_at, updated_at
		`
		row := tx.QueryRow(ctx, upsert,
			tenantID, stateID, info.ID, tag, nullIfEmpty(description), actor,
		)
		var (
			versionID string
			desc      string
			gotActor  string
			created   time.Time
			updated   time.Time
		)
		if err := row.Scan(&versionID, &desc, &gotActor, &created, &updated); err != nil {
			return fmt.Errorf("upsert tag: %w", err)
		}

		// Audit event. payload carries the resolved version_id and
		// the source ref so future investigations can reconstruct
		// "what did the operator mean".
		const auditQ = `
			INSERT INTO events (tenant_id, state_id, kind, payload, actor)
			VALUES ($1, $2, 'tag.set',
			        jsonb_build_object(
			            'tag', $3::text,
			            'ref', $4::text,
			            'resolved_serial', $5::bigint,
			            'resolved_version_id', $6::uuid,
			            'description', $7::text
			        ),
			        $8)
		`
		if _, err := tx.Exec(ctx, auditQ,
			tenantID, stateID, tag, versionRef, info.Serial, info.ID, description, actor,
		); err != nil {
			return fmt.Errorf("write tag event: %w", err)
		}

		out = &VersionTag{
			Tag: tag, StateName: name, StateID: stateID,
			VersionID: versionID, Serial: info.Serial,
			Description: desc, Actor: gotActor,
			CreatedAt: created, UpdatedAt: updated,
		}
		return nil
	})
	return out, err
}

// UnsetTag deletes the named tag from the state. Returns
// ErrTagNotFound if the tag does not exist. Idempotent semantics
// (no error on already-absent) are deliberately NOT provided: the
// operator should be sure of what they're deleting.
func (s *Store) UnsetTag(ctx context.Context, name, tag, actor string) error {
	if tag == "" {
		return fmt.Errorf("UnsetTag: tag name must not be empty")
	}
	tenantID := auth.TenantFromContext(ctx)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		stateID, _, err := s.lookupStateIDAndCurrentTx(ctx, tx, name)
		if err != nil {
			return err
		}

		// DELETE ... RETURNING tells us whether anything was
		// actually removed without a separate SELECT first.
		var deletedVersionID string
		err = tx.QueryRow(ctx,
			`DELETE FROM state_version_tags
			 WHERE tenant_id = $1 AND state_id = $2 AND tag = $3
			 RETURNING state_version_id`,
			tenantID, stateID, tag,
		).Scan(&deletedVersionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTagNotFound
		}
		if err != nil {
			return fmt.Errorf("delete tag: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO events (tenant_id, state_id, kind, payload, actor)
			 VALUES ($1, $2, 'tag.unset',
			         jsonb_build_object(
			             'tag', $3::text,
			             'was_version_id', $4::uuid
			         ),
			         $5)`,
			tenantID, stateID, tag, deletedVersionID, actor,
		); err != nil {
			return fmt.Errorf("write unset-tag event: %w", err)
		}
		return nil
	})
}

// ListTags returns all tags on the given state, newest first
// (by updated_at). The serial column is JOIN-ed in from
// state_versions so the renderer doesn't need a second round trip
// to display the resolved version.
func (s *Store) ListTags(ctx context.Context, name string) ([]VersionTag, error) {
	where, args := s.stateByNameWhere(ctx, name)
	q := `
		SELECT t.tag, s.name, t.state_id, t.state_version_id, sv.serial,
		       COALESCE(t.description, ''), t.actor, t.created_at, t.updated_at
		FROM   state_version_tags t
		JOIN   states         s  ON s.id  = t.state_id
		JOIN   state_versions sv ON sv.id = t.state_version_id
		WHERE  ` + where + `
		ORDER  BY t.updated_at DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	var out []VersionTag
	for rows.Next() {
		var t VersionTag
		if err := rows.Scan(
			&t.Tag, &t.StateName, &t.StateID, &t.VersionID, &t.Serial,
			&t.Description, &t.Actor, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan tag row: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTagsByVersionID returns the tags currently pointing at each
// of the supplied state_version IDs. Used by `kl history`
// to annotate the version list — we batch the lookup in one query
// to avoid an N+1 across the history pagination window.
func (s *Store) ListTagsByVersionID(ctx context.Context, versionIDs []string) (map[string][]string, error) {
	if len(versionIDs) == 0 {
		return nil, nil
	}
	var (
		rows pgx.Rows
		err  error
	)
	if s.isolated {
		const q = `
		SELECT state_version_id::text, tag
		FROM   state_version_tags
		WHERE  state_version_id = ANY($1::uuid[])
		ORDER  BY tag`
		rows, err = s.pool.Query(ctx, q, versionIDs)
	} else {
		tenantID := auth.TenantFromContext(ctx)
		const q = `
		SELECT state_version_id::text, tag
		FROM   state_version_tags
		WHERE  tenant_id = $1
		  AND  state_version_id = ANY($2::uuid[])
		ORDER  BY tag`
		rows, err = s.pool.Query(ctx, q, tenantID, versionIDs)
	}
	if err != nil {
		return nil, fmt.Errorf("list tags by version: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var vid, tag string
		if err := rows.Scan(&vid, &tag); err != nil {
			return nil, fmt.Errorf("scan tag row: %w", err)
		}
		out[vid] = append(out[vid], tag)
	}
	return out, rows.Err()
}

// resolveTagToVersionID looks up a tag within a state and returns
// the state_version it currently points at, or ErrTagNotFound. Used
// by the version-ref resolver to make tag names a first-class shape
// for `--from` / `--to` / `--ref` style flags everywhere.
//
// Runs inside the same transaction as the surrounding lookup so a
// tag move + read can race without producing inconsistent reads.
func (s *Store) resolveTagToVersionID(ctx context.Context, tx pgx.Tx, tenantID, stateID, tag string) (string, error) {
	var (
		q    string
		args []any
	)
	if s.isolated {
		q = `
		SELECT state_version_id::text
		FROM   state_version_tags
		WHERE  state_id = $1 AND tag = $2`
		args = []any{stateID, tag}
	} else {
		q = `
		SELECT state_version_id::text
		FROM   state_version_tags
		WHERE  tenant_id = $1 AND state_id = $2 AND tag = $3`
		args = []any{tenantID, stateID, tag}
	}
	var vid string
	err := tx.QueryRow(ctx, q, args...).Scan(&vid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTagNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve tag: %w", err)
	}
	return vid, nil
}

// ValidateTagName enforces the same rules as the SQL CHECK
// constraint in migration 0010. Pulling them out into Go lets us
// surface a meaningful error message to the operator before the
// trip to the database. The CHECK is still the source of truth —
// this function MUST stay in sync with it.
//
// Rules:
//
//   - non-empty
//   - length <= 64
//   - must not start with '@'
//   - must not be pure digits
//   - must not equal "current"
func ValidateTagName(tag string) error {
	if tag == "" {
		return fmt.Errorf("%w: tag name must not be empty", ErrTagReservedName)
	}
	if len(tag) > 64 {
		return fmt.Errorf("%w: tag name is too long (max 64 chars)", ErrTagReservedName)
	}
	if tag == "current" {
		return fmt.Errorf("%w: 'current' is a reserved version alias", ErrTagReservedName)
	}
	if strings.HasPrefix(tag, "@") {
		return fmt.Errorf("%w: tag names starting with '@' collide with @N relative refs", ErrTagReservedName)
	}
	allDigits := true
	for _, c := range tag {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return fmt.Errorf("%w: numeric-only tag names collide with serial refs", ErrTagReservedName)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
