package plan

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// FileScope carries the normalized scope requested by --file flags.
type FileScope struct {
	// Relative contains normalized slash-form paths relative to config dir.
	Relative []string
}

// NormalizeFileScope canonicalizes file selectors relative to configDir.
func NormalizeFileScope(configDir string, files []string) (*FileScope, error) {
	if len(files) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(files))
	out := make([]string, 0, len(files))
	for _, raw := range files {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("--file must not be empty")
		}
		abs := raw
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(configDir, raw)
		}
		rel, err := filepath.Rel(configDir, abs)
		if err != nil {
			return nil, fmt.Errorf("normalize --file %q: %w", raw, err)
		}
		rel = filepath.Clean(rel)
		if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			return nil, fmt.Errorf("--file %q resolves outside config dir %s", raw, configDir)
		}
		rel = filepath.ToSlash(rel)
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		out = append(out, rel)
	}
	sort.Strings(out)
	return &FileScope{Relative: out}, nil
}

// ApplyFileScope rewrites a full PlanSpec into a file-scoped subset.
// It narrows write_set by ownership (resource decl filename), then
// recomputes read_set + reservations from the narrowed write_set.
func ApplyFileScope(f *File, spec *PlanSpec, scope *FileScope) (*PlanSpec, error) {
	if scope == nil || len(scope.Relative) == 0 {
		return spec, nil
	}
	if f == nil || spec == nil {
		return nil, fmt.Errorf("apply file scope: nil file/spec")
	}

	owners := map[string]string{}
	walkConfigForFileOwnership(f.Configuration.RootModule, "", owners)
	selected, err := AnalyzeSelectedFiles(spec.ConfigDir, scope.Relative)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(scope.Relative))
	for _, s := range scope.Relative {
		allowed[s] = struct{}{}
	}

	actionsByAddr := make(map[string][]string, len(f.ResourceChanges))
	for _, rc := range f.ResourceChanges {
		if strings.TrimSpace(rc.Address) == "" {
			continue
		}
		actionsByAddr[rc.Address] = rc.Change.Actions
	}

	narrowWrite := make([]string, 0, len(spec.WriteSet))
	var droppedDeletes []string
	for _, addr := range spec.WriteSet {
		owner := normalizePlanFilename(spec.ConfigDir, owners[addr])
		if owner != "" {
			if _, ok := allowed[owner]; ok {
				narrowWrite = append(narrowWrite, addr)
			}
			continue
		}
		kept := false
		// Fallback for terraform versions/blocks that omit range metadata.
		if _, ok := selected.RootResources[addr]; ok {
			narrowWrite = append(narrowWrite, addr)
			kept = true
		} else {
			for _, prefix := range selected.ModulePrefixes {
				if strings.HasPrefix(addr, prefix+".") {
					narrowWrite = append(narrowWrite, addr)
					kept = true
					break
				}
			}
		}
		// If Terraform didn't attach range metadata (or the config no longer
		// contains this resource), we have no trustworthy mapping from address
		// to "owning file". For delete/forget actions, silently dropping the
		// write is a footgun — the operator expects a destroy. Fail closed and
		// force the operator to add an explicit `removed { from = ... }` block
		// (or run a full plan/apply).
		if !kept && isDeleteOrForget(actionsByAddr[addr]) {
			droppedDeletes = append(droppedDeletes, addr)
		}
	}
	sort.Strings(narrowWrite)
	sort.Strings(droppedDeletes)
	if len(droppedDeletes) > 0 {
		return nil, fmt.Errorf("file-scoped plan dropped %d delete/forget action(s) due to unknown ownership (add `removed { from = ... }` blocks or run a full plan): %v",
			len(droppedDeletes), droppedDeletes)
	}

	g := BuildDepGraph(f)
	narrowRead := CloseReadSet(narrowWrite, g)
	spec.WriteSet = narrowWrite
	spec.ReadSet = narrowRead
	spec.Reservations = buildReservations(narrowWrite, narrowRead)
	spec.ScopedFiles = append([]string(nil), scope.Relative...)
	return spec, nil
}

func isDeleteOrForget(actions []string) bool {
	for _, a := range actions {
		switch strings.TrimSpace(a) {
		case "delete", "forget":
			return true
		}
	}
	return false
}

func walkConfigForFileOwnership(m ConfigModule, prefix string, out map[string]string) {
	for _, r := range m.Resources {
		addr := joinAddr(prefix, r.Address)
		if r.DeclRange != nil && strings.TrimSpace(r.DeclRange.Filename) != "" {
			out[addr] = r.DeclRange.Filename
		}
	}
	for name, mc := range m.ModuleCalls {
		addr := joinAddr(prefix, "module."+name)
		if mc.DeclRange != nil && strings.TrimSpace(mc.DeclRange.Filename) != "" {
			out[addr] = mc.DeclRange.Filename
		}
		walkConfigForFileOwnership(mc.Module, joinAddr(prefix, "module."+name), out)
	}
}

func normalizePlanFilename(configDir, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	abs := name
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(configDir, abs)
	}
	rel, err := filepath.Rel(configDir, abs)
	if err != nil {
		return ""
	}
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return ""
	}
	return filepath.ToSlash(rel)
}

var resourceDeclRE = regexp.MustCompile(`(?m)^\s*resource\s+"([^"]+)"\s+"([^"]+)"\s*{`)
