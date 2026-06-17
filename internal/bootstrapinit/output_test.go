package bootstrapinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "init.json")
	createdAt := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
	want := Output{
		Tenant:     "operator",
		TenantName: "Operator",
		TokenName:  "operator-bootstrap",
		Token:      "kl_test_secret",
		CreatedAt:  createdAt,
	}

	if err := WriteFile(path, want); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got Output
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("output mismatch: got %+v want %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perms := info.Mode().Perm(); perms != 0o600 {
		t.Fatalf("file perms = %#o want %#o", perms, 0o600)
	}
}
