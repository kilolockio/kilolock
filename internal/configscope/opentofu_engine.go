package configscope

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/kilolockio/kilolock/internal/plan"
)

func opentofuEngineAvailable() bool {
	return true
}

type opentofuEngine struct{}

func (opentofuEngine) Name() string { return EngineOpenTofu }

func (opentofuEngine) DiscoverForFiles(configDir string, scope *plan.FileScope) (*Intent, error) {
	// Keep current ownership/meta-block discovery for selected files, but use
	// the OpenTofu-style HCL AST walk to derive dependency closure.
	selected, err := plan.AnalyzeSelectedFiles(configDir, scope.Relative)
	if err != nil {
		return nil, err
	}
	graph, err := loadOpenTofuGraph(configDir)
	if err != nil {
		return nil, err
	}
	sliceCandidates, err := expandTargetSliceAddressesFromGraph(graph, selected.Targets)
	if err != nil {
		return nil, err
	}
	intent := buildIntent(selected.Targets, selected.RootResources, selected.RemovedResources, sliceCandidates, graph)
	intent.DiscoveryEngine = EngineOpenTofu
	return intent, nil
}

func (opentofuEngine) DiscoverForTargets(configDir string, targets []string) (*Intent, error) {
	graph, err := loadOpenTofuGraph(configDir)
	if err != nil {
		return nil, err
	}
	sliceCandidates, err := expandTargetSliceAddressesFromGraph(graph, targets)
	if err != nil {
		return nil, err
	}
	rootResources := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" || strings.HasPrefix(target, "module.") {
			continue
		}
		rootResources[target] = struct{}{}
	}
	intent := buildIntent(targets, rootResources, nil, sliceCandidates, graph)
	intent.DiscoveryEngine = EngineOpenTofu
	return intent, nil
}

func expandTargetSliceAddressesOpenTofu(configDir string, targets []string) ([]string, error) {
	graph, err := loadOpenTofuGraph(configDir)
	if err != nil {
		return nil, err
	}
	return expandTargetSliceAddressesFromGraph(graph, targets)
}

func expandTargetSliceAddressesFromGraph(graph map[string][]string, targets []string) ([]string, error) {
	seen := map[string]struct{}{}
	queue := make([]string, 0, len(targets)*2)
	push := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		if _, ok := seen[addr]; ok {
			return
		}
		seen[addr] = struct{}{}
		queue = append(queue, addr)
	}
	for _, target := range targets {
		push(target)
		if head := moduleHead(target); head != "" && head != target {
			push(head)
		}
	}
	for i := 0; i < len(queue); i++ {
		cur := queue[i]
		for _, dep := range graph[cur] {
			push(dep)
			if head := moduleHead(dep); head != "" && head != dep {
				push(head)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for addr := range seen {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out, nil
}

func loadOpenTofuGraph(configDir string) (map[string][]string, error) {
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return nil, fmt.Errorf("opentofu discovery: read dir %s: %w", configDir, err)
	}
	parser := hclparse.NewParser()
	graph := map[string][]string{}
	localRefs := map[string]struct{}{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".tf" {
			continue
		}
		path := filepath.Join(configDir, entry.Name())
		file, diags := parser.ParseHCLFile(path)
		if diags.HasErrors() {
			return nil, fmt.Errorf("opentofu discovery: parse %s: %s", path, diags.Error())
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			switch block.Type {
			case "resource":
				if len(block.Labels) != 2 {
					continue
				}
				key := block.Labels[0] + "." + block.Labels[1]
				graph[key] = append(graph[key], extractBlockRefs(block.Body, localRefs)...)
			case "data":
				if len(block.Labels) != 2 {
					continue
				}
				key := "data." + block.Labels[0] + "." + block.Labels[1]
				graph[key] = append(graph[key], extractBlockRefs(block.Body, localRefs)...)
			case "module":
				if len(block.Labels) != 1 {
					continue
				}
				key := "module." + block.Labels[0]
				graph[key] = append(graph[key], extractBlockRefs(block.Body, localRefs)...)
			case "locals":
				for _, ref := range extractBlockRefs(block.Body, localRefs) {
					localRefs[ref] = struct{}{}
				}
			}
		}
	}
	if len(localRefs) == 0 {
		return normalizeGraphRefs(graph), nil
	}
	localDeps := make([]string, 0, len(localRefs))
	for ref := range localRefs {
		localDeps = append(localDeps, ref)
	}
	sort.Strings(localDeps)
	for key, refs := range graph {
		if usesLocalMarker(refs) {
			refs = append(refs, localDeps...)
		}
		graph[key] = refs
	}
	return normalizeGraphRefs(graph), nil
}

func extractBlockRefs(body *hclsyntax.Body, localRefs map[string]struct{}) []string {
	if body == nil {
		return nil
	}
	var out []string
	for _, attr := range body.Attributes {
		out = append(out, extractExprRefs(attr.Expr, localRefs)...)
	}
	for _, block := range body.Blocks {
		out = append(out, extractBlockRefs(block.Body, localRefs)...)
	}
	return out
}

func extractExprRefs(expr hcl.Expression, localRefs map[string]struct{}) []string {
	if expr == nil {
		return nil
	}
	traversals := expr.Variables()
	out := make([]string, 0, len(traversals))
	for _, tr := range traversals {
		ref, ok, local := traversalToRef(tr)
		if local {
			out = append(out, "__local__")
			continue
		}
		if ok {
			out = append(out, ref)
		}
	}
	return out
}

func traversalToRef(tr hcl.Traversal) (ref string, ok bool, local bool) {
	if len(tr) == 0 {
		return "", false, false
	}
	root := tr.RootName()
	switch root {
	case "var", "path", "each", "count", "self", "terraform", "tofu":
		return "", false, false
	case "local":
		return "", false, true
	case "data":
		if len(tr) < 3 {
			return "", false, false
		}
		typeStep, typeOK := tr[1].(hcl.TraverseAttr)
		nameStep, nameOK := tr[2].(hcl.TraverseAttr)
		if !typeOK || !nameOK {
			return "", false, false
		}
		return "data." + typeStep.Name + "." + nameStep.Name, true, false
	case "module":
		if len(tr) < 2 {
			return "", false, false
		}
		callStep, callOK := tr[1].(hcl.TraverseAttr)
		if !callOK {
			return "", false, false
		}
		return "module." + callStep.Name, true, false
	default:
		if len(tr) < 2 {
			return "", false, false
		}
		nameStep, nameOK := tr[1].(hcl.TraverseAttr)
		if !nameOK {
			return "", false, false
		}
		return root + "." + nameStep.Name, true, false
	}
}

func normalizeGraphRefs(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, refs := range in {
		seen := map[string]struct{}{}
		list := make([]string, 0, len(refs))
		for _, ref := range refs {
			ref = strings.TrimSpace(ref)
			if ref == "" || ref == "__local__" {
				continue
			}
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			list = append(list, ref)
		}
		sort.Strings(list)
		out[key] = list
	}
	return out
}

func usesLocalMarker(refs []string) bool {
	for _, ref := range refs {
		if ref == "__local__" {
			return true
		}
	}
	return false
}

func moduleHead(addr string) string {
	if !strings.HasPrefix(addr, "module.") {
		return ""
	}
	parts := strings.Split(addr, ".")
	if len(parts) < 2 {
		return ""
	}
	return "module." + parts[1]
}
