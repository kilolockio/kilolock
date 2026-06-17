package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ReservationMode string

const (
	ReservationRead  ReservationMode = "read"
	ReservationWrite ReservationMode = "write"
)

func (m ReservationMode) Valid() bool {
	return m == ReservationRead || m == ReservationWrite
}

type Reservation struct {
	AddressGlob string
	Mode        ReservationMode
}

type ActiveReservation struct {
	ID          string
	StateID     string
	AddressGlob string
	Mode        ReservationMode
	Holder      string
	ApplyID     string
	AcquiredAt  time.Time
	ExpiresAt   time.Time
}

var ErrReservationConflict = errors.New("reservation conflict")

type ReservationConflictError struct {
	StateID   string
	Conflicts []ActiveReservation
}

func (e *ReservationConflictError) Error() string {
	if len(e.Conflicts) == 0 {
		return "reservation conflict"
	}
	parts := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		parts = append(parts, fmt.Sprintf("%s/%s held by %s (apply %s, expires %s)",
			c.AddressGlob, c.Mode, c.Holder, c.ApplyID, c.ExpiresAt.UTC().Format(time.RFC3339)))
	}
	sort.Strings(parts)
	return fmt.Sprintf("reservation conflict: %s", strings.Join(parts, "; "))
}

func (e *ReservationConflictError) Is(target error) bool {
	return target == ErrReservationConflict
}

// AcquireReservations requests a set of granular address locks. It guarantees that if the
// function returns successfully, the transaction has safely claimed all specified subgraphs.
func (s *Store) AcquireReservations(ctx context.Context, stateID, applyID, actor string, want []Reservation, lease time.Duration) error {
	if len(want) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize reservation attempts for this state
	var dummy string
	if err := tx.QueryRow(ctx, `SELECT id FROM states WHERE id = $1 FOR UPDATE`, stateID).Scan(&dummy); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}

	// Purge expired reservations directly
	if _, err := tx.Exec(ctx, `DELETE FROM resource_reservations WHERE state_id = $1 AND expires_at < now()`, stateID); err != nil {
		return fmt.Errorf("purge expired: %w", err)
	}

	// Fetch currently active reservations
	rows, err := tx.Query(ctx, `
		SELECT id, address_glob, mode, holder, apply_id, acquired_at, expires_at
		FROM resource_reservations
		WHERE state_id = $1
	`, stateID)
	if err != nil {
		return fmt.Errorf("fetch active: %w", err)
	}
	defer rows.Close()

	var active []ActiveReservation
	for rows.Next() {
		var r ActiveReservation
		r.StateID = stateID
		if err := rows.Scan(&r.ID, &r.AddressGlob, &r.Mode, &r.Holder, &r.ApplyID, &r.AcquiredAt, &r.ExpiresAt); err != nil {
			return err
		}
		active = append(active, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Conflict Matrix: (Write/Read, Write/Write = Blocked). (Read/Read = OK).
	var conflicts []ActiveReservation
	for _, w := range want {
		for _, a := range active {
			if w.Mode == ReservationRead && a.Mode == ReservationRead {
				continue // Read/Read is acceptable
			}
			if a.ApplyID == applyID {
				continue // Ignore our own reservations (idempotent path)
			}
			if globsIntersect(w.AddressGlob, a.AddressGlob) {
				conflicts = append(conflicts, a)
			}
		}
	}

	if len(conflicts) > 0 {
		return &ReservationConflictError{Conflicts: dedupeConflicts(conflicts)}
	}

	expiresAt := time.Now().Add(lease)
	for _, w := range want {
		_, err := tx.Exec(ctx, `
			INSERT INTO resource_reservations (state_id, address_glob, mode, holder, apply_id, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (state_id, address_glob, holder, apply_id) 
			DO UPDATE SET expires_at = EXCLUDED.expires_at, mode = EXCLUDED.mode
		`, stateID, w.AddressGlob, w.Mode, actor, applyID, expiresAt)
		if err != nil {
			return fmt.Errorf("insert reservation %s: %w", w.AddressGlob, err)
		}
	}

	return tx.Commit(ctx)
}

func (s *Store) ReleaseReservations(ctx context.Context, applyID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM resource_reservations WHERE apply_id = $1`, applyID)
	return err
}

// ListActiveReservations returns all currently-active reservations for the named state.
func (s *Store) ListActiveReservations(ctx context.Context, stateName string) ([]ActiveReservation, error) {
	stateName = strings.TrimSpace(stateName)
	if stateName == "" {
		return nil, ErrStateNotFound
	}
	rows, err := s.pool.Query(ctx, `
SELECT rr.id::text, rr.state_id::text, rr.address_glob, rr.mode, rr.holder,
       rr.apply_id::text, rr.acquired_at, rr.expires_at
FROM resource_reservations rr
JOIN states s ON s.id = rr.state_id
WHERE s.name = $1
  AND rr.expires_at > now()
ORDER BY rr.address_glob, rr.mode
`, stateName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveReservation
	for rows.Next() {
		var r ActiveReservation
		if err := rows.Scan(&r.ID, &r.StateID, &r.AddressGlob, &r.Mode, &r.Holder, &r.ApplyID, &r.AcquiredAt, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RenewReservations extends the lease for all reservations held by applyID.
// Returns the number of updated rows. A zero count is not an error.
func (s *Store) RenewReservations(ctx context.Context, applyID string, lease time.Duration) (int, error) {
	applyID = strings.TrimSpace(applyID)
	if applyID == "" {
		return 0, nil
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE resource_reservations
SET expires_at = $2
WHERE apply_id = $1
  AND expires_at > now()
`, applyID, time.Now().Add(lease))
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// globsIntersect handles strict equality and suffix wildcard overlaps between requested reservations.
// This executes entirely in Go logic for deterministic speed and accuracy.
func globsIntersect(g1, g2 string) bool {
	// Fallback to legacy single-file locks.
	if g1 == "*" || g2 == "*" {
		return true
	}

	isGlob1 := strings.HasSuffix(g1, ".*")
	p1 := strings.TrimSuffix(g1, ".*")

	isGlob2 := strings.HasSuffix(g2, ".*")
	p2 := strings.TrimSuffix(g2, ".*")

	if !isGlob1 && !isGlob2 {
		return g1 == g2
	}
	if isGlob1 && !isGlob2 {
		return strings.HasPrefix(g2, p1+".") || g2 == p1
	}
	if !isGlob1 && isGlob2 {
		return strings.HasPrefix(g1, p2+".") || g1 == p2
	}
	return strings.HasPrefix(p1, p2) || strings.HasPrefix(p2, p1)
}

func dedupeConflicts(in []ActiveReservation) []ActiveReservation {
	seen := make(map[string]bool)
	var out []ActiveReservation
	for _, r := range in {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}
