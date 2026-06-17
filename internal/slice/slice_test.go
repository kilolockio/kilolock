package slice

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fixtureTrunk is a hand-crafted v4 Terraform state with five
// resources covering managed/data/module modes and one count-indexed
// resource group. The slice tests filter this trunk in different
// ways to validate that the keep/drop logic preserves lineage,
// serial, outputs, and the per-group instance arrays.
const fixtureTrunk = `{
	"version": 4,
	"terraform_version": "1.13.4",
	"serial": 42,
	"lineage": "11111111-2222-3333-4444-555555555555",
	"outputs": {
		"vpc_id": { "value": "vpc-1", "type": "string" }
	},
	"resources": [
		{
			"mode": "managed",
			"type": "aws_vpc",
			"name": "main",
			"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			"instances": [
				{ "schema_version": 0, "attributes": { "id": "vpc-1" }, "sensitive_attributes": [] }
			]
		},
		{
			"mode": "data",
			"type": "aws_caller_identity",
			"name": "current",
			"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			"instances": [
				{ "schema_version": 0, "attributes": { "account_id": "123" }, "sensitive_attributes": [] }
			]
		},
		{
			"module": "module.web",
			"mode": "managed",
			"type": "aws_subnet",
			"name": "app",
			"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			"instances": [
				{ "schema_version": 0, "attributes": { "id": "subnet-1" }, "sensitive_attributes": [] }
			]
		},
		{
			"mode": "managed",
			"type": "aws_instance",
			"name": "fleet",
			"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			"each": "list",
			"instances": [
				{ "schema_version": 0, "index_key": 0, "attributes": { "id": "i-0" }, "sensitive_attributes": [] },
				{ "schema_version": 0, "index_key": 1, "attributes": { "id": "i-1" }, "sensitive_attributes": [] }
			]
		},
		{
			"mode": "managed",
			"type": "aws_lonely",
			"name": "untouched",
			"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			"instances": [
				{ "schema_version": 0, "attributes": { "id": "lonely-1" }, "sensitive_attributes": [] }
			]
		}
	]
}`

// ---------------------------------------------------------------------------
// Round-trip: parse + marshal + parse again should yield an
// equivalent state. Catches typos in the embedded fixture and any
// silent field loss in the TrunkState type definition.
// ---------------------------------------------------------------------------

func TestParseTrunkState_RoundTripPreservesEssentials(t *testing.T) {
	trunk, err := ParseTrunkState([]byte(fixtureTrunk))
	if err != nil {
		t.Fatalf("ParseTrunkState: %v", err)
	}
	if trunk.Serial != 42 {
		t.Errorf("Serial = %d, want 42", trunk.Serial)
	}
	if trunk.Lineage != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("Lineage = %q", trunk.Lineage)
	}
	if len(trunk.Resources) != 5 {
		t.Fatalf("Resources = %d, want 5", len(trunk.Resources))
	}
	if string(trunk.Outputs) == "" || !strings.Contains(string(trunk.Outputs), "vpc_id") {
		t.Errorf("Outputs lost or truncated: %s", trunk.Outputs)
	}

	b, err := MarshalTrunkState(trunk)
	if err != nil {
		t.Fatalf("MarshalTrunkState: %v", err)
	}
	got, err := ParseTrunkState(b)
	if err != nil {
		t.Fatalf("re-parse marshalled trunk: %v", err)
	}
	if !reflect.DeepEqual(trunk.Serial, got.Serial) || trunk.Lineage != got.Lineage {
		t.Errorf("round-trip drifted: serial/lineage mismatch")
	}
	if len(trunk.Resources) != len(got.Resources) {
		t.Errorf("resource count drifted: %d -> %d", len(trunk.Resources), len(got.Resources))
	}
}

// ---------------------------------------------------------------------------
// TrunkResource.Address() reconstruction
// ---------------------------------------------------------------------------

func TestTrunkResource_Address(t *testing.T) {
	cases := []struct {
		r    TrunkResource
		want string
	}{
		{TrunkResource{Mode: "managed", Type: "aws_vpc", Name: "main"}, "aws_vpc.main"},
		{TrunkResource{Mode: "data", Type: "aws_caller_identity", Name: "current"},
			"data.aws_caller_identity.current"},
		{TrunkResource{Module: "module.web", Mode: "managed", Type: "aws_subnet", Name: "app"},
			"module.web.aws_subnet.app"},
		// module + data combo: rare but legal
		{TrunkResource{Module: "module.web", Mode: "data", Type: "aws_iam_user", Name: "me"},
			"module.web.data.aws_iam_user.me"},
	}
	for _, tc := range cases {
		got := tc.r.Address()
		if got != tc.want {
			t.Errorf("Address(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// stripIndex
// ---------------------------------------------------------------------------

func TestStripIndex(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"aws_vpc.main", "aws_vpc.main"},
		{"aws_instance.web[0]", "aws_instance.web"},
		{`aws_instance.web["us-east"]`, "aws_instance.web"},
		// Module key with index — module key brackets must NOT be
		// trimmed (only the trailing per-instance index is dropped).
		// Worth a separate case because a naive lastIndexOf would
		// trim the wrong bracket pair.
		{`module.web["a"].aws_instance.app`, `module.web["a"].aws_instance.app`},
		{`module.web["a"].aws_instance.app[0]`, `module.web["a"].aws_instance.app`},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := stripIndex(tc.in); got != tc.want {
				t.Errorf("stripIndex(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IndexFootprintByGroup
// ---------------------------------------------------------------------------

func TestIndexFootprintByGroup_CollapsesIndexedSiblings(t *testing.T) {
	in := []string{
		"aws_instance.fleet[0]",
		"aws_instance.fleet[1]",
		"aws_instance.fleet[2]",
		"aws_vpc.main",
	}
	got := IndexFootprintByGroup(in)
	want := map[string]struct{}{
		"aws_instance.fleet": {},
		"aws_vpc.main":       {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("group index = %v, want %v", sortedKeys(got), sortedKeys(want))
	}
}

// ---------------------------------------------------------------------------
// Build — the main event. Each case asserts the slice contains
// exactly the expected resource set AND that lineage/serial/outputs
// pass through.
// ---------------------------------------------------------------------------

func TestBuild_KeepsOnlyFootprintMembers(t *testing.T) {
	trunk, err := ParseTrunkState([]byte(fixtureTrunk))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	footprint := map[string]struct{}{
		"aws_vpc.main":              {},
		"module.web.aws_subnet.app": {},
		"aws_instance.fleet":        {}, // pulls the count group as a whole
		// "aws_lonely.untouched" intentionally omitted
		// "data.aws_caller_identity.current" intentionally omitted
	}
	got, err := Build(trunk, footprint)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.Serial != trunk.Serial {
		t.Errorf("Serial drifted: %d -> %d", trunk.Serial, got.Serial)
	}
	if got.Lineage != trunk.Lineage {
		t.Errorf("Lineage drifted: %q -> %q", trunk.Lineage, got.Lineage)
	}
	if string(got.Outputs) != string(trunk.Outputs) {
		t.Errorf("Outputs drifted: %s -> %s", trunk.Outputs, got.Outputs)
	}

	gotAddrs := make([]string, 0, len(got.Resources))
	for _, r := range got.Resources {
		gotAddrs = append(gotAddrs, r.Address())
	}
	sort.Strings(gotAddrs)
	wantAddrs := []string{
		"aws_instance.fleet",
		"aws_vpc.main",
		"module.web.aws_subnet.app",
	}
	if !reflect.DeepEqual(gotAddrs, wantAddrs) {
		t.Errorf("slice resources = %v, want %v", gotAddrs, wantAddrs)
	}

	// The count-group survivor must still carry BOTH of its
	// instances. Slicing operates at the resource-group level, not
	// per instance — losing instances here would mean Terraform
	// re-plans the missing ones as `create`, which is the bug the
	// spike specifically called out.
	var fleet *TrunkResource
	for i, r := range got.Resources {
		if r.Address() == "aws_instance.fleet" {
			fleet = &got.Resources[i]
			break
		}
	}
	if fleet == nil {
		t.Fatal("aws_instance.fleet missing from slice")
	}
	var instances []map[string]any
	if err := json.Unmarshal(fleet.Instances, &instances); err != nil {
		t.Fatalf("decode fleet instances: %v", err)
	}
	if len(instances) != 2 {
		t.Errorf("fleet instances = %d, want 2 (count group must stay whole)", len(instances))
	}
}

func TestBuild_EmptyFootprintProducesEmptyResourceSlice(t *testing.T) {
	trunk, err := ParseTrunkState([]byte(fixtureTrunk))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := Build(trunk, map[string]struct{}{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.Resources) != 0 {
		t.Errorf("empty footprint kept %d resources", len(got.Resources))
	}
	// Serial / lineage / outputs still pass through — those are
	// state-file metadata, not per-resource.
	if got.Serial != trunk.Serial || got.Lineage != trunk.Lineage {
		t.Errorf("empty-footprint slice dropped metadata")
	}
}

func TestBuild_NilFootprintIsTreatedAsEmpty(t *testing.T) {
	trunk, err := ParseTrunkState([]byte(fixtureTrunk))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := Build(trunk, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.Resources) != 0 {
		t.Errorf("nil footprint kept %d resources, want 0", len(got.Resources))
	}
}

func TestBuild_NilTrunkRejected(t *testing.T) {
	_, err := Build(nil, map[string]struct{}{"x": {}})
	if err == nil {
		t.Fatal("Build(nil trunk) returned nil, expected error")
	}
}

func TestBuild_KeepsCheckResults(t *testing.T) {
	// Synthetic trunk with check_results — the field is rare but
	// present in newer states (post-1.5 preconditions/postconditions
	// machinery). Slice must preserve it; losing check_results would
	// confuse Terraform's plan logic on re-plan inside the apply dir.
	const trunkWithChecks = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "abc",
		"check_results": [
			{"object_kind": "resource", "config_addr": "aws_vpc.main", "status": "pass"}
		],
		"resources": []
	}`
	trunk, err := ParseTrunkState([]byte(trunkWithChecks))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := Build(trunk, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if string(got.CheckResults) == "" || !strings.Contains(string(got.CheckResults), "aws_vpc.main") {
		t.Errorf("CheckResults dropped or mangled: %s", got.CheckResults)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
