package main

import "testing"

// TestValidImportSource gates the closed set of provenance strings
// the import subcommand accepts. The current_resource_drift view
// filters on state_versions.source = 'refresh'; accepting arbitrary
// strings here would silently disable that view for the imported
// rows, so the allowlist is part of the contract.
func TestValidImportSource(t *testing.T) {
	allowed := []string{"import", "apply", "refresh", "unknown"}
	for _, s := range allowed {
		if !validImportSource(s) {
			t.Errorf("validImportSource(%q): got false, want true", s)
		}
	}
	rejected := []string{"", "IMPORT", "drift", "Apply", " refresh", "refresh "}
	for _, s := range rejected {
		if validImportSource(s) {
			t.Errorf("validImportSource(%q): got true, want false", s)
		}
	}
}
