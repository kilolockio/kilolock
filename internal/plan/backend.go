package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// terraformInitState is the `.terraform/terraform.tfstate` file
// that `terraform init` writes alongside the providers cache.
// Despite the name it is NOT a terraform state — it records the
// resolved backend configuration so that subsequent terraform
// commands run in the same directory know where state lives.
//
// We only model the fields we need; unknown keys are ignored. The
// format is documented at
// https://developer.hashicorp.com/terraform/language/state/backends
// in the "backend state file" section and has been stable since
// terraform 0.13.
type terraformInitState struct {
	Backend struct {
		Type   string                     `json:"type"`
		Config map[string]any             `json:"config"`
		Hash   any                        `json:"hash,omitempty"`
		_      map[string]json.RawMessage `json:"-"`
	} `json:"backend"`
}

// BackendInfo is the resolved backend configuration we use to set
// sensible defaults on plan/apply. Only HTTP backends are
// recognized; the v1/v2 kl design only supports them, so
// anything else returns ErrUnsupportedBackend.
type BackendInfo struct {
	// Type is the backend label, always "http" today.
	Type string

	// Address is the unmodified backend address from the init
	// state file (e.g. "http://localhost:8080/states/big-state").
	Address string

	// StateName is the Kilolock state name implied by Address,
	// used as the default value for `kl apply --state=…`.
	// For HTTP backend addresses under `/states/...`, this is the
	// full path suffix after `/states/` so hierarchical names like
	// `ws_.../env_.../name` survive intact.
	StateName string

	// Username/Password are optional HTTP backend basic-auth
	// credentials resolved by `terraform init` (from backend config).
	Username string
	Password string
}

// ErrNoBackendConfigured signals that the working directory has
// not been `terraform init`-ed (or was init'd against a local
// backend, which kl doesn't manage). Callers turn this
// into a hint like "run `terraform init` first" or fall back to
// requiring `--state=…` explicitly.
var ErrNoBackendConfigured = errors.New("no terraform http backend configured in this directory")

// ErrUnsupportedBackend signals that `terraform init` succeeded
// but configured a non-http backend (s3, gcs, remote, …). The
// v1/v2 kl backend protocol only speaks HTTP; using it
// against a different backend would require a separate adapter.
var ErrUnsupportedBackend = errors.New("only the http backend is supported by kl")

// DiscoverBackend reads the `.terraform/terraform.tfstate` file
// in configDir and returns the configured HTTP backend's address
// + derived state name. The file is JSON, not HCL, and is
// authoritative even when the .tf backend block is split across
// multiple files or augmented with `-backend-config=…` overrides
// at init time. That makes JSON-reading strictly more correct
// than parsing the operator's HCL ourselves.
//
// Returns ErrNoBackendConfigured if the init file is missing
// (most commonly "terraform init was never run here") and
// ErrUnsupportedBackend if a non-http backend is configured.
func DiscoverBackend(configDir string) (*BackendInfo, error) {
	path := filepath.Join(configDir, ".terraform", "terraform.tfstate")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s missing (did you `terraform init`?)",
				ErrNoBackendConfigured, path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var doc terraformInitState
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if doc.Backend.Type == "" {
		return nil, fmt.Errorf("%w: %s has no backend.type", ErrNoBackendConfigured, path)
	}
	if doc.Backend.Type != "http" {
		return nil, fmt.Errorf("%w: backend.type=%q in %s",
			ErrUnsupportedBackend, doc.Backend.Type, path)
	}

	addrAny, ok := doc.Backend.Config["address"]
	if !ok {
		return nil, fmt.Errorf("%w: http backend has no address attribute", ErrNoBackendConfigured)
	}
	addr, ok := addrAny.(string)
	if !ok {
		return nil, fmt.Errorf("%w: http backend address is %T, expected string",
			ErrNoBackendConfigured, addrAny)
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("%w: http backend address is empty", ErrNoBackendConfigured)
	}

	name, err := stateNameFromAddress(addr)
	if err != nil {
		return nil, fmt.Errorf("derive state name from %q: %w", addr, err)
	}

	bi := &BackendInfo{
		Type:      "http",
		Address:   addr,
		StateName: name,
		Username:  stringConfig(doc.Backend.Config, "username"),
		Password:  stringConfig(doc.Backend.Config, "password"),
	}
	if err := populateBackendAuthFallback(configDir, bi); err != nil {
		return nil, err
	}
	return bi, nil
}

var (
	httpBackendBlockPattern = regexp.MustCompile(`(?s)backend\s+"http"\s*\{.*?\}`)
	backendUsernamePattern  = regexp.MustCompile(`(?m)^\s*username\s*=\s*"([^"\n]*)"`)
	backendPasswordPattern  = regexp.MustCompile(`(?m)^\s*password\s*=\s*"([^"\n]*)"`)
)

func populateBackendAuthFallback(configDir string, bi *BackendInfo) error {
	if bi == nil {
		return nil
	}
	if strings.TrimSpace(bi.Username) != "" && strings.TrimSpace(bi.Password) != "" {
		return nil
	}
	user, pass := discoverBackendHTTPAuthFromConfigDir(configDir)
	if strings.TrimSpace(bi.Username) == "" {
		bi.Username = user
	}
	if strings.TrimSpace(bi.Password) == "" {
		bi.Password = pass
	}
	return nil
}

func discoverBackendHTTPAuthFromConfigDir(configDir string) (username, password string) {
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return "", ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".tf") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(configDir, name))
		if err != nil {
			continue
		}
		block := httpBackendBlockPattern.Find(raw)
		if len(block) == 0 {
			continue
		}
		if username == "" {
			if match := backendUsernamePattern.FindSubmatch(block); len(match) == 2 {
				username = string(match[1])
			}
		}
		if password == "" {
			if match := backendPasswordPattern.FindSubmatch(block); len(match) == 2 {
				password = string(match[1])
			}
		}
		if username != "" && password != "" {
			return username, password
		}
	}
	return username, password
}

func stringConfig(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// stateNameFromAddress extracts the Kilolock state name from the
// backend URL.
//
// For Kilolock backends, the canonical path is `/states/<name>`,
// where `<name>` may itself contain slashes:
//
//	http://localhost:8080/states/big-state                               → big-state
//	http://localhost:8080/states/ws_123/env_456/blarg                    → ws_123/env_456/blarg
//	http://kl.example/states/ws_123/env_456/blarg?x=1            → ws_123/env_456/blarg
//
// Returns an error for malformed URLs or addresses with no path.
// We use net/url so query strings and fragments are stripped
// safely. If the URL is not under `/states/`, we fall back to the
// legacy "last path segment" behavior.
func stateNameFromAddress(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "" || p == "/" {
		return "", fmt.Errorf("address has no path segment to use as state name")
	}
	if i := strings.Index(p, "/states/"); i >= 0 {
		name := strings.Trim(strings.TrimPrefix(p[i:], "/states/"), "/")
		if name == "" {
			return "", fmt.Errorf("address path %q ends without a state name segment", u.Path)
		}
		return name, nil
	}
	i := strings.LastIndex(p, "/")
	name := p[i+1:]
	if name == "" {
		return "", fmt.Errorf("address path %q ends without a state name segment", u.Path)
	}
	return name, nil
}
