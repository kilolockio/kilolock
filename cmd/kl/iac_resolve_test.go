package main

import "testing"

func TestResolveIACBinaryDefaults(t *testing.T) {
	got, err := resolveIACBinary("", "", "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "terraform" {
		t.Fatalf("got %q, want terraform", got)
	}
}
