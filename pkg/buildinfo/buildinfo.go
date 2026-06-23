package buildinfo

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"strings"
)

// These variables are overridden at build time via -ldflags.
var (
	Version   = "0.0.0-dev"
	Commit    = ""
	BuildTime = ""
	Dirty     = ""
)

type Info struct {
	Product   string `json:"product"`
	Binary    string `json:"binary"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Dirty     bool   `json:"dirty"`
	BuiltAt   string `json:"built_at,omitempty"`
	GoVersion string `json:"go_version,omitempty"`
}

func Current(binary, product string) Info {
	info := Info{
		Product:   strings.TrimSpace(product),
		Binary:    strings.TrimSpace(binary),
		Version:   strings.TrimSpace(Version),
		Commit:    strings.TrimSpace(Commit),
		BuiltAt:   strings.TrimSpace(BuildTime),
		GoVersion: runtime.Version(),
	}
	dirty := strings.TrimSpace(Dirty)

	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = strings.TrimSpace(setting.Value)
				}
			case "vcs.time":
				if info.BuiltAt == "" {
					info.BuiltAt = strings.TrimSpace(setting.Value)
				}
			case "vcs.modified":
				if dirty == "" {
					dirty = strings.TrimSpace(setting.Value)
				}
			}
		}
	}

	switch strings.ToLower(dirty) {
	case "1", "true", "dirty", "yes":
		info.Dirty = true
	}
	if info.Version == "" {
		info.Version = "0.0.0-dev"
	}
	return info
}

func (i Info) ShortString() string {
	parts := []string{i.Version}
	if i.Commit != "" {
		parts = append(parts, "commit="+shortCommit(i.Commit))
	}
	if i.Dirty {
		parts = append(parts, "dirty")
	}
	if i.BuiltAt != "" {
		parts = append(parts, "built="+i.BuiltAt)
	}
	return strings.Join(parts, " ")
}

func (i Info) JSON() []byte {
	out, _ := json.MarshalIndent(i, "", "  ")
	return out
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
