package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

type StateCoexistenceMode string

const (
	StateCoexistenceWarn   StateCoexistenceMode = "warn"
	StateCoexistenceStrict StateCoexistenceMode = "strict"
)

func (m StateCoexistenceMode) Valid() bool {
	return m == StateCoexistenceWarn || m == StateCoexistenceStrict
}

// SetStateExclusiveLocks flips the per-state exclusive_locks toggle.
// When on=true, vanilla Terraform clients serialize through the v1
// whole-state lock. When on=false (default), multiple locks may coexist
// and the backend relies on optimistic write-set merge at commit time.
func (s *Store) SetStateExclusiveLocks(ctx context.Context, stateName string, on bool) error {
	stateName = strings.TrimSpace(stateName)
	if stateName == "" {
		return errors.New("SetStateExclusiveLocks: stateName must not be empty")
	}

	where, args := s.statesByNameWhere(ctx, stateName)
	args = append(args, on)
	onParam := len(args)

	q := `UPDATE states SET exclusive_locks = $` + fmt.Sprint(onParam) + ` WHERE ` + where
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStateNotFound
	}
	return nil
}

// GetStateExclusiveLocks reports the current value of the exclusive_locks toggle.
func (s *Store) GetStateExclusiveLocks(ctx context.Context, stateName string) (bool, error) {
	stateName = strings.TrimSpace(stateName)
	if stateName == "" {
		return false, errors.New("GetStateExclusiveLocks: stateName must not be empty")
	}
	where, args := s.statesByNameWhere(ctx, stateName)
	var v bool
	err := s.pool.QueryRow(ctx, `SELECT exclusive_locks FROM states WHERE `+where, args...).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrStateNotFound
	}
	return v, err
}

// SetStateCoexistenceMode flips the per-state mixed-mode policy between
// vanilla Terraform whole-state locks and `kl apply`.
func (s *Store) SetStateCoexistenceMode(ctx context.Context, stateName string, mode StateCoexistenceMode) error {
	stateName = strings.TrimSpace(stateName)
	if stateName == "" {
		return errors.New("SetStateCoexistenceMode: stateName must not be empty")
	}
	if !mode.Valid() {
		return fmt.Errorf("SetStateCoexistenceMode: invalid mode %q", mode)
	}

	where, args := s.statesByNameWhere(ctx, stateName)
	args = append(args, string(mode))
	modeParam := len(args)

	q := `UPDATE states SET coexistence_mode = $` + fmt.Sprint(modeParam) + ` WHERE ` + where
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStateNotFound
	}
	return nil
}

// GetStateCoexistenceMode reports the current per-state mixed-mode policy.
func (s *Store) GetStateCoexistenceMode(ctx context.Context, stateName string) (StateCoexistenceMode, error) {
	stateName = strings.TrimSpace(stateName)
	if stateName == "" {
		return "", errors.New("GetStateCoexistenceMode: stateName must not be empty")
	}
	where, args := s.statesByNameWhere(ctx, stateName)
	var v string
	err := s.pool.QueryRow(ctx, `SELECT coexistence_mode FROM states WHERE `+where, args...).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrStateNotFound
	}
	return StateCoexistenceMode(v), err
}
