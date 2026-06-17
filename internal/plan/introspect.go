package plan

import (
	"encoding/json"
	"strings"
)

// Spec represents the predicted reservations required for a plan.
type Spec struct {
	WriteSet []string `json:"write_set"`
	ReadSet  []string `json:"read_set"`
}

// tfPlan is a partial representation of Terraform's plan JSON format.
type tfPlan struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Change  struct {
			Actions []string `json:"actions"`
		} `json:"change"`
	} `json:"resource_changes"`

	Configuration struct {
		RootModule map[string]any `json:"root_module"`
	} `json:"configuration"`
}

// Introspect reads the output of `terraform show -json` and extracts
// the disjoint Write-Set and Read-Set required to apply it.
func Introspect(planJSON []byte) (*Spec, error) {
	var parsed tfPlan
	if err := json.Unmarshal(planJSON, &parsed); err != nil {
		return nil, err
	}

	spec := &Spec{
		WriteSet: []string{},
		ReadSet:  []string{},
	}

	writeSetMap := make(map[string]bool)

	// 1. Extract Write-Set from ResourceChanges
	for _, rc := range parsed.ResourceChanges {
		isWrite := false
		for _, action := range rc.Change.Actions {
			if action != "no-op" && action != "read" {
				isWrite = true
				break
			}
		}
		if isWrite {
			spec.WriteSet = append(spec.WriteSet, rc.Address)
			writeSetMap[rc.Address] = true
		}
	}

	// 2. Extract Read-Set from the dependency graph expressions
	refs := extractReferences(parsed.Configuration.RootModule)

	readSetMap := make(map[string]bool)
	for _, ref := range refs {
		base := cleanReference(ref)
		if base != "" && !writeSetMap[base] {
			readSetMap[base] = true
		}
	}

	for k := range readSetMap {
		spec.ReadSet = append(spec.ReadSet, k)
	}

	return spec, nil
}

func extractReferences(node any) []string {
	var refs []string
	switch v := node.(type) {
	case map[string]any:
		if refArr, ok := v["references"].([]any); ok {
			for _, r := range refArr {
				if str, ok := r.(string); ok {
					refs = append(refs, str)
				}
			}
		}
		for _, child := range v {
			refs = append(refs, extractReferences(child)...)
		}
	case []any:
		for _, child := range v {
			refs = append(refs, extractReferences(child)...)
		}
	}
	return refs
}

func cleanReference(ref string) string {
	// Ignore variables, locals, terraform configs, etc.
	if strings.HasPrefix(ref, "var.") || strings.HasPrefix(ref, "local.") || strings.HasPrefix(ref, "each.") || strings.HasPrefix(ref, "count.") || strings.HasPrefix(ref, "path.") || strings.HasPrefix(ref, "terraform.") {
		return ""
	}
	return strings.Split(ref, ".")[0] + "." + strings.Split(ref, ".")[1]
}
