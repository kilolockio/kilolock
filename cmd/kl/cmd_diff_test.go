package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

func mockHeader() diffHeader {
	t1 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC)
	return diffHeader{
		StateName:  "demo",
		FromSerial: 1, FromID: "11111111-1111-1111-1111-111111111111",
		FromActor: "alice", FromWrittenAt: t1,
		ToSerial: 2, ToID: "22222222-2222-2222-2222-222222222222",
		ToActor: "bob", ToWrittenAt: t2,
	}
}

// TestRenderDiffTable_SensitiveCollapsesToOneLine is the privacy
// contract for the table renderer: when a changed leaf is sensitive,
// we must NOT print before/after side-by-side as "<sensitive> ->
// <sensitive>" (visually noisy and reveals the LENGTH information of
// the redaction marker on both sides). We collapse to a single line.
func TestRenderDiffTable_SensitiveCollapsesToOneLine(t *testing.T) {
	rows := []store.ResourceAttrDelta{{
		Address:       "aws_db_instance.primary",
		Status:        "changed",
		FromAttrs:     json.RawMessage(`{"password":"old"}`),
		ToAttrs:       json.RawMessage(`{"password":"new"}`),
		FromSensitive: json.RawMessage(`[["password"]]`),
		ToSensitive:   json.RawMessage(`[["password"]]`),
	}}
	var buf bytes.Buffer
	if code := renderDiffTable(&buf, mockHeader(), rows, false); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	out := buf.String()
	if !strings.Contains(out, "<sensitive value changed>") {
		t.Errorf("want 'sensitive value changed' marker; output:\n%s", out)
	}
	if strings.Contains(out, "<sensitive> -> <sensitive>") {
		t.Errorf("must NOT show side-by-side sensitive renderings; output:\n%s", out)
	}
}

// TestRenderDiffJSON_RedactsSensitive: the JSON renderer must
// redact sensitive values on both before/after AND emit the
// sensitive:true marker. JSON output is the format most likely
// to land in log aggregators; a regression here is a secret leak.
func TestRenderDiffJSON_RedactsSensitive(t *testing.T) {
	rows := []store.ResourceAttrDelta{{
		Address:       "aws_db_instance.primary",
		Status:        "changed",
		FromAttrs:     json.RawMessage(`{"password":"oldsecret"}`),
		ToAttrs:       json.RawMessage(`{"password":"newsecret"}`),
		FromSensitive: json.RawMessage(`[["password"]]`),
		ToSensitive:   json.RawMessage(`[["password"]]`),
	}}
	var buf bytes.Buffer
	if code := renderDiffJSON(&buf, mockHeader(), rows, false); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if strings.Contains(out, "oldsecret") || strings.Contains(out, "newsecret") {
		t.Errorf("raw secret leaked into JSON output:\n%s", out)
	}
	if !strings.Contains(out, "<sensitive>") {
		t.Errorf("expected '<sensitive>' marker; got:\n%s", out)
	}
	if !strings.Contains(out, `"sensitive": true`) {
		t.Errorf("expected sensitive:true marker; got:\n%s", out)
	}
}

// TestRenderDiffTable_AddedAndRemovedFlavours: an added or removed
// resource should produce one leaf row per attribute, with the
// correct +/− marker. Pins the "you can see the SHAPE of what
// disappeared" property.
func TestRenderDiffTable_AddedAndRemovedFlavours(t *testing.T) {
	rows := []store.ResourceAttrDelta{
		{
			Address: "aws_s3_bucket.new",
			Status:  "added",
			ToAttrs: json.RawMessage(`{"acl":"private","versioning":true}`),
		},
		{
			Address:   "aws_instance.gone",
			Status:    "removed",
			FromAttrs: json.RawMessage(`{"id":"i-9","ami":"ami-1"}`),
		},
	}
	var buf bytes.Buffer
	renderDiffTable(&buf, mockHeader(), rows, false)
	out := buf.String()

	for _, want := range []string{
		"+  aws_s3_bucket.new",
		`+ root.acl = "private"`,
		"+ root.versioning = true",
		"-  aws_instance.gone",
		`- root.id = "i-9"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q; full output:\n%s", want, out)
		}
	}
}

// TestRenderDiffTable_EmptyChangeSet renders an explicit "no
// changes" message rather than a dangling header. Pins the
// glance-friendly form: an operator running `diff` on identical
// versions should immediately see that nothing's different.
func TestRenderDiffTable_EmptyChangeSet(t *testing.T) {
	var buf bytes.Buffer
	renderDiffTable(&buf, mockHeader(), nil, false)
	if !strings.Contains(buf.String(), "no attribute changes") {
		t.Errorf("empty diff must say so; got:\n%s", buf.String())
	}
}

// TestApplyAddressFilter checks the filtering pre-render step in
// isolation. Coverage for matchAddressGlob already lives in
// jsondelta_test.go; this test focuses on the integration of glob
// with the row slice rather than the glob itself.
func TestApplyAddressFilter(t *testing.T) {
	rows := []store.ResourceAttrDelta{
		{Address: "aws_instance.web"},
		{Address: "aws_instance.db"},
		{Address: "aws_s3_bucket.logs"},
	}
	got := applyAddressFilter(rows, "aws_instance.*")
	if len(got) != 2 {
		t.Errorf("aws_instance.* filter kept %d rows, want 2: %+v", len(got), got)
	}
	got = applyAddressFilter(rows, "")
	if len(got) != 3 {
		t.Errorf("empty glob should keep all rows, got %d", len(got))
	}
}
