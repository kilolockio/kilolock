package store

import (
	"errors"
	"testing"
)

// TestValidateTagName_AcceptsCommonNames exercises the happy path.
// Operators want descriptive tag names; these are the ones that
// must keep working forever, so the test is the contract.
func TestValidateTagName_AcceptsCommonNames(t *testing.T) {
	for _, name := range []string{
		"prod-deploy",
		"pre-migration",
		"v2.0", // contains a dot
		"hotfix-2026-05",
		"alice/before-friday", // operator-namespaced
		"under_score_ok",
		"alpha123", // alphanumeric
	} {
		if err := ValidateTagName(name); err != nil {
			t.Errorf("ValidateTagName(%q) = %v, want nil", name, err)
		}
	}
}

// TestValidateTagName_RejectsReservedNames is the contract that the
// CHECK constraint in 0010 also enforces. If this list ever needs
// to grow we must update both.
func TestValidateTagName_RejectsReservedNames(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"", "empty"},
		{"current", "alias collision"},
		{"@1", "starts with @"},
		{"@0", "starts with @"},
		{"42", "pure digits"},
		{"00123", "pure digits with leading zero"},
		// 65-char name (one over the 64 cap)
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "too long"},
	}
	for _, c := range cases {
		err := ValidateTagName(c.name)
		if err == nil {
			t.Errorf("ValidateTagName(%q) = nil, want reserved-name error (%s)", c.name, c.why)
		}
		if err != nil && !errors.Is(err, ErrTagReservedName) {
			t.Errorf("ValidateTagName(%q) = %v, want ErrTagReservedName-wrapped (%s)", c.name, err, c.why)
		}
	}
}

// TestRefLooksLikeTag exercises the predicate used by the
// version-ref resolver to decide whether to consult
// state_version_tags. Each of the OTHER ref shapes must be a
// "no" so the tag table is never queried unnecessarily; tag-shaped
// names must be a "yes" so they resolve correctly.
func TestRefLooksLikeTag(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"", false},
		{"current", false},
		{"@0", false},
		{"@1", false},
		{"42", false},
		{"11111111-1111-1111-1111-111111111111", false}, // UUID
		{"prod-deploy", true},
		{"pre-mig", true},
		{"v2", true},
		{"alpha123", true}, // mixed alphanumeric
	}
	for _, c := range cases {
		got := refLooksLikeTag(c.ref)
		if got != c.want {
			t.Errorf("refLooksLikeTag(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}
