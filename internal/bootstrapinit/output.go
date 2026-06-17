package bootstrapinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type Output struct {
	Tenant     string    `json:"tenant"`
	TenantName string    `json:"tenant_name"`
	TokenName  string    `json:"token_name"`
	Token      string    `json:"token"`
	CreatedAt  time.Time `json:"created_at"`
}

func WriteFile(path string, out Output) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return os.WriteFile(path, payload, 0o600)
}
