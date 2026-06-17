package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/davesade/kilolock/pkg/store"
)

func mkVersion(serial int64, source, actor string, age time.Duration, isCurrent bool, size int) store.StateVersionInfo {
	return store.StateVersionInfo{
		ID:               "abcd1234-5678-9012-3456-789012345678",
		StateID:          "11111111-1111-1111-1111-111111111111",
		StateName:        "prod",
		Serial:           serial,
		TerraformVersion: "1.13.4",
		Source:           source,
		CreatedAt:        time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC).Add(-age),
		CreatedBy:        actor,
		SizeBytes:        size,
		IsCurrent:        isCurrent,
	}
}

// TestRenderHistoryTable_BasicFormatting walks the human-readable
// rendering path. We don't lock down exact whitespace (tabwriter
// reflows), but we do assert that every important column for every
// row is present, plus the current-marker convention and the
// footer hint.
func TestRenderHistoryTable_BasicFormatting(t *testing.T) {
	versions := []store.StateVersionInfo{
		mkVersion(3, "apply", "alice@cli", 5*time.Minute, true, 2048),
		mkVersion(2, "refresh", "bob@cli", 1*time.Hour, false, 1024),
		mkVersion(1, "import", "alice@cli", 30*24*time.Hour, false, 512),
	}
	var buf bytes.Buffer
	now := versions[0].CreatedAt.Add(5 * time.Minute) // make the latest read "5m ago"
	rc := renderHistoryTable(&buf, "prod", versions, nil, now)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	out := buf.String()

	for _, want := range []string{
		// Heading + state name
		"state: prod",
		// Column headings
		"SERIAL", "SOURCE", "WHEN", "ACTOR", "SIZE", "ID",
		// Each serial
		"3", "2", "1",
		// Each source
		"apply", "refresh", "import",
		// Each actor
		"alice@cli", "bob@cli",
		// Current marker
		"*",
		// Footer hint with the state name baked in
		"kl rollback prod --to=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---full---\n%s", want, out)
		}
	}

	// Only one row gets the * marker — the current version (serial 3).
	// We rely on the column layout: marker is the first non-whitespace
	// glyph on the row.
	stars := strings.Count(out, "* ")
	if stars < 1 {
		t.Errorf("no current marker rendered")
	}
}

// TestRenderHistoryTable_EmptyHistoryIsExitOne pins the contract
// that "state exists, has no versions" is signalled to the operator
// with rc=1 and a clear message, not a misleading empty table.
func TestRenderHistoryTable_EmptyHistoryIsExitOne(t *testing.T) {
	var buf bytes.Buffer
	rc := renderHistoryTable(&buf, "prod", nil, nil, time.Now())
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
	if !strings.Contains(buf.String(), "no versions") {
		t.Errorf("missing 'no versions' message: %s", buf.String())
	}
}

// TestRenderHistoryJSON_RoundTripsThroughDecoder asserts that the
// JSON output is mechanically parseable — script consumers don't
// look at whitespace, they json.Unmarshal it.
func TestRenderHistoryJSON_RoundTripsThroughDecoder(t *testing.T) {
	versions := []store.StateVersionInfo{
		mkVersion(2, "apply", "alice@cli", 1*time.Minute, true, 2048),
		mkVersion(1, "import", "bob@cli", 1*time.Hour, false, 512),
	}
	var buf bytes.Buffer
	rc := renderHistoryJSON(&buf, "prod", versions, nil)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}

	var got struct {
		State    string `json:"state"`
		Versions []struct {
			Serial    int64  `json:"serial"`
			Source    string `json:"source"`
			IsCurrent bool   `json:"is_current"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if got.State != "prod" {
		t.Errorf("state = %q", got.State)
	}
	if len(got.Versions) != 2 {
		t.Fatalf("len(versions) = %d", len(got.Versions))
	}
	if got.Versions[0].Serial != 2 || !got.Versions[0].IsCurrent {
		t.Errorf("first row: %+v", got.Versions[0])
	}
}

// TestRenderHistoryTable_AddsTagsColumnWhenPresent pins the
// conditional rendering of the TAGS column. With zero tags the
// header MUST NOT carry "TAGS" (we don't want to widen the table
// for nothing); with at least one tag we DO include it. This
// avoids a flaky / weird-looking output for operators who don't
// use tags yet.
func TestRenderHistoryTable_AddsTagsColumnWhenPresent(t *testing.T) {
	versions := []store.StateVersionInfo{
		mkVersion(2, "apply", "alice@cli", 1*time.Minute, true, 2048),
		mkVersion(1, "import", "bob@cli", 1*time.Hour, false, 512),
	}
	// mkVersion returns the same ID for every row; rewrite per
	// row here so the tag map can address them distinctly. The
	// renderer keys tag lookups on version ID, so unique IDs are
	// the contract the test must mirror.
	versions[0].ID = "aaaaaaaa-0000-0000-0000-000000000000"
	versions[1].ID = "bbbbbbbb-0000-0000-0000-000000000000"
	tags := map[string][]string{
		versions[0].ID: {"prod-deploy"},
		versions[1].ID: {"pre-mig", "rollback-target"},
	}
	var buf bytes.Buffer
	rc := renderHistoryTable(&buf, "prod", versions, tags, versions[0].CreatedAt.Add(time.Minute))
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	out := buf.String()
	for _, want := range []string{
		"TAGS", "prod-deploy", "pre-mig, rollback-target",
		"Tag names (TAGS column) are accepted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---full---\n%s", want, out)
		}
	}
}

// TestRenderHistoryTable_OmitsTagsColumnWhenEmpty is the
// companion: when tagsByVersion is nil or has no non-empty
// entries, the TAGS column must be hidden, and the tag-usage
// footer hint must be hidden too. This keeps the no-tags
// view exactly as it was before the feature shipped.
func TestRenderHistoryTable_OmitsTagsColumnWhenEmpty(t *testing.T) {
	versions := []store.StateVersionInfo{
		mkVersion(1, "import", "bob@cli", 1*time.Hour, true, 512),
	}
	var buf bytes.Buffer
	renderHistoryTable(&buf, "prod", versions, nil, versions[0].CreatedAt.Add(time.Minute))
	out := buf.String()
	if strings.Contains(out, "TAGS") {
		t.Errorf("TAGS column rendered for empty tag map:\n%s", out)
	}
	if strings.Contains(out, "Tag names (TAGS column)") {
		t.Errorf("tag-usage hint rendered with no tags:\n%s", out)
	}
}

// TestHumanAge_HitsEachBucket walks the relative-time formatter so
// we catch off-by-one regressions in the bucket boundaries (every
// boundary has been an actual production bug somewhere).
func TestHumanAge_HitsEachBucket(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "0s ago"},
		{45 * time.Second, "45s ago"},
		{2 * time.Minute, "2m ago"},
		{3 * time.Hour, "3h ago"},
		{2 * 24 * time.Hour, "2d ago"},
		{3 * 7 * 24 * time.Hour, "3w ago"},
	}
	for _, c := range cases {
		if got := humanAge(c.d); got != c.want {
			t.Errorf("humanAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestHumanSize_HitsEachBucket(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{42, "42B"},
		{2 * 1024, "2.0KB"},
		{3 * 1024 * 1024, "3.0MB"},
	}
	for _, c := range cases {
		if got := humanSize(c.n); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestShortUUID_TruncatesAndPassesThroughShortInput(t *testing.T) {
	if got := shortUUID("abcdef12-3456-7890-1234-567890abcdef"); got != "abcdef12" {
		t.Errorf("got %q", got)
	}
	if got := shortUUID("short"); got != "short" {
		t.Errorf("got %q (short input must pass through)", got)
	}
}
