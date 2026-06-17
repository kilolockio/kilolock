package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kilolockio/kilolock/internal/plan"
)

func formatFileScopeEmptyWriteSet(f *plan.File, spec *plan.PlanSpec, scope *plan.FileScope) error {
	base := "file scope produced an empty write_set; selected files did not own any planned writes"
	if f == nil || spec == nil || scope == nil || len(scope.Relative) == 0 {
		return errors.New(base)
	}
	mutating := mutatingAddresses(f)
	if len(mutating) == 0 {
		return fmt.Errorf("%s\n  selected files: %s\n  full plan has no mutating actions (no-op/read only)", base, strings.Join(scope.Relative, ", "))
	}
	owners := ownerFilesForAddresses(f.Configuration.RootModule, spec.ConfigDir, mutating)
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n  selected files: ")
	b.WriteString(strings.Join(scope.Relative, ", "))
	b.WriteString("\n  mutating addresses (preview): ")
	b.WriteString(strings.Join(previewSlice(mutating, scopePreviewLimit), ", "))
	if len(owners) > 0 {
		b.WriteString("\n  planned mutating addresses are owned by: ")
		b.WriteString(strings.Join(previewSlice(owners, scopePreviewLimit), ", "))
	}
	b.WriteString("\n  hint: include the owning file(s) in --file, or run full plan/apply")
	b.WriteString("\n  hint: if you removed a block and expected a destroy, add `removed { from = ... }` to a selected file, or rely on `.kl/ownership.json` (seeded/updated by prior `kl plan` runs)")
	return fmt.Errorf("%s", b.String())
}

func mutatingAddresses(f *plan.File) []string {
	out := make([]string, 0, len(f.ResourceChanges))
	for _, rc := range f.ResourceChanges {
		if plan.ClassifyChange(rc.Change).IsWrite() {
			out = append(out, strings.TrimSpace(rc.Address))
		}
	}
	sort.Strings(out)
	return out
}

func ownerFilesForAddresses(root plan.ConfigModule, configDir string, addrs []string) []string {
	ownersByAddr := map[string]string{}
	walkConfigOwners(root, "", configDir, ownersByAddr)
	set := map[string]struct{}{}
	for _, addr := range addrs {
		if owner := strings.TrimSpace(ownersByAddr[addr]); owner != "" {
			set[owner] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func walkConfigOwners(m plan.ConfigModule, prefix, configDir string, out map[string]string) {
	for _, r := range m.Resources {
		addr := joinScopeAddr(prefix, r.Address)
		if r.DeclRange != nil {
			if rel := normalizeOwnerFilename(configDir, r.DeclRange.Filename); rel != "" {
				out[addr] = rel
			}
		}
	}
	for name, mc := range m.ModuleCalls {
		addr := joinScopeAddr(prefix, "module."+name)
		if mc.DeclRange != nil {
			if rel := normalizeOwnerFilename(configDir, mc.DeclRange.Filename); rel != "" {
				out[addr] = rel
			}
		}
		walkConfigOwners(mc.Module, joinScopeAddr(prefix, "module."+name), configDir, out)
	}
}

func joinScopeAddr(prefix, addr string) string {
	if prefix == "" {
		return addr
	}
	if addr == "" {
		return prefix
	}
	return prefix + "." + addr
}

func normalizeOwnerFilename(configDir, filename string) string {
	name := strings.TrimSpace(filename)
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
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}
