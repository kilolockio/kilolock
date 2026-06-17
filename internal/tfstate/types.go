// Package tfstate models the on-disk Terraform v4 state format and
// computes canonical resource addresses. It is intentionally a partial
// model: only the fields Kilolock needs in order to normalize state
// into the graph schema are typed. Everything else round-trips through
// state_versions.raw_state untouched.
//
// References:
//
//   - https://developer.hashicorp.com/terraform/internals/state-format
//   - https://github.com/hashicorp/terraform/blob/main/internal/states/statefile
package tfstate

import (
	"encoding/json"
	"fmt"
)

// State is the top-level Terraform v4 state.
type State struct {
	Version          int               `json:"version"`
	TerraformVersion string            `json:"terraform_version"`
	Serial           int64             `json:"serial"`
	Lineage          string            `json:"lineage"`
	Outputs          map[string]Output `json:"outputs"`
	Resources        []Resource        `json:"resources"`
	CheckResults     json.RawMessage   `json:"check_results,omitempty"`
}

// Output is a single state-level output value.
type Output struct {
	// Value is the output value as JSON. Stored verbatim.
	Value json.RawMessage `json:"value"`
	// Type is Terraform's cty type encoding, also stored verbatim.
	Type json.RawMessage `json:"type"`
	// Sensitive is true if Terraform flagged the output as sensitive.
	Sensitive bool `json:"sensitive,omitempty"`
}

// Resource is a Terraform resource group: one type+name+module triple,
// possibly with multiple instances if count or for_each is used.
type Resource struct {
	// Mode is "managed" (resource) or "data" (data source).
	Mode string `json:"mode"`
	// Type is e.g. "aws_instance".
	Type string `json:"type"`
	// Name is the HCL local name.
	Name string `json:"name"`
	// Provider is the full provider reference, e.g.
	// `provider["registry.terraform.io/hashicorp/aws"]`.
	Provider string `json:"provider"`
	// Module is the module path, e.g. "module.vpc.module.private".
	// Empty string for the root module.
	Module string `json:"module,omitempty"`
	// Instances are the concrete resource instances. Always at least
	// one in a well-formed state.
	Instances []ResourceInstance `json:"instances"`
}

// ResourceInstance is one concrete row in the resource graph: the result
// of expanding count or for_each. State without count or for_each has a
// single instance with IndexKey = nil.
type ResourceInstance struct {
	// SchemaVersion is the provider's internal versioning.
	SchemaVersion int `json:"schema_version,omitempty"`

	// Attributes is the full attribute tree as provided by the provider.
	// Stored verbatim as JSONB on the row.
	Attributes json.RawMessage `json:"attributes,omitempty"`

	// SensitiveAttributes is a list of attribute paths flagged sensitive.
	// Terraform writes this as a list of cty path encodings; we preserve
	// it verbatim.
	SensitiveAttributes json.RawMessage `json:"sensitive_attributes,omitempty"`

	// Dependencies is the list of resource addresses this instance
	// depends on. Each entry is an address string (e.g. "aws_vpc.main"
	// without an index, or "aws_instance.web[0]" with).
	Dependencies []string `json:"dependencies,omitempty"`

	// IndexKey is the count index (int) or for_each key (string), or
	// nil for resources without count or for_each. We accept both JSON
	// numbers and strings.
	IndexKey json.RawMessage `json:"index_key,omitempty"`

	// CreateBeforeDestroy mirrors the lifecycle option.
	CreateBeforeDestroy bool `json:"create_before_destroy,omitempty"`

	// Tainted indicates the instance is marked for replacement.
	Tainted bool `json:"tainted,omitempty"`

	// Private is provider-internal opaque data.
	Private string `json:"private,omitempty"`

	// Status is occasionally set by Terraform during half-applied state.
	Status string `json:"status,omitempty"`
}

// Parse parses a Terraform v4 state JSON document.
//
// The function accepts only version 4 states. Earlier versions are not
// supported by v0 of Kilolock; users on Terraform 0.11 or older must
// upgrade their state with `terraform init` first.
func Parse(raw []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.Version != 4 {
		return nil, fmt.Errorf("state version %d is not supported (expected 4)", s.Version)
	}
	return &s, nil
}

// EmptyState returns a minimal Terraform v4 state suitable for genesis
// flows where no prior trunk exists yet.
func EmptyState(lineage string) *State {
	return &State{
		Version:          4,
		TerraformVersion: "0.0.0",
		Serial:           0,
		Lineage:          lineage,
		Outputs:          map[string]Output{},
		Resources:        []Resource{},
	}
}

// EmptyStateBytes marshals a minimal Terraform v4 empty state document.
func EmptyStateBytes(lineage string) ([]byte, error) {
	raw, err := json.Marshal(EmptyState(lineage))
	if err != nil {
		return nil, fmt.Errorf("marshal empty state: %w", err)
	}
	return raw, nil
}

// IndexKind reports how this instance is indexed.
type IndexKind int

const (
	IndexNone IndexKind = iota
	IndexInt
	IndexString
)

func (k IndexKind) String() string {
	switch k {
	case IndexInt:
		return "int"
	case IndexString:
		return "string"
	default:
		return "none"
	}
}

// DecodeIndex inspects the IndexKey JSON token and returns its kind and a
// stringified value suitable for storage. A nil or empty IndexKey decodes
// as IndexNone with an empty value.
func (ri ResourceInstance) DecodeIndex() (IndexKind, string, error) {
	if len(ri.IndexKey) == 0 || string(ri.IndexKey) == "null" {
		return IndexNone, "", nil
	}

	// Try as integer first; fall back to string. Anything else is an error.
	var asInt json.Number
	if err := json.Unmarshal(ri.IndexKey, &asInt); err == nil {
		// json.Number accepts both ints and floats; require integer here.
		if _, err := asInt.Int64(); err == nil {
			return IndexInt, asInt.String(), nil
		}
	}

	var asString string
	if err := json.Unmarshal(ri.IndexKey, &asString); err == nil {
		return IndexString, asString, nil
	}

	return IndexNone, "", fmt.Errorf("unsupported index_key value %s", string(ri.IndexKey))
}
