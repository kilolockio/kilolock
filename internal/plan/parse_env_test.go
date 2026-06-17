package plan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunTerraformPlan_PassesTFEnv(t *testing.T) {
	dir := t.TempDir()
	envOut := filepath.Join(dir, "tf_log.txt")
	planOut := filepath.Join(dir, "plan.tfplan")
	bin := filepath.Join(dir, "fake-terraform.sh")
	script := `#!/bin/sh
set -eu
cmd="$1"
shift
if [ "$cmd" = "plan" ]; then
  : "${TF_LOG:=}"
  printf '%s' "$TF_LOG" > "` + envOut + `"
  for a in "$@"; do
    case "$a" in
      -out=*)
        p="${a#-out=}"
        : > "$p"
        ;;
    esac
  done
  exit 0
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake terraform: %v", err)
	}
	t.Setenv("TF_LOG", "TRACE")

	if err := RunTerraformPlan(context.Background(), bin, dir, planOut, PlanRunOptions{
		Lock:    false,
		Refresh: false,
	}); err != nil {
		t.Fatalf("RunTerraformPlan: %v", err)
	}
	got, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read env output: %v", err)
	}
	if strings.TrimSpace(string(got)) != "TRACE" {
		t.Fatalf("TF_LOG not passed through, got %q", string(got))
	}
}
