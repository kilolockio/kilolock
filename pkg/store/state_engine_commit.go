package store

import (
	"encoding/json"

	"github.com/kilolockio/kilolock/internal/tfstate"
)

// StateEngineDeltaCommit is the narrow state-engine commit payload used by
// native KL apply. It carries only the selected resource instances/groups that
// belong to the write_set, explicit delete intent for addresses absent from the
// payload, plus the full output map and top-level metadata needed to produce
// the next canonical state version.
type StateEngineDeltaCommit struct {
	TerraformVersion string                    `json:"terraform_version,omitempty"`
	Lineage          string                    `json:"lineage,omitempty"`
	OutputWrites     map[string]tfstate.Output `json:"output_writes,omitempty"`
	OutputDeleteSet  []string                  `json:"output_delete_set,omitempty"`
	CheckResults     json.RawMessage           `json:"check_results,omitempty"`
	Resources        []tfstate.Resource        `json:"resources,omitempty"`
	WriteSet         []string                  `json:"write_set,omitempty"`
	DeleteSet        []string                  `json:"delete_set,omitempty"`
}
