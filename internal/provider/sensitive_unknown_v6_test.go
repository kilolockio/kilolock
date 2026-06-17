package provider

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	tfplugin6 "github.com/kilolockio/kilolock/internal/grpcwire/tfplugin6"
)

func TestExtractReadResourceSensitiveAttributes(t *testing.T) {
	p1 := &tfplugin6.AttributePath{
		Steps: []*tfplugin6.AttributePath_Step{
			{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "foo"}},
			{Selector: &tfplugin6.AttributePath_Step_ElementKeyInt{ElementKeyInt: 0}},
			{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "bar"}},
		},
	}
	p2 := &tfplugin6.AttributePath{
		Steps: []*tfplugin6.AttributePath_Step{
			{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "baz"}},
		},
	}
	b1, err := proto.Marshal(p1)
	if err != nil {
		t.Fatalf("proto.Marshal(p1): %v", err)
	}
	b2, err := proto.Marshal(p2)
	if err != nil {
		t.Fatalf("proto.Marshal(p2): %v", err)
	}

	var unknown []byte
	unknown = protowire.AppendTag(unknown, readResourceSensitivePathsFieldNumber, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, b1)
	unknown = protowire.AppendTag(unknown, readResourceSensitivePathsFieldNumber, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, b2)

	resp := &tfplugin6.ReadResource_Response{}
	resp.ProtoReflect().SetUnknown(unknown)

	raw, err := extractReadResourceSensitiveAttributes(resp)
	if err != nil {
		t.Fatalf("extractReadResourceSensitiveAttributes: %v", err)
	}
	if got, want := string(raw), `[["foo",0,"bar"],["baz"]]`; got != want {
		t.Fatalf("SensitiveAttributes JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestExtractReadResourceSensitiveAttributes_None(t *testing.T) {
	raw, err := extractReadResourceSensitiveAttributes(&tfplugin6.ReadResource_Response{})
	if err != nil {
		t.Fatalf("extractReadResourceSensitiveAttributes: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected nil, got %q", string(raw))
	}
}
