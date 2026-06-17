package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These tests do not launch any provider binary; they verify
// Launch's input validation and the Diagnostics helper methods.
// The end-to-end tests live in launch_integration_test.go and
// require -tags=integration.

func TestLaunch_EmptyBinary(t *testing.T) {
	_, err := Launch(context.Background(), "", LaunchOptions{})
	if err == nil {
		t.Fatal("expected error for empty binary path, got nil")
	}
	if !strings.Contains(err.Error(), "empty binary path") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestLaunch_NonexistentBinary(t *testing.T) {
	_, err := Launch(context.Background(), "/this/path/definitely/does/not/exist", LaunchOptions{})
	if err == nil {
		t.Fatal("expected stat error, got nil")
	}
}

func TestLaunch_DirectoryNotBinary(t *testing.T) {
	dir := t.TempDir()
	_, err := Launch(context.Background(), dir, LaunchOptions{})
	if err == nil {
		t.Fatal("expected error when path is a directory")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDiagnostics_HasError(t *testing.T) {
	cases := []struct {
		name string
		d    Diagnostics
		want bool
	}{
		{"empty", nil, false},
		{"only warnings", Diagnostics{
			{Severity: SeverityWarning, Summary: "w"},
		}, false},
		{"mixed", Diagnostics{
			{Severity: SeverityWarning, Summary: "w"},
			{Severity: SeverityError, Summary: "e"},
		}, true},
		{"only error", Diagnostics{
			{Severity: SeverityError, Summary: "e"},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.HasError(); got != tc.want {
				t.Errorf("HasError: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiagnostics_PartitionByCategory(t *testing.T) {
	d := Diagnostics{
		{Severity: SeverityWarning, Summary: "w1"},
		{Severity: SeverityError, Summary: "e1"},
		{Severity: SeverityWarning, Summary: "w2"},
		{Severity: SeverityError, Summary: "e2"},
	}
	if got := len(d.Errors()); got != 2 {
		t.Errorf("Errors(): want 2, got %d", got)
	}
	if got := len(d.Warnings()); got != 2 {
		t.Errorf("Warnings(): want 2, got %d", got)
	}
}

func TestSeverity_String(t *testing.T) {
	cases := map[Severity]string{
		SeverityInvalid: "INVALID",
		SeverityError:   "ERROR",
		SeverityWarning: "WARNING",
		Severity(99):    "INVALID",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Severity(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestNestingMode_String(t *testing.T) {
	cases := map[NestingMode]string{
		NestingInvalid:  "invalid",
		NestingSingle:   "single",
		NestingList:     "list",
		NestingSet:      "set",
		NestingMap:      "map",
		NestingGroup:    "group",
		NestingMode(99): "invalid",
	}
	for n, want := range cases {
		if got := n.String(); got != want {
			t.Errorf("NestingMode(%d).String() = %q, want %q", n, got, want)
		}
	}
}

func TestErrProviderClosed_Identity(t *testing.T) {
	wrapped := errors.New("wrapped")
	if errors.Is(wrapped, ErrProviderClosed) {
		t.Fatal("sentinel should not match an unrelated error")
	}
	if !errors.Is(ErrProviderClosed, ErrProviderClosed) {
		t.Fatal("sentinel should match itself")
	}
}
