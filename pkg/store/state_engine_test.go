package store

import "testing"

func TestBuildStateEngineGraphSnapshot_ResolvesBaseDependenciesToIndexedInstances(t *testing.T) {
	rows := []stateEngineGraphRow{
		{
			Resource:        StateEngineResourceInventory{Address: "aws_vpc.main"},
			DependenciesRaw: "[]",
		},
		{
			Resource: StateEngineResourceInventory{
				Address: "aws_instance.web[0]",
			},
			DependenciesRaw: `["aws_vpc.main"]`,
		},
		{
			Resource: StateEngineResourceInventory{
				Address: "aws_instance.web[1]",
			},
			DependenciesRaw: `["aws_vpc.main"]`,
		},
		{
			Resource: StateEngineResourceInventory{
				Address: "null_resource.consumer",
			},
			DependenciesRaw: `["aws_instance.web"]`,
		},
	}

	resources, adjacency, err := buildStateEngineGraphSnapshot(rows)
	if err != nil {
		t.Fatalf("buildStateEngineGraphSnapshot: %v", err)
	}
	if len(resources) != 4 {
		t.Fatalf("len(resources) = %d, want 4", len(resources))
	}
	got := adjacency["null_resource.consumer"]
	want := []string{"aws_instance.web[0]", "aws_instance.web[1]"}
	if len(got) != len(want) {
		t.Fatalf("len(adjacency[consumer]) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("adjacency[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}
