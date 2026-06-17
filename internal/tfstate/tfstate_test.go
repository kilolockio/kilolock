package tfstate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const minimalStateJSON = `{
  "version": 4,
  "terraform_version": "1.13.4",
  "serial": 7,
  "lineage": "9b39e2c0-1111-2222-3333-444455556666",
  "outputs": {
    "endpoint": {
      "value": "https://example.test",
      "type": "string",
      "sensitive": false
    }
  },
  "resources": [
    {
      "mode": "managed",
      "type": "aws_vpc",
      "name": "main",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [
        {
          "schema_version": 0,
          "attributes": {"id": "vpc-1", "cidr_block": "10.0.0.0/16"},
          "sensitive_attributes": []
        }
      ]
    },
    {
      "mode": "managed",
      "type": "aws_subnet",
      "name": "private",
      "module": "module.network",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [
        {
          "schema_version": 0,
          "attributes": {"id": "subnet-1"},
          "sensitive_attributes": [],
          "dependencies": ["aws_vpc.main"],
          "index_key": 0
        },
        {
          "schema_version": 0,
          "attributes": {"id": "subnet-2"},
          "sensitive_attributes": [],
          "dependencies": ["aws_vpc.main"],
          "index_key": "primary"
        }
      ]
    },
    {
      "mode": "data",
      "type": "aws_ami",
      "name": "ubuntu",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [
        {
          "schema_version": 0,
          "attributes": {"id": "ami-1"}
        }
      ]
    }
  ]
}`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(minimalStateJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Version != 4 {
		t.Errorf("Version = %d, want 4", s.Version)
	}
	if s.Serial != 7 {
		t.Errorf("Serial = %d, want 7", s.Serial)
	}
	if got, want := len(s.Resources), 3; got != want {
		t.Errorf("len(Resources) = %d, want %d", got, want)
	}
}

func TestParse_RejectsWrongVersion(t *testing.T) {
	_, err := Parse([]byte(`{"version": 3}`))
	if err == nil {
		t.Fatal("Parse: expected error for version 3, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error %q does not mention support", err.Error())
	}
}

func TestInstanceAddress(t *testing.T) {
	tests := []struct {
		name     string
		resource Resource
		index    json.RawMessage
		want     string
	}{
		{
			name:     "root managed no index",
			resource: Resource{Mode: "managed", Type: "aws_vpc", Name: "main"},
			want:     "aws_vpc.main",
		},
		{
			name:     "root data source",
			resource: Resource{Mode: "data", Type: "aws_ami", Name: "ubuntu"},
			want:     "data.aws_ami.ubuntu",
		},
		{
			name:     "int index",
			resource: Resource{Mode: "managed", Type: "aws_instance", Name: "web"},
			index:    json.RawMessage(`0`),
			want:     "aws_instance.web[0]",
		},
		{
			name:     "string index",
			resource: Resource{Mode: "managed", Type: "aws_instance", Name: "web"},
			index:    json.RawMessage(`"api"`),
			want:     `aws_instance.web["api"]`,
		},
		{
			name: "nested module with int index",
			resource: Resource{
				Mode:   "managed",
				Type:   "aws_subnet",
				Name:   "private",
				Module: "module.network.module.private",
			},
			index: json.RawMessage(`1`),
			want:  "module.network.module.private.aws_subnet.private[1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := ResourceInstance{IndexKey: tt.index}
			got, err := InstanceAddress(tt.resource, inst)
			if err != nil {
				t.Fatalf("InstanceAddress: %v", err)
			}
			if got != tt.want {
				t.Errorf("InstanceAddress = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecodeIndex(t *testing.T) {
	tests := []struct {
		name     string
		raw      json.RawMessage
		wantKind IndexKind
		wantVal  string
	}{
		{name: "absent", raw: nil, wantKind: IndexNone, wantVal: ""},
		{name: "null", raw: json.RawMessage(`null`), wantKind: IndexNone, wantVal: ""},
		{name: "int", raw: json.RawMessage(`3`), wantKind: IndexInt, wantVal: "3"},
		{name: "string", raw: json.RawMessage(`"primary"`), wantKind: IndexString, wantVal: "primary"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := ResourceInstance{IndexKey: tt.raw}
			kind, val, err := inst.DecodeIndex()
			if err != nil {
				t.Fatalf("DecodeIndex: %v", err)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %s, want %s", kind, tt.wantKind)
			}
			if val != tt.wantVal {
				t.Errorf("val = %q, want %q", val, tt.wantVal)
			}
		})
	}
}

func TestParseProviderRef(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantSource string
		wantAlias  string
	}{
		{
			name:       "default unaliased",
			in:         `provider["registry.terraform.io/hashicorp/aws"]`,
			wantSource: "registry.terraform.io/hashicorp/aws",
		},
		{
			name:       "with alias",
			in:         `provider["registry.terraform.io/hashicorp/aws"].west`,
			wantSource: "registry.terraform.io/hashicorp/aws",
			wantAlias:  "west",
		},
		{
			name:       "module-prefixed strips prefix",
			in:         `module.vpc.provider["registry.terraform.io/hashicorp/aws"]`,
			wantSource: "registry.terraform.io/hashicorp/aws",
		},
		{
			name:       "module-prefixed with alias",
			in:         `module.vpc.module.private.provider["registry.terraform.io/hashicorp/aws"].east`,
			wantSource: "registry.terraform.io/hashicorp/aws",
			wantAlias:  "east",
		},
		{
			name:       "non-registry source",
			in:         `provider["example.com/acme/widget"]`,
			wantSource: "example.com/acme/widget",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, alias, err := ParseProviderRef(tc.in)
			if err != nil {
				t.Fatalf("ParseProviderRef(%q): %v", tc.in, err)
			}
			if src != tc.wantSource {
				t.Errorf("source: got %q, want %q", src, tc.wantSource)
			}
			if alias != tc.wantAlias {
				t.Errorf("alias: got %q, want %q", alias, tc.wantAlias)
			}
		})
	}
}

func TestParseProviderRef_Rejects(t *testing.T) {
	cases := []string{
		"",
		"not-a-provider-ref",
		`provider[registry.terraform.io/hashicorp/aws]`,    // unquoted
		`provider["registry.terraform.io/hashicorp/aws"`,   // missing closing
		`provider["registry.terraform.io/hashicorp/aws"].`, // trailing dot
		`provider[""]`,         // empty source
		`provider["aws"]extra`, // junk after closing bracket
		`fooprovider["registry.terraform.io/hashicorp/aws"]`, // text glued to keyword
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, _, err := ParseProviderRef(in)
			if err == nil {
				t.Fatalf("ParseProviderRef(%q) returned nil error", in)
			}
			if !errors.Is(err, ErrInvalidProviderRef) {
				t.Errorf("ParseProviderRef(%q): err %v not wrapping ErrInvalidProviderRef", in, err)
			}
		})
	}
}
