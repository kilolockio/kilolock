package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// (imports above intentionally minimal; mergeVariables uses
// encoding/json's RawMessage type.)

// SpecBuildInput carries everything BuildSpec needs that isn't
// already on the parsed plan File. Separating it from File keeps
// the parse path pure (no I/O, no clock) and gives the caller a
// hook to inject test values.
type SpecBuildInput struct {
	ConfigDir   string
	GeneratedAt time.Time

	// StateName, when non-empty, is stamped into spec.StateName
	// so `kl apply` can default its --state= argument to
	// it. Typically derived from DiscoverBackend(configDir).
	StateName string

	// SourceSerial, when non-nil, is stamped into spec.SourceSerial
	// so apply can validate the plan's read-set against the current
	// trunk before running terraform.
	SourceSerial *int64

	// ExplicitVars is the set of --var=NAME=VALUE pairs the
	// operator passed on the command line. Values are
	// JSON-encoded (strings as `"foo"`, numbers as `3`, etc.).
	// BuildSpec merges these on top of File.Variables; the
	// operator's explicit overrides win over what terraform
	// observed from other sources for the same name.
	ExplicitVars map[string]json.RawMessage

	// PinAllVars controls whether BuildSpec copies File.Variables
	// (the full effective input set terraform's planner used)
	// into the spec. Default behaviour from cmd_plan is true;
	// the --no-pin-vars escape hatch flips it to false, which
	// makes ExplicitVars the only source. Use false when an
	// operator considers some of their plan-time variables too
	// sensitive to land in a spec file that may end up in PR
	// review or CI artifacts.
	PinAllVars bool
}

// BuildSpec assembles a PlanSpec from a parsed plan File and the
// caller-supplied build context. Pure function — no I/O, no clock,
// no environment lookup. The output is what `kl plan`
// serializes to kl-plan.json.
//
// Note: callers must NOT pass File pointers shared across goroutines
// while mutating them; BuildSpec reads the File concurrently with no
// internal locking. In practice plan parsing is per-invocation and
// this is a non-issue, but worth pinning explicitly.
func BuildSpec(f *File, in SpecBuildInput) *PlanSpec {
	if f == nil {
		return &PlanSpec{FormatVersion: CurrentSpecFormatVersion}
	}
	writeSet := ExtractWriteSet(f)
	g := BuildDepGraph(f)
	readSet := CloseReadSet(writeSet, g)
	footprint := ExtractHCLFootprint(f)

	return &PlanSpec{
		FormatVersion:    CurrentSpecFormatVersion,
		GeneratedAt:      in.GeneratedAt,
		ConfigDir:        in.ConfigDir,
		StateName:        in.StateName,
		SourceSerial:     in.SourceSerial,
		TerraformVersion: f.TerraformVersion,
		PlanSummary:      SummarizeActions(f.ResourceChanges),
		WriteSet:         writeSet,
		ReadSet:          readSet,
		HCLFootprint:     footprint,
		Reservations:     buildReservations(writeSet, readSet),
		DependencyEdges:  g.Edges(),
		Variables:        mergeVariables(f.Variables, in.ExplicitVars, in.PinAllVars),
	}
}

// mergeVariables produces the spec.Variables payload by combining
// the two sources of variable values:
//
//   - terraform's effective input set (file.Variables), copied
//     only when pinAll is true. This is the cure for env-var ghost.
//   - the operator's explicit --var=NAME=VALUE overrides.
//
// Explicit overrides win over the terraform-observed value for
// the same name. Both are JSON-encoded RawMessage so the merged
// map is uniformly typed.
//
// The result is nil when both inputs are empty so MarshalSpec's
// omitempty keeps the JSON field absent on plans with no
// variables (a clean byte-stable round trip).
func mergeVariables(observed map[string]PlanVariable, explicit map[string]json.RawMessage, pinAll bool) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if pinAll {
		for k, v := range observed {
			if len(v.Value) == 0 {
				continue
			}
			cp := make(json.RawMessage, len(v.Value))
			copy(cp, v.Value)
			out[k] = cp
		}
	}
	for k, v := range explicit {
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		out[k] = cp
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildReservations projects the (write_set, read_set) pair into the
// reservation list the apply orchestrator will hand to
// Store.AcquireReservations. Every write_set address becomes
// mode='write'; every read_set\write_set address becomes mode='read'.
// Sorted by (address, mode); for any given address there's exactly
// one entry, so the sort is effectively by address.
func buildReservations(writeSet, readSet []string) []PlanReservation {
	writes := map[string]struct{}{}
	for _, a := range writeSet {
		writes[a] = struct{}{}
	}
	out := make([]PlanReservation, 0, len(readSet))
	for _, a := range readSet {
		mode := "read"
		if _, w := writes[a]; w {
			mode = "write"
		}
		out = append(out, PlanReservation{Address: a, Mode: mode})
	}
	// readSet was already sorted by CloseReadSet, so the slice is
	// stable by address; modes are 1:1 with addresses (no duplicates
	// because a single address can't be both read and write — write
	// wins above). Re-sort defensively in case CloseReadSet ordering
	// ever changes.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Address != out[j].Address {
			return out[i].Address < out[j].Address
		}
		return out[i].Mode < out[j].Mode
	})
	return out
}

// MarshalSpec serializes a PlanSpec into the operator-readable JSON
// form. Indent-formatted because operators read the file directly
// in PR diffs; the size cost vs. compact is negligible (plans rarely
// exceed a few thousand addresses).
//
// Stamps FormatVersion to CurrentSpecFormatVersion if it's empty, so
// callers that constructed the spec by hand still produce valid
// output.
func MarshalSpec(s *PlanSpec) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("MarshalSpec: spec is nil")
	}
	if s.FormatVersion == "" {
		s.FormatVersion = CurrentSpecFormatVersion
	}
	return json.MarshalIndent(s, "", "  ")
}

// UnmarshalSpec is the inverse, used by `kl apply` to read
// the spec back. Rejects format versions other than CurrentSpec-
// FormatVersion explicitly so a forward-incompatible file produces
// a clear error rather than a partial decode.
func UnmarshalSpec(b []byte) (*PlanSpec, error) {
	var s PlanSpec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode plan spec: %w", err)
	}
	if s.FormatVersion == "" {
		return nil, fmt.Errorf("decode plan spec: missing format_version")
	}
	if s.FormatVersion != CurrentSpecFormatVersion {
		return nil, fmt.Errorf("decode plan spec: unsupported format_version %q (this build understands %q)",
			s.FormatVersion, CurrentSpecFormatVersion)
	}
	if len(s.Reservations) == 0 && (len(s.WriteSet) > 0 || len(s.ReadSet) > 0) {
		s.Reservations = buildReservations(s.WriteSet, s.ReadSet)
	}
	return &s, nil
}
