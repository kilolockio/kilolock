package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveIACBinary(explicitBin, explicitVersion string, cfgBin, cfgVersion string) (string, error) {
	base := strings.TrimSpace(explicitBin)
	if base == "" {
		base = strings.TrimSpace(cfgBin)
	}
	if base == "" {
		base = "terraform"
	}
	ver := strings.TrimSpace(explicitVersion)
	if ver == "" {
		ver = strings.TrimSpace(cfgVersion)
	}
	if ver == "" {
		return base, nil
	}
	candidates := []string{base}
	if strings.Contains(base, string(filepath.Separator)) {
		dir := filepath.Dir(base)
		file := filepath.Base(base)
		candidates = append(candidates, filepath.Join(dir, file+"-"+ver), filepath.Join(dir, file+ver))
	} else {
		candidates = append(candidates, base+"-"+ver, base+ver)
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no IaC binary found for %q version %q (tried: %s)", base, ver, strings.Join(candidates, ", "))
}
