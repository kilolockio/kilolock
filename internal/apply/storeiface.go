package apply

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

// StoreAPI is the minimal execution substrate the apply orchestrator needs.
// A concrete *store.Store satisfies it for direct DB mode; a server-backed
// HTTP client can satisfy it for OSS/cloud-friendly CLI mode.
type StoreAPI interface {
	GetCurrentStateInfo(ctx context.Context, name string) (*store.CurrentStateInfo, error)
	EnsureCurrentStateInfo(ctx context.Context, name string) (*store.CurrentStateInfo, error)
	GetStateRawAtSerial(ctx context.Context, name string, serial int64) ([]byte, error)
	BeginApplyRun(ctx context.Context, stateID, fromVersionID, actor string, sourceSerial int64, info json.RawMessage) (*store.ApplyRun, error)
	GetApplyRunStatus(ctx context.Context, id string) (store.ApplyRunStatus, error)
	FinishApplyRun(ctx context.Context, id string, in store.FinishApplyRunInput) error
	AbortApplyRun(ctx context.Context, id, reason string) error
	AcquireStateEngineLock(ctx context.Context, name, applyID, holder string, scopeSummary []string) (store.LockInfo, error)
	ReleaseStateEngineLock(ctx context.Context, name, applyID, actor string) error
	AcquireReservations(ctx context.Context, stateID, applyID, actor string, want []store.Reservation, lease time.Duration) error
	RenewReservations(ctx context.Context, applyID string, lease time.Duration) (int, error)
	ReleaseReservations(ctx context.Context, applyID string) error
	WriteStateForApply(ctx context.Context, name, applyID string, baseSerial int64, rawState []byte, source, actor string) error
	WriteStateEngineDeltaForApply(ctx context.Context, name, applyID string, baseSerial int64, delta store.StateEngineDeltaCommit, source, actor string) error
	LookupStateVersionID(ctx context.Context, stateID string, serial int64) (string, error)
}
