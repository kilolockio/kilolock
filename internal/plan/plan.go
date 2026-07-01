// Package plan parses Terraform's `terraform show -json plan.tfplan`
// output and derives the inputs v2 needs to drive a sliced apply:
// the write set (addresses with non-no-op actions), the static
// dependency graph (from the configuration block's reference
// expressions), the read set (write set + transitive dep closure),
// and the HCL footprint (every address the operator's HCL declares).
//
// The package is intentionally self-contained: no DB, no provider
// RPC, no Terraform binary invocation. parse.go does the JSON
// decode; the rest is pure-function transforms.
//
// Promoted from `_spike/v2-plan-introspection/main.go` (validated
// against terraform 1.13.x). When the spike's findings change the
// shape of any function here, update both this file and the spike
// NOTES.md so the design record stays current.
//
// The wire-format types are deliberately minimal: only fields v2b
// actually consumes are defined. Terraform's JSON plan format is
// documented at
// https://developer.hashicorp.com/terraform/internals/json-format
// and is stable since Terraform 0.12 (format_version 1.x).
package plan

import (
	"encoding/json"
	"time"
)

// File is the top-level shape of `terraform show -json plan.tfplan`.
// Only the subset v2b needs is declared; unknown fields are ignored
// by encoding/json so future Terraform additions don't break us.
type File struct {
	FormatVersion    string `json:"format_version"`
	TerraformVersion string `json:"terraform_version"`

	// Per-resource changes. The write set is derived from this.
	ResourceChanges []ResourceChange `json:"resource_changes"`

	// HCL-described addresses, expanded for count/for_each. Source of
	// the HCL footprint used to compute the state slice; necessary
	// because Terraform's re-plan inside the apply tmp dir will
	// declare any HCL-described resource missing from the slice as
	// `create`, which would diverge from the operator's plan.
	PlannedValues PlannedValues `json:"planned_values"`

	// HCL configuration with reference expressions. Source of the
	// static dependency graph (the configuration block carries the
	// resolved references between resources).
	Configuration Configuration `json:"configuration"`

	// Prior state attached to the plan; useful in apply-time checks
	// (does the trunk's serial match what we planned against?). Not
	// consumed in v2b but parsed so callers can inspect it without
	// having to redo the JSON decode.
	PriorState json.RawMessage `json:"prior_state,omitempty"`

	// Variables records every input variable terraform's planner
	// observed and used, regardless of where the value came from
	// (CLI -var=, TF_VAR_* env var, terraform.tfvars, *.auto.tfvars,
	// or -var-file). Each entry's Value is the variable's actual
	// HCL value rendered as JSON, so strings come out as `"foo"`,
	// numbers as `3`, objects as `{"k":"v"}` etc.
	//
	// This is terraform's idea of "the effective input set" — by
	// pinning it verbatim into the kl PlanSpec we make the
	// apply-time re-plan reproducible regardless of the operator's
	// shell environment at apply time. It's the canonical fix for
	// the env-var-ghost class of plan/apply drift.
	Variables map[string]PlanVariable `json:"variables,omitempty"`
}

// PlanVariable is one entry of File.Variables. Mirrors terraform's
// JSON schema for the top-level variables map; only the Value
// field is consumed by kl (we don't carry sensitive-flag
// metadata, descriptions, etc.).
type PlanVariable struct {
	Value json.RawMessage `json:"value"`
}

// ResourceChange is one row of resource_changes[] in the plan JSON.
//
// Address is canonical (e.g. `module.web.aws_instance.app[0]`).
// Module-qualified, count/for_each-expanded, ready to use as a
// reservation key.
type ResourceChange struct {
	Address       string `json:"address"`
	ModuleAddress string `json:"module_address,omitempty"`
	Mode          string `json:"mode"` // managed | data
	Type          string `json:"type"`
	Name          string `json:"name"`
	ProviderName  string `json:"provider_name"`
	Change        Change `json:"change"`
}

// Change carries the per-resource action vector + before/after
// snapshots. The Actions slice is what every write/read decision
// keys off; full action vocabulary:
//
//	["no-op"]             — present in plan, no work
//	["create"]            — new managed resource
//	["update"]            — in-place attribute change
//	["delete"]            — resource removed
//	["delete","create"]   — replace (forces-new attribute changed)
//	["read"]              — data-source refresh
//	["forget"]            — `removed { lifecycle { destroy = false } }` exit
//
// The shape is a slice (not a string) because Terraform encodes
// replaces as two-element vectors and may extend the vocabulary
// in future minor releases without breaking the schema.
type Change struct {
	Actions []string        `json:"actions"`
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
}

// PlannedValues mirrors `terraform show -json`'s planned_values
// block. We only need the address list (root + nested modules) for
// the HCL footprint; attribute values are out of scope here.
type PlannedValues struct {
	RootModule PlannedModule `json:"root_module"`
}

// PlannedModule is one node in the tree of planned values; modules
// nest recursively via child_modules. resources[] within a module
// each carry the fully-qualified address (so the module-walk in
// footprint.go can just concat them flat).
type PlannedModule struct {
	Resources    []PlannedResource `json:"resources,omitempty"`
	ChildModules []PlannedModule   `json:"child_modules,omitempty"`
}

// PlannedResource is the trimmed-down form of a resource in
// planned_values; we only need the canonical address.
type PlannedResource struct {
	Address      string `json:"address"`
	Mode         string `json:"mode"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	ProviderName string `json:"provider_name,omitempty"`
	// Attribute values omitted on purpose; their volume balloons
	// for large states and we never read them.
}

// Configuration is the configuration block from the plan JSON. The
// dependency-graph builder walks this recursively to collect every
// `references` array nested inside an `expressions` blob.
type Configuration struct {
	RootModule ConfigModule `json:"root_module"`
}

// ConfigModule is one node in the configuration tree. ModuleCalls
// addresses nest via the same recursive walk used in PlannedModule;
// the difference is that ConfigModule's resources still carry HCL
// expressions (with reference arrays), while PlannedModule's
// resources carry concrete planned attribute values.
type ConfigModule struct {
	Resources   []ConfigResource      `json:"resources,omitempty"`
	ModuleCalls map[string]ModuleCall `json:"module_calls,omitempty"`
}

// ModuleCall wraps a child ConfigModule. The map key is the call
// label (e.g. `web`); the walker prepends `module.web.` to every
// resource address in the child.
type ModuleCall struct {
	Module    ConfigModule `json:"module,omitempty"`
	DeclRange *SourceRange `json:"range,omitempty"`
}

// SourceRange is Terraform's source location envelope in plan JSON.
// ADR-0014 MVP uses only Filename to map resources to --file scopes.
type SourceRange struct {
	Filename string `json:"filename"`
}

// ConfigResource is one resource block as it appears in the
// configuration tree. Expressions is opaque JSON because Terraform's
// expression representations vary (objects, arrays, nested
// references); the dep-graph builder walks them recursively.
type ConfigResource struct {
	Address     string         `json:"address"`
	Mode        string         `json:"mode"`
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	DeclRange   *SourceRange   `json:"range,omitempty"`
	Expressions map[string]any `json:"expressions,omitempty"`
}

// ---------------------------------------------------------------------------
// PlanSpec — the v2 plan-spec output format. This is OUR descriptor
// that kl plan writes and kl apply consumes; it is
// NOT a Terraform plan file. The format is stable JSON (rooted at
// FormatVersion="1") so the apply orchestrator can rely on it.
// ---------------------------------------------------------------------------

// PlanSpec is the operator-facing summary of one kl plan
// invocation, written to disk by `kl plan` and read by
// `kl apply`.
//
// The field names are intentionally explicit so the file is
// readable as-is in a terminal or PR diff.
type PlanSpec struct {
	FormatVersion    string    `json:"format_version"`
	GeneratedAt      time.Time `json:"generated_at"`
	ConfigDir        string    `json:"config_dir"`
	TerraformVersion string    `json:"terraform_version"`

	// SourceSerial is the trunk state serial this plan was computed
	// against (when discoverable). Used by the apply orchestrator's
	// staleness guard to refuse applying a plan whose read-set inputs
	// changed since the plan was generated.
	//
	// Omitted when the plan was generated without a discoverable HTTP
	// backend (or when the backend was unreachable at plan time).
	SourceSerial *int64 `json:"source_serial,omitempty"`

	// StateName is the trunk state this plan applies to, derived
	// from the http backend's address (last path segment of e.g.
	// `http://…/v1/states/big-state`). Lets `kl apply` work
	// without an explicit `--state=…`. May be empty for plans
	// generated outside a `terraform init`-ed directory; in that
	// case apply requires `--state=…` to be passed explicitly.
	StateName string `json:"state_name,omitempty"`

	// ScopedFiles records file-level selectors passed to
	// `kl plan -f ...`. Paths are relative to ConfigDir and
	// slash-normalized for stable JSON output across OSes.
	//
	// Empty means "full config plan" (no file scope requested).
	ScopedFiles []string `json:"scoped_files,omitempty"`

	// StateEngine records backend-authored scope/slice metadata when the
	// plan used the state-engine protocol instead of a full trunk fetch.
	StateEngine *StateEnginePlanMetadata `json:"state_engine,omitempty"`

	// Per-action counters derived from resource_changes[]. Sum
	// equals len(ResourceChanges); the per-action breakdown is
	// what the apply UX prints in its preflight summary.
	PlanSummary PlanSummary `json:"plan_summary"`

	// Sorted, canonical addresses.
	WriteSet     []string `json:"write_set"`
	ReadSet      []string `json:"read_set"`
	HCLFootprint []string `json:"hcl_footprint"`

	// Predicted reservations the apply orchestrator will acquire.
	// Derived from WriteSet (mode='write') ∪ (ReadSet \ WriteSet)
	// (mode='read'). Sorted by (address, mode) for stable output.
	Reservations []PlanReservation `json:"reservations"`

	// Static dependency edges harvested from the configuration
	// block. Useful for debugging "why is X in my read set?".
	DependencyEdges []DependencyEdge `json:"dependency_edges,omitempty"`

	// Variables records every input variable terraform's planner
	// observed and used, regardless of source: CLI --var=, TF_VAR_*
	// env vars, terraform.tfvars, *.auto.tfvars, -var-file=. Values
	// are JSON-encoded so HCL types round-trip faithfully: a string
	// is `"v2"`, a number is `3`, an object is `{"env":"prod"}`.
	// `kl apply` replays them verbatim as `-var=NAME=VALUE`,
	// which terraform parses as HCL (JSON is a subset).
	//
	// Pinning the effective input set here is what closes the
	// env-var-ghost gap from the v2d shakedown: the apply-time
	// re-plan inside the tmp dir sees the same variable values
	// the original plan did, regardless of what env vars happen
	// to live in the apply shell.
	//
	// omitempty so plans generated with --no-pin-vars (and the
	// legacy v1 format that predated this field) round-trip
	// without an explicit empty map in the JSON.
	Variables map[string]json.RawMessage `json:"variables,omitempty"`
}

type StateEnginePlanMetadata struct {
	Mode                   string   `json:"mode,omitempty"`
	DiscoveryEngine        string   `json:"discovery_engine,omitempty"`
	FallbackReason         string   `json:"fallback_reason,omitempty"`
	FetchAddresses         []string `json:"fetch_addresses,omitempty"`
	WriteAddresses         []string `json:"write_addresses,omitempty"`
	ConfigRequiredNodes    []string `json:"config_required_nodes,omitempty"`
	RemovedConfigNodes     []string `json:"removed_config_nodes,omitempty"`
	MissingFromState       []string `json:"missing_from_state,omitempty"`
	UndeployedCandidates   []string `json:"undeployed_candidates,omitempty"`
	UnknownMissing         []string `json:"unknown_missing_from_state,omitempty"`
	Confidence             string   `json:"confidence,omitempty"`
	Notes                  []string `json:"notes,omitempty"`
	ResolveDurationMs      int64    `json:"resolve_duration_ms,omitempty"`
	ExpandDurationMs       int64    `json:"expand_duration_ms,omitempty"`
	SliceFetchDurationMs   int64    `json:"slice_fetch_duration_ms,omitempty"`
	SliceResourceCount     int      `json:"slice_resource_count,omitempty"`
	GraphCacheHit          bool     `json:"graph_cache_hit,omitempty"`
	RealizedResourceCount  int      `json:"realized_resource_count,omitempty"`
	DependencyEdgeCount    int      `json:"dependency_edge_count,omitempty"`
	InventoryScanCount     int      `json:"inventory_scan_count,omitempty"`
	WalkedNodeCount        int      `json:"walked_node_count,omitempty"`
	ConfigNodeCount        int      `json:"config_node_count,omitempty"`
	ModuleSelectorCount    int      `json:"module_selector_count,omitempty"`
	FetchAddressCount      int      `json:"fetch_address_count,omitempty"`
	WriteAddressCount      int      `json:"write_address_count,omitempty"`
	ReadAddressCount       int      `json:"read_address_count,omitempty"`
	ServerExpandMs         int64    `json:"server_expand_duration_ms,omitempty"`
	SliceRequestedCount    int      `json:"slice_requested_count,omitempty"`
	SliceMaterializedCount int      `json:"slice_materialized_count,omitempty"`
	ServerSliceMs          int64    `json:"server_slice_duration_ms,omitempty"`
	SliceBytes             int      `json:"slice_bytes,omitempty"`
	FullStateBytes         int      `json:"full_state_bytes,omitempty"`
}

// PlanSummary is the action-counter breakdown.
type PlanSummary struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Delete  int `json:"delete"`
	Replace int `json:"replace"`
	Read    int `json:"read"`
	NoOp    int `json:"no_op"`
	Forget  int `json:"forget"`
	Total   int `json:"total"`
}

// PlanReservation is one entry in PlanSpec.Reservations.
type PlanReservation struct {
	Address string `json:"address"`
	Mode    string `json:"mode"` // "read" | "write"
}

// DependencyEdge is from → to in the static configuration dep graph.
type DependencyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// CurrentSpecFormatVersion is the value PlanSpec.FormatVersion is
// stamped with on write. Bumping this version is a breaking change
// for any consumer (kl apply, external tooling) and must
// land alongside a parser that supports both old and new.
const CurrentSpecFormatVersion = "1"
