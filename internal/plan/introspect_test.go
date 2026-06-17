package plan

import (
	"testing"
)

func TestIntrospect(t *testing.T) {
	planJSON := []byte(`{
		"resource_changes": [
			{
				"address": "aws_instance.web",
				"change": { "actions": ["create"] }
			},
			{
				"address": "aws_s3_bucket.data",
				"change": { "actions": ["no-op"] }
			}
		],
		"configuration": {
			"root_module": {
				"resources": [
					{
						"address": "aws_instance.web",
						"expressions": {
							"ami": {
								"references": ["var.ami"]
							},
							"subnet_id": {
								"references": ["aws_subnet.main.id", "aws_subnet.main"]
							}
						}
					}
				]
			}
		}
	}`)

	spec, err := Introspect(planJSON)
	if err != nil {
		t.Fatalf("Introspect failed: %v", err)
	}

	if len(spec.WriteSet) != 1 || spec.WriteSet[0] != "aws_instance.web" {
		t.Errorf("expected WriteSet to contain aws_instance.web, got %v", spec.WriteSet)
	}

	// The ReadSet should pick up 'aws_subnet.main' and drop the 'var.ami'
	if len(spec.ReadSet) != 1 || spec.ReadSet[0] != "aws_subnet.main" {
		t.Errorf("expected ReadSet to contain aws_subnet.main, got %v", spec.ReadSet)
	}
}
