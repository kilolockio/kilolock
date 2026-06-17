package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/davesade/kilolock/pkg/store"
)

func TestDescribeResourceHistoryRow(t *testing.T) {
	tests := []struct {
		name string
		row  store.ResourceHistoryEntry
		want string
	}{
		{
			name: "current normal row",
			row:  store.ResourceHistoryEntry{CreateVersionSrc: "apply"},
			want: "current",
		},
		{
			name: "superseded normal row",
			row: func() store.ResourceHistoryEntry {
				serial := int64(5)
				return store.ResourceHistoryEntry{CreateVersionSrc: "apply", DeleteSerial: &serial}
			}(),
			want: "superseded",
		},
		{
			name: "restored current row",
			row:  store.ResourceHistoryEntry{CreateVersionSrc: "resource-rollback:aws_instance.web"},
			want: "restored-current",
		},
		{
			name: "restored old row",
			row: func() store.ResourceHistoryEntry {
				serial := int64(9)
				return store.ResourceHistoryEntry{CreateVersionSrc: "resource-rollback:aws_instance.web", DeleteSerial: &serial}
			}(),
			want: "restored-old",
		},
	}
	for _, test := range tests {
		if got := describeResourceHistoryRow(test.row); got != test.want {
			t.Fatalf("%s: got %q want %q", test.name, got, test.want)
		}
	}
}

func TestRenderResourceRollbackPreview_ShowsAttributeDiff(t *testing.T) {
	before, _ := json.Marshal(map[string]any{"id": "new", "triggers": map[string]any{"version": "v2"}})
	after, _ := json.Marshal(map[string]any{"id": "old", "triggers": map[string]any{"version": "v1"}})
	preview := &store.ResourceRollbackPreview{
		StateName:    "ws_x/env_y/demo",
		Address:      "time_sleep.slow_b",
		Action:       "replace",
		CurrentAttrs: before,
		TargetAttrs:  after,
	}
	var buf bytes.Buffer
	renderResourceRollbackPreview(&buf, preview)
	out := buf.String()
	for _, want := range []string{
		"Attribute changes:",
		"Changed:",
		"root.id",
		`"new" -> "old"`,
		"root.triggers.version",
		`"v2" -> "v1"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderResourceRollbackPreviewJSON_GroupsLeafChanges(t *testing.T) {
	before, _ := json.Marshal(map[string]any{"id": "new"})
	after, _ := json.Marshal(map[string]any{"id": "old", "note": "restored"})
	preview := &store.ResourceRollbackPreview{
		StateName:    "ws_x/env_y/demo",
		Address:      "time_sleep.slow_b",
		Action:       "replace",
		CurrentAttrs: before,
		TargetAttrs:  after,
	}
	var buf bytes.Buffer
	if err := renderResourceRollbackPreviewJSON(&buf, preview); err != nil {
		t.Fatalf("renderResourceRollbackPreviewJSON: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`"changed": [`,
		`"added": [`,
		`"path": "root.id"`,
		`"path": "root.note"`,
		`"before": "new"`,
		`"after": "old"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestEnforceResourceRollbackStrict(t *testing.T) {
	tests := []struct {
		name    string
		preview *store.ResourceRollbackPreview
		wantErr string
	}{
		{
			name: "allows safe replay",
			preview: &store.ResourceRollbackPreview{
				Action: "replace",
			},
		},
		{
			name: "rejects current dependents",
			preview: &store.ResourceRollbackPreview{
				Action:     "replace",
				Dependents: []string{"module.a.aws_instance.web", "module.b.aws_lb.main"},
			},
			wantErr: "strict rollback rejected apply",
		},
		{
			name: "rejects remove action",
			preview: &store.ResourceRollbackPreview{
				Action: "remove",
			},
			wantErr: "would remove the current resource from state",
		},
	}
	for _, test := range tests {
		err := enforceResourceRollbackStrict(true, test.preview)
		if test.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", test.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Fatalf("%s: got %v, want substring %q", test.name, err, test.wantErr)
		}
	}
	if err := enforceResourceRollbackStrict(false, &store.ResourceRollbackPreview{
		Action:     "remove",
		Dependents: []string{"aws_instance.web"},
	}); err != nil {
		t.Fatalf("strict disabled should allow preview, got %v", err)
	}
}
