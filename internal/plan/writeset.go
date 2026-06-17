package plan

import "sort"

// Action enumerates the possible action vectors a ResourceChange can
// carry. Terraform encodes replaces as a two-element slice; this
// type collapses the vocabulary to a single value per resource for
// counter/grouping use.
type Action string

const (
	ActionNoop    Action = "no-op"
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionDelete  Action = "delete"
	ActionReplace Action = "replace" // delete+create
	ActionRead    Action = "read"
	ActionForget  Action = "forget"
	ActionUnknown Action = "unknown"
)

// IsWrite reports whether an action mutates state. Reads (data-source
// refreshes) and no-ops do not; everything else does.
//
// 'forget' is a write here because while it doesn't call any provider
// RPCs it DOES remove the resource from state (`terraform state rm`
// at apply time). The orchestrator must reserve the address to
// prevent a concurrent apply from observing the row mid-forget.
func (a Action) IsWrite() bool {
	switch a {
	case ActionCreate, ActionUpdate, ActionDelete, ActionReplace, ActionForget:
		return true
	}
	return false
}

// ClassifyChange collapses a Change.Actions slice to a single Action.
// The mapping is:
//
//	[]            → unknown (defensive: every real ResourceChange has actions)
//	["no-op"]     → no-op
//	["create"]    → create
//	["update"]    → update
//	["delete"]    → delete
//	["read"]      → read
//	["forget"]    → forget
//	["delete","create"] | ["create","delete"] → replace
//	anything else → unknown
//
// "unknown" is reserved for future Terraform vocabulary additions;
// the orchestrator treats it as "fail loud" so a new action class
// can't sneak through as a no-op.
func ClassifyChange(c Change) Action {
	switch len(c.Actions) {
	case 0:
		return ActionUnknown
	case 1:
		a := Action(c.Actions[0])
		switch a {
		case ActionNoop, ActionCreate, ActionUpdate, ActionDelete, ActionRead, ActionForget:
			return a
		}
		return ActionUnknown
	case 2:
		// Replace is encoded as [delete, create] in the current
		// Terraform plan format. Accept the reverse ordering too
		// to be tolerant of any future schema variation.
		first, second := Action(c.Actions[0]), Action(c.Actions[1])
		if (first == ActionDelete && second == ActionCreate) ||
			(first == ActionCreate && second == ActionDelete) {
			return ActionReplace
		}
		return ActionUnknown
	}
	return ActionUnknown
}

// SummarizeActions counts each action across a slice of
// ResourceChanges. The Total field equals the input length and is
// surfaced so callers don't have to recompute it.
//
// Categories with zero count are still present in the struct (they
// just hold 0). This is intentional — the plan-summary JSON object
// has stable shape regardless of which actions appeared in the plan.
func SummarizeActions(changes []ResourceChange) PlanSummary {
	var s PlanSummary
	for _, c := range changes {
		s.Total++
		switch ClassifyChange(c.Change) {
		case ActionCreate:
			s.Create++
		case ActionUpdate:
			s.Update++
		case ActionDelete:
			s.Delete++
		case ActionReplace:
			s.Replace++
		case ActionRead:
			s.Read++
		case ActionNoop:
			s.NoOp++
		case ActionForget:
			s.Forget++
		}
	}
	return s
}

// ExtractWriteSet returns the canonical addresses of every resource
// whose action is a write (per IsWrite). The result is sorted
// lexicographically so the CLI output, the PlanSpec JSON, and any
// downstream diff against a previous plan are byte-stable.
//
// Addresses are returned as Terraform writes them in resource_changes
// — already fully-qualified (module.x.aws_instance.app[0]). No
// normalization is done here; callers should treat the strings as
// opaque, matched only by equality.
func ExtractWriteSet(f *File) []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.ResourceChanges))
	for _, rc := range f.ResourceChanges {
		if ClassifyChange(rc.Change).IsWrite() {
			out = append(out, rc.Address)
		}
	}
	sort.Strings(out)
	return out
}

// ExtractReadActionSet returns addresses whose action is exactly
// "read" (data-source refresh). These are NOT writes — they don't
// touch state — but the orchestrator may want to surface them
// separately (e.g. as data-source reservations in a future class).
//
// Kept separate from the dep-graph "read set" computed in
// depgraph.go. Two different concepts, intentionally named so the
// distinction is hard to miss:
//
//   - read_set (depgraph.go) = write_set ∪ static dep closure
//   - read_action_set (here) = resources whose action == "read"
//
// In v2b we don't emit the read_action_set into PlanSpec because no
// downstream consumer needs it yet, but the helper is here for v2c
// to use when it wires data-source reservations.
func ExtractReadActionSet(f *File) []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, 4)
	for _, rc := range f.ResourceChanges {
		if ClassifyChange(rc.Change) == ActionRead {
			out = append(out, rc.Address)
		}
	}
	sort.Strings(out)
	return out
}
