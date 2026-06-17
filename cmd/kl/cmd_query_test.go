package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

func TestReadQuerySource_PositionalArgument(t *testing.T) {
	got, err := readQuerySource("", []string{"  SELECT 1  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "SELECT 1" {
		t.Errorf("got %q, want %q", got, "SELECT 1")
	}
}

func TestReadQuerySource_Errors(t *testing.T) {
	cases := []struct {
		name        string
		file        string
		positional  []string
		wantSubstr  string
		shouldError bool
	}{
		{"empty input", "", nil, "no SQL supplied", true},
		{"too many positional", "", []string{"SELECT 1", "SELECT 2"}, "exactly one SQL argument", true},
		{"empty positional", "", []string{"   "}, "SQL argument is empty", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readQuerySource(tc.file, tc.positional)
			if (err != nil) != tc.shouldError {
				t.Fatalf("error = %v, want shouldError=%v", err, tc.shouldError)
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestNewQueryWriter_UnknownFormat(t *testing.T) {
	_, err := newQueryWriter("yaml", &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for unknown format")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTableQueryWriter_RendersRows(t *testing.T) {
	var buf bytes.Buffer
	w, err := newQueryWriter("table", &buf)
	if err != nil {
		t.Fatalf("newQueryWriter: %v", err)
	}
	cols := []store.ColumnInfo{{Name: "name"}, {Name: "count"}}
	if err := w.OnColumns(cols); err != nil {
		t.Fatalf("OnColumns: %v", err)
	}
	if err := w.OnRow([]any{"prod", int64(42)}); err != nil {
		t.Fatalf("OnRow: %v", err)
	}
	if err := w.OnRow([]any{"staging", int64(7)}); err != nil {
		t.Fatalf("OnRow: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"name", "count", "prod", "42", "staging", "7", "(2 rows)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestTableQueryWriter_ZeroRows(t *testing.T) {
	var buf bytes.Buffer
	w, _ := newQueryWriter("table", &buf)
	if err := w.OnColumns([]store.ColumnInfo{{Name: "x"}}); err != nil {
		t.Fatalf("OnColumns: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(buf.String(), "(0 rows)") {
		t.Errorf("expected (0 rows) marker, got:\n%s", buf.String())
	}
}

func TestJSONQueryWriter_StreamsObjects(t *testing.T) {
	var buf bytes.Buffer
	w, _ := newQueryWriter("json", &buf)
	cols := []store.ColumnInfo{
		{Name: "name"},
		{Name: "attrs", TypeOID: oidJSONB},
		{Name: "active"},
	}
	if err := w.OnColumns(cols); err != nil {
		t.Fatalf("OnColumns: %v", err)
	}
	if err := w.OnRow([]any{"prod", []byte(`{"region":"eu-west-1"}`), true}); err != nil {
		t.Fatalf("OnRow: %v", err)
	}
	if err := w.OnRow([]any{"staging", []byte(`{"region":"us-east-1"}`), false}); err != nil {
		t.Fatalf("OnRow: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0]["name"] != "prod" {
		t.Errorf("row 0 name = %v, want prod", got[0]["name"])
	}
	attrs, ok := got[0]["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("row 0 attrs not a nested object, got %T (%v)", got[0]["attrs"], got[0]["attrs"])
	}
	if attrs["region"] != "eu-west-1" {
		t.Errorf("row 0 attrs.region = %v, want eu-west-1", attrs["region"])
	}
	if got[0]["active"] != true {
		t.Errorf("row 0 active = %v, want true", got[0]["active"])
	}
}

func TestCSVQueryWriter_EscapesCommas(t *testing.T) {
	var buf bytes.Buffer
	w, _ := newQueryWriter("csv", &buf)
	cols := []store.ColumnInfo{{Name: "a"}, {Name: "b"}}
	if err := w.OnColumns(cols); err != nil {
		t.Fatalf("OnColumns: %v", err)
	}
	if err := w.OnRow([]any{"plain", "has,comma"}); err != nil {
		t.Fatalf("OnRow: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"has,comma"`) {
		t.Errorf("comma not quoted in CSV output:\n%s", out)
	}
	if !strings.HasPrefix(out, "a,b\n") {
		t.Errorf("CSV header missing, got: %q", out)
	}
}

func TestFormatScalarForText(t *testing.T) {
	tm := time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		value   any
		typeOID uint32
		want    string
	}{
		{"nil", nil, 0, ""},
		{"string", "hello", 0, "hello"},
		{"bool true", true, 0, "true"},
		{"jsonb bytes", []byte(`{"k":1}`), oidJSONB, `{"k":1}`},
		{"plain bytes", []byte("raw"), 0, "raw"},
		{"time", tm, 0, "2026-05-12T13:00:00Z"},
		{"int", 42, 0, "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatScalarForText(tc.value, tc.typeOID)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMarshalForJSON_PreservesJSONB(t *testing.T) {
	raw, err := marshalForJSON([]byte(`{"region":"eu"}`), oidJSONB)
	if err != nil {
		t.Fatalf("marshalForJSON: %v", err)
	}
	if string(raw) != `{"region":"eu"}` {
		t.Errorf("got %s, want %s", string(raw), `{"region":"eu"}`)
	}
}

func TestMarshalForJSON_NilToJSONNull(t *testing.T) {
	raw, err := marshalForJSON(nil, 0)
	if err != nil {
		t.Fatalf("marshalForJSON: %v", err)
	}
	if string(raw) != "null" {
		t.Errorf("got %s, want null", string(raw))
	}
}

func TestMarshalForJSON_InvalidJSONBFallsBackToString(t *testing.T) {
	raw, err := marshalForJSON([]byte("not json"), oidJSONB)
	if err != nil {
		t.Fatalf("marshalForJSON: %v", err)
	}
	if string(raw) != `"not json"` {
		t.Errorf("got %s, want %q", string(raw), "not json")
	}
}
