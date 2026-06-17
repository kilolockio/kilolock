// Package apply implements the v2 sliced-apply orchestrator:
// the workflow that ties v2a's reservations and v2b's plan spec to
// a real `terraform apply` invocation inside a backend-rewritten
// tmp working directory, then commits the resulting row-level
// changes back into the trunk via the v1 lifecycle write path.
//
// Scope of the first cut (v2c-1):
//
//   - Acquire reservations on (write_set, read_set\write_set) for
//     the apply's duration.
//   - Set up a tmp working directory: copy HCL, strip the operator's
//     backend block, inject `terraform { backend "local" {} }`,
//     materialize the trunk-slice as `terraform.tfstate`.
//   - Run `terraform init && terraform apply -auto-approve` inside
//     the tmp dir.
//   - Validate that Terraform did not mutate any resource outside
//     the predicted write set (any extra mutation is a fail-loud
//     bug per ADR 0007).
//   - Merge the post-apply state back into the trunk by overlaying
//     write_set rows on a copy of the current trunk and routing
//     the merged blob through the existing Store.WriteState path.
//
// Shipped follow-up hardening (v2c-2, v2c-3):
//
//   - Plan staleness guard — refuse to apply if the trunk's serial
//     advanced past the spec's source_serial on any read-set
//     address.
//   - Heartbeat goroutine — extend lease while a long apply runs.
//   - Re-plan validation — run `terraform plan` inside the apply
//     dir and assert the resulting write_set equals the predicted
//     one before mutating anything.
//   - `kl apply abort` operator escape hatch.
//
// Together these harden the happy path into something we can safely
// demo and operate: long applies keep their leases alive, stale plans
// fail closed before commit, and operators have a break-glass abort.
package apply

import (
	"time"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/pkg/store"
)

// Options configures a single Run invocation. All fields are
// required unless noted.
type Options struct {
	// Spec is the parsed plan-spec.json (output of `kl plan`).
	// The orchestrator derives write_set, reservations, and HCL
	// footprint from it.
	Spec *plan.PlanSpec

	// StateName names the trunk we're applying against. Currently
	// supplied via CLI flag; future versions will extract from the
	// HCL backend block during `kl plan` and stamp into the
	// spec.
	StateName string

	// Actor is the audit identity (apply_runs.actor +
	// resource_reservations.holder). Free-form; tools that wrap
	// this typically set it to "$USER@$HOST" or a service name.
	Actor string

	// TerraformBin is the terraform binary path. Empty falls back
	// to "terraform" on $PATH.
	TerraformBin string

	// Lease is the per-reservation lease duration. Default 15 min.
	// The orchestrator renews this lease on a heartbeat while
	// terraform is running; the value still defines the safety
	// window if the process is killed or heartbeats stop.
	Lease time.Duration

	// WorkDir is the operator's HCL directory. The orchestrator
	// reads it for the .tf files; does not write back to it.
	// Equal to Spec.ConfigDir when invoked from the CLI;
	// overridable for tests where the spec was hand-crafted.
	WorkDir string

	// SkipCleanup leaves the apply tmp directory around when set,
	// so operators can inspect it during debugging. Default false.
	SkipCleanup bool

	// NoColor disables ANSI color in terraform's output. Tests set
	// this to true so the captured logs are diffable; humans
	// usually want it false.
	NoColor bool

	// WaitForReservations is the total time the orchestrator is
	// allowed to spend waiting for conflicting reservations to
	// clear before giving up. 0 = no wait (fail fast on the first
	// reservation conflict, matching the original v2 semantics).
	//
	// The wait is bounded by min(WaitForReservations, context
	// deadline). On wait timeout the orchestrator surfaces the
	// LAST observed conflict set so the operator sees what was
	// blocking them.
	//
	// Default 0 in the struct keeps existing tests deterministic;
	// the CLI sets a 5-minute default that operators rarely have
	// to override.
	WaitForReservations time.Duration

	// ReservationWaitNotifier is invoked once for each wait
	// iteration so callers can render progress to the operator.
	// Optional: nil means "wait silently". The CLI installs a
	// stderr renderer that throttles to one line every 5s; tests
	// can install a recorder.
	//
	// The callback is invoked synchronously on the orchestrator
	// goroutine — keep it cheap.
	ReservationWaitNotifier func(WaitEvent)
}

// WaitEvent carries the per-iteration progress signal for the
// reservation-wait loop. The orchestrator emits one of these every
// time a conflict observation happens, so callers can render
// "waiting Xs/Ys, blocked by Z" without re-querying the store.
type WaitEvent struct {
	// Iteration is 1-based: the first conflict observation is
	// iteration 1, the second is 2, etc.
	Iteration int

	// Elapsed is wall-clock time since the wait began.
	Elapsed time.Duration

	// Remaining is the wait budget left after this iteration's
	// sleep. Zero or negative means the next attempt is the last.
	Remaining time.Duration

	// Conflicts is the addresses currently holding the orchestrator
	// up, mirrored from the last AcquireReservations error. Sorted
	// by address.
	Conflicts []store.ActiveReservation

	// NextRetryIn is how long the orchestrator will sleep before
	// retrying. Callers SHOULD use this to throttle their output —
	// printing every iteration would be too chatty under 1s
	// intervals.
	NextRetryIn time.Duration
}

// Result is the success-path output of Run.
type Result struct {
	ApplyID         string
	StateName       string
	StateID         string
	SourceSerial    int64
	CommittedSerial int64
	NewVersionID    string

	// Counters mirror the apply_runs row.
	ResourcesPlanned int // |write_set|
	ResourcesApplied int // number of write_set rows that actually changed in the merge
	ResourcesFailed  int // post-apply rows with non-empty diagnostics; v2c-1 always 0

	// Sorted addresses Terraform actually mutated in the tmp-dir
	// state. Should equal Spec.WriteSet on the happy path. On
	// failure (writes ⊄ write_set) the orchestrator aborts before
	// committing and returns the violating subset in this field.
	AppliedAddresses []string

	StartedAt  time.Time
	FinishedAt time.Time

	// TempDir is the path of the apply working directory. Always
	// populated for observability even when SkipCleanup is false
	// (in which case the directory was removed by the time Result
	// is returned).
	TempDir string
}

// DefaultLease is the per-reservation lease for `kl apply`
// runs that don't override it. The heartbeat renews it during a
// healthy run; the duration mainly controls how quickly a wedged or
// killed apply can be reclaimed by a competing writer.
const DefaultLease = 15 * time.Minute
