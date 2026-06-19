package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/pkg/store"
)

func TestManagedResourcePlanDelta_IgnoresNonNetMutations(t *testing.T) {
	f := &plan.File{
		ResourceChanges: []plan.ResourceChange{
			{Mode: "managed", Address: "aws_s3_bucket.a", Change: plan.Change{Actions: []string{"create"}}},
			{Mode: "managed", Address: "aws_s3_bucket.b", Change: plan.Change{Actions: []string{"delete"}}},
			{Mode: "managed", Address: "aws_s3_bucket.c", Change: plan.Change{Actions: []string{"forget"}}},
			{Mode: "managed", Address: "aws_s3_bucket.d", Change: plan.Change{Actions: []string{"update"}}},
			{Mode: "managed", Address: "aws_s3_bucket.e", Change: plan.Change{Actions: []string{"delete", "create"}}},
			{Mode: "data", Address: "data.aws_caller_identity.me", Change: plan.Change{Actions: []string{"read"}}},
		},
	}
	if got, want := managedResourcePlanDelta(f), -1; got != want {
		t.Fatalf("managedResourcePlanDelta() = %d, want %d", got, want)
	}
}

func TestQuotaPreviewExitCode_WarnsOnSoftFailsOnHard(t *testing.T) {
	var stderr bytes.Buffer
	soft := &store.QuotaPreview{
		State: store.QuotaDimensionPreview{SoftExceeded: true},
	}
	if got := quotaPreviewExitCode(&stderr, soft, "kl plan"); got != 0 {
		t.Fatalf("soft warning exit code = %d, want 0", got)
	}
	if !strings.Contains(stderr.String(), "WARN quota soft limit exceeded") {
		t.Fatalf("soft warning output = %q", stderr.String())
	}

	stderr.Reset()
	hard := &store.QuotaPreview{
		Environment: store.QuotaDimensionPreview{HardExceeded: true},
	}
	if got := quotaPreviewExitCode(&stderr, hard, "kl plan"); got != 1 {
		t.Fatalf("hard exceed exit code = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "quota hard limit exceeded") {
		t.Fatalf("hard exceed output = %q", stderr.String())
	}
}

func TestQuotaClientAndState_UsesLiveBackendConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "backend.tf"), []byte(`
terraform {
  backend "http" {
    address = "https://api.kilolock.cloud/states/ws_123/env_456/demo"
  }
}
`), 0o644); err != nil {
		t.Fatalf("write backend.tf: %v", err)
	}

	t.Setenv("TF_HTTP_PASSWORD", "env-pass")
	stateName, client, err := quotaClientAndState(dir, "")
	if err != nil {
		t.Fatalf("quotaClientAndState: %v", err)
	}
	if stateName != "ws_123/env_456/demo" {
		t.Fatalf("stateName=%q want ws_123/env_456/demo", stateName)
	}
	if client.baseURL != "https://api.kilolock.cloud" {
		t.Fatalf("baseURL=%q want https://api.kilolock.cloud", client.baseURL)
	}
	if client.password != "env-pass" {
		t.Fatalf("password=%q want env-pass", client.password)
	}
}
