package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// roundTrip is the central helper used by most tests in this file:
// given a tftypes.Type and a JSON-shaped Go value, encode to msgpack,
// decode back, and verify the result equals the input.
//
// Equality is reflect.DeepEqual after a JSON round-trip on both sides.
// The JSON round-trip canonicalizes numeric types (int → float64) so
// inputs like int(0) compare equal to the float64(0) the decoder
// returns, mirroring how this code is actually used (always after
// json.Unmarshal).
func roundTrip(t *testing.T, typ tftypes.Type, in any) any {
	t.Helper()

	canonical := jsonNormalize(t, in)

	data, err := EncodeMsgpack(typ, canonical)
	if err != nil {
		t.Fatalf("EncodeMsgpack(%s, %v): %v", typ, in, err)
	}
	out, err := DecodeMsgpack(typ, data)
	if err != nil {
		t.Fatalf("DecodeMsgpack(%s, %d bytes): %v", typ, len(data), err)
	}

	got := jsonNormalize(t, out)
	if !reflect.DeepEqual(canonical, got) {
		ib, _ := json.MarshalIndent(canonical, "", "  ")
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("round-trip mismatch for type %s\n--- in ---\n%s\n--- got ---\n%s", typ, ib, gb)
	}
	return out
}

// jsonNormalize forces a value through encoding/json.Marshal +
// Unmarshal into any so that integer literals (which would otherwise
// fail equality against float64 values returned by the decoder)
// become float64s. Refresh inputs always come from json.Unmarshal in
// production, so this mirrors real usage.
func jsonNormalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return out
}

// --- AttributeType ----------------------------------------------------------

func TestAttributeType_PrimitiveCtyJSON(t *testing.T) {
	cases := []struct {
		name   string
		json   string
		wantIs tftypes.Type
	}{
		{"string", `"string"`, tftypes.String},
		{"bool", `"bool"`, tftypes.Bool},
		{"number", `"number"`, tftypes.Number},
		{"dynamic", `"dynamic"`, tftypes.DynamicPseudoType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, err := AttributeType(SchemaAttribute{Name: "x", Type: json.RawMessage(tc.json)})
			if err != nil {
				t.Fatalf("AttributeType: %v", err)
			}
			if !typ.Is(tc.wantIs) {
				t.Errorf("got %s, want %s", typ, tc.wantIs)
			}
		})
	}
}

func TestAttributeType_Collections(t *testing.T) {
	t.Run("list of string", func(t *testing.T) {
		typ, err := AttributeType(SchemaAttribute{Name: "x", Type: json.RawMessage(`["list","string"]`)})
		if err != nil {
			t.Fatalf("AttributeType: %v", err)
		}
		l, ok := typ.(tftypes.List)
		if !ok {
			t.Fatalf("got %T, want tftypes.List", typ)
		}
		if !l.ElementType.Is(tftypes.String) {
			t.Errorf("element: got %s, want String", l.ElementType)
		}
	})
	t.Run("map of number", func(t *testing.T) {
		typ, err := AttributeType(SchemaAttribute{Name: "x", Type: json.RawMessage(`["map","number"]`)})
		if err != nil {
			t.Fatalf("AttributeType: %v", err)
		}
		m, ok := typ.(tftypes.Map)
		if !ok {
			t.Fatalf("got %T, want tftypes.Map", typ)
		}
		if !m.ElementType.Is(tftypes.Number) {
			t.Errorf("element: got %s, want Number", m.ElementType)
		}
	})
}

func TestAttributeType_NestedType(t *testing.T) {
	attr := SchemaAttribute{
		Name: "outer",
		NestedType: &SchemaObject{
			Nesting: NestingList,
			Attributes: []SchemaAttribute{
				{Name: "inner", Type: json.RawMessage(`"string"`)},
			},
		},
	}
	typ, err := AttributeType(attr)
	if err != nil {
		t.Fatalf("AttributeType: %v", err)
	}
	l, ok := typ.(tftypes.List)
	if !ok {
		t.Fatalf("outer type: got %T, want List", typ)
	}
	o, ok := l.ElementType.(tftypes.Object)
	if !ok {
		t.Fatalf("inner type: got %T, want Object", l.ElementType)
	}
	if !o.AttributeTypes["inner"].Is(tftypes.String) {
		t.Errorf("inner.inner: got %s, want String", o.AttributeTypes["inner"])
	}
}

func TestAttributeType_MissingType(t *testing.T) {
	_, err := AttributeType(SchemaAttribute{Name: "x"})
	if err == nil {
		t.Fatal("expected error for missing Type, got nil")
	}
}

// --- BlockType -------------------------------------------------------------

func TestBlockType_NullResourceShape(t *testing.T) {
	// Approximates the schema null_resource ships with: id (computed
	// string) and triggers (optional map of string).
	block := &SchemaBlock{
		Attributes: []SchemaAttribute{
			{Name: "id", Type: json.RawMessage(`"string"`), Computed: true},
			{Name: "triggers", Type: json.RawMessage(`["map","string"]`), Optional: true},
		},
	}
	typ, err := BlockType(block)
	if err != nil {
		t.Fatalf("BlockType: %v", err)
	}
	obj, ok := typ.(tftypes.Object)
	if !ok {
		t.Fatalf("got %T, want tftypes.Object", typ)
	}
	if !obj.AttributeTypes["id"].Is(tftypes.String) {
		t.Errorf("id: got %s, want String", obj.AttributeTypes["id"])
	}
	m, ok := obj.AttributeTypes["triggers"].(tftypes.Map)
	if !ok {
		t.Fatalf("triggers: got %T, want Map", obj.AttributeTypes["triggers"])
	}
	if !m.ElementType.Is(tftypes.String) {
		t.Errorf("triggers element: got %s", m.ElementType)
	}
}

func TestBlockType_NestedBlock(t *testing.T) {
	block := &SchemaBlock{
		BlockTypes: []SchemaNestedBlock{
			{
				TypeName: "rule",
				Nesting:  NestingList,
				Block: &SchemaBlock{
					Attributes: []SchemaAttribute{
						{Name: "name", Type: json.RawMessage(`"string"`), Required: true},
					},
				},
			},
		},
	}
	typ, err := BlockType(block)
	if err != nil {
		t.Fatalf("BlockType: %v", err)
	}
	obj := typ.(tftypes.Object)
	list, ok := obj.AttributeTypes["rule"].(tftypes.List)
	if !ok {
		t.Fatalf("rule: got %T, want List", obj.AttributeTypes["rule"])
	}
	if _, ok := list.ElementType.(tftypes.Object); !ok {
		t.Fatalf("rule element: got %T, want Object", list.ElementType)
	}
}

func TestBlockType_NilBlock(t *testing.T) {
	_, err := BlockType(nil)
	if err == nil {
		t.Fatal("expected error for nil block, got nil")
	}
}

// --- Encode/Decode round-trips ----------------------------------------------

func TestRoundTrip_Primitives(t *testing.T) {
	roundTrip(t, tftypes.String, "hello")
	roundTrip(t, tftypes.String, "")
	roundTrip(t, tftypes.Bool, true)
	roundTrip(t, tftypes.Bool, false)
	roundTrip(t, tftypes.Number, 42.0)
	roundTrip(t, tftypes.Number, -3.14)
	roundTrip(t, tftypes.Number, 0.0)
}

func TestRoundTrip_Nulls(t *testing.T) {
	roundTrip(t, tftypes.String, nil)
	roundTrip(t, tftypes.Bool, nil)
	roundTrip(t, tftypes.Number, nil)
	roundTrip(t, tftypes.List{ElementType: tftypes.String}, nil)
	roundTrip(t, tftypes.Map{ElementType: tftypes.String}, nil)
	roundTrip(t, tftypes.Object{AttributeTypes: map[string]tftypes.Type{"x": tftypes.String}}, nil)
}

func TestRoundTrip_ListOfString(t *testing.T) {
	typ := tftypes.List{ElementType: tftypes.String}
	roundTrip(t, typ, []any{"a", "b", "c"})
	roundTrip(t, typ, []any{})
}

func TestRoundTrip_SetOfString(t *testing.T) {
	// Sets do not preserve order; compare as sorted slices via
	// JSON normalization, then sort both before equality. Skip the
	// helper for this case.
	typ := tftypes.Set{ElementType: tftypes.String}
	in := []any{"c", "a", "b"}
	data, err := EncodeMsgpack(typ, in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeMsgpack(typ, data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	arr, ok := out.([]any)
	if !ok {
		t.Fatalf("got %T, want []any", out)
	}
	if len(arr) != len(in) {
		t.Fatalf("len mismatch: got %d, want %d", len(arr), len(in))
	}
	have := map[string]bool{}
	for _, v := range arr {
		have[v.(string)] = true
	}
	for _, v := range in {
		if !have[v.(string)] {
			t.Errorf("set missing element %v", v)
		}
	}
}

func TestRoundTrip_MapOfString(t *testing.T) {
	typ := tftypes.Map{ElementType: tftypes.String}
	roundTrip(t, typ, map[string]any{"foo": "bar", "baz": "qux"})
	roundTrip(t, typ, map[string]any{})
}

func TestRoundTrip_Tuple(t *testing.T) {
	typ := tftypes.Tuple{ElementTypes: []tftypes.Type{
		tftypes.String, tftypes.Number, tftypes.Bool,
	}}
	roundTrip(t, typ, []any{"hello", 42.0, true})
}

func TestRoundTrip_Object_NullResourceShape(t *testing.T) {
	// Drives the same object type null_resource exposes on the wire:
	// id (string, computed) + triggers (map of string, optional).
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"id":       tftypes.String,
		"triggers": tftypes.Map{ElementType: tftypes.String},
	}}
	roundTrip(t, typ, map[string]any{
		"id":       "abc-123",
		"triggers": map[string]any{"version": "1", "purpose": "test"},
	})
	roundTrip(t, typ, map[string]any{
		"id":       nil,
		"triggers": nil,
	})
}

func TestRoundTrip_DeepNesting(t *testing.T) {
	inner := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"name":     tftypes.String,
		"weight":   tftypes.Number,
		"disabled": tftypes.Bool,
	}}
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"rules": tftypes.List{ElementType: inner},
		"meta":  tftypes.Map{ElementType: tftypes.String},
	}}
	in := map[string]any{
		"rules": []any{
			map[string]any{"name": "a", "weight": 1.0, "disabled": false},
			map[string]any{"name": "b", "weight": 2.5, "disabled": true},
		},
		"meta": map[string]any{"env": "test"},
	}
	roundTrip(t, typ, in)
}

func TestRoundTrip_OmittedObjectKeyEncodesAsNull(t *testing.T) {
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"present": tftypes.String,
		"missing": tftypes.String,
	}}
	in := map[string]any{"present": "hello"}
	data, err := EncodeMsgpack(typ, in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeMsgpack(typ, data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := out.(map[string]any)
	if m["present"] != "hello" {
		t.Errorf("present: got %v, want hello", m["present"])
	}
	if v, ok := m["missing"]; !ok || v != nil {
		t.Errorf("missing: got (%v, %v), want (nil, true)", v, ok)
	}
}

// --- Negative paths --------------------------------------------------------

func TestEncode_StringExpectsString(t *testing.T) {
	_, err := EncodeMsgpack(tftypes.String, 42.0)
	if err == nil || !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("want type mismatch, got %v", err)
	}
}

func TestEncode_ObjectRejectsExtraKey(t *testing.T) {
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"a": tftypes.String,
	}}
	_, err := EncodeMsgpack(typ, map[string]any{"a": "x", "b": "y"})
	if err == nil || !strings.Contains(err.Error(), "not in schema") {
		t.Fatalf("want schema rejection, got %v", err)
	}
}

func TestEncode_TupleRejectsLengthMismatch(t *testing.T) {
	typ := tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.String, tftypes.Number}}
	_, err := EncodeMsgpack(typ, []any{"only-one"})
	if err == nil || !strings.Contains(err.Error(), "tuple length mismatch") {
		t.Fatalf("want tuple length mismatch, got %v", err)
	}
}

func TestEncode_NilTypeIsError(t *testing.T) {
	_, err := EncodeMsgpack(nil, "x")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDecode_NilTypeIsError(t *testing.T) {
	_, err := DecodeMsgpack(nil, []byte{0xC0})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestDecode_AcceptsUnknown asserts that tftypes.UnknownValue decodes
// into our UnknownValue sentinel struct cleanly, which is required for
// parsing provider plans.
func TestDecode_AcceptsUnknown(t *testing.T) {
	tv := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	dv, err := tfprotov6.NewDynamicValue(tftypes.String, tv)
	if err != nil {
		t.Fatalf("synthesize unknown: %v", err)
	}
	out, err := DecodeMsgpack(tftypes.String, dv.MsgPack)
	if err != nil {
		t.Fatalf("decode unknown: %v", err)
	}
	if _, ok := out.(UnknownValue); !ok {
		t.Fatalf("want UnknownValue, got %T", out)
	}
}

// TestRoundTrip_DynamicPseudoType exercises the dynamic path: the
// schema declares "anything goes" and we encode primitives + nested
// shapes. tftypes infers the runtime cty type from the Go value.
func TestRoundTrip_DynamicPseudoType(t *testing.T) {
	typ := tftypes.DynamicPseudoType
	roundTrip(t, typ, "hello")
	roundTrip(t, typ, true)
	roundTrip(t, typ, 42.0)
	// Note: dynamic collections come back as tuples/objects on the
	// wire (cty has no "dynamic list" concept; the type is inferred
	// per-value). The round-trip helper compares JSON-normalized
	// shapes, which masks the type difference and validates the
	// data shape.
	roundTrip(t, typ, []any{"a", 1.0, true})
	roundTrip(t, typ, map[string]any{"k": "v", "n": 5.0})
}

// TestRoundTrip_LargeIntegerPrecisionLoss documents the float64
// precision boundary. Any integer > 2^53 will not round-trip
// exactly. The test asserts the limit so a future fix (json.Number
// pipeline) can update the expected behavior.
func TestRoundTrip_LargeIntegerPrecisionLoss(t *testing.T) {
	// 2^53 - 1 round-trips exactly.
	const safeInt = float64(1 << 53)
	data, err := EncodeMsgpack(tftypes.Number, safeInt)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeMsgpack(tftypes.Number, data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.(float64) != safeInt {
		t.Errorf("2^53 should round-trip exactly: got %v", out)
	}

	// 2^53 + 1 is not representable in float64 and rounds to 2^53.
	// The decoder reports the rounded value, not the input. This
	// is the documented precision limit.
	const unsafeInt = float64(1<<53) + 1
	if unsafeInt != safeInt {
		t.Skip("float64 widening preserves 2^53+1 on this platform; precision boundary differs")
	}
	t.Logf("confirmed: float64 precision boundary at 2^53")
}
