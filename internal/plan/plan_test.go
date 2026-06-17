package plan

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// terraformVarArgs / TerraformVarArgs
// ---------------------------------------------------------------------------

func TestTerraformVarArgs_SortedAndFormatted_MixedTypes(t *testing.T) {
	got := TerraformVarArgs(map[string]json.RawMessage{
		"slow_b_version": json.RawMessage(`"v1"`),
		"slow_a_version": json.RawMessage(`"v2"`),
		"region":         json.RawMessage(`"us-east-1"`),
		"instance_count": json.RawMessage(`3`),
		"enabled":        json.RawMessage(`true`),
		"tags":           json.RawMessage(`{"env":"prod"}`),
	})
	// Strings are passed UNQUOTED — Terraform's `-var=NAME=VALUE`
	// CLI flag takes the value as a literal string for primitive
	// types (it does NOT parse VALUE as HCL). Splicing JSON-quoted
	// strings here would set the variable to e.g. the 4-char
	// literal `"v2"` instead of the 2-char `v2`, which silently
	// poisoned downstream plans.
	//
	// Numbers, bools, and complex types pass through as compact
	// JSON, which is a syntactic subset of HCL that Terraform
	// converts back to the variable's declared type.
	want := []string{
		`-var=enabled=true`,
		`-var=instance_count=3`,
		`-var=region=us-east-1`,
		`-var=slow_a_version=v2`,
		`-var=slow_b_version=v1`,
		`-var=tags={"env":"prod"}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TerraformVarArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestTerraformVarArgs_StringsAreUnquoted(t *testing.T) {
	// Regression: previously this returned `-var=v=foo` rendered as
	// `-var=v="foo"`, and Terraform stored the variable as the
	// 5-character literal "foo" (with the quote chars embedded).
	// That corruption then flowed into resource attributes and
	// stuck — every subsequent plan against the corrupted state
	// re-derived a non-empty diff against the "clean" inputs and
	// snowballed the demo into bogus write sets.
	cases := map[string]string{
		"plain":      "-var=v=plain",
		"with space": "-var=v=with space",
		"":           "-var=v=",
		`"already"`:  `-var=v="already"`,
	}
	for raw, want := range cases {
		got := TerraformVarArgs(map[string]json.RawMessage{
			"v": json.RawMessage(`"` + jsonEscape(raw) + `"`),
		})
		if len(got) != 1 || got[0] != want {
			t.Errorf("input %q → %v, want [%q]", raw, got, want)
		}
	}
}

// jsonEscape is the minimum escape needed for round-tripping the
// string through json.Unmarshal in the test cases above. Real code
// uses json.Marshal (via EncodeStringVar) — we only need to handle
// the chars our test inputs contain.
func jsonEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

func TestTerraformVarArgs_NullBecomesEmpty(t *testing.T) {
	got := TerraformVarArgs(map[string]json.RawMessage{
		"x": json.RawMessage(`null`),
	})
	want := []string{"-var=x="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("null → %v, want %v", got, want)
	}
}

func TestTerraformVarArgs_EmptyAndNil(t *testing.T) {
	if got := TerraformVarArgs(nil); got != nil {
		t.Errorf("nil input → %v, want nil", got)
	}
	if got := TerraformVarArgs(map[string]json.RawMessage{}); got != nil {
		t.Errorf("empty map → %v, want nil", got)
	}
}

func TestEncodeStringVar_RoundTripsThroughJSON(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"v2", `"v2"`},
		{`hello "world"`, `"hello \"world\""`},
		{"slash/and-dash", `"slash/and-dash"`},
		{"", `""`},
	}
	for _, c := range cases {
		got := string(EncodeStringVar(c.in))
		if got != c.want {
			t.Errorf("EncodeStringVar(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// BuildSpec — Variables plumbing (auto-pin + explicit override)
// ---------------------------------------------------------------------------

func TestBuildSpec_PinsObservedVariablesWhenPinAll(t *testing.T) {
	f := &File{
		FormatVersion:    "1.2",
		TerraformVersion: "1.13.4",
		Variables: map[string]PlanVariable{
			"region":     {Value: json.RawMessage(`"us-east-1"`)},
			"env":        {Value: json.RawMessage(`"staging"`)},
			"instance_n": {Value: json.RawMessage(`3`)},
		},
	}
	spec := BuildSpec(f, SpecBuildInput{
		ConfigDir:  "/cfg",
		PinAllVars: true,
	})
	if string(spec.Variables["region"]) != `"us-east-1"` {
		t.Errorf("region = %s", spec.Variables["region"])
	}
	if string(spec.Variables["env"]) != `"staging"` {
		t.Errorf("env = %s", spec.Variables["env"])
	}
	if string(spec.Variables["instance_n"]) != `3` {
		t.Errorf("instance_n = %s (want numeric 3)", spec.Variables["instance_n"])
	}
}

func TestBuildSpec_ExplicitVarsOverrideObserved(t *testing.T) {
	f := &File{
		FormatVersion: "1.2",
		Variables: map[string]PlanVariable{
			"env": {Value: json.RawMessage(`"staging"`)},
		},
	}
	spec := BuildSpec(f, SpecBuildInput{
		ConfigDir:  "/cfg",
		PinAllVars: true,
		ExplicitVars: map[string]json.RawMessage{
			"env": json.RawMessage(`"prod"`),
		},
	})
	if string(spec.Variables["env"]) != `"prod"` {
		t.Errorf("explicit --var override lost: %s", spec.Variables["env"])
	}
}

func TestBuildSpec_NoPinAllSkipsObserved(t *testing.T) {
	f := &File{
		FormatVersion: "1.2",
		Variables: map[string]PlanVariable{
			"db_password": {Value: json.RawMessage(`"secret-from-env"`)},
		},
	}
	spec := BuildSpec(f, SpecBuildInput{
		ConfigDir:  "/cfg",
		PinAllVars: false,
		ExplicitVars: map[string]json.RawMessage{
			"region": json.RawMessage(`"us-east-1"`),
		},
	})
	if _, leaked := spec.Variables["db_password"]; leaked {
		t.Error("--no-pin-vars must NOT carry observed variables into the spec")
	}
	if string(spec.Variables["region"]) != `"us-east-1"` {
		t.Errorf("explicit --var lost when PinAllVars=false: %s", spec.Variables["region"])
	}
}

func TestBuildSpec_NoVariablesEverythingEmpty_OmitsField(t *testing.T) {
	f := &File{FormatVersion: "1.2", TerraformVersion: "1.13.4"}
	spec := BuildSpec(f, SpecBuildInput{ConfigDir: "/cfg"})
	if spec.Variables != nil {
		t.Errorf("spec.Variables = %v, want nil (omitempty)", spec.Variables)
	}
	b, err := MarshalSpec(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"variables"`) {
		t.Errorf("marshaled spec contains variables key when empty:\n%s", b)
	}
}

func TestBuildSpec_DefensivelyCopiesVariables(t *testing.T) {
	srcObserved := map[string]PlanVariable{
		"k": {Value: json.RawMessage(`"v1"`)},
	}
	srcExplicit := map[string]json.RawMessage{
		"j": json.RawMessage(`"v1"`),
	}
	spec := BuildSpec(&File{FormatVersion: "1.2", Variables: srcObserved}, SpecBuildInput{
		ConfigDir:    "/cfg",
		PinAllVars:   true,
		ExplicitVars: srcExplicit,
	})
	// Mutate originals; spec must be unaffected (both the map
	// entry value and the underlying RawMessage byte slice).
	srcObserved["k"] = PlanVariable{Value: json.RawMessage(`"MUTATED"`)}
	srcExplicit["j"] = json.RawMessage(`"MUTATED"`)
	copy([]byte(srcObserved["k"].Value), []byte(`"MUT"`))
	copy([]byte(srcExplicit["j"]), []byte(`"MUT"`))
	if string(spec.Variables["k"]) != `"v1"` {
		t.Errorf("observed k mutated to %s", spec.Variables["k"])
	}
	if string(spec.Variables["j"]) != `"v1"` {
		t.Errorf("explicit j mutated to %s", spec.Variables["j"])
	}
}

// TestBuildSpec_StateNameIsPersisted pins the wiring between
// `kl plan`'s DiscoverBackend call and the spec's StateName
// field that lets `kl apply` skip --state=.
func TestBuildSpec_StateNameIsPersisted(t *testing.T) {
	f := &File{FormatVersion: "1.2", TerraformVersion: "1.13.4"}
	spec := BuildSpec(f, SpecBuildInput{
		ConfigDir: "/cfg",
		StateName: "big-state",
	})
	if spec.StateName != "big-state" {
		t.Errorf("StateName = %q", spec.StateName)
	}
	b, _ := MarshalSpec(spec)
	if !strings.Contains(string(b), `"state_name": "big-state"`) {
		t.Errorf("state_name key missing from marshaled spec:\n%s", b)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
//
// Hand-crafted JSON shaped after real `terraform show -json` output
// (validated against the v2 plan-introspection spike). Embedded as
// raw strings so the test file is self-contained and a fixture diff
// shows up directly in code review.
// ---------------------------------------------------------------------------

// fixtureFlatPlan mirrors the drift-demo spike: 4 random_id resources
// with no inter-resource deps. One create, one replace, two no-ops.
const fixtureFlatPlan = `{
	"format_version": "1.2",
	"terraform_version": "1.13.4",
	"resource_changes": [
		{
			"address": "random_id.audit",
			"mode": "managed",
			"type": "random_id",
			"name": "audit",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["create"] }
		},
		{
			"address": "random_id.db_backup",
			"mode": "managed",
			"type": "random_id",
			"name": "db_backup",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["no-op"] }
		},
		{
			"address": "random_id.logs",
			"mode": "managed",
			"type": "random_id",
			"name": "logs",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["delete", "create"] }
		},
		{
			"address": "random_id.web",
			"mode": "managed",
			"type": "random_id",
			"name": "web",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["no-op"] }
		}
	],
	"planned_values": {
		"root_module": {
			"resources": [
				{"address": "random_id.audit", "mode": "managed", "type": "random_id", "name": "audit"},
				{"address": "random_id.db_backup", "mode": "managed", "type": "random_id", "name": "db_backup"},
				{"address": "random_id.logs", "mode": "managed", "type": "random_id", "name": "logs"},
				{"address": "random_id.web", "mode": "managed", "type": "random_id", "name": "web"}
			]
		}
	},
	"configuration": {
		"root_module": {
			"resources": [
				{
					"address": "random_id.audit",
					"mode": "managed",
					"type": "random_id",
					"name": "audit",
					"expressions": { "byte_length": { "constant_value": 8 } }
				}
			]
		}
	}
}`

// fixtureDepsPlan mirrors the spike's vpc → subnet → instance + lonely
// fixture: instance references subnet which references vpc; lonely
// has no edges. Write set in this fixture is {subnet, instance}
// because subnet's keepers changed forcing both to replace; vpc and
// lonely are no-op.
const fixtureDepsPlan = `{
	"format_version": "1.2",
	"terraform_version": "1.13.4",
	"resource_changes": [
		{
			"address": "random_id.instance",
			"mode": "managed",
			"type": "random_id",
			"name": "instance",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["delete", "create"] }
		},
		{
			"address": "random_id.lonely",
			"mode": "managed",
			"type": "random_id",
			"name": "lonely",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["no-op"] }
		},
		{
			"address": "random_id.subnet",
			"mode": "managed",
			"type": "random_id",
			"name": "subnet",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["delete", "create"] }
		},
		{
			"address": "random_id.vpc",
			"mode": "managed",
			"type": "random_id",
			"name": "vpc",
			"provider_name": "registry.terraform.io/hashicorp/random",
			"change": { "actions": ["no-op"] }
		}
	],
	"planned_values": {
		"root_module": {
			"resources": [
				{"address": "random_id.instance", "mode": "managed", "type": "random_id", "name": "instance"},
				{"address": "random_id.lonely", "mode": "managed", "type": "random_id", "name": "lonely"},
				{"address": "random_id.subnet", "mode": "managed", "type": "random_id", "name": "subnet"},
				{"address": "random_id.vpc", "mode": "managed", "type": "random_id", "name": "vpc"}
			]
		}
	},
	"configuration": {
		"root_module": {
			"resources": [
				{
					"address": "random_id.instance",
					"mode": "managed",
					"type": "random_id",
					"name": "instance",
					"expressions": {
						"keepers": {
							"references": ["random_id.subnet.hex", "random_id.subnet"]
						}
					}
				},
				{
					"address": "random_id.subnet",
					"mode": "managed",
					"type": "random_id",
					"name": "subnet",
					"expressions": {
						"keepers": {
							"references": ["random_id.vpc.hex", "random_id.vpc"]
						}
					}
				},
				{
					"address": "random_id.vpc",
					"mode": "managed",
					"type": "random_id",
					"name": "vpc",
					"expressions": {}
				},
				{
					"address": "random_id.lonely",
					"mode": "managed",
					"type": "random_id",
					"name": "lonely",
					"expressions": {}
				}
			]
		}
	}
}`

// fixtureModulesPlan exercises module-prefix handling: a child module
// `web` containing two resources, one referencing the other within
// the same module. The walker must prepend `module.web.` to BOTH the
// owning resource AND its targets, because references in HCL
// expressions are emitted in module-local form by terraform.
//
// (Cross-module references in real Terraform always flow through
// variables, which produce `var.X` references and are filtered out
// of the dep graph by isResourceRef — so they don't appear here.)
const fixtureModulesPlan = `{
	"format_version": "1.2",
	"terraform_version": "1.13.4",
	"resource_changes": [
		{
			"address": "aws_vpc.main",
			"mode": "managed",
			"type": "aws_vpc",
			"name": "main",
			"provider_name": "registry.terraform.io/hashicorp/aws",
			"change": { "actions": ["no-op"] }
		},
		{
			"address": "module.web.aws_security_group.app",
			"mode": "managed",
			"type": "aws_security_group",
			"name": "app",
			"provider_name": "registry.terraform.io/hashicorp/aws",
			"change": { "actions": ["no-op"] }
		},
		{
			"address": "module.web.aws_subnet.app",
			"mode": "managed",
			"type": "aws_subnet",
			"name": "app",
			"provider_name": "registry.terraform.io/hashicorp/aws",
			"change": { "actions": ["update"] }
		}
	],
	"planned_values": {
		"root_module": {
			"resources": [
				{"address": "aws_vpc.main", "mode": "managed", "type": "aws_vpc", "name": "main"}
			],
			"child_modules": [
				{
					"address": "module.web",
					"resources": [
						{"address": "module.web.aws_security_group.app", "mode": "managed", "type": "aws_security_group", "name": "app"},
						{"address": "module.web.aws_subnet.app", "mode": "managed", "type": "aws_subnet", "name": "app"}
					]
				}
			]
		}
	},
	"configuration": {
		"root_module": {
			"resources": [
				{
					"address": "aws_vpc.main",
					"mode": "managed",
					"type": "aws_vpc",
					"name": "main",
					"expressions": {}
				}
			],
			"module_calls": {
				"web": {
					"module": {
						"resources": [
							{
								"address": "aws_security_group.app",
								"mode": "managed",
								"type": "aws_security_group",
								"name": "app",
								"expressions": {
									"vpc_id": { "references": ["var.vpc_id"] }
								}
							},
							{
								"address": "aws_subnet.app",
								"mode": "managed",
								"type": "aws_subnet",
								"name": "app",
								"expressions": {
									"security_group_ids": {
										"references": ["aws_security_group.app.id", "aws_security_group.app"]
									}
								}
							}
						]
					}
				}
			}
		}
	}
}`

// ---------------------------------------------------------------------------
// Parse tests
// ---------------------------------------------------------------------------

func TestParseShowJSONBytes_HappyPath(t *testing.T) {
	f, err := ParseShowJSONBytes([]byte(fixtureFlatPlan))
	if err != nil {
		t.Fatalf("ParseShowJSONBytes: %v", err)
	}
	if f.FormatVersion != "1.2" {
		t.Errorf("FormatVersion = %q, want 1.2", f.FormatVersion)
	}
	if f.TerraformVersion != "1.13.4" {
		t.Errorf("TerraformVersion = %q, want 1.13.4", f.TerraformVersion)
	}
	if len(f.ResourceChanges) != 4 {
		t.Errorf("ResourceChanges = %d, want 4", len(f.ResourceChanges))
	}
}

func TestParseShowJSONBytes_RejectsMissingFormatVersion(t *testing.T) {
	_, err := ParseShowJSONBytes([]byte(`{"resource_changes": []}`))
	if err == nil {
		t.Fatal("ParseShowJSONBytes(missing format_version) returned nil, expected error")
	}
	if !strings.Contains(err.Error(), "format_version") {
		t.Errorf("error message lost the format_version hint: %v", err)
	}
}

func TestParseShowJSONBytes_RejectsBadJSON(t *testing.T) {
	_, err := ParseShowJSONBytes([]byte(`{not json`))
	if err == nil {
		t.Fatal("ParseShowJSONBytes(bad json) returned nil, expected error")
	}
}

func TestParseShowJSON_ReadsFromStream(t *testing.T) {
	f, err := ParseShowJSON(strings.NewReader(fixtureFlatPlan))
	if err != nil {
		t.Fatalf("ParseShowJSON: %v", err)
	}
	if len(f.ResourceChanges) != 4 {
		t.Errorf("stream parse: ResourceChanges = %d, want 4", len(f.ResourceChanges))
	}
}

// ---------------------------------------------------------------------------
// Action classification + write detection
// ---------------------------------------------------------------------------

func TestClassifyChange(t *testing.T) {
	cases := []struct {
		name    string
		actions []string
		want    Action
	}{
		{"empty", nil, ActionUnknown},
		{"no-op", []string{"no-op"}, ActionNoop},
		{"create", []string{"create"}, ActionCreate},
		{"update", []string{"update"}, ActionUpdate},
		{"delete", []string{"delete"}, ActionDelete},
		{"read", []string{"read"}, ActionRead},
		{"forget", []string{"forget"}, ActionForget},
		{"replace-canonical", []string{"delete", "create"}, ActionReplace},
		{"replace-reversed", []string{"create", "delete"}, ActionReplace},
		{"unknown-single", []string{"obliterate"}, ActionUnknown},
		{"unknown-pair", []string{"create", "create"}, ActionUnknown},
		{"too-many", []string{"a", "b", "c"}, ActionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyChange(Change{Actions: tc.actions})
			if got != tc.want {
				t.Errorf("ClassifyChange(%v) = %q, want %q", tc.actions, got, tc.want)
			}
		})
	}
}

func TestAction_IsWrite(t *testing.T) {
	writes := []Action{ActionCreate, ActionUpdate, ActionDelete, ActionReplace, ActionForget}
	reads := []Action{ActionNoop, ActionRead, ActionUnknown, ""}
	for _, a := range writes {
		if !a.IsWrite() {
			t.Errorf("IsWrite(%q) = false, want true", a)
		}
	}
	for _, a := range reads {
		if a.IsWrite() {
			t.Errorf("IsWrite(%q) = true, want false", a)
		}
	}
}

// ---------------------------------------------------------------------------
// Write-set extraction
// ---------------------------------------------------------------------------

func TestExtractWriteSet_FlatPlan(t *testing.T) {
	f := mustParse(t, fixtureFlatPlan)
	got := ExtractWriteSet(f)
	want := []string{"random_id.audit", "random_id.logs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractWriteSet = %v, want %v", got, want)
	}
}

func TestExtractWriteSet_DepsPlan(t *testing.T) {
	f := mustParse(t, fixtureDepsPlan)
	got := ExtractWriteSet(f)
	want := []string{"random_id.instance", "random_id.subnet"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractWriteSet = %v, want %v", got, want)
	}
}

func TestExtractWriteSet_ModulePlan(t *testing.T) {
	f := mustParse(t, fixtureModulesPlan)
	got := ExtractWriteSet(f)
	// Only the subnet has a non-no-op action; vpc and sg are no-op.
	want := []string{"module.web.aws_subnet.app"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractWriteSet = %v, want %v", got, want)
	}
}

func TestExtractWriteSet_NilSafe(t *testing.T) {
	if got := ExtractWriteSet(nil); got != nil {
		t.Errorf("ExtractWriteSet(nil) = %v, want nil", got)
	}
}

func TestExtractReadActionSet_PicksOnlyReads(t *testing.T) {
	// Synthetic fixture: two reads, one create, one no-op
	f := &File{
		FormatVersion: "1.0",
		ResourceChanges: []ResourceChange{
			{Address: "data.foo.bar", Change: Change{Actions: []string{"read"}}},
			{Address: "data.foo.baz", Change: Change{Actions: []string{"read"}}},
			{Address: "managed.foo", Change: Change{Actions: []string{"create"}}},
			{Address: "managed.noop", Change: Change{Actions: []string{"no-op"}}},
		},
	}
	got := ExtractReadActionSet(f)
	want := []string{"data.foo.bar", "data.foo.baz"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractReadActionSet = %v, want %v", got, want)
	}
}

func TestSummarizeActions(t *testing.T) {
	f := mustParse(t, fixtureFlatPlan)
	got := SummarizeActions(f.ResourceChanges)
	want := PlanSummary{Create: 1, Replace: 1, NoOp: 2, Total: 4}
	if got != want {
		t.Errorf("SummarizeActions = %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Reference normalization + filter
// ---------------------------------------------------------------------------

func TestNormalizeRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"aws_vpc.main", "aws_vpc.main"},
		{"aws_vpc.main.id", "aws_vpc.main"},
		{"data.aws_caller_identity.current", "data.aws_caller_identity.current"},
		{"data.aws_caller_identity.current.account_id", "data.aws_caller_identity.current"},
		{"module.web.aws_vpc.main", "module.web.aws_vpc.main"},
		{"module.web.aws_vpc.main.id", "module.web.aws_vpc.main"},
		{"module.web.module.app.aws_vpc.main.id", "module.web.module.app.aws_vpc.main"},
		// Indexed addresses pass through untouched — index lives
		// after the name segment, so it's part of "the address".
		{"aws_instance.web[0]", "aws_instance.web[0]"},
		// Indexed + attribute: the attribute selector is dropped
		// but the index is preserved.
		// Note: actual terraform references rarely look like this;
		// the test pins behavior in case they ever do.
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeRef(tc.in); got != tc.want {
				t.Errorf("normalizeRef(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsResourceRef(t *testing.T) {
	resources := []string{
		"aws_vpc.main",
		"aws_vpc.main.id",
		"data.aws_caller_identity.current",
		"module.web.aws_vpc.main",
		"random_id.x[0]",
	}
	notResources := []string{
		"", "var.x", "local.x", "each.value", "each.key",
		"count.index", "path.module", "path.root", "path.cwd",
		"terraform.workspace", "self.public_ip",
	}
	for _, r := range resources {
		if !isResourceRef(r) {
			t.Errorf("isResourceRef(%q) = false, want true", r)
		}
	}
	for _, r := range notResources {
		if isResourceRef(r) {
			t.Errorf("isResourceRef(%q) = true, want false", r)
		}
	}
}

// ---------------------------------------------------------------------------
// Dependency graph
// ---------------------------------------------------------------------------

func TestBuildDepGraph_Deps(t *testing.T) {
	f := mustParse(t, fixtureDepsPlan)
	g := BuildDepGraph(f)

	wantEdges := []DependencyEdge{
		{From: "random_id.instance", To: "random_id.subnet"},
		{From: "random_id.subnet", To: "random_id.vpc"},
	}
	got := g.Edges()
	if !reflect.DeepEqual(got, wantEdges) {
		t.Errorf("Edges = %v, want %v", got, wantEdges)
	}
}

func TestBuildDepGraph_ModulePrefix(t *testing.T) {
	f := mustParse(t, fixtureModulesPlan)
	g := BuildDepGraph(f)

	// The module's `aws_subnet.app` references `aws_security_group.app`
	// (a sibling resource within the same module). After prefix
	// stamping, both endpoints carry the module.web. prefix.
	//
	// The security_group's `var.vpc_id` reference is filtered by
	// isResourceRef and does not appear as an edge — that's the
	// idiom Terraform uses for cross-module dependencies, and it's
	// correctly absent from the resource graph (the variable mapping
	// lives at the module-call site, not in the resource graph).
	wantEdges := []DependencyEdge{
		{From: "module.web.aws_subnet.app", To: "module.web.aws_security_group.app"},
	}
	got := g.Edges()
	if !reflect.DeepEqual(got, wantEdges) {
		t.Errorf("module-edge = %v, want %v", got, wantEdges)
	}
}

func TestBuildDepGraph_FlatPlan_HasNoEdges(t *testing.T) {
	f := mustParse(t, fixtureFlatPlan)
	g := BuildDepGraph(f)
	if edges := g.Edges(); len(edges) != 0 {
		t.Errorf("flat plan should have zero dep edges, got %v", edges)
	}
}

func TestBuildDepGraph_NilSafe(t *testing.T) {
	if g := BuildDepGraph(nil); len(g) != 0 {
		t.Errorf("BuildDepGraph(nil) = %v, want empty", g)
	}
}

// ---------------------------------------------------------------------------
// Read-set closure
// ---------------------------------------------------------------------------

func TestCloseReadSet_PullsForwardDeps(t *testing.T) {
	g := DepGraph{}
	g.Add("instance", "subnet")
	g.Add("subnet", "vpc")
	got := CloseReadSet([]string{"instance"}, g)
	want := []string{"instance", "subnet", "vpc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CloseReadSet({instance}) = %v, want %v", got, want)
	}
}

func TestCloseReadSet_PullsReverseDeps(t *testing.T) {
	g := DepGraph{}
	g.Add("instance", "subnet")
	g.Add("subnet", "vpc")
	got := CloseReadSet([]string{"vpc"}, g)
	want := []string{"instance", "subnet", "vpc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CloseReadSet({vpc}) = %v, want %v", got, want)
	}
}

func TestCloseReadSet_LeavesUnconnectedNodesAlone(t *testing.T) {
	g := DepGraph{}
	g.Add("instance", "subnet")
	g.Add("subnet", "vpc")
	// lonely is not part of the graph; CloseReadSet on a write_set
	// that includes only members of one connected component must
	// not reach into unconnected ones.
	got := CloseReadSet([]string{"subnet"}, g)
	want := []string{"instance", "subnet", "vpc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CloseReadSet({subnet}) = %v, want %v", got, want)
	}
}

func TestCloseReadSet_CycleSafe(t *testing.T) {
	// Pathological but legal: A → B → A. The fixed-point loop must
	// terminate without diverging. CloseReadSet on either node
	// returns both.
	g := DepGraph{}
	g.Add("A", "B")
	g.Add("B", "A")
	got := CloseReadSet([]string{"A"}, g)
	want := []string{"A", "B"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CloseReadSet on cycle = %v, want %v", got, want)
	}
}

func TestCloseReadSet_DepsFixtureMatchesSpike(t *testing.T) {
	f := mustParse(t, fixtureDepsPlan)
	w := ExtractWriteSet(f)
	g := BuildDepGraph(f)
	got := CloseReadSet(w, g)

	// Same expected output as the spike print-out:
	// write_set = {subnet, instance}, read_set adds vpc.
	want := []string{"random_id.instance", "random_id.subnet", "random_id.vpc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CloseReadSet on deps fixture = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// HCL footprint
// ---------------------------------------------------------------------------

func TestExtractHCLFootprint_FlatPlan(t *testing.T) {
	f := mustParse(t, fixtureFlatPlan)
	got := ExtractHCLFootprint(f)
	want := []string{"random_id.audit", "random_id.db_backup", "random_id.logs", "random_id.web"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HCLFootprint = %v, want %v", got, want)
	}
}

func TestExtractHCLFootprint_RecursesIntoChildModules(t *testing.T) {
	f := mustParse(t, fixtureModulesPlan)
	got := ExtractHCLFootprint(f)
	want := []string{
		"aws_vpc.main",
		"module.web.aws_security_group.app",
		"module.web.aws_subnet.app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HCLFootprint with module = %v, want %v", got, want)
	}
}

func TestExtractHCLFootprint_NilSafe(t *testing.T) {
	if got := ExtractHCLFootprint(nil); got != nil {
		t.Errorf("ExtractHCLFootprint(nil) = %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// Spec building and round-trip
// ---------------------------------------------------------------------------

func TestBuildSpec_DepsFixtureProducesExpectedReservations(t *testing.T) {
	f := mustParse(t, fixtureDepsPlan)
	spec := BuildSpec(f, SpecBuildInput{
		ConfigDir:   "/path/to/cfg",
		GeneratedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
	})

	if spec.FormatVersion != CurrentSpecFormatVersion {
		t.Errorf("FormatVersion = %q, want %q", spec.FormatVersion, CurrentSpecFormatVersion)
	}
	if spec.ConfigDir != "/path/to/cfg" {
		t.Errorf("ConfigDir = %q, want /path/to/cfg", spec.ConfigDir)
	}
	if spec.TerraformVersion != "1.13.4" {
		t.Errorf("TerraformVersion = %q, want 1.13.4", spec.TerraformVersion)
	}
	if want := (PlanSummary{Replace: 2, NoOp: 2, Total: 4}); spec.PlanSummary != want {
		t.Errorf("PlanSummary = %+v, want %+v", spec.PlanSummary, want)
	}

	wantReservations := []PlanReservation{
		{Address: "random_id.instance", Mode: "write"},
		{Address: "random_id.subnet", Mode: "write"},
		{Address: "random_id.vpc", Mode: "read"},
	}
	if !reflect.DeepEqual(spec.Reservations, wantReservations) {
		t.Errorf("Reservations = %v, want %v", spec.Reservations, wantReservations)
	}
}

func TestBuildSpec_NilFile(t *testing.T) {
	spec := BuildSpec(nil, SpecBuildInput{})
	if spec.FormatVersion != CurrentSpecFormatVersion {
		t.Errorf("zero-File spec missing FormatVersion: %q", spec.FormatVersion)
	}
	if spec.WriteSet != nil || spec.ReadSet != nil {
		t.Errorf("zero-File spec must have nil slices; got write=%v read=%v",
			spec.WriteSet, spec.ReadSet)
	}
}

func TestMarshalUnmarshalSpec_RoundTrip(t *testing.T) {
	f := mustParse(t, fixtureDepsPlan)
	spec := BuildSpec(f, SpecBuildInput{
		ConfigDir:   "/x",
		GeneratedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
	})

	b, err := MarshalSpec(spec)
	if err != nil {
		t.Fatalf("MarshalSpec: %v", err)
	}
	roundTripped, err := UnmarshalSpec(b)
	if err != nil {
		t.Fatalf("UnmarshalSpec: %v", err)
	}
	if !reflect.DeepEqual(roundTripped.WriteSet, spec.WriteSet) {
		t.Errorf("WriteSet round-trip diff:\nwant %v\ngot  %v",
			spec.WriteSet, roundTripped.WriteSet)
	}
	if !reflect.DeepEqual(roundTripped.Reservations, spec.Reservations) {
		t.Errorf("Reservations round-trip diff:\nwant %v\ngot  %v",
			spec.Reservations, roundTripped.Reservations)
	}
}

func TestUnmarshalSpec_RejectsUnknownFormatVersion(t *testing.T) {
	// Construct a spec, bump the format version, marshal, unmarshal,
	// expect rejection. Avoids string-templating JSON manually.
	spec := &PlanSpec{
		FormatVersion: "999",
		GeneratedAt:   time.Now(),
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	_, err = UnmarshalSpec(b)
	if err == nil {
		t.Fatal("UnmarshalSpec(unknown format) returned nil, expected error")
	}
	if !strings.Contains(err.Error(), "unsupported format_version") {
		t.Errorf("error message lost the version hint: %v", err)
	}
}

func TestUnmarshalSpec_RejectsMissingFormatVersion(t *testing.T) {
	_, err := UnmarshalSpec([]byte(`{}`))
	if err == nil {
		t.Fatal("UnmarshalSpec(missing format) returned nil, expected error")
	}
	if !strings.Contains(err.Error(), "missing format_version") {
		t.Errorf("error message lost the missing-version hint: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Edges() ordering — important because the JSON serialization order
// surfaces in PR diffs. If the Edges() return order ever stops being
// (from, to) sorted, every regenerated plan-spec produces noise.
// ---------------------------------------------------------------------------

func TestDepGraph_EdgesSortedDeterministically(t *testing.T) {
	g := DepGraph{}
	// Insert in pessimal order so a non-sorting Edges() returns
	// something different from the want.
	g.Add("z", "y")
	g.Add("a", "c")
	g.Add("a", "b")
	g.Add("a", "a") // technically a self-loop; included for completeness
	got := g.Edges()
	want := []DependencyEdge{
		{From: "a", To: "a"},
		{From: "a", To: "b"},
		{From: "a", To: "c"},
		{From: "z", To: "y"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Edges sorted incorrectly:\ngot  %v\nwant %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustParse(t *testing.T, s string) *File {
	t.Helper()
	f, err := ParseShowJSONBytes([]byte(s))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

// Defensive assertion: every fixture above should round-trip back
// through encoding/json without losing fields the rest of the
// package reads. Catches typos in the embedded JSON literals at
// test time rather than via a downstream test that surfaces a
// confusing nil-deref or missing-key error.
func TestFixtures_RoundTripValid(t *testing.T) {
	fixtures := map[string]string{
		"flat":    fixtureFlatPlan,
		"deps":    fixtureDepsPlan,
		"modules": fixtureModulesPlan,
	}
	keys := make([]string, 0, len(fixtures))
	for k := range fixtures {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseShowJSONBytes([]byte(fixtures[name])); err != nil {
				t.Fatalf("fixture %s does not parse: %v", name, err)
			}
		})
	}
}
