package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type SelectedScope struct {
	Targets          []string
	RootResources    map[string]struct{}
	ModulePrefixes   []string
	SliceAddresses   []string
	LocalModulePaths []string
}

var (
	moduleDeclRE   = regexp.MustCompile(`(?m)^\s*module\s+"([^"]+)"\s*{`)
	moduleSourceRE = regexp.MustCompile(`(?m)^\s*source\s*=\s*"([^"]+)"`)
	dataDeclRE     = regexp.MustCompile(`(?m)^\s*data\s+"([^"]+)"\s+"([^"]+)"\s*{`)
	blockFromRE    = regexp.MustCompile(`(?m)^\s*from\s*=\s*([^\s#\r\n]+)`)
	blockToRE      = regexp.MustCompile(`(?m)^\s*to\s*=\s*([^\s#\r\n]+)`)
	// Captures:
	//   data.aws_iam_policy_document.main
	//   random_pet.deployment_name
	//   module.small_herd (module prefix)
	refLikeRE = regexp.MustCompile(`\b(?:data\.[A-Za-z0-9_]+\.[A-Za-z0-9_]+|module\.[A-Za-z0-9_]+|[A-Za-z0-9_]+\.[A-Za-z0-9_]+)\b`)
)

func AnalyzeSelectedFiles(configDir string, relFiles []string) (*SelectedScope, error) {
	scope := &SelectedScope{
		RootResources: map[string]struct{}{},
	}
	targetSeen := map[string]struct{}{}
	modulePathSeen := map[string]struct{}{}
	sliceSeen := map[string]struct{}{}

	// Deletion-aware scoping: if an address used to be owned by one of the
	// selected files but has since been removed from configuration, we still
	// want it included in the state slice so a scoped plan can surface the
	// expected destroy action.
	//
	// Important: we do NOT add cached addresses to Targets. Terraform refuses
	// `-target` addresses that do not exist in configuration, and deleted blocks
	// are exactly that. The cache only expands the sliced state inputs, while
	// planning targets remain declaration-based (resources/modules/removed/moved).
	if owned, err := ownershipAddressesForFiles(configDir, relFiles); err != nil {
		return nil, err
	} else {
		for addr := range owned {
			addSelectedOwnedSliceAddr(scope, sliceSeen, addr)
		}
	}

	for _, rel := range relFiles {
		path := filepath.Join(configDir, filepath.FromSlash(rel))
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read scoped file %s: %w", rel, err)
		}
		blocks, err := scanRootBlocks(b)
		if err != nil {
			return nil, fmt.Errorf("parse scoped file %s: %w", rel, err)
		}
		for _, block := range blocks {
			switch block.Type {
			case "resource":
				m := resourceDeclRE.FindSubmatch(block.Bytes)
				if len(m) == 3 {
					addr := string(m[1]) + "." + string(m[2])
					scope.RootResources[addr] = struct{}{}
					targetSeen[addr] = struct{}{}
					sliceSeen[addr] = struct{}{}
				}
				for _, dep := range blockDependencyAddresses(block.Bytes) {
					sliceSeen[dep] = struct{}{}
				}
			case "module":
				m := moduleDeclRE.FindSubmatch(block.Bytes)
				if len(m) != 2 {
					return nil, fmt.Errorf("parse module block in %s: missing module label", rel)
				}
				prefix := "module." + string(m[1])
				scope.ModulePrefixes = append(scope.ModulePrefixes, prefix)
				targetSeen[prefix] = struct{}{}
				sliceSeen[prefix] = struct{}{}
				for _, dep := range blockDependencyAddresses(block.Bytes) {
					sliceSeen[dep] = struct{}{}
				}

				sm := moduleSourceRE.FindSubmatch(block.Bytes)
				if len(sm) == 2 {
					src := string(sm[1])
					if isLocalModuleSource(src) {
						local := filepath.Clean(filepath.Join(configDir, filepath.FromSlash(src)))
						if _, ok := modulePathSeen[local]; !ok {
							modulePathSeen[local] = struct{}{}
							scope.LocalModulePaths = append(scope.LocalModulePaths, local)
						}
					}
				}
			case "moved", "removed", "import":
				// These meta-blocks are how operators express refactors and
				// deletions without keeping full resource blocks in the selected
				// file. They must participate in target/slice derivation so
				// `plan --file` can surface destroys/moves reliably.
				switch block.Type {
				case "removed":
					from := parseMetaAddr(block.Bytes, blockFromRE)
					if from == "" {
						return nil, fmt.Errorf("parse removed block in %s: missing from =", rel)
					}
					addSelectedMetaTarget(scope, targetSeen, sliceSeen, from)
				case "moved":
					from := parseMetaAddr(block.Bytes, blockFromRE)
					to := parseMetaAddr(block.Bytes, blockToRE)
					if from == "" || to == "" {
						return nil, fmt.Errorf("parse moved block in %s: moved blocks must set from = and to =", rel)
					}
					addSelectedMetaTarget(scope, targetSeen, sliceSeen, from)
					addSelectedMetaTarget(scope, targetSeen, sliceSeen, to)
				case "import":
					to := parseMetaAddr(block.Bytes, blockToRE)
					if to == "" {
						return nil, fmt.Errorf("parse import block in %s: missing to =", rel)
					}
					addSelectedMetaTarget(scope, targetSeen, sliceSeen, to)
				}
			}
		}
	}

	for target := range targetSeen {
		scope.Targets = append(scope.Targets, target)
	}
	for addr := range sliceSeen {
		scope.SliceAddresses = append(scope.SliceAddresses, addr)
	}
	sort.Strings(scope.Targets)
	sort.Strings(scope.ModulePrefixes)
	scope.ModulePrefixes = dedupeSortedStrings(scope.ModulePrefixes)
	sort.Strings(scope.SliceAddresses)
	sort.Strings(scope.LocalModulePaths)
	return scope, nil
}

func dedupeSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	var prev string
	for i, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if i == 0 || s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}

func parseMetaAddr(block []byte, re *regexp.Regexp) string {
	m := re.FindSubmatch(block)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

func addSelectedMetaTarget(scope *SelectedScope, targetSeen, sliceSeen map[string]struct{}, addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	// Treat as both a planning target and a slice address. For module-prefixed
	// addresses, we also record the module head so ownership narrowing can keep
	// writes under that module prefix.
	targetSeen[addr] = struct{}{}
	sliceSeen[addr] = struct{}{}
	if strings.HasPrefix(addr, "module.") {
		parts := strings.Split(addr, ".")
		if len(parts) >= 2 {
			head := "module." + parts[1]
			scope.ModulePrefixes = append(scope.ModulePrefixes, head)
		}
	}
	scope.RootResources[addr] = struct{}{}
}

func addSelectedOwnedSliceAddr(scope *SelectedScope, sliceSeen map[string]struct{}, addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	// Cached ownership expands the state slice only; see the comment in
	// AnalyzeSelectedFiles for why we intentionally do not treat these as plan
	// targets.
	sliceSeen[addr] = struct{}{}
	scope.RootResources[addr] = struct{}{}
	if strings.HasPrefix(addr, "module.") {
		parts := strings.Split(addr, ".")
		if len(parts) >= 2 {
			head := "module." + parts[1]
			scope.ModulePrefixes = append(scope.ModulePrefixes, head)
		}
	}
}

func isLocalModuleSource(src string) bool {
	return len(src) > 0 && src[0] == '.'
}

func blockDependencyAddresses(block []byte) []string {
	matches := refLikeRE.FindAll(block, -1)
	if len(matches) == 0 {
		return nil
	}
	skipHeads := map[string]struct{}{
		"var":       {},
		"local":     {},
		"path":      {},
		"each":      {},
		"count":     {},
		"self":      {},
		"terraform": {},
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		addr := string(m)
		head := addr
		if i := strings.IndexByte(head, '.'); i > 0 {
			head = head[:i]
		}
		if _, skip := skipHeads[head]; skip {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

// SelectedRootResourceAddresses is the compatibility wrapper used by
// existing tests. It now returns all selected planning targets:
// root resources and root module prefixes.
func SelectedRootResourceAddresses(configDir string, relFiles []string) ([]string, error) {
	selected, err := AnalyzeSelectedFiles(configDir, relFiles)
	if err != nil {
		return nil, err
	}
	return selected.Targets, nil
}
