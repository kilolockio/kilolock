// Package refresh is the orchestrator for `kl refresh`. It
// reads a state from the store, groups its resource instances by
// (provider source, alias), opens an RPC client per group via a
// caller-supplied ClientFactory, walks each group asking the provider
// what each resource currently looks like in the cloud, splices the
// new attributes back into the state, and writes a new state_version
// with source="refresh".
//
// v1.6b ships the orchestrator and the ClientFactory abstraction. The
// production factory (Discover → Launch → Configure-from-Postgres) is
// added in v1.6c alongside the CLI; this commit's tests inject mocks
// so the orchestrator's logic is exercised without the wire.
//
// Concurrency model:
//
//   - One worker per (provider source, alias) group runs in its own
//     goroutine. Within a group, resources are refreshed serially
//     because the underlying Client multiplexes but providers do not
//     guarantee thread-safe ReadResource semantics.
//   - Options.Concurrency caps the number of groups running at once;
//     0 means "no cap" (every group launches immediately).
//
// Error handling:
//
//   - By default the orchestrator collects per-resource failures into
//     Result.Errors and continues. The terminal status reflects the
//     outcome: 'succeeded' when no resources failed, 'failed' when
//     any did.
//   - With Options.FailFast the first per-resource error cancels the
//     run; remaining groups observe the cancelled context and exit
//     promptly. The triggering error is also recorded in
//     Result.Errors so callers can surface its address.
//
// Locking:
//
//   - v1.6b does not acquire a state lock. WriteState rejects with
//     ErrStateLocked if a lock is held by another writer (e.g. an
//     in-progress terraform apply); refresh surfaces that as a run
//     failure. v1.6c (the CLI) is the right layer to take and release
//     a refresh-scoped lock around Run.
package refresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/kilolockio/kilolock/internal/provider"
	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/store"
)

// OpenedClient bundles a configured RPC client with the metadata
// downstream code needs to look schemas up in the cache and surface
// useful diagnostics. The orchestrator never constructs one of these
// directly; it asks the ClientFactory to.
//
// Version is the resolved provider binary version (e.g. "3.2.4"),
// used as the schema cache key. It must be non-empty even when the
// factory is a mock — the orchestrator does not invent one.
//
// Schema is the provider's full declared schema (resources, data
// sources, provider config block). The production factory populates
// it from the live provider via GetSchema and the persistent
// provider_schemas cache; test factories that short-circuit the
// encoding pipeline may leave it nil.
type OpenedClient struct {
	Client          provider.Client
	Source          provider.SourceAddress
	Alias           string
	Version         string
	ProtocolVersion int
	Schema          *provider.Schema
}

// ClientFactory abstracts "give me a configured client for this
// (source, alias) pair". Production wiring (v1.6c) discovers the
// binary via provider.Discover, launches it via provider.Launch,
// and Configures it from provider_configs. Tests use whatever they
// like — typically a fakeFactory backed by a hand-rolled Client.
//
// The factory owns the Client's lifecycle from Open through Close:
// the orchestrator calls Close on every successfully opened client
// at run end (success or failure) and the factory is expected not
// to leak resources for clients that never reach Close (e.g. when
// Open itself errors out).
type ClientFactory interface {
	Open(ctx context.Context, source provider.SourceAddress, alias string) (*OpenedClient, error)
}

// Options control a single Run invocation. Zero values are valid and
// produce safe defaults: serial within groups, unlimited group
// parallelism, collect-all error mode, no-op actor.
type Options struct {
	// StateName is the named state to refresh. Required.
	StateName string

	// Concurrency caps the number of (provider source, alias)
	// groups that run in parallel. <= 0 means "no cap" (every
	// group launches immediately). Set to 1 to force fully
	// serial behavior — useful for reproducing flaky test
	// failures without concurrency noise.
	Concurrency int

	// FailFast stops the run on the first per-resource error.
	// The remaining workers observe a cancelled context. The
	// triggering error is preserved in Result.Errors and also
	// returned as the Run-level error.
	FailFast bool

	// DryRun executes every RPC and counts drift but skips the
	// final WriteState. Result.SerialAfter equals SerialBefore
	// in dry-run mode. Useful for `kl refresh --dry-run`
	// to preview drift without committing.
	DryRun bool

	// Actor is the free-form actor string recorded against the
	// refresh_runs row and the resulting state_version. Empty is
	// fine; the audit row simply records NULL actor.
	Actor string
}

// ResourceError is one per-resource failure during a run. Address is
// the canonical Terraform address (e.g. `aws_instance.web[0]`) and
// is always populated; Err is wrapped with provider-name + diagnostics
// context so callers do not need to unwrap.
type ResourceError struct {
	Address string
	Err     error
}

func (re ResourceError) Error() string { return re.Address + ": " + re.Err.Error() }
func (re ResourceError) Unwrap() error { return re.Err }

// Result is what Run returns on completion. It is populated even on
// failure paths so the CLI can render counters and addresses for the
// operator regardless of outcome.
type Result struct {
	RunID            string
	StateName        string
	Status           store.RefreshRunStatus
	SerialBefore     int64
	SerialAfter      int64
	ResourcesChecked int
	ResourcesChanged int
	ResourcesFailed  int
	Errors           []ResourceError
	StartedAt        time.Time
	FinishedAt       time.Time
	DryRun           bool

	// ChangedAddresses is the canonical Terraform address of every
	// resource the refresh found to be different from its prior
	// stored state. len(ChangedAddresses) == ResourcesChanged is an
	// invariant: the slice is the per-resource view of the
	// aggregate counter.
	//
	// Sorted lexicographically before Run returns, so the CLI
	// renders a stable output and operators can diff two refresh
	// outputs deterministically.
	//
	// Memory: addresses only, no full attribute blobs. A refresh
	// that finds 10k drifted resources keeps a 10k-entry string
	// slice in memory; the full before/after view is recoverable
	// from the database via state_versions.raw_state, which is
	// what the current_resource_drift view (v1.7b) exposes for
	// SQL consumers.
	ChangedAddresses []string

	// parsed is the *State the orchestrator mutates. It is unexported
	// because callers should not poke at it after Run returns;
	// SerialAfter on the result is the canonical "what got
	// committed" signal, and the new state is observable in the
	// store via GetCurrentState.
	parsed *tfstate.State

	// newVersionID is the row id of the state_version that the
	// refresh produced (if any). Set only after a successful
	// commit; threaded into FinishRefreshRunInput.ToVersionID.
	newVersionID string
}

// Run is the orchestrator entrypoint. It loads the named state,
// records a refresh_runs row, refreshes resources via the supplied
// factory, writes a new state_version unless DryRun is set, and
// finalizes the audit row.
//
// Returns a non-nil Result whenever a refresh_runs row was created;
// inspect Result.Errors and Result.Status for the per-resource and
// terminal outcomes. The error return is reserved for run-level
// failures that prevented the orchestrator from completing
// meaningfully (state not found, audit-row writes failing, etc.) —
// per-resource failures live in Result.Errors and do not surface
// here.
func Run(ctx context.Context, st *store.Store, factory ClientFactory, opts Options) (*Result, error) {
	if opts.StateName == "" {
		return nil, errors.New("refresh.Run: StateName must not be empty")
	}
	if st == nil {
		return nil, errors.New("refresh.Run: store must not be nil")
	}
	if factory == nil {
		return nil, errors.New("refresh.Run: factory must not be nil")
	}

	info, err := st.GetCurrentStateInfo(ctx, opts.StateName)
	if err != nil {
		return nil, fmt.Errorf("load state %q: %w", opts.StateName, err)
	}
	parsed, err := tfstate.Parse(info.Raw)
	if err != nil {
		return nil, fmt.Errorf("parse state %q: %w", opts.StateName, err)
	}

	run, err := st.BeginRefreshRun(ctx, info.StateID, info.VersionID, opts.Actor)
	if err != nil {
		return nil, fmt.Errorf("begin refresh run: %w", err)
	}

	result := &Result{
		RunID:        run.ID,
		StateName:    opts.StateName,
		Status:       store.RefreshRunRunning,
		SerialBefore: info.Serial,
		SerialAfter:  info.Serial, // updated post-WriteState; equals before on dry-run / no commit
		StartedAt:    run.StartedAt,
		DryRun:       opts.DryRun,
		parsed:       parsed,
	}

	// All branches below converge on a single Finish, regardless of
	// which path fails. Anything observed during refresh accumulates
	// into result; Run-level fatal errors return early via
	// finalizeWithError.
	groups, parseErr := groupByProvider(parsed)
	if parseErr != nil {
		return finalizeWithError(ctx, st, result, run.ID,
			fmt.Errorf("group resources: %w", parseErr))
	}

	processGroups(ctx, factory, groups, opts, result)

	// Stable per-resource output: sort the addresses once after
	// processGroups returns, before commit and finalize. The slice
	// was built across goroutines so iteration order is otherwise
	// non-deterministic. Sorting here costs O(N log N) on the
	// drifted-resource count (typically << total resources) and
	// gives the CLI and tests a stable surface to assert on.
	sort.Strings(result.ChangedAddresses)

	// Decide whether to commit:
	//
	//   - DryRun: never commits (counters and audit only).
	//   - Any per-resource failure: never commits. A partially-
	//     refreshed state is more dangerous than no refresh: the
	//     resulting state_version pretends to be the truth but is
	//     actually a mix of new and stale attributes. Force the
	//     operator to re-run after fixing the cause. v1.6c may
	//     add an opt-in --commit-partial flag once we have a UX
	//     answer for "here's what didn't refresh".
	//   - Otherwise commit. "Always-write on no-drift" is
	//     intentional: a refresh that confirmed every resource is
	//     unchanged still records that fact as a new
	//     state_version with source='refresh', matching terraform
	//     refresh's own behavior. The version chain stays honest
	//     about when refresh ran.
	commit := !opts.DryRun && result.ResourcesFailed == 0
	if commit {
		if err := commitRefreshedState(ctx, st, parsed, info, opts, result); err != nil {
			return finalizeWithError(ctx, st, result, run.ID, err)
		}
	}

	return finalize(ctx, st, result, run.ID)
}

// providerGroup is one batch of resource instances bound to the same
// (source, alias) pair. The pointers in entries point into the
// caller's parsed *State, so updates to the new attributes mutate the
// document we ultimately re-marshal and write.
type providerGroup struct {
	Source  provider.SourceAddress
	Alias   string
	Entries []*groupEntry
}

type groupEntry struct {
	// Resource and Instance index back into parsed.Resources for
	// the eventual splice-back. We keep the indices rather than the
	// pointers to avoid escaping the slice element addresses across
	// goroutines (the slice itself is read-only after grouping).
	ResourceIdx int
	InstanceIdx int

	// Address is the canonical Terraform address, materialized once
	// at grouping time so per-resource error messages have it
	// without re-parsing.
	Address string

	// TypeName is the resource type (e.g. "null_resource").
	TypeName string

	// SchemaVersion is the per-instance schema_version field from
	// state. Reserved for upgrade-resource-state plumbing in a
	// later commit; v1.6 does not yet drive schema upgrades.
	SchemaVersion int
}

// groupByProvider partitions every managed resource instance in the
// parsed state into groups keyed by (source, alias). Data sources are
// skipped — refresh is a managed-resource concept; data sources are
// recomputed during plan and have no persistent state to drift.
//
// The groups are sorted by (source, alias) so iteration order is
// stable across Run invocations. Any provider reference that fails
// to parse aborts grouping with an error rather than silently
// dropping resources; an unrecognizable provider ref is a state
// integrity issue worth surfacing.
func groupByProvider(s *tfstate.State) ([]*providerGroup, error) {
	byKey := map[string]*providerGroup{}
	for ri := range s.Resources {
		r := &s.Resources[ri]
		if r.Mode != "managed" {
			continue
		}
		src, alias, err := tfstate.ParseProviderRef(r.Provider)
		if err != nil {
			return nil, fmt.Errorf("resource %s.%s: %w", r.Type, r.Name, err)
		}
		addr, err := provider.ParseSourceAddress(src)
		if err != nil {
			return nil, fmt.Errorf("resource %s.%s: parse source %q: %w", r.Type, r.Name, src, err)
		}
		key := addr.String() + "\x00" + alias
		g, ok := byKey[key]
		if !ok {
			g = &providerGroup{Source: addr, Alias: alias}
			byKey[key] = g
		}
		for ii := range r.Instances {
			inst := &r.Instances[ii]
			a, err := tfstate.InstanceAddress(*r, *inst)
			if err != nil {
				return nil, fmt.Errorf("resource %s.%s: address: %w", r.Type, r.Name, err)
			}
			g.Entries = append(g.Entries, &groupEntry{
				ResourceIdx:   ri,
				InstanceIdx:   ii,
				Address:       a,
				TypeName:      r.Type,
				SchemaVersion: inst.SchemaVersion,
			})
		}
	}

	out := make([]*providerGroup, 0, len(byKey))
	out = slices.AppendSeq(out, maps.Values(byKey))
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source.String() != out[j].Source.String() {
			return out[i].Source.String() < out[j].Source.String()
		}
		return out[i].Alias < out[j].Alias
	})
	return out, nil
}

// processGroups runs one goroutine per group, bounded by
// opts.Concurrency. Per-resource outcomes are merged into result
// under a mutex; groups never write to the same Resource index, so
// the parsed state itself is mutated lock-free.
func processGroups(ctx context.Context, factory ClientFactory, groups []*providerGroup, opts Options, result *Result) {
	if len(groups) == 0 {
		return
	}

	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var sem chan struct{}
	if opts.Concurrency > 0 {
		sem = make(chan struct{}, opts.Concurrency)
	}

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, g := range groups {
		g := g
		wg.Add(1)
		if sem != nil {
			sem <- struct{}{}
		}
		go func() {
			defer wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			processGroup(groupCtx, factory, g, opts, result, &mu, cancel)
		}()
	}
	wg.Wait()
}

// processGroup opens a single provider client and refreshes every
// resource in the group serially. Failures inside the group are
// recorded into result; if FailFast is set, the first failure also
// cancels the shared groupCtx so peer goroutines exit promptly.
func processGroup(
	ctx context.Context,
	factory ClientFactory,
	g *providerGroup,
	opts Options,
	result *Result,
	mu *sync.Mutex,
	cancel context.CancelFunc,
) {
	if err := ctx.Err(); err != nil {
		// Already cancelled; record every entry as failed so the
		// counters reflect that they were not actually checked.
		recordGroupAborted(g, err, result, mu)
		return
	}

	opened, err := factory.Open(ctx, g.Source, g.Alias)
	if err != nil {
		recordGroupAborted(g, fmt.Errorf("open %s[%s]: %w", g.Source.String(), g.Alias, err), result, mu)
		if opts.FailFast {
			cancel()
		}
		return
	}
	stopOnce := sync.Once{}
	stop := func() {
		stopOnce.Do(func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = opened.Client.Stop(stopCtx)
			stopCancel()
		})
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	defer func() {
		close(done)
		// Best-effort graceful stop on cancellation. Close() hard-kills
		// the provider child process; Stop() gives the provider a chance
		// to shut down cleanly when the operator cancels the run.
		if ctx.Err() != nil {
			stop()
		}
		_ = opened.Client.Close()
	}()

	for _, e := range g.Entries {
		if err := ctx.Err(); err != nil {
			recordEntryFailed(e, err, result, mu)
			continue
		}
		err := refreshOne(ctx, opened, g, e, result, mu)
		if err != nil && opts.FailFast {
			cancel()
			return
		}
	}
}

func recordGroupAborted(g *providerGroup, err error, result *Result, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	for _, e := range g.Entries {
		result.ResourcesFailed++
		result.Errors = append(result.Errors, ResourceError{Address: e.Address, Err: err})
	}
}

func recordEntryFailed(e *groupEntry, err error, result *Result, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	result.ResourcesFailed++
	result.Errors = append(result.Errors, ResourceError{Address: e.Address, Err: err})
}

// refreshOne is the per-resource RPC dance. Returns the recorded
// error (so the caller can decide on FailFast) or nil on success.
//
// Flow:
//
//  1. Read prior attributes (verbatim JSON from state).
//  2. If the live provider schema version is higher than the
//     recorded schema_version, call UpgradeResourceState. The
//     provider returns the prior attributes re-shaped under the
//     current schema. The orchestrator splices that result back
//     and bumps the instance's schema_version so subsequent runs
//     don't repeat the work.
//  3. Call ReadResource with the (post-upgrade) prior.
//  4. Splice the response NewState back into the parsed state.
//
// The change signal compares the *original* stored bytes against
// the *final* NewState — that way a schema upgrade and a cloud
// drift collapse into a single "changed" tick. v1.7's drift
// surfacing can split them apart when the per-resource view lands.
//
// The encodingClient wrapper takes care of JSON↔msgpack at both
// the upgrade and read boundaries; the orchestrator itself only
// ever sees JSON. Tests inject a non-encoding mock that produces
// JSON directly.
func refreshOne(ctx context.Context, opened *OpenedClient, g *providerGroup, e *groupEntry, result *Result, mu *sync.Mutex) error {
	originalRaw := getInstanceAttributes(g, e, result)
	priorRaw := originalRaw

	if needsUpgrade(opened, e) {
		upgraded, err := upgradeOne(ctx, opened, g, e, originalRaw, result, mu)
		if err != nil {
			return err
		}
		if upgraded != nil {
			priorRaw = upgraded
		}
	}

	resp, diags, err := opened.Client.ReadResource(ctx, provider.ReadResourceRequest{
		TypeName:     e.TypeName,
		CurrentState: priorRaw,
	})
	if err != nil {
		recordEntryFailed(e, fmt.Errorf("ReadResource: %w", err), result, mu)
		return err
	}
	if diags.HasError() {
		joined := joinDiagnostics(diags.Errors())
		recordEntryFailed(e, fmt.Errorf("provider error: %s", joined), result, mu)
		return errors.New(joined)
	}

	// Compare original stored bytes vs the final NewState. A
	// semantic-aware comparison waits for v1.7 (drift surfacing);
	// v1.6 only needs the change/no-change signal for counters.
	changed := !bytesEqualJSON(originalRaw, resp.NewState)

	mu.Lock()
	result.ResourcesChecked++
	if changed {
		result.ResourcesChanged++
		result.ChangedAddresses = append(result.ChangedAddresses, e.Address)
	}
	mu.Unlock()

	if resp.NewState != nil {
		setInstanceAttributes(g, e, resp.NewState, result)
		setInstanceSensitiveAttributes(g, e, resp.SensitiveAttributes, result)
	}
	return nil
}

// needsUpgrade decides whether the orchestrator must run
// UpgradeResourceState before reading the resource. The wire spec
// requires upgrade *whenever* the stored schema_version is lower
// than the live schema's Version; equal or higher means "do not
// touch", and any provider that asked for an upgrade when the
// versions already matched would consider the call a contract bug.
//
// When the schema is unavailable (e.g. the test's recordingClient
// returns &Schema{}), we can't tell, and we conservatively skip
// the upgrade. The provider's own ReadResource will surface a
// "schema version mismatch" diagnostic if there's a real problem.
func needsUpgrade(opened *OpenedClient, e *groupEntry) bool {
	if opened == nil || opened.Schema == nil {
		return false
	}
	rs, ok := opened.Schema.Resources[e.TypeName]
	if !ok || rs == nil {
		return false
	}
	return int64(e.SchemaVersion) < rs.Version
}

// upgradeOne issues a single UpgradeResourceState RPC, splices the
// upgraded attributes back into the parsed state, and bumps the
// instance's schema_version. Returns the upgraded raw bytes that
// the caller should feed into ReadResource as the new "prior";
// nil if the upgrade was a no-op or produced no bytes (the caller
// continues with the original prior).
//
// Errors and Error diagnostics are recorded against the entry the
// same way ReadResource failures are, so they aggregate into the
// same Errors slice the operator sees.
func upgradeOne(
	ctx context.Context,
	opened *OpenedClient,
	g *providerGroup,
	e *groupEntry,
	originalRaw []byte,
	result *Result,
	mu *sync.Mutex,
) ([]byte, error) {
	resp, diags, err := opened.Client.UpgradeResourceState(ctx, provider.UpgradeResourceStateRequest{
		TypeName: e.TypeName,
		Version:  int64(e.SchemaVersion),
		RawState: originalRaw,
	})
	if err != nil {
		recordEntryFailed(e, fmt.Errorf("UpgradeResourceState: %w", err), result, mu)
		return nil, err
	}
	if diags.HasError() {
		joined := joinDiagnostics(diags.Errors())
		recordEntryFailed(e, fmt.Errorf("provider upgrade error: %s", joined), result, mu)
		return nil, errors.New(joined)
	}
	if resp == nil || len(resp.UpgradedState) == 0 {
		// Some providers legally return no bytes (e.g. a no-op
		// upgrade ladder that already trusted the prior). Treat
		// it the same as "use the original"; we don't bump the
		// instance's schema_version in that case because we
		// haven't proven a successful upgrade.
		return nil, nil
	}

	// Splice the upgraded attributes back into the parsed state
	// and bump the recorded schema_version so subsequent runs
	// (and downstream consumers of the new state version) see the
	// migrated shape. Per the orchestrator's concurrency contract,
	// each group's goroutine owns its Instances[] slots
	// exclusively, so no lock is needed around the mutation.
	setInstanceAttributes(g, e, resp.UpgradedState, result)
	if r := result.parsedResource(g, e); r != nil {
		if liveVersion := liveSchemaVersion(opened, e.TypeName); liveVersion > 0 {
			r.Instances[e.InstanceIdx].SchemaVersion = int(liveVersion)
			e.SchemaVersion = int(liveVersion)
		}
	}

	return resp.UpgradedState, nil
}

// liveSchemaVersion looks up the provider's currently-published
// version for a resource type. Returns 0 when the schema is
// unavailable or the type is unknown — callers should treat
// "no information" as "don't change the recorded version".
func liveSchemaVersion(opened *OpenedClient, typeName string) int64 {
	if opened == nil || opened.Schema == nil {
		return 0
	}
	rs, ok := opened.Schema.Resources[typeName]
	if !ok || rs == nil {
		return 0
	}
	return rs.Version
}

// getInstanceAttributes fetches the prior attributes JSON for an
// entry. Wrapped in a helper so the refresh body reads cleanly even
// once the indirection grows for thread-safety considerations.
//
// The mutex argument from the caller is intentionally not threaded
// through here: each goroutine owns its own group, so different
// goroutines never read the same Resources[ri].Instances[ii] slot.
func getInstanceAttributes(g *providerGroup, e *groupEntry, result *Result) []byte {
	r := result.parsedResource(g, e)
	if r == nil {
		return nil
	}
	return []byte(r.Instances[e.InstanceIdx].Attributes)
}

// setInstanceAttributes writes the refreshed attributes back into
// the parsed state. Caller-supplied bytes must already be valid JSON;
// the orchestrator does not re-encode them.
func setInstanceAttributes(g *providerGroup, e *groupEntry, newRaw []byte, result *Result) {
	r := result.parsedResource(g, e)
	if r == nil {
		return
	}
	r.Instances[e.InstanceIdx].Attributes = json.RawMessage(newRaw)
}

// setInstanceSensitiveAttributes updates instances[].sensitive_attributes when
// the provider reports dynamically-sensitive paths at runtime.
//
// Static sensitivity (schema attribute Sensitive=true) is re-derived by
// Terraform on load, so it is always safe to leave an existing list unchanged.
// Callers should pass nil to preserve prior sensitive paths.
func setInstanceSensitiveAttributes(g *providerGroup, e *groupEntry, raw json.RawMessage, result *Result) {
	if raw == nil {
		return
	}
	r := result.parsedResource(g, e)
	if r == nil {
		return
	}
	r.Instances[e.InstanceIdx].SensitiveAttributes = raw
}

// parsedResource returns the *tfstate.Resource the entry references,
// or nil if the parsed state has been detached from result. (It
// never is during normal flow; the helper exists to make the
// intent of "go through result.parsed" explicit at call sites.)
func (r *Result) parsedResource(g *providerGroup, e *groupEntry) *tfstate.Resource {
	if r.parsed == nil {
		return nil
	}
	if e.ResourceIdx < 0 || e.ResourceIdx >= len(r.parsed.Resources) {
		return nil
	}
	return &r.parsed.Resources[e.ResourceIdx]
}

// commitRefreshedState bumps the parsed state's serial, re-marshals
// it to JSON, and writes a new state_version with source="refresh".
// Updates result.SerialAfter on success.
func commitRefreshedState(
	ctx context.Context,
	st *store.Store,
	parsed *tfstate.State,
	info *store.CurrentStateInfo,
	opts Options,
	result *Result,
) error {
	parsed.Serial = info.Serial + 1
	// Outputs is declared without omitempty; if Parse came from a
	// state with no outputs key, the field is nil and would re-emit
	// as `"outputs": null` — Terraform tolerates it but downstream
	// consumers may not. Promote nil to {} defensively.
	if parsed.Outputs == nil {
		parsed.Outputs = map[string]tfstate.Output{}
	}
	newRaw, err := json.Marshal(parsed)
	if err != nil {
		return fmt.Errorf("marshal refreshed state: %w", err)
	}
	if err := st.WriteState(ctx, opts.StateName, "", newRaw, "refresh", opts.Actor); err != nil {
		return fmt.Errorf("write refreshed state: %w", err)
	}
	newID, err := st.LookupStateVersionID(ctx, info.StateID, parsed.Serial)
	if err != nil {
		return fmt.Errorf("lookup new version id: %w", err)
	}
	result.SerialAfter = parsed.Serial
	result.newVersionID = newID
	return nil
}

// finalize stamps the run terminal and returns the final result.
// finalizeWithError is a sibling that records the run as 'failed'
// and propagates the failure to the Run caller (since this kind of
// failure is run-level, not per-resource).
func finalize(ctx context.Context, st *store.Store, result *Result, runID string) (*Result, error) {
	status := store.RefreshRunSucceeded
	var summary string
	if result.ResourcesFailed > 0 {
		status = store.RefreshRunFailed
		summary = fmt.Sprintf("%d resource(s) failed to refresh", result.ResourcesFailed)
	}
	result.Status = status
	result.FinishedAt = time.Now()

	in := store.FinishRefreshRunInput{
		Status:           status,
		ToVersionID:      result.newVersionID,
		ResourcesChecked: result.ResourcesChecked,
		ResourcesChanged: result.ResourcesChanged,
		ResourcesFailed:  result.ResourcesFailed,
		ErrorSummary:     summary,
	}
	if err := st.FinishRefreshRun(ctx, runID, in); err != nil {
		return result, fmt.Errorf("finish refresh run: %w", err)
	}
	return result, nil
}

func finalizeWithError(ctx context.Context, st *store.Store, result *Result, runID string, runErr error) (*Result, error) {
	result.Status = store.RefreshRunFailed
	result.FinishedAt = time.Now()
	in := store.FinishRefreshRunInput{
		Status:           store.RefreshRunFailed,
		ResourcesChecked: result.ResourcesChecked,
		ResourcesChanged: result.ResourcesChanged,
		ResourcesFailed:  result.ResourcesFailed,
		ErrorSummary:     truncateSummary(runErr.Error(), 500),
	}
	if err := st.FinishRefreshRun(ctx, runID, in); err != nil {
		// The run-level failure is the more interesting story; if
		// finish itself fails, surface both via errors.Join so the
		// CLI logs them together.
		return result, errors.Join(runErr, fmt.Errorf("finish refresh run: %w", err))
	}
	return result, runErr
}

func truncateSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func joinDiagnostics(d provider.Diagnostics) string {
	if len(d) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d))
	for _, x := range d {
		seg := x.Summary
		if x.Detail != "" {
			seg += ": " + x.Detail
		}
		parts = append(parts, seg)
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "; " + p
	}
	return out
}

// bytesEqualJSON returns true if the two byte slices are JSON-equal
// in the structural sense — same shape, same scalar values. It is
// implemented as a parse-then-compare rather than byte equality
// because providers freely vary JSON key ordering, and a refresh
// that only changes key order is not semantic drift.
//
// Empty inputs (one or both nil) are treated as equal because the
// "no prior state, no new state" path is not drift; they will only
// co-occur for a deleted-then-readded resource that round-trips
// through nil, which is itself a no-op outcome.
func bytesEqualJSON(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return jsonEqual(av, bv)
}

// jsonEqual compares two map[string]any-shaped JSON values for
// structural equality, ignoring key order. Unlike reflect.DeepEqual,
// it accepts that two maps with the same content but different
// internal hash orders are equal — which is exactly what we want
// for "did the cloud diverge from the prior state".
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, exists := bv[k]
			if !exists || !jsonEqual(v, bvv) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
