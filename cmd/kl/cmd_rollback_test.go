package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

func mkPreviewVersion(id string, serial int64, source, actor string) *store.StateVersionInfo {
	return &store.StateVersionInfo{
		ID:        id,
		StateID:   "11111111-1111-1111-1111-111111111111",
		StateName: "prod",
		Serial:    serial,
		Source:    source,
		CreatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		CreatedBy: actor,
	}
}

// TestRenderRollbackPreview_ShowsTheRightWarnings checks the most
// load-bearing thing about the rollback UX: when resources would be
// removed (and thus orphaned in the cloud), the warning paragraph
// appears prominently. Without this assertion a future refactor
// could silently delete the warning and we'd ship a footgun.
func TestRenderRollbackPreview_ShowsTheRightWarnings(t *testing.T) {
	current := mkPreviewVersion("aaaaaaaa-1111-2222-3333-444444444444", 47, "apply", "bob@cli")
	target := mkPreviewVersion("bbbbbbbb-1111-2222-3333-444444444444", 42, "apply", "alice@cli")
	diff := &store.VersionAddressDiff{
		Added:   []string{"aws_instance.zombie"},
		Removed: []string{"aws_instance.web", "aws_security_group.web"},
		Changed: []string{"aws_route53_record.web"},
	}
	var buf bytes.Buffer
	renderRollbackPreview(&buf, "prod", current, target, diff)
	out := buf.String()

	for _, want := range []string{
		"Rollback preview for state \"prod\"",
		// Both versions named with serial + short uuid prefix
		"47", "42",
		"aaaaaaaa", "bbbbbbbb",
		// Each diff section
		"added to state", "aws_instance.zombie",
		"removed from state", "aws_instance.web", "aws_security_group.web",
		"attributes change", "aws_route53_record.web",
		// The load-bearing warnings
		"unmanaged orphans",
		"recreate",
		"WARNING:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---full---\n%s", want, out)
		}
	}
}

// TestRenderRollbackPreview_NoDiffStillRenders covers the
// edge case where two versions differ in metadata only (e.g. a
// re-apply with no resource changes). The preview must still
// show both serial pairs and not advertise removed/added
// warnings that would alarm the operator over nothing.
func TestRenderRollbackPreview_NoDiffStillRenders(t *testing.T) {
	current := mkPreviewVersion("aaaaaaaa-0000-0000-0000-000000000000", 5, "apply", "alice")
	target := mkPreviewVersion("bbbbbbbb-0000-0000-0000-000000000000", 4, "apply", "alice")
	var buf bytes.Buffer
	renderRollbackPreview(&buf, "prod", current, target, &store.VersionAddressDiff{})
	out := buf.String()

	for _, want := range []string{"5", "4", "(no resource-level differences"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---full---\n%s", want, out)
		}
	}
	// Should NOT advertise the orphan warning when nothing was
	// removed — otherwise operators learn to ignore it, which is
	// worse than not showing it.
	if strings.Contains(out, "unmanaged orphans") {
		t.Errorf("orphan warning printed on a no-removal preview:\n%s", out)
	}
}

// TestConfirmRollback_AcceptsExactStateName ensures we don't allow
// "y" / "yes" / "Y" / random whitespace-only inputs to confirm a
// destructive operation. The contract is: the operator must type
// the literal state name back, full stop.
func TestConfirmRollback_AcceptsExactStateName(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"prod\n", true},
		{"prod\r\n", true},
		{"  prod  \n", true}, // TrimSpace per contract
		{"y\n", false},
		{"yes\n", false},
		{"PROD\n", false}, // case-sensitive
		{"\n", false},     // empty
		{"prod-staging\n", false},
	}
	for _, c := range cases {
		var stdout bytes.Buffer
		got := confirmRollback(strings.NewReader(c.input), &stdout, "prod")
		if got != c.want {
			t.Errorf("confirmRollback(%q) = %v, want %v", c.input, got, c.want)
		}
		if !strings.Contains(stdout.String(), `"prod"`) {
			t.Errorf("prompt did not include state name: %s", stdout.String())
		}
	}
}

// TestConfirmRollback_HandlesClosedStdin ensures the operator
// closing stdin (Ctrl-D) is treated as a cancellation, not a
// silent accept. Critical for SSH-detached / piped contexts.
func TestConfirmRollback_HandlesClosedStdin(t *testing.T) {
	var stdout bytes.Buffer
	if got := confirmRollback(strings.NewReader(""), &stdout, "prod"); got {
		t.Error("closed stdin should NOT confirm")
	}
}

// TestRenderAddressList_TruncatesWithCount pins the "...and N more"
// behavior. The threshold matters because the rollback preview is
// commonly viewed in a terminal — a 200-row dump pushes the warning
// off the screen and operators stop reading.
func TestRenderAddressList_TruncatesWithCount(t *testing.T) {
	addrs := make([]string, 50)
	for i := range addrs {
		addrs[i] = "aws_instance.worker_" + paddedIndex(i)
	}
	var buf bytes.Buffer
	renderAddressList(&buf, "-", "removed", addrs)
	out := buf.String()
	if !strings.Contains(out, "[50]") {
		t.Errorf("count missing: %s", out)
	}
	if !strings.Contains(out, "and 30 more") {
		t.Errorf("truncation hint missing: %s", out)
	}
	if strings.Contains(out, "aws_instance.worker_49") {
		t.Errorf("row 49 should NOT be rendered (above truncation cap):\n%s", out)
	}
	if !strings.Contains(out, "aws_instance.worker_19") {
		t.Errorf("row 19 should be rendered (below cap):\n%s", out)
	}
}

func TestRenderAddressList_EmptyIsSilent(t *testing.T) {
	var buf bytes.Buffer
	renderAddressList(&buf, "-", "removed", nil)
	if buf.Len() != 0 {
		t.Errorf("empty list should render nothing; got: %q", buf.String())
	}
}

func paddedIndex(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
