//go:build integration

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func seedStateEngineMutationStateRaw(t *testing.T) []byte {
	t.Helper()
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "c4a1f2e0-aaaa-bbbb-cccc-123456789abc",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "aws_vpc",
				"name":     "main",
				"provider": `provider["registry.terraform.io/hashicorp/aws"]`,
				"instances": []any{
					map[string]any{
						"schema_version":       0,
						"attributes":           map[string]any{"id": "vpc-1"},
						"sensitive_attributes": []any{},
					},
				},
			},
			map[string]any{
				"mode":     "managed",
				"type":     "aws_subnet",
				"name":     "private",
				"provider": `provider["registry.terraform.io/hashicorp/aws"]`,
				"instances": []any{
					map[string]any{
						"schema_version":       0,
						"attributes":           map[string]any{"id": "subnet-1"},
						"sensitive_attributes": []any{},
						"dependencies":         []any{"aws_vpc.main"},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return raw
}

func TestStateEngineMutations_MoveUsesRowDeltaAndRewritesDependents(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if err := s.WriteState(ctx, "qtest", "", seedStateEngineMutationStateRaw(t), "seed", "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	info, preview, err := s.ApplyMoveResourceCurrent(ctx, "qtest", "aws_vpc.main", "module.edge.aws_vpc.main", "tester")
	if err != nil {
		t.Fatalf("ApplyMoveResourceCurrent: %v", err)
	}
	if info == nil || info.Serial != 2 {
		t.Fatalf("version serial = %+v, want serial 2", info)
	}
	if preview == nil || preview.Action != "move" {
		t.Fatalf("preview = %+v, want move", preview)
	}

	raw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	current, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	assertStateHasID(t, current, "module.edge.aws_vpc.main", "vpc-1")
	subnet, err := findResourceInstance(current, "aws_subnet.private")
	if err != nil {
		t.Fatalf("find subnet after move: %v", err)
	}
	if subnet == nil {
		t.Fatalf("aws_subnet.private missing after move")
	}
	if len(subnet.Instance.Dependencies) != 1 || subnet.Instance.Dependencies[0] != "module.edge.aws_vpc.main" {
		t.Fatalf("subnet dependencies = %v, want module.edge.aws_vpc.main", subnet.Instance.Dependencies)
	}

	rows, err := pool.Query(ctx, `
		SELECT address, create_serial, COALESCE(delete_serial, 0), dependencies_raw::text
		FROM resources
		WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')
		ORDER BY address, create_serial
	`)
	if err != nil {
		t.Fatalf("query resources: %v", err)
	}
	defer rows.Close()

	type row struct {
		address      string
		createSerial int64
		deleteSerial int64
		deps         string
	}
	var got []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.address, &item.createSerial, &item.deleteSerial, &item.deps); err != nil {
			t.Fatalf("scan resource row: %v", err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate resources: %v", err)
	}

	want := map[string]row{
		"aws_subnet.private#1":       {address: "aws_subnet.private", createSerial: 1, deleteSerial: 2, deps: `["aws_vpc.main"]`},
		"aws_subnet.private#2":       {address: "aws_subnet.private", createSerial: 2, deleteSerial: 0, deps: `["module.edge.aws_vpc.main"]`},
		"aws_vpc.main#1":             {address: "aws_vpc.main", createSerial: 1, deleteSerial: 2, deps: `[]`},
		"module.edge.aws_vpc.main#2": {address: "module.edge.aws_vpc.main", createSerial: 2, deleteSerial: 0, deps: `[]`},
	}
	if len(got) != len(want) {
		t.Fatalf("resource row count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for _, item := range got {
		key := fmt.Sprintf("%s#%d", item.address, item.createSerial)
		expected, ok := want[key]
		if !ok {
			t.Fatalf("unexpected row: %+v", item)
		}
		if item.deleteSerial != expected.deleteSerial || item.deps != expected.deps {
			t.Fatalf("row %s mismatch: got %+v want %+v", key, item, expected)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing rows after move: %+v", want)
	}
}

func TestStateEngineMutations_RemoveClosesOnlyTargetRow(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if err := s.WriteState(ctx, "qtest", "", seedStateEngineMutationStateRaw(t), "seed", "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	info, preview, err := s.ApplyRemoveResourceCurrent(ctx, "qtest", "aws_subnet.private", "tester")
	if err != nil {
		t.Fatalf("ApplyRemoveResourceCurrent: %v", err)
	}
	if info == nil || info.Serial != 2 {
		t.Fatalf("version serial = %+v, want serial 2", info)
	}
	if preview == nil || preview.Action != "remove" {
		t.Fatalf("preview = %+v, want remove", preview)
	}

	raw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	current, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	assertStateHasID(t, current, "aws_vpc.main", "vpc-1")
	if loc, err := findResourceInstance(current, "aws_subnet.private"); err != nil {
		t.Fatalf("find removed subnet: %v", err)
	} else if loc != nil {
		t.Fatalf("aws_subnet.private still present after remove")
	}

	rows, err := pool.Query(ctx, `
		SELECT address, create_serial, COALESCE(delete_serial, 0)
		FROM resources
		WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')
		ORDER BY address, create_serial
	`)
	if err != nil {
		t.Fatalf("query resources: %v", err)
	}
	defer rows.Close()

	type row struct {
		address      string
		createSerial int64
		deleteSerial int64
	}
	var got []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.address, &item.createSerial, &item.deleteSerial); err != nil {
			t.Fatalf("scan resource row: %v", err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate resources: %v", err)
	}

	want := []row{
		{address: "aws_subnet.private", createSerial: 1, deleteSerial: 2},
		{address: "aws_vpc.main", createSerial: 1, deleteSerial: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("resource row count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReplayResourceVersion_RestoresClosedResourceAsNewOpenRow(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if err := s.WriteState(ctx, "qtest", "", seedStateEngineMutationStateRaw(t), "seed", "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if _, _, err := s.ApplyRemoveResourceCurrent(ctx, "qtest", "aws_subnet.private", "tester"); err != nil {
		t.Fatalf("ApplyRemoveResourceCurrent: %v", err)
	}

	info, preview, err := s.ReplayResourceVersion(ctx, "qtest", "aws_subnet.private", "@1", "tester")
	if err != nil {
		t.Fatalf("ReplayResourceVersion: %v", err)
	}
	if info == nil || info.Serial != 3 {
		t.Fatalf("version serial = %+v, want serial 3", info)
	}
	if preview == nil || preview.Action != "restore" {
		t.Fatalf("preview = %+v, want restore", preview)
	}

	raw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	current, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	assertStateHasID(t, current, "aws_subnet.private", "subnet-1")

	rows, err := pool.Query(ctx, `
		SELECT address, create_serial, COALESCE(delete_serial, 0)
		FROM resources
		WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')
		ORDER BY address, create_serial
	`)
	if err != nil {
		t.Fatalf("query resources: %v", err)
	}
	defer rows.Close()

	type row struct {
		address      string
		createSerial int64
		deleteSerial int64
	}
	var got []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.address, &item.createSerial, &item.deleteSerial); err != nil {
			t.Fatalf("scan resource row: %v", err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate resources: %v", err)
	}

	want := []row{
		{address: "aws_subnet.private", createSerial: 1, deleteSerial: 2},
		{address: "aws_subnet.private", createSerial: 3, deleteSerial: 0},
		{address: "aws_vpc.main", createSerial: 1, deleteSerial: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("resource row count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	var source string
	if err := pool.QueryRow(ctx, `
		SELECT source
		FROM state_versions
		WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')
		  AND serial = 3
	`).Scan(&source); err != nil {
		t.Fatalf("read version source: %v", err)
	}
	if source != "resource-rollback:aws_subnet.private" {
		t.Fatalf("version source = %q, want resource-rollback:aws_subnet.private", source)
	}
}

func TestWriteStateDeltaForApply_ReprojectsOnlySelectedRows(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	raw := seedStateEngineMutationStateRaw(t)
	if err := s.WriteState(ctx, "qtest", "", raw, "seed", "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	st, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse seed state: %v", err)
	}
	vpc, err := findResourceInstance(st, "aws_vpc.main")
	if err != nil {
		t.Fatalf("find vpc: %v", err)
	}
	if vpc == nil {
		t.Fatal("aws_vpc.main missing in seed state")
	}
	var attrs map[string]any
	if err := json.Unmarshal(vpc.Instance.Attributes, &attrs); err != nil {
		t.Fatalf("unmarshal vpc attrs: %v", err)
	}
	attrs["id"] = "vpc-2"
	updatedAttrs, err := json.Marshal(attrs)
	if err != nil {
		t.Fatalf("marshal updated attrs: %v", err)
	}
	resource := st.Resources[vpc.ResourceIndex]
	resource.Instances = append([]tfstate.ResourceInstance(nil), resource.Instances...)
	resource.Instances[vpc.InstanceIndex].Attributes = updatedAttrs
	st.Resources[vpc.ResourceIndex] = resource

	subnet, err := findResourceInstance(st, "aws_subnet.private")
	if err != nil {
		t.Fatalf("find subnet: %v", err)
	}
	if subnet == nil {
		t.Fatal("aws_subnet.private missing in seed state")
	}
	var subnetAttrs map[string]any
	if err := json.Unmarshal(subnet.Instance.Attributes, &subnetAttrs); err != nil {
		t.Fatalf("unmarshal subnet attrs: %v", err)
	}
	subnetAttrs["id"] = "subnet-999"
	updatedSubnetAttrs, err := json.Marshal(subnetAttrs)
	if err != nil {
		t.Fatalf("marshal updated subnet attrs: %v", err)
	}
	subnetResource := st.Resources[subnet.ResourceIndex]
	subnetResource.Instances = append([]tfstate.ResourceInstance(nil), subnetResource.Instances...)
	subnetResource.Instances[subnet.InstanceIndex].Attributes = updatedSubnetAttrs
	st.Resources[subnet.ResourceIndex] = subnetResource
	st.Serial = 2

	updatedRaw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal updated state: %v", err)
	}
	if err := s.WriteStateDeltaForApply(ctx, "qtest", "apply-delta", 1, updatedRaw, "state-engine-apply", "tester", []string{"aws_vpc.main"}); err != nil {
		t.Fatalf("WriteStateDeltaForApply: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT address, create_serial, COALESCE(delete_serial, 0), attributes::text
		FROM resources
		WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')
		ORDER BY address, create_serial
	`)
	if err != nil {
		t.Fatalf("query resources: %v", err)
	}
	defer rows.Close()

	type row struct {
		address      string
		createSerial int64
		deleteSerial int64
		attrs        string
	}
	var got []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.address, &item.createSerial, &item.deleteSerial, &item.attrs); err != nil {
			t.Fatalf("scan resource row: %v", err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate resources: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("resource row count = %d, want 3 (%+v)", len(got), got)
	}
	if got[0].address != "aws_subnet.private" || got[0].createSerial != 1 || got[0].deleteSerial != 0 {
		t.Fatalf("untouched subnet row = %+v, want one still-open row", got[0])
	}
	if got[1].address != "aws_vpc.main" || got[1].createSerial != 1 || got[1].deleteSerial != 2 {
		t.Fatalf("old vpc row = %+v, want closed at serial 2", got[1])
	}
	if got[2].address != "aws_vpc.main" || got[2].createSerial != 2 || got[2].deleteSerial != 0 {
		t.Fatalf("new vpc row = %+v, want new open row at serial 2", got[2])
	}
	if !strings.Contains(got[2].attrs, `"vpc-2"`) {
		t.Fatalf("new vpc attrs = %s, want updated id", got[2].attrs)
	}

	currentRaw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	current, err := tfstate.Parse(currentRaw)
	if err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	assertStateHasID(t, current, "aws_vpc.main", "vpc-2")
	assertStateHasID(t, current, "aws_subnet.private", "subnet-1")
}

func TestWriteStateEngineDeltaForApply_CommitsNarrowPayload(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	raw := seedStateEngineMutationStateRaw(t)
	if err := s.WriteState(ctx, "qtest", "", raw, "seed", "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	st, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse seed state: %v", err)
	}
	vpc, err := findResourceInstance(st, "aws_vpc.main")
	if err != nil {
		t.Fatalf("find vpc: %v", err)
	}
	if vpc == nil {
		t.Fatal("aws_vpc.main missing in seed state")
	}
	var attrs map[string]any
	if err := json.Unmarshal(vpc.Instance.Attributes, &attrs); err != nil {
		t.Fatalf("unmarshal vpc attrs: %v", err)
	}
	attrs["id"] = "vpc-3"
	updatedAttrs, err := json.Marshal(attrs)
	if err != nil {
		t.Fatalf("marshal updated vpc attrs: %v", err)
	}

	vpcResource := st.Resources[vpc.ResourceIndex]
	vpcResource.Instances = append([]tfstate.ResourceInstance(nil), vpcResource.Instances...)
	vpcResource.Instances[vpc.InstanceIndex].Attributes = updatedAttrs

	if err := s.WriteStateEngineDeltaForApply(ctx, "qtest", "apply-native-delta", 1, StateEngineDeltaCommit{
		TerraformVersion: st.TerraformVersion,
		Lineage:          st.Lineage,
		OutputWrites:     st.Outputs,
		CheckResults:     st.CheckResults,
		Resources:        []tfstate.Resource{vpcResource},
		WriteSet:         []string{"aws_vpc.main"},
	}, "state-engine-apply", "tester"); err != nil {
		t.Fatalf("WriteStateEngineDeltaForApply: %v", err)
	}

	currentRaw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	current, err := tfstate.Parse(currentRaw)
	if err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	assertStateHasID(t, current, "aws_vpc.main", "vpc-3")
	assertStateHasID(t, current, "aws_subnet.private", "subnet-1")
}

func TestWriteStateEngineDeltaForApply_MergesOutputDelta(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	raw := seedStateEngineMutationStateRaw(t)
	if err := s.WriteState(ctx, "qtest", "", raw, "seed", "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if err := s.WriteStateEngineDeltaForApply(ctx, "qtest", "apply-output-delta", 1, StateEngineDeltaCommit{
		TerraformVersion: "1.13.4",
		Lineage:          "c4a1f2e0-aaaa-bbbb-cccc-123456789abc",
		OutputWrites: map[string]tfstate.Output{
			"new_output": {
				Value: json.RawMessage(`"hello"`),
				Type:  json.RawMessage(`"string"`),
			},
		},
	}, "state-engine-apply", "tester"); err != nil {
		t.Fatalf("WriteStateEngineDeltaForApply output delta: %v", err)
	}

	currentRaw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	current, err := tfstate.Parse(currentRaw)
	if err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	if len(current.Outputs) != 1 {
		t.Fatalf("outputs len=%d want 1 (%v)", len(current.Outputs), current.Outputs)
	}
	if out, ok := current.Outputs["new_output"]; !ok {
		t.Fatalf("missing new_output in %v", current.Outputs)
	} else if string(out.Value) != `"hello"` {
		t.Fatalf("new_output value=%s want \"hello\"", string(out.Value))
	}
}
