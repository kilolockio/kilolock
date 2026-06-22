package workdir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveScratchRoot returns the directory KL should use for temporary
// workspaces and transient plan files.
//
// Precedence:
//  1. KL_DATA_DIR
//  2. TF_DATA_DIR
//  3. defaultRoot
//
// Relative env var values are resolved against the current working
// directory. The selected directory is created if it does not exist.
func ResolveScratchRoot(defaultRoot string) (string, error) {
	root, source := selectedRoot(defaultRoot)
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", source, err)
		}
		root = abs
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("prepare %s %q: %w", source, root, err)
	}
	return root, nil
}

func selectedRoot(defaultRoot string) (root, source string) {
	if v := strings.TrimSpace(os.Getenv("KL_DATA_DIR")); v != "" {
		return v, "KL_DATA_DIR"
	}
	if v := strings.TrimSpace(os.Getenv("TF_DATA_DIR")); v != "" {
		return v, "TF_DATA_DIR"
	}
	return strings.TrimSpace(defaultRoot), "default workdir"
}
