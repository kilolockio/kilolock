package provider

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	tfplugin6 "github.com/davesade/kilolock/internal/grpcwire/tfplugin6"
)

// readResourceSensitivePathsFieldNumber is the proto field number used by newer
// protocol minor versions to report dynamically-sensitive attribute paths from
// ReadResource.Response.
//
// Our vendored tfplugin6.proto (6.11) does not declare the field, so we parse
// it from the response's unknown fields for forward-compatibility.
const readResourceSensitivePathsFieldNumber = 6

func extractReadResourceSensitiveAttributes(resp *tfplugin6.ReadResource_Response) (json.RawMessage, error) {
	if resp == nil {
		return nil, nil
	}
	unknown := resp.ProtoReflect().GetUnknown()
	if len(unknown) == 0 {
		return nil, nil
	}

	paths, err := extractRepeatedAttributePathsFromUnknown(unknown, readResourceSensitivePathsFieldNumber)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}

	encoded := make([][]any, 0, len(paths))
	for _, p := range paths {
		encoded = append(encoded, encodeAttributePathForState(p))
	}
	out, err := json.Marshal(encoded)
	if err != nil {
		return nil, fmt.Errorf("marshal sensitive_attributes: %w", err)
	}
	return json.RawMessage(out), nil
}

func extractRepeatedAttributePathsFromUnknown(unknown []byte, fieldNumber protowire.Number) ([]*tfplugin6.AttributePath, error) {
	var out []*tfplugin6.AttributePath

	for len(unknown) > 0 {
		num, wt, n := protowire.ConsumeTag(unknown)
		if n < 0 {
			return nil, fmt.Errorf("decode unknown fields: %v", protowire.ParseError(n))
		}
		unknown = unknown[n:]

		var v []byte
		switch wt {
		case protowire.VarintType:
			_, n = protowire.ConsumeVarint(unknown)
		case protowire.Fixed32Type:
			_, n = protowire.ConsumeFixed32(unknown)
		case protowire.Fixed64Type:
			_, n = protowire.ConsumeFixed64(unknown)
		case protowire.BytesType:
			v, n = protowire.ConsumeBytes(unknown)
		case protowire.StartGroupType:
			// Groups are deprecated and unused in tfplugin6 messages. Still,
			// ConsumeGroup handles skipping them safely if they appear.
			_, n = protowire.ConsumeGroup(num, unknown)
		default:
			return nil, fmt.Errorf("decode unknown fields: unsupported wire type %d", wt)
		}
		if n < 0 {
			return nil, fmt.Errorf("decode unknown fields: %v", protowire.ParseError(n))
		}
		unknown = unknown[n:]

		if num != fieldNumber || wt != protowire.BytesType {
			continue
		}

		var ap tfplugin6.AttributePath
		if err := proto.Unmarshal(v, &ap); err != nil {
			return nil, fmt.Errorf("decode sensitive path AttributePath: %w", err)
		}
		out = append(out, &ap)
	}

	return out, nil
}

func encodeAttributePathForState(p *tfplugin6.AttributePath) []any {
	if p == nil {
		return nil
	}
	steps := make([]any, 0, len(p.GetSteps()))
	for _, s := range p.GetSteps() {
		switch sel := s.GetSelector().(type) {
		case *tfplugin6.AttributePath_Step_AttributeName:
			steps = append(steps, sel.AttributeName)
		case *tfplugin6.AttributePath_Step_ElementKeyString:
			steps = append(steps, sel.ElementKeyString)
		case *tfplugin6.AttributePath_Step_ElementKeyInt:
			steps = append(steps, sel.ElementKeyInt)
		default:
			// Unknown selector variants can't be encoded; skip so we don't
			// corrupt the state file. If Terraform adds a new selector, we
			// should update this code to match.
		}
	}
	return steps
}
