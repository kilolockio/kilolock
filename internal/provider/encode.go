package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// UnknownValue is a sentinel type used to represent values that are not yet
// known during the plan phase (e.g., computed attributes).
type UnknownValue struct{}

// AttributeType returns the tftypes.Type that this SchemaAttribute
// represents.
//
// Two shapes are supported, matching the wire schema:
//
//   - Type non-nil: the attribute carries a JSON-encoded cty type
//     descriptor. Parsed via the same JSON shape Terraform emits. This is the v5
//     path and most of v6.
//
//   - NestedType non-nil: the attribute is the v6 nested-attribute
//     form. We recursively build a tftypes.Object whose fields are
//     the nested attribute types, then wrap it according to
//     NestedType.Nesting (single → object, list/set/map → collection
//     of object).
//
// An attribute with neither Type nor NestedType is a malformed
// schema; returns an error rather than silently producing a degenerate
// type.
func AttributeType(a SchemaAttribute) (tftypes.Type, error) {
	if a.NestedType != nil {
		obj := tftypes.Object{AttributeTypes: map[string]tftypes.Type{}}
		for _, child := range a.NestedType.Attributes {
			t, err := AttributeType(child)
			if err != nil {
				return nil, fmt.Errorf("attribute %q: %w", child.Name, err)
			}
			obj.AttributeTypes[child.Name] = t
		}
		switch a.NestedType.Nesting {
		case NestingSingle:
			return obj, nil
		case NestingList:
			return tftypes.List{ElementType: obj}, nil
		case NestingSet:
			return tftypes.Set{ElementType: obj}, nil
		case NestingMap:
			return tftypes.Map{ElementType: obj}, nil
		default:
			return nil, fmt.Errorf("attribute %q: unsupported NestedType.Nesting %s", a.Name, a.NestedType.Nesting)
		}
	}
	if len(a.Type) == 0 {
		return nil, fmt.Errorf("attribute %q has neither Type nor NestedType", a.Name)
	}
	t, err := parseSchemaJSONType([]byte(a.Type))
	if err != nil {
		return nil, fmt.Errorf("attribute %q: parse type: %w", a.Name, err)
	}
	return t, nil
}

// BlockType returns the tftypes.Object implied by a SchemaBlock as a
// whole. Each top-level attribute becomes an Object field of the
// attribute's type; each nested block becomes a field whose type
// reflects the block's nesting (single/group → object, list → list
// of object, set → set of object, map → map of object).
//
// The returned type is always assignable to a tftypes.Object value
// because providers represent resource state as a flat block at
// the top level.
func BlockType(b *SchemaBlock) (tftypes.Type, error) {
	if b == nil {
		return nil, errors.New("BlockType: nil block")
	}
	obj := tftypes.Object{AttributeTypes: map[string]tftypes.Type{}}
	for _, a := range b.Attributes {
		t, err := AttributeType(a)
		if err != nil {
			return nil, err
		}
		obj.AttributeTypes[a.Name] = t
	}
	for _, nb := range b.BlockTypes {
		inner, err := BlockType(nb.Block)
		if err != nil {
			return nil, fmt.Errorf("block %q: %w", nb.TypeName, err)
		}
		switch nb.Nesting {
		case NestingSingle, NestingGroup:
			obj.AttributeTypes[nb.TypeName] = inner
		case NestingList:
			obj.AttributeTypes[nb.TypeName] = tftypes.List{ElementType: inner}
		case NestingSet:
			obj.AttributeTypes[nb.TypeName] = tftypes.Set{ElementType: inner}
		case NestingMap:
			obj.AttributeTypes[nb.TypeName] = tftypes.Map{ElementType: inner}
		default:
			return nil, fmt.Errorf("block %q: unsupported Nesting %s", nb.TypeName, nb.Nesting)
		}
	}
	return obj, nil
}

// EncodeMsgpack converts a JSON-shaped Go value (typically the result
// of json.Unmarshal into an interface{} or map[string]interface{})
// into wire-format msgpack bytes that can be sent as a DynamicValue
// to a provider RPC.
//
// The val parameter must structurally conform to typ:
//
//   - tftypes.String → string or nil
//   - tftypes.Bool   → bool or nil
//   - tftypes.Number → float64, json.Number, or nil
//   - List/Set/Tuple → []any or nil
//   - Map/Object     → map[string]any or nil
//
// Unknown values are not accepted on encode — they are a config-time
// construct that should never appear in refresh inputs. To signal a
// known-null value, pass nil.
//
// Numeric precision: JSON numbers come through json.Unmarshal as
// float64 (or json.Number with UseNumber). Encoding goes through
// *big.Float, so integer values up to 2^53 round-trip exactly.
// Larger integers lose precision. v1 refresh does not depend on
// preserving full integer precision for arbitrary cty.Number; if
// that ever becomes important, switch the JSON pipeline to use
// json.Number and update goToTfValue accordingly.
func EncodeMsgpack(typ tftypes.Type, val any) ([]byte, error) {
	if typ == nil {
		return nil, errors.New("EncodeMsgpack: nil type")
	}
	tv, err := goToTfValue(typ, val)
	if err != nil {
		return nil, err
	}
	dv, err := tfprotov6.NewDynamicValue(typ, tv)
	if err != nil {
		return nil, fmt.Errorf("marshal msgpack: %w", err)
	}
	return dv.MsgPack, nil
}

// DecodeMsgpack is the inverse of EncodeMsgpack: it takes msgpack
// bytes received from a provider RPC and a schema type, and returns
// a JSON-shaped Go value matching the type.
//
// Unknown values are decoded as the UnknownValue{} sentinel struct.
//
// Numeric values come back as float64 to match the input shape of
// EncodeMsgpack. See the precision note there.
func DecodeMsgpack(typ tftypes.Type, data []byte) (any, error) {
	if typ == nil {
		return nil, errors.New("DecodeMsgpack: nil type")
	}
	tv, err := (tfprotov6.DynamicValue{MsgPack: data}).Unmarshal(typ)
	if err != nil {
		return nil, fmt.Errorf("unmarshal msgpack: %w", err)
	}
	return tfToGoValue(tv)
}

func parseSchemaJSONType(buf []byte) (tftypes.Type, error) {
	var jt schemaJSONType
	if err := json.Unmarshal(buf, &jt); err != nil {
		return nil, err
	}
	return jt.t, nil
}

type schemaJSONType struct {
	t tftypes.Type
}

func (jt *schemaJSONType) UnmarshalJSON(buf []byte) error {
	dec := json.NewDecoder(bytes.NewReader(buf))

	tok, err := dec.Token()
	if err != nil {
		return err
	}

	switch v := tok.(type) {
	case string:
		switch v {
		case "bool":
			jt.t = tftypes.Bool
		case "number":
			jt.t = tftypes.Number
		case "string":
			jt.t = tftypes.String
		case "dynamic":
			jt.t = tftypes.DynamicPseudoType
		default:
			return fmt.Errorf("invalid primitive type name %q", v)
		}
		if dec.More() {
			return fmt.Errorf("extraneous data after type description")
		}
		return nil

	case json.Delim:
		if rune(v) != '[' {
			return fmt.Errorf("invalid complex type description")
		}
		kindTok, err := dec.Token()
		if err != nil {
			return err
		}
		kind, ok := kindTok.(string)
		if !ok {
			return fmt.Errorf("invalid complex type kind name")
		}
		t, err := parseComplexSchemaJSONType(dec, kind)
		if err != nil {
			return err
		}
		jt.t = t
		endTok, err := dec.Token()
		if err != nil {
			return err
		}
		end, ok := endTok.(json.Delim)
		if !ok || rune(end) != ']' {
			return fmt.Errorf("complex type missing closing bracket")
		}
		if dec.More() {
			return fmt.Errorf("extraneous data after type description")
		}
		return nil

	default:
		return fmt.Errorf("invalid type description")
	}
}

func parseComplexSchemaJSONType(dec *json.Decoder, kind string) (tftypes.Type, error) {
	switch kind {
	case "list":
		el, err := decodeNestedSchemaJSONType(dec)
		if err != nil {
			return nil, err
		}
		return tftypes.List{ElementType: el}, nil
	case "set":
		el, err := decodeNestedSchemaJSONType(dec)
		if err != nil {
			return nil, err
		}
		return tftypes.Set{ElementType: el}, nil
	case "map":
		el, err := decodeNestedSchemaJSONType(dec)
		if err != nil {
			return nil, err
		}
		return tftypes.Map{ElementType: el}, nil
	case "tuple":
		startTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		start, ok := startTok.(json.Delim)
		if !ok || rune(start) != '[' {
			return nil, fmt.Errorf("tuple requires element type array")
		}
		var elems []tftypes.Type
		for dec.More() {
			t, err := decodeNestedSchemaJSONType(dec)
			if err != nil {
				return nil, err
			}
			elems = append(elems, t)
		}
		endTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		end, ok := endTok.(json.Delim)
		if !ok || rune(end) != ']' {
			return nil, fmt.Errorf("tuple element type array missing closing bracket")
		}
		return tftypes.Tuple{ElementTypes: elems}, nil
	case "object":
		startTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		start, ok := startTok.(json.Delim)
		if !ok || rune(start) != '{' {
			return nil, fmt.Errorf("object requires attribute type map")
		}
		attrs := make(map[string]tftypes.Type)
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name, ok := nameTok.(string)
			if !ok {
				return nil, fmt.Errorf("object attribute name must be string")
			}
			t, err := decodeNestedSchemaJSONType(dec)
			if err != nil {
				return nil, fmt.Errorf("attribute %q: %w", name, err)
			}
			attrs[name] = t
		}
		endTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		end, ok := endTok.(json.Delim)
		if !ok || rune(end) != '}' {
			return nil, fmt.Errorf("object attribute type map missing closing brace")
		}
		return tftypes.Object{AttributeTypes: attrs}, nil
	default:
		return nil, fmt.Errorf("invalid complex type kind %q", kind)
	}
}

func decodeNestedSchemaJSONType(dec *json.Decoder) (tftypes.Type, error) {
	var jt schemaJSONType
	if err := dec.Decode(&jt); err != nil {
		return nil, err
	}
	return jt.t, nil
}

// goToTfValue maps a JSON-shaped Go value to a tftypes.Value of the
// declared type. It is the single recursive driver used by
// EncodeMsgpack; tests exercise it directly to keep diagnostics
// pinpointable.
func goToTfValue(typ tftypes.Type, val any) (tftypes.Value, error) {
	if val == nil {
		return tftypes.NewValue(typ, nil), nil
	}

	switch {
	case typ.Is(tftypes.String):
		s, ok := val.(string)
		if !ok {
			return tftypes.Value{}, typeMismatch("string", val)
		}
		return tftypes.NewValue(tftypes.String, s), nil

	case typ.Is(tftypes.Bool):
		b, ok := val.(bool)
		if !ok {
			return tftypes.Value{}, typeMismatch("bool", val)
		}
		return tftypes.NewValue(tftypes.Bool, b), nil

	case typ.Is(tftypes.Number):
		bf, err := toBigFloat(val)
		if err != nil {
			return tftypes.Value{}, err
		}
		return tftypes.NewValue(tftypes.Number, bf), nil

	case typ.Is(tftypes.DynamicPseudoType):
		// DynamicPseudoType: tftypes.NewValue inspects the runtime
		// Go type and infers the matching cty type. Pass the raw
		// scalar through; for collections, recurse with a concrete
		// type (List of Dynamic, Map of Dynamic) by inspecting val.
		return dynamicGoToTf(val)
	}

	switch t := typ.(type) {
	case tftypes.List:
		return goSliceToTf(val, func(v any) (tftypes.Value, error) {
			return goToTfValue(t.ElementType, v)
		}, func(elems []tftypes.Value) tftypes.Value {
			return tftypes.NewValue(t, elems)
		})

	case tftypes.Set:
		return goSliceToTf(val, func(v any) (tftypes.Value, error) {
			return goToTfValue(t.ElementType, v)
		}, func(elems []tftypes.Value) tftypes.Value {
			return tftypes.NewValue(t, elems)
		})

	case tftypes.Tuple:
		arr, ok := val.([]any)
		if !ok {
			return tftypes.Value{}, typeMismatch("tuple/array", val)
		}
		if len(arr) != len(t.ElementTypes) {
			return tftypes.Value{}, fmt.Errorf("tuple length mismatch: got %d, want %d", len(arr), len(t.ElementTypes))
		}
		elems := make([]tftypes.Value, len(arr))
		for i, v := range arr {
			ev, err := goToTfValue(t.ElementTypes[i], v)
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("tuple[%d]: %w", i, err)
			}
			elems[i] = ev
		}
		return tftypes.NewValue(t, elems), nil

	case tftypes.Map:
		return goMapToTf(val, func(v any) (tftypes.Value, error) {
			return goToTfValue(t.ElementType, v)
		}, func(m map[string]tftypes.Value) tftypes.Value {
			return tftypes.NewValue(t, m)
		})

	case tftypes.Object:
		m, ok := val.(map[string]any)
		if !ok {
			return tftypes.Value{}, typeMismatch("object/map", val)
		}
		built := make(map[string]tftypes.Value, len(t.AttributeTypes))
		for name, attrType := range t.AttributeTypes {
			child, present := m[name]
			if !present {
				// Allow callers to omit attribute keys for null.
				// The wire still carries them as null entries.
				built[name] = tftypes.NewValue(attrType, nil)
				continue
			}
			v, err := goToTfValue(attrType, child)
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("object[%q]: %w", name, err)
			}
			built[name] = v
		}
		// Reject extra keys not in the schema; providers reject them.
		for k := range m {
			if _, ok := t.AttributeTypes[k]; !ok {
				return tftypes.Value{}, fmt.Errorf("object has key %q not in schema", k)
			}
		}
		return tftypes.NewValue(t, built), nil
	}

	return tftypes.Value{}, fmt.Errorf("unsupported type: %s", typ)
}

// tfToGoValue maps a tftypes.Value to a JSON-shaped Go value. The
// result is safe to encoding/json.Marshal directly.
func tfToGoValue(val tftypes.Value) (any, error) {
	if !val.IsKnown() {
		return UnknownValue{}, nil
	}
	if val.IsNull() {
		return nil, nil
	}

	typ := val.Type()
	switch {
	case typ.Is(tftypes.String):
		var s string
		if err := val.As(&s); err != nil {
			return nil, fmt.Errorf("decode string: %w", err)
		}
		return s, nil

	case typ.Is(tftypes.Bool):
		var b bool
		if err := val.As(&b); err != nil {
			return nil, fmt.Errorf("decode bool: %w", err)
		}
		return b, nil

	case typ.Is(tftypes.Number):
		var bf big.Float
		if err := val.As(&bf); err != nil {
			return nil, fmt.Errorf("decode number: %w", err)
		}
		f, _ := bf.Float64()
		return f, nil
	}

	switch typ.(type) {
	case tftypes.List, tftypes.Set, tftypes.Tuple:
		var elems []tftypes.Value
		if err := val.As(&elems); err != nil {
			return nil, fmt.Errorf("decode list/set/tuple: %w", err)
		}
		out := make([]any, len(elems))
		for i, e := range elems {
			conv, err := tfToGoValue(e)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = conv
		}
		return out, nil

	case tftypes.Map, tftypes.Object:
		var m map[string]tftypes.Value
		if err := val.As(&m); err != nil {
			return nil, fmt.Errorf("decode map/object: %w", err)
		}
		out := make(map[string]any, len(m))
		for k, v := range m {
			conv, err := tfToGoValue(v)
			if err != nil {
				return nil, fmt.Errorf("[%q]: %w", k, err)
			}
			out[k] = conv
		}
		return out, nil
	}

	return nil, fmt.Errorf("unsupported value type: %s", typ)
}

// goSliceToTf is shared between List and Set encoding: both accept a
// Go []any input and produce a flat []tftypes.Value where each
// element is encoded under the same element type.
func goSliceToTf(
	val any,
	encodeElem func(any) (tftypes.Value, error),
	build func([]tftypes.Value) tftypes.Value,
) (tftypes.Value, error) {
	arr, ok := val.([]any)
	if !ok {
		return tftypes.Value{}, typeMismatch("list/set", val)
	}
	elems := make([]tftypes.Value, len(arr))
	for i, v := range arr {
		ev, err := encodeElem(v)
		if err != nil {
			return tftypes.Value{}, fmt.Errorf("[%d]: %w", i, err)
		}
		elems[i] = ev
	}
	return build(elems), nil
}

// goMapToTf is the Map version of goSliceToTf.
func goMapToTf(
	val any,
	encodeElem func(any) (tftypes.Value, error),
	build func(map[string]tftypes.Value) tftypes.Value,
) (tftypes.Value, error) {
	m, ok := val.(map[string]any)
	if !ok {
		return tftypes.Value{}, typeMismatch("map", val)
	}
	built := make(map[string]tftypes.Value, len(m))
	for k, v := range m {
		ev, err := encodeElem(v)
		if err != nil {
			return tftypes.Value{}, fmt.Errorf("[%q]: %w", k, err)
		}
		built[k] = ev
	}
	return build(built), nil
}

// dynamicGoToTf encodes a value declared as DynamicPseudoType.
//
// Subtle but critical: tftypes.NewValue(DynamicPseudoType, ...) stamps
// the resulting Value's type as DynamicPseudoType (not the inferred
// concrete type). marshalMsgPack then sees both schema-type and
// value-type as DynamicPseudoType, falls past its dynamic-wrap
// dispatch (which requires the value-type to be concrete), and errors
// with "unknown type tftypes.DynamicPseudoType".
//
// Workaround: recurse into goToTfValue with the inferred concrete
// type. Each recursive call returns a Value whose Type() is concrete
// (String, Number, Object{...}, Tuple{...}). The top-level
// EncodeMsgpack still calls MarshalMsgPack with schema-type =
// DynamicPseudoType; marshalMsgPack sees the value's concrete type
// and dispatches to its dynamic-wrap encoder, which is what the wire
// expects.
func dynamicGoToTf(val any) (tftypes.Value, error) {
	switch v := val.(type) {
	case string:
		return goToTfValue(tftypes.String, v)
	case bool:
		return goToTfValue(tftypes.Bool, v)
	case float64, json.Number, *big.Float,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return goToTfValue(tftypes.Number, v)
	case []any:
		elemTypes := make([]tftypes.Type, len(v))
		elems := make([]tftypes.Value, len(v))
		for i, e := range v {
			ev, err := goToTfValue(tftypes.DynamicPseudoType, e)
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("dynamic[%d]: %w", i, err)
			}
			elemTypes[i] = ev.Type()
			elems[i] = ev
		}
		return tftypes.NewValue(tftypes.Tuple{ElementTypes: elemTypes}, elems), nil
	case map[string]any:
		attrTypes := make(map[string]tftypes.Type, len(v))
		attrs := make(map[string]tftypes.Value, len(v))
		for k, e := range v {
			ev, err := goToTfValue(tftypes.DynamicPseudoType, e)
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("dynamic[%q]: %w", k, err)
			}
			attrTypes[k] = ev.Type()
			attrs[k] = ev
		}
		return tftypes.NewValue(tftypes.Object{AttributeTypes: attrTypes}, attrs), nil
	}
	return tftypes.Value{}, fmt.Errorf("DynamicPseudoType: unsupported go type %T", val)
}

// toBigFloat handles the common JSON→cty.Number conversion paths.
func toBigFloat(val any) (*big.Float, error) {
	switch x := val.(type) {
	case float64:
		return big.NewFloat(x), nil
	case json.Number:
		bf, _, err := big.ParseFloat(string(x), 10, 512, big.ToNearestEven)
		if err != nil {
			return nil, fmt.Errorf("parse json.Number %q: %w", x, err)
		}
		return bf, nil
	case int:
		return new(big.Float).SetInt64(int64(x)), nil
	case int64:
		return new(big.Float).SetInt64(x), nil
	case uint64:
		return new(big.Float).SetUint64(x), nil
	case *big.Float:
		return x, nil
	}
	return nil, typeMismatch("number", val)
}

func typeMismatch(want string, got any) error {
	return fmt.Errorf("type mismatch: want %s, got %T", want, got)
}
