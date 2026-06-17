package plan

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/kilolockio/kilolock/internal/tfstate"
)

// FetchCurrentStateFromBackend reads the current state bytes from the
// configured HTTP backend address discovered from terraform init.
func FetchCurrentStateFromBackend(ctx context.Context, bi *BackendInfo) ([]byte, error) {
	if bi == nil {
		return nil, fmt.Errorf("backend info is required")
	}
	addr := strings.TrimSpace(bi.Address)
	if addr == "" {
		return nil, fmt.Errorf("backend address is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %q: %w", addr, err)
	}
	user, pass := backendHTTPAuth(bi)
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", addr, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		empty, err := tfstate.EmptyStateBytes("")
		if err != nil {
			return nil, err
		}
		return empty, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", addr, resp.Status)
	}
	return body, nil
}

func backendHTTPAuth(bi *BackendInfo) (user, pass string) {
	// Terraform-compatible env vars win because operators commonly rely on
	// TF_HTTP_* rather than embedding credentials in backend config.
	if user = strings.TrimSpace(os.Getenv("TF_HTTP_USERNAME")); user == "" {
		user = strings.TrimSpace(os.Getenv("TF_HTTP_USER")) // compatibility alias
	}
	if pass = strings.TrimSpace(os.Getenv("TF_HTTP_PASSWORD")); pass != "" || user != "" {
		return user, pass
	}

	user = strings.TrimSpace(bi.Username)
	pass = strings.TrimSpace(bi.Password)
	if user != "" || pass != "" {
		return user, pass
	}
	u, err := url.Parse(strings.TrimSpace(bi.Address))
	if err != nil || u == nil || u.User == nil {
		return "", ""
	}
	user = u.User.Username()
	pass, _ = u.User.Password()
	return user, pass
}
