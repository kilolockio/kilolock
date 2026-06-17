package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/refresh"
	"github.com/kilolockio/kilolock/pkg/store"
)

// resolveSearchPaths is the only piece of CLI logic with non-trivial
// branching: precedence between --provider-search-path, the env var,
// and built-in defaults. The function is pure (it only reads a
// directly-passed env value, not os.Getenv), so the tests assert
// behavior without env munging.

func TestResolveSearchPaths_FlagsBeforeEnv(t *testing.T) {
	tmp := t.TempDir() // a real, absolute directory
	got, err := resolveSearchPaths([]string{tmp + "/flag-a", tmp + "/flag-b"}, tmp+"/env-a:"+tmp+"/env-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPrefix := []string{
		tmp + "/flag-a",
		tmp + "/flag-b",
		tmp + "/env-a",
		tmp + "/env-b",
	}
	for i, p := range wantPrefix {
		if i >= len(got) {
			t.Fatalf("only got %d paths, want at least %d", len(got), len(wantPrefix))
		}
		// normalizePaths runs filepath.Abs, but we already passed
		// absolute paths so they should round-trip unchanged.
		if got[i] != p {
			t.Errorf("path[%d]: got %q, want %q", i, got[i], p)
		}
	}
}

func TestResolveSearchPaths_DedupesAndSkipsEmpty(t *testing.T) {
	tmp := t.TempDir()

	// Same path appears in flags and env. Empty entries (from a
	// trailing colon, e.g. "/foo:") must not produce empty paths
	// in the output.
	got, err := resolveSearchPaths(
		[]string{tmp + "/p1", "  ", tmp + "/p1"},
		tmp+"/p1:"+":"+tmp+"/p2",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, p := range got {
		if p == "" {
			t.Errorf("output contains empty path: %v", got)
		}
	}
	count := 0
	for _, p := range got {
		if p == tmp+"/p1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one occurrence of %q, got %d (%v)", tmp+"/p1", count, got)
	}
}

func TestResolveSearchPaths_DefaultsApplied(t *testing.T) {
	// No flags, no env: defaults must yield at least one path
	// (UserHomeDir typically succeeds in CI; CWD always does).
	got, err := resolveSearchPaths(nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected default paths, got none")
	}

	// Verify CWD/.terraform/providers is among them. This is
	// the most reliable default across environments.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, ".terraform", "providers")
	found := false
	for _, p := range got {
		if p == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %q in defaults, got %v", want, got)
	}
}

func TestResolveSearchPaths_RelativeFlagBecomesAbsolute(t *testing.T) {
	// The flag accepts relative paths (matching Terraform's CLI
	// behavior), but we normalize to absolute so error messages
	// from Discover are stable regardless of where the operator
	// later resolves them. This guarantees the contract.
	got, err := resolveSearchPaths([]string{"./relative/path"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(got[0]) {
		t.Errorf("got[0] = %q, want absolute path", got[0])
	}
}

// effectiveConcurrency translates a 0/negative flag value into NumCPU.
// Tests cover every branch of the tiny helper.
func TestEffectiveConcurrency(t *testing.T) {
	cases := map[string]struct {
		in   int
		want int
	}{
		"zero":     {0, runtime.NumCPU()},
		"negative": {-3, runtime.NumCPU()},
		"explicit": {4, 4},
		"large":    {1024, 1024},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := effectiveConcurrency(tc.in); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"a", "b"}, "a"},
		{[]string{"", "b"}, "b"},
		{[]string{"", "", "c"}, "c"},
		{[]string{"", ""}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := firstNonEmpty(tc.in...); got != tc.want {
			t.Errorf("firstNonEmpty(%v): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// multiString is a flag.Value; the contract is "Set appends, String
// joins with comma". Tests guarantee both, and that the joined view
// is intelligible for help output.
func TestMultiStringFlag(t *testing.T) {
	var m multiString
	if err := m.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("b"); err != nil {
		t.Fatal(err)
	}
	if got := m.String(); got != "a,b" {
		t.Errorf("String() = %q, want %q", got, "a,b")
	}
	if len(m) != 2 || m[0] != "a" || m[1] != "b" {
		t.Errorf("unexpected slice contents: %v", m)
	}

	// nil receiver is legal because flag library introspects help
	// before any Set call.
	var zero *multiString
	if got := zero.String(); got != "" {
		t.Errorf("nil receiver: got %q, want empty", got)
	}
}

// renderRefreshResult is the "what does the operator see?" surface;
// tests assert key invariants on the formatted output rather than
// snapshotting whole strings (which break on cosmetic tabwriter
// changes).
func TestRenderRefreshResult_Committed(t *testing.T) {
	res := &refresh.Result{
		RunID:            "run-123",
		StateName:        "prod-vpc",
		Status:           store.RefreshRunSucceeded,
		SerialBefore:     7,
		SerialAfter:      8,
		ResourcesChecked: 5,
		ResourcesChanged: 2,
		ResourcesFailed:  0,
		StartedAt:        time.Now().Add(-2 * time.Second),
		FinishedAt:       time.Now(),
	}

	var buf bytes.Buffer
	renderRefreshResult(&buf, res, nil)

	out := buf.String()
	for _, want := range []string{
		"prod-vpc",
		"run-123",
		"7 → 8 (committed)",
		"succeeded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "errors:") {
		t.Errorf("did not expect errors section, got:\n%s", out)
	}
}

// TestRenderRefreshResult_DriftAddresses renders a committed run with
// per-resource drift addresses (v1.7a) and asserts the output carries
// the section header + each address, prefixed by two spaces. This is
// the user-visible payoff of the v1.7 work: "which resources changed,
// readable at a glance."
func TestRenderRefreshResult_DriftAddresses(t *testing.T) {
	res := &refresh.Result{
		RunID:            "run-drift",
		StateName:        "prod-vpc",
		Status:           store.RefreshRunSucceeded,
		SerialBefore:     10,
		SerialAfter:      11,
		ResourcesChecked: 5,
		ResourcesChanged: 2,
		ChangedAddresses: []string{"aws_instance.web[0]", "aws_s3_bucket.logs"},
	}

	var buf bytes.Buffer
	renderRefreshResult(&buf, res, nil)
	out := buf.String()

	if !strings.Contains(out, "drift addresses:") {
		t.Errorf("expected drift addresses section, got:\n%s", out)
	}
	for _, addr := range res.ChangedAddresses {
		if !strings.Contains(out, "  "+addr) {
			t.Errorf("expected indented address %q in output, got:\n%s", addr, out)
		}
	}
	// Truncation footer must NOT appear for a small list.
	if strings.Contains(out, "... and") {
		t.Errorf("unexpected truncation footer on short list:\n%s", out)
	}
}

// TestRenderRefreshResult_DriftAddressesTruncated proves the output
// stays bounded when the drift list is large. The footer must
// quantify what was elided and point operators at the SQL view for
// the full picture.
func TestRenderRefreshResult_DriftAddressesTruncated(t *testing.T) {
	// 30 addresses; cap is 25 → 5 elided.
	addrs := make([]string, 30)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("aws_vpc.v%02d", i)
	}
	res := &refresh.Result{
		RunID:            "run-many",
		StateName:        "many",
		Status:           store.RefreshRunSucceeded,
		ResourcesChanged: 30,
		ChangedAddresses: addrs,
	}

	var buf bytes.Buffer
	renderRefreshResult(&buf, res, nil)
	out := buf.String()

	// First 25 must be present.
	for _, addr := range addrs[:25] {
		if !strings.Contains(out, "  "+addr) {
			t.Errorf("expected %q in output, got:\n%s", addr, out)
		}
	}
	// 26th onwards must NOT be present.
	for _, addr := range addrs[25:] {
		if strings.Contains(out, "  "+addr+"\n") {
			t.Errorf("address %q should have been truncated; output:\n%s", addr, out)
		}
	}
	if !strings.Contains(out, "... and 5 more") {
		t.Errorf("expected '... and 5 more' footer, got:\n%s", out)
	}
	if !strings.Contains(out, "current_resource_drift") {
		t.Errorf("truncation footer should point at the SQL view, got:\n%s", out)
	}
}

func TestTruncateAddrList(t *testing.T) {
	cases := []struct {
		in   []string
		n    int
		want []string
	}{
		{[]string{"a", "b", "c"}, 5, []string{"a", "b", "c"}},
		{[]string{"a", "b", "c"}, 3, []string{"a", "b", "c"}},
		{[]string{"a", "b", "c", "d", "e"}, 3, []string{"a", "b", "c"}},
		{nil, 5, nil},
	}
	for i, tc := range cases {
		got := truncateAddrList(tc.in, tc.n)
		if len(got) != len(tc.want) {
			t.Errorf("case %d: got len=%d, want len=%d", i, len(got), len(tc.want))
			continue
		}
		for j := range got {
			if got[j] != tc.want[j] {
				t.Errorf("case %d: index %d: got %q, want %q", i, j, got[j], tc.want[j])
			}
		}
	}
}

func TestRenderRefreshResult_DryRun(t *testing.T) {
	res := &refresh.Result{
		RunID:        "run-dry",
		StateName:    "stg",
		Status:       store.RefreshRunSucceeded,
		SerialBefore: 12,
		SerialAfter:  12,
		DryRun:       true,
	}

	var buf bytes.Buffer
	renderRefreshResult(&buf, res, nil)
	out := buf.String()
	if !strings.Contains(out, "12 (dry-run; no version written)") {
		t.Errorf("dry-run line missing; got:\n%s", out)
	}
	if strings.Contains(out, "(committed)") {
		t.Errorf("must not say 'committed' on dry-run; got:\n%s", out)
	}
}

func TestRenderRefreshResult_Failed(t *testing.T) {
	res := &refresh.Result{
		RunID:            "run-fail",
		StateName:        "broken",
		Status:           store.RefreshRunFailed,
		SerialBefore:     3,
		SerialAfter:      3,
		ResourcesChecked: 2,
		ResourcesChanged: 0,
		ResourcesFailed:  1,
		Errors: []refresh.ResourceError{
			{Address: "aws_instance.web[0]", Err: errFakeBoom},
		},
	}

	var buf bytes.Buffer
	renderRefreshResult(&buf, res, nil)
	out := buf.String()
	for _, want := range []string{
		"3 (no version written; refresh failed)",
		"failed",
		"errors:",
		"aws_instance.web[0]",
		"boom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got:\n%s", want, out)
		}
	}
}

func TestRenderRefreshResult_RunErrSurfacedAsWarning(t *testing.T) {
	// The orchestrator returns (result, err) when the run produced
	// a result but a side-effect (Finish, commit) failed. The CLI
	// must keep the result visible AND surface the error so the
	// operator understands the audit row may be inconsistent.
	res := &refresh.Result{
		RunID:        "run-warn",
		StateName:    "weird",
		Status:       store.RefreshRunSucceeded,
		SerialBefore: 1,
		SerialAfter:  2,
	}

	var buf bytes.Buffer
	renderRefreshResult(&buf, res, errors.New("finish refresh run: db unavailable"))
	out := buf.String()
	if !strings.Contains(out, "warnings:") {
		t.Errorf("expected warnings line, got:\n%s", out)
	}
	if !strings.Contains(out, "db unavailable") {
		t.Errorf("expected wrapped error to surface, got:\n%s", out)
	}
}

// errFakeBoom is a sentinel for the failed-result test. Defining it
// once makes the test cases easier to read.
var errFakeBoom = fmt.Errorf("read resource: boom")
