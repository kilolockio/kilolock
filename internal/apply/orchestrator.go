package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/internal/slice"
	"github.com/kilolockio/kilolock/pkg/store"
)

// Run drives one end-to-end `kl apply` invocation. The
// signature mirrors refresh.Run for consistency: take a Store and
// an Options struct, return a Result + error.
//
// On any error after BeginApplyRun, the orchestrator finalizes
// the apply_run row with status='failed' or 'aborted' as
// appropriate, releases reservations, and returns. The caller (CLI
// or test) decides how to surface the error.
//
// Currency-of-trunk handling (v2.5 optimistic-retry commit):
// the trunk is fetched once at the start (for plan-staleness
// checks) and re-fetched inside a retry loop just before each
// commit attempt. If another writer's commit lands between our
// re-fetch and WriteStateForApply, our INSERT collides on
// state_versions.serial uniqueness and we get ErrSerialConflict;
// the orchestrator then re-fetches trunk, re-merges our post-
// apply slice over it, and retries up to maxCommitRetries times.
// Reservations on the write_set are held across the entire loop,
// so the retry is safe — no other writer can touch our addresses
// between iterations. A future v2.5b hardening can swap optimistic
// retry for a row-level SELECT FOR UPDATE inside WriteStateForApply
// if production traffic shows pathological retry rates.
func Run(ctx context.Context, st StoreAPI, opts Options, logger *slog.Logger) (*Result, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	lease := opts.Lease
	if lease <= 0 {
		lease = DefaultLease
	}

	startedAt := time.Now()

	// 1. Fetch the trunk to learn the state id + source serial.
	trunkInfo, err := st.EnsureCurrentStateInfo(ctx, opts.StateName)
	if err != nil {
		return nil, fmt.Errorf("read trunk %q: %w", opts.StateName, err)
	}

	// 2. Begin the apply_run row. Anchors all subsequent
	//    reservations to a single auditable identity. From here
	//    on, every early return MUST call finalizeApplyRun before
	//    returning.
	sourceSerial := trunkInfo.Serial
	if opts.Spec != nil && opts.Spec.SourceSerial != nil && *opts.Spec.SourceSerial > 0 {
		sourceSerial = *opts.Spec.SourceSerial
	}
	applyRun, err := st.BeginApplyRun(ctx,
		trunkInfo.StateID, trunkInfo.VersionID, opts.Actor,
		sourceSerial, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("begin apply_run: %w", err)
	}
	logger.Info("apply started",
		"apply_id", applyRun.ID,
		"state", opts.StateName,
		"source_serial", trunkInfo.Serial,
	)

	res := &Result{
		ApplyID:          applyRun.ID,
		StateName:        opts.StateName,
		StateID:          trunkInfo.StateID,
		SourceSerial:     sourceSerial,
		ResourcesPlanned: len(opts.Spec.WriteSet),
		StartedAt:        startedAt,
	}

	coarseLockHeld := false
	if opts.UseStateEngineLock {
		if err := acquireStateEngineCoarseLock(ctx, st, opts, applyRun.ID, logger); err != nil {
			res.FinishedAt = time.Now()
			finalizeApplyRun(ctx, st, applyRun.ID, res, err, logger)
			return res, err
		}
		coarseLockHeld = true
	}

	commitErr := runInner(ctx, st, opts, trunkInfo, applyRun, lease, logger, res)
	res.FinishedAt = time.Now()

	finalizeApplyRun(ctx, st, applyRun.ID, res, commitErr, logger)
	releaseReservations(ctx, st, applyRun.ID, logger)
	if coarseLockHeld {
		releaseStateEngineCoarseLock(ctx, st, opts.StateName, applyRun.ID, opts.Actor, logger)
	}

	if commitErr != nil {
		return res, commitErr
	}
	return res, nil
}

func acquireStateEngineCoarseLock(ctx context.Context, st StoreAPI, opts Options, applyID string, logger *slog.Logger) error {
	scopeSummary := append([]string(nil), opts.Spec.WriteSet...)
	current, err := st.AcquireStateEngineLock(ctx, opts.StateName, applyID, opts.Actor, scopeSummary)
	if err == nil {
		logger.Info("state-engine coarse lock acquired",
			"apply_id", applyID,
			"state", opts.StateName,
			"scope_count", len(scopeSummary),
		)
		return nil
	}
	if errors.Is(err, store.ErrAlreadyLocked) {
		return fmt.Errorf("acquire state-engine coarse lock: state %q is already locked by who=%q lock_id=%q path=%q",
			opts.StateName, current.Who, current.ID, current.Path)
	}
	return fmt.Errorf("acquire state-engine coarse lock: %w", err)
}

func releaseStateEngineCoarseLock(
	ctx context.Context,
	st StoreAPI,
	stateName, applyID, actor string,
	logger *slog.Logger,
) {
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.ReleaseStateEngineLock(fctx, stateName, applyID, actor); err != nil {
		logger.Warn("release state-engine coarse lock failed", "apply_id", applyID, "state", stateName, "err", err)
		return
	}
	logger.Info("state-engine coarse lock released", "apply_id", applyID, "state", stateName)
}

// runInner executes the post-Begin phase of the apply. Pulled out
// so Run can centralize cleanup (finalize + release) in one place.
func runInner(
	ctx context.Context,
	st StoreAPI,
	opts Options,
	trunkInfo *store.CurrentStateInfo,
	applyRun *store.ApplyRun,
	lease time.Duration,
	logger *slog.Logger,
	res *Result,
) error {
	// 3. Acquire reservations. With Options.WaitForReservations set,
	//    the call returns only when (a) the conflict clears and we
	//    own all addresses, (b) the wait deadline expires (return
	//    the last observed conflict), or (c) the parent ctx is
	//    cancelled. Without wait, behaviour is the original
	//    fail-fast.
	want := reservationsFromSpec(opts.Spec)
	if err := acquireWithWait(ctx, st, trunkInfo.StateID, applyRun.ID, opts.Actor, want, lease, opts, logger); err != nil {
		return fmt.Errorf("acquire reservations: %w", err)
	}
	logger.Info("reservations acquired", "count", len(want))

	// 3b. Heartbeat: renew reservations while terraform runs. Without
	// this, long applies can lose their leases mid-run, allowing a
	// conflicting writer to acquire the same addresses.
	//
	// The heartbeat owns cancellation: if we fail to renew (for example
	// because the lease already expired), we cancel the apply context so
	// terraform is interrupted and the run fails closed.
	applyCtx, cancelApply := context.WithCancel(ctx)
	defer cancelApply()

	hbErr := make(chan error, 1)
	go func() {
		err := renewReservationLeases(applyCtx, st, applyRun.ID, lease, len(want), logger)
		if err != nil {
			cancelApply()
		}
		hbErr <- err
	}()

	// 4. Staleness guard (v2c-3): if the plan spec recorded a source serial,
	// validate the read-set did not change since that serial before running
	// terraform.
	currentTrunkInfo, err := st.EnsureCurrentStateInfo(ctx, opts.StateName)
	if err != nil {
		return fmt.Errorf("re-read trunk for staleness guard: %w", err)
	}
	if opts.Spec.SourceSerial != nil && *opts.Spec.SourceSerial > 0 {
		if currentTrunkInfo.Serial < *opts.Spec.SourceSerial {
			return fmt.Errorf("plan spec source_serial=%d is newer than current trunk serial=%d (wrong state?)",
				*opts.Spec.SourceSerial, currentTrunkInfo.Serial)
		}
		if currentTrunkInfo.Serial > *opts.Spec.SourceSerial {
			baseRaw, err := st.GetStateRawAtSerial(ctx, opts.StateName, *opts.Spec.SourceSerial)
			if err != nil {
				return fmt.Errorf("staleness guard: read trunk at source_serial=%d: %w", *opts.Spec.SourceSerial, err)
			}
			changed, err := changedReadSetAddresses(baseRaw, currentTrunkInfo.Raw, opts.Spec.ReadSet)
			if err != nil {
				return fmt.Errorf("staleness guard: %w", err)
			}
			if len(changed) > 0 {
				return fmt.Errorf("plan is stale: trunk advanced from serial %d to %d and %d read_set address(es) changed: %v",
					*opts.Spec.SourceSerial, currentTrunkInfo.Serial, len(changed), changed)
			}
		}
	}

	// 5. Parse trunk and build the HCL-footprint slice.
	trunkState, err := slice.ParseTrunkState(currentTrunkInfo.Raw)
	if err != nil {
		return fmt.Errorf("parse trunk state: %w", err)
	}
	footprint := effectiveSliceFootprint(opts.Spec)
	sliceState, err := slice.Build(trunkState, footprint)
	if err != nil {
		return fmt.Errorf("build slice: %w", err)
	}

	// 5. Setup tmp working directory.
	setup, err := setupApplyDir(opts.WorkDir, sliceState, opts.SkipCleanup)
	if err != nil {
		return fmt.Errorf("setup apply dir: %w", err)
	}
	res.TempDir = setup.Dir
	defer setup.Cleanup()
	logger.Info("apply working directory ready", "dir", setup.Dir)

	// 6. Run terraform. Pass the write_set as -target so terraform
	//    prunes its graph (and refresh phase) to just the
	//    write_set + transitive deps instead of walking every
	//    resource in the HCL footprint. On a 10k-resource state,
	//    this drops apply latency from ~50s to ~1-2s for a single-
	//    resource update.
	tfLog := &bytes.Buffer{}
	tfIntent, err := runTerraform(applyCtx, setup.Dir, opts.TerraformBin, opts.NoColor, opts.Spec.WriteSet, opts.Spec.Variables, setup.PluginCacheDir, tfLog)
	if err != nil {
		select {
		case hb := <-hbErr:
			if hb != nil {
				return hb
			}
		default:
		}
		return fmt.Errorf("terraform: %w", err)
	}

	// Stop heartbeat and surface any renewal failure before we commit.
	cancelApply()
	if hb := <-hbErr; hb != nil {
		return hb
	}

	// Abort escape hatch: an operator may have aborted this apply while
	// terraform was running. Never proceed to merge/commit in that case.
	if stt, err := st.GetApplyRunStatus(ctx, applyRun.ID); err == nil && stt == store.ApplyRunAborted {
		return fmt.Errorf("apply was aborted by operator; refusing to commit")
	}

	// 7. Read & parse post-apply state.
	postApplyBytes, err := readPostApplyState(setup.Dir)
	if err != nil {
		return err
	}
	postApplyState, err := slice.ParseTrunkState(postApplyBytes)
	if err != nil {
		return fmt.Errorf("parse post-apply state: %w", err)
	}

	// 8. Safety check: every post-apply address must be in HCL
	//    footprint. A surprise resource means terraform decided
	//    to make something we have no reservation for.
	if bad := validatePostApplyHasNoSurprises(postApplyState, footprint); len(bad) > 0 {
		return fmt.Errorf("post-apply state has resources outside HCL footprint: %v", bad)
	}

	// 9-11. Re-fetch trunk → merge → write, with retry on serial
	//       conflict. The retry loop is the v2c-1 → v2.5 fix: under
	//       parallel applies on disjoint write sets, the substrate
	//       admits both reservations but the commit step still
	//       contends on state_versions.serial uniqueness because we
	//       compute the next serial from the trunk we last fetched.
	//       When another apply commits first, our INSERT collides on
	//       the (state_id, serial) unique constraint and we get
	//       ErrSerialConflict.
	//
	//       The retry is safe and bounded:
	//
	//         - safe: our reservations on the write_set are still
	//           held, so no other writer can touch our addresses
	//           between iterations. The fresh trunk reflects only
	//           non-conflicting writes (in particular, other apply
	//           runs that committed disjoint write_sets); we re-
	//           overlay our post-apply slice over that and the
	//           result is the next monotonic serial.
	//
	//         - bounded: maxCommitRetries caps the loop. In practice
	//           every retry is preceded by another commit landing, so
	//           the loop converges as quickly as concurrent apply
	//           latency allows. We log every retry so an operator
	//           can spot pathological churn.
	//
	//       Future hardening (v2.5b): take a row lock on states
	//       inside WriteStateForApply (SELECT ... FOR UPDATE) so the
	//       serial is allocated under the lock. That makes the
	//       commit step strictly serial instead of optimistic-retry,
	//       at the cost of holding a tx-scoped Postgres lock during
	//       the INSERT. Optimistic retry is the right default for
	//       v2d because typical parallel apply windows are short
	//       and retries are cheap.
	const maxCommitRetries = 8
	var merged *mergeResult
	var nativeAppliedAddresses []string
	var nativeDeletedAddresses []string
	var nativeIntentSource string
	var committedSerial int64
	var commitErr error
	commitKind := "merged state"
	if opts.UseStateEngineLock {
		commitKind = "state-engine delta"
	}
	for attempt := 0; attempt < maxCommitRetries; attempt++ {
		freshTrunkInfo, err := st.EnsureCurrentStateInfo(ctx, opts.StateName)
		if err != nil {
			return fmt.Errorf("re-read trunk before commit (attempt %d): %w", attempt+1, err)
		}
		freshTrunkState, err := slice.ParseTrunkState(freshTrunkInfo.Raw)
		if err != nil {
			return fmt.Errorf("parse fresh trunk (attempt %d): %w", attempt+1, err)
		}

		if opts.UseStateEngineLock {
			if err := validateTrustedStateEngineIntent(opts.Spec, tfIntent); err != nil {
				return fmt.Errorf("trusted state-engine intent validation failed (attempt %d): %w", attempt+1, err)
			}
			nativeAppliedAddresses = append([]string(nil), tfIntent.ExactWriteSet...)
			nativeDeletedAddresses = append([]string(nil), tfIntent.DeleteSet...)
			nativeIntentSource = "terraform validation replan"
			delta, err := buildStateEngineDeltaCommit(postApplyState, nativeAppliedAddresses, nativeDeletedAddresses)
			if err != nil {
				return fmt.Errorf("build state-engine delta commit (attempt %d): %w", attempt+1, err)
			}
			if err := attachOutputDelta(&delta, freshTrunkState, postApplyState); err != nil {
				return fmt.Errorf("attach state-engine output delta (attempt %d): %w", attempt+1, err)
			}
			committedSerial = freshTrunkInfo.Serial + 1
			res.CommitMode = "state-engine delta"
			logger.Info("state-engine delta intent prepared",
				"apply_id", applyRun.ID,
				"intent_source", nativeIntentSource,
				"intent_write_count", len(nativeAppliedAddresses),
				"intent_delete_count", len(nativeDeletedAddresses),
				"intent_writes", nativeAppliedAddresses,
				"intent_deletes", nativeDeletedAddresses,
				"delta_resources", len(delta.Resources),
			)
			commitErr = st.WriteStateEngineDeltaForApply(ctx,
				opts.StateName, applyRun.ID, freshTrunkInfo.Serial, delta,
				"state-engine-apply", opts.Actor,
			)
		} else {
			merged, err = buildMergedState(
				freshTrunkState, postApplyState,
				opts.Spec.WriteSet, footprint,
			)
			if err != nil {
				return fmt.Errorf("merge (attempt %d): %w", attempt+1, err)
			}
			committedSerial = merged.NewSerial
			res.CommitMode = "snapshot merge"
			commitErr = st.WriteStateForApply(ctx,
				opts.StateName, applyRun.ID, freshTrunkInfo.Serial, merged.MergedBytes,
				"apply", opts.Actor,
			)
		}
		if commitErr == nil {
			if attempt > 0 {
				logger.Info("commit succeeded after retries",
					"apply_id", applyRun.ID,
					"attempts", attempt+1,
					"final_serial", committedSerial,
				)
			}
			break
		}
		if !errors.Is(commitErr, store.ErrSerialConflict) {
			return fmt.Errorf("write %s: %w", commitKind, commitErr)
		}

		logger.Info("commit lost serial race; retrying on top of newer trunk",
			"apply_id", applyRun.ID,
			"attempt", attempt+1,
			"tried_serial", committedSerial,
		)
	}
	if commitErr != nil {
		return fmt.Errorf("write %s: exhausted %d retries against parallel commits; last error: %w",
			commitKind, maxCommitRetries, commitErr)
	}

	// 12. Recover the new state_version id for the audit row.
	newVersionID, err := st.LookupStateVersionID(ctx, trunkInfo.StateID, committedSerial)
	if err != nil {
		// The write succeeded but we can't find the row.
		// Surface as an error so the audit reflects "commit
		// happened but bookkeeping broke" rather than silently
		// completing with empty fields.
		return fmt.Errorf("lookup new state_version id: %w", err)
	}

	res.CommittedSerial = committedSerial
	res.NewVersionID = newVersionID
	if opts.UseStateEngineLock {
		res.ResourcesApplied = len(nativeAppliedAddresses)
		res.AppliedAddresses = nativeAppliedAddresses
		res.NativeIntentSource = nativeIntentSource
		res.NativeIntentWriteSet = append([]string(nil), nativeAppliedAddresses...)
		res.NativeIntentDeleteSet = append([]string(nil), nativeDeletedAddresses...)
	} else {
		res.ResourcesApplied = len(merged.AppliedAddresses)
		res.AppliedAddresses = merged.AppliedAddresses
	}
	logger.Info("apply committed",
		"apply_id", applyRun.ID,
		"new_serial", committedSerial,
		"applied", res.ResourcesApplied,
	)
	return nil
}

func effectiveSliceFootprint(spec *plan.PlanSpec) map[string]struct{} {
	if spec == nil {
		return map[string]struct{}{}
	}
	footprint := slice.IndexFootprintByGroup(spec.HCLFootprint)
	if spec.StateEngine == nil {
		return footprint
	}
	for _, addr := range spec.StateEngine.FetchAddresses {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			footprint[addr] = struct{}{}
		}
	}
	for _, addr := range spec.StateEngine.ConfigRequiredNodes {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			footprint[addr] = struct{}{}
		}
	}
	for _, addr := range spec.StateEngine.RemovedConfigNodes {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			footprint[addr] = struct{}{}
		}
	}
	return footprint
}

func renewReservationLeases(ctx context.Context, st StoreAPI, applyID string, lease time.Duration, expected int, logger *slog.Logger) error {
	if expected <= 0 {
		return nil
	}
	interval := lease / 3
	switch {
	case interval <= 0:
		interval = 1 * time.Second
	case interval > 30*time.Second:
		interval = 30 * time.Second
	case interval < 1*time.Second:
		interval = 1 * time.Second
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}

		status, err := st.GetApplyRunStatus(ctx, applyID)
		if err == nil && status == store.ApplyRunAborted {
			logger.Warn("reservation heartbeat stopping: apply aborted", "apply_id", applyID)
			return fmt.Errorf("apply aborted by operator")
		}

		n, err := st.RenewReservations(ctx, applyID, lease)
		if err != nil {
			logger.Warn("reservation heartbeat renew failed", "apply_id", applyID, "err", err)
			return fmt.Errorf("reservation heartbeat: renew leases: %w", err)
		}
		if n < expected {
			logger.Warn("reservation heartbeat lost leases", "apply_id", applyID, "renewed", n, "expected", expected)
			return fmt.Errorf("reservation heartbeat: only renewed %d/%d reservations (lease lost?)", n, expected)
		}
	}
}

// acquireWithWait is the conflict-tolerant wrapper around
// store.AcquireReservations. With opts.WaitForReservations == 0
// it preserves the original fail-fast semantics. With a positive
// budget it loops with exponential backoff until either the
// conflict clears (return nil) or the budget is exhausted (return
// the last observed *ReservationConflictError so the caller can
// surface the blockers).
//
// Design notes:
//
//   - Backoff schedule: 1s → 2s → 4s → 8s capped at 10s. Chosen
//     to keep the early-conflict feel snappy while not hammering
//     postgres if a long-running apply holds reservations for many
//     minutes. The cap is well below the typical lease duration
//     so we will re-attempt at least once per lease window.
//
//   - The progress callback fires AFTER each conflict observation,
//     BEFORE the sleep, so the CLI can render "blocked by X
//     reservations, retrying in 2s" rather than print nothing while
//     the orchestrator sleeps.
//
//   - On the final iteration (Remaining <= 0) we still try one
//     more acquire so the operator's bound is interpreted as "wait
//     up to this long" rather than "give up exactly this long
//     after first conflict observation".
//
//   - Context cancellation propagates immediately: an operator
//     hitting Ctrl-C during the wait gets out without waiting for
//     the next backoff tick.
func acquireWithWait(
	ctx context.Context,
	st StoreAPI,
	stateID, applyID, actor string,
	want []store.Reservation,
	lease time.Duration,
	opts Options,
	logger *slog.Logger,
) error {
	err := st.AcquireReservations(ctx, stateID, applyID, actor, want, lease)
	if err == nil || opts.WaitForReservations <= 0 {
		return err
	}
	var conflict *store.ReservationConflictError
	if !errors.As(err, &conflict) {
		return err
	}

	deadline := time.Now().Add(opts.WaitForReservations)
	logger.Info("reservation conflict; waiting", "budget", opts.WaitForReservations.String())

	backoffs := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		10 * time.Second,
	}
	start := time.Now()
	for iter := 1; ; iter++ {
		remaining := time.Until(deadline)
		idx := iter - 1
		if idx >= len(backoffs) {
			idx = len(backoffs) - 1
		}
		nextSleep := backoffs[idx]
		if nextSleep > remaining {
			nextSleep = remaining
		}

		if opts.ReservationWaitNotifier != nil {
			opts.ReservationWaitNotifier(WaitEvent{
				Iteration:   iter,
				Elapsed:     time.Since(start),
				Remaining:   remaining,
				Conflicts:   conflict.Conflicts,
				NextRetryIn: nextSleep,
			})
		}

		if remaining <= 0 {
			return conflict
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nextSleep):
		}

		err = st.AcquireReservations(ctx, stateID, applyID, actor, want, lease)
		if err == nil {
			logger.Info("reservation conflict cleared",
				"waited", time.Since(start).Round(time.Second).String(),
				"iterations", iter,
			)
			return nil
		}
		if !errors.As(err, &conflict) {
			return err
		}
	}
}

// reservationsFromSpec projects PlanSpec.Reservations into the
// shape Store.AcquireReservations wants.
func reservationsFromSpec(spec *plan.PlanSpec) []store.Reservation {
	out := make([]store.Reservation, 0, len(spec.Reservations))
	for _, r := range spec.Reservations {
		out = append(out, store.Reservation{
			AddressGlob: r.Address,
			Mode:        store.ReservationMode(r.Mode),
		})
	}
	return out
}

// finalizeApplyRun closes the apply_run row with the right status
// + counters. Tolerates errors (the caller already has the apply
// error in hand; a finalize failure shouldn't replace it).
//
// Status rules:
//   - commitErr == nil           → committed
//   - errors.Is(commitErr, ctx.Canceled) → aborted
//   - else                       → failed
func finalizeApplyRun(
	ctx context.Context,
	st StoreAPI,
	applyID string,
	res *Result,
	commitErr error,
	logger *slog.Logger,
) {
	// Use a fresh background context with a short timeout so the
	// finalize survives even if the outer ctx is already canceled
	// (which is exactly when we MOST need the audit row written).
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := store.ApplyRunCommitted
	switch {
	case commitErr == nil:
		// keep committed
	case errors.Is(commitErr, context.Canceled),
		errors.Is(commitErr, context.DeadlineExceeded):
		status = store.ApplyRunAborted
	default:
		status = store.ApplyRunFailed
	}

	in := store.FinishApplyRunInput{
		Status:           status,
		ResourcesPlanned: res.ResourcesPlanned,
		ResourcesApplied: res.ResourcesApplied,
		ResourcesFailed:  res.ResourcesFailed,
	}
	if status == store.ApplyRunCommitted {
		serial := res.CommittedSerial
		in.CommittedSerial = &serial
		in.ToVersionID = res.NewVersionID
	} else if commitErr != nil {
		in.ErrorSummary = commitErr.Error()
	}

	if err := st.FinishApplyRun(fctx, applyID, in); err != nil {
		logger.Warn("finalize apply_run failed", "apply_id", applyID, "err", err)
	}
}

// releaseReservations clears every reservation held by applyID.
// Same fresh-context pattern as finalizeApplyRun.
func releaseReservations(
	ctx context.Context,
	st StoreAPI,
	applyID string,
	logger *slog.Logger,
) {
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.ReleaseReservations(fctx, applyID); err != nil {
		logger.Warn("release reservations failed", "apply_id", applyID, "err", err)
	}
}

// validateOptions checks the Options the caller assembled. The
// goal is to fail FAST with a clear message before BeginApplyRun
// produces an audit row for a request that never had a chance.
func validateOptions(opts Options) error {
	if opts.Spec == nil {
		return errors.New("apply.Run: Options.Spec is nil")
	}
	if opts.Spec.FormatVersion != plan.CurrentSpecFormatVersion {
		return fmt.Errorf("apply.Run: spec format_version %q not supported (this build understands %q)",
			opts.Spec.FormatVersion, plan.CurrentSpecFormatVersion)
	}
	if opts.StateName == "" {
		return errors.New("apply.Run: Options.StateName is empty")
	}
	if opts.Actor == "" {
		return errors.New("apply.Run: Options.Actor is empty")
	}
	if opts.WorkDir == "" {
		return errors.New("apply.Run: Options.WorkDir is empty")
	}
	return nil
}
