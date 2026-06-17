package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/davesade/kilolock/internal/slice"
)

type ScopedPlanOptions struct {
	Refresh bool
	Vars    map[string]json.RawMessage
}

const scopedBackendOverrideFilename = "kl_backend_override.auto.tf"

func RunScopedTerraformPlan(ctx context.Context, terraformBin, configDir string, trunkRaw []byte, scope *FileScope, opts ScopedPlanOptions) ([]byte, error) {
	if scope == nil || len(scope.Relative) == 0 {
		return nil, fmt.Errorf("scoped plan: file scope is empty")
	}
	selected, err := AnalyzeSelectedFiles(configDir, scope.Relative)
	if err != nil {
		return nil, err
	}
	if len(selected.Targets) == 0 {
		return nil, fmt.Errorf("scoped plan: selected files declare no root resources/modules (for deletions, add a `removed { from = ... }` block or run a full plan)")
	}

	trunk, err := slice.ParseTrunkState(trunkRaw)
	if err != nil {
		return nil, fmt.Errorf("scoped plan: parse trunk state: %w", err)
	}
	sliceTargets, err := expandScopedSliceAddresses(configDir, selected)
	if err != nil {
		return nil, err
	}
	sliced := buildScopedStateSlice(trunk, sliceTargets)

	// Create the scoped workspace under the project directory so CI
	// environments with noexec /tmp mounts can still execute provider
	// binaries from the copied .terraform cache.
	dir, err := os.MkdirTemp(configDir, ".kl-plan-scope-*")
	if err != nil {
		return nil, fmt.Errorf("scoped plan: mkdir tmp: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := buildScopedWorkspace(configDir, dir, scope, selected); err != nil {
		return nil, err
	}
	if err := writeScopedState(dir, sliced); err != nil {
		return nil, err
	}
	if err := RunTerraformInit(ctx, terraformBin, dir); err != nil {
		return nil, err
	}

	tfplan, err := os.CreateTemp(dir, ".kl-plan-*.tfplan")
	if err != nil {
		return nil, fmt.Errorf("scoped plan: create tmp plan: %w", err)
	}
	tfplan.Close()
	defer os.Remove(tfplan.Name())

	if err := RunTerraformPlan(ctx, terraformBin, dir, tfplan.Name(), PlanRunOptions{
		Lock:    false,
		Refresh: opts.Refresh,
		Vars:    opts.Vars,
		Targets: selected.Targets,
	}); err != nil {
		return nil, err
	}
	return RunTerraformShow(ctx, terraformBin, dir, tfplan.Name())
}

// RunTargetScopedTerraformPlan runs a targeted terraform plan in a local
// backend workspace built from the operator config + a sliced trunk state.
// Targets are Terraform addresses (`resource.type.name`, `module.x`, ...).
func RunTargetScopedTerraformPlan(ctx context.Context, terraformBin, configDir string, trunkRaw []byte, targets []string, opts ScopedPlanOptions) ([]byte, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("targeted plan: target list is empty")
	}
	sliceTargets, err := ExpandTargetSliceAddresses(configDir, targets)
	if err != nil {
		return nil, err
	}
	trunk, err := slice.ParseTrunkState(trunkRaw)
	if err != nil {
		return nil, fmt.Errorf("targeted plan: parse trunk state: %w", err)
	}
	sliced := buildScopedStateSlice(trunk, sliceTargets)

	dir, err := os.MkdirTemp(configDir, ".kl-plan-target-*")
	if err != nil {
		return nil, fmt.Errorf("targeted plan: mkdir tmp: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := buildTargetWorkspace(configDir, dir); err != nil {
		return nil, err
	}
	if err := writeScopedState(dir, sliced); err != nil {
		return nil, err
	}
	if err := RunTerraformInit(ctx, terraformBin, dir); err != nil {
		return nil, err
	}
	tfplan, err := os.CreateTemp(dir, ".kl-target-*.tfplan")
	if err != nil {
		return nil, fmt.Errorf("targeted plan: create tmp plan: %w", err)
	}
	tfplan.Close()
	defer os.Remove(tfplan.Name())

	if err := RunTerraformPlan(ctx, terraformBin, dir, tfplan.Name(), PlanRunOptions{
		Lock:    false,
		Refresh: opts.Refresh,
		Vars:    opts.Vars,
		Targets: targets,
	}); err != nil {
		return nil, err
	}
	return RunTerraformShow(ctx, terraformBin, dir, tfplan.Name())
}

func expandScopedSliceAddresses(configDir string, selected *SelectedScope) ([]string, error) {
	if selected == nil {
		return nil, fmt.Errorf("scoped plan: selected scope is nil")
	}
	if len(selected.Targets) == 0 {
		return append([]string(nil), selected.SliceAddresses...), nil
	}
	expanded, err := ExpandTargetSliceAddresses(configDir, selected.Targets)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(expanded)+len(selected.SliceAddresses))
	for _, a := range expanded {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	for _, a := range selected.SliceAddresses {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

func ExpandTargetSliceAddresses(configDir string, targets []string) ([]string, error) {
	deps := map[string][]string{}
	localDeps := map[string]struct{}{}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return nil, fmt.Errorf("targeted plan: read dir %s: %w", configDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		path := filepath.Join(configDir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("targeted plan: read file %s: %w", path, err)
		}
		blocks, err := scanRootBlocks(b)
		if err != nil {
			return nil, fmt.Errorf("targeted plan: parse file %s: %w", path, err)
		}
		for _, block := range blocks {
			var key string
			switch block.Type {
			case "resource":
				m := resourceDeclRE.FindSubmatch(block.Bytes)
				if len(m) == 3 {
					key = string(m[1]) + "." + string(m[2])
				}
			case "data":
				m := dataDeclRE.FindSubmatch(block.Bytes)
				if len(m) == 3 {
					key = "data." + string(m[1]) + "." + string(m[2])
				}
			case "module":
				m := moduleDeclRE.FindSubmatch(block.Bytes)
				if len(m) == 2 {
					key = "module." + string(m[1])
				}
			case "locals":
				for _, d := range blockDependencyAddresses(block.Bytes) {
					localDeps[d] = struct{}{}
				}
				continue
			}
			if key == "" {
				continue
			}
			ds := blockDependencyAddresses(block.Bytes)
			if strings.Contains(string(block.Bytes), "local.") {
				for d := range localDeps {
					ds = append(ds, d)
				}
			}
			deps[key] = append(deps[key], ds...)
		}
	}

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
	for _, t := range targets {
		push(t)
		if head := moduleHead(t); head != "" && head != t {
			push(head)
		}
	}
	for i := 0; i < len(queue); i++ {
		cur := queue[i]
		for _, d := range deps[cur] {
			push(d)
			if head := moduleHead(d); head != "" && head != d {
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

func buildScopedWorkspace(srcDir, dstDir string, scope *FileScope, analyzed *SelectedScope) error {
	selectedFiles := map[string]struct{}{}
	for _, rel := range scope.Relative {
		selectedFiles[rel] = struct{}{}
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("scoped plan: read dir %s: %w", srcDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "terraform.tfstate" || name == "terraform.tfstate.backup" {
			continue
		}
		srcPath := filepath.Join(srcDir, name)
		dstPath := filepath.Join(dstDir, name)
		if e.IsDir() {
			if name == ".terraform" {
				if err := copyDir(srcPath, dstPath); err != nil {
					return err
				}
			}
			continue
		}
		if name == ".terraform.lock.hcl" {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if filepath.Ext(name) != ".tf" {
			continue
		}
		rel := filepath.ToSlash(name)
		if _, ok := selectedFiles[rel]; ok {
			if err := copyTerraformFileForScopedWorkspace(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		filtered, err := filterSupportBlocks(srcPath)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(filtered)) == 0 {
			continue
		}
		filtered, err = sanitizeTerraformForScopedWorkspace(filtered)
		if err != nil {
			return fmt.Errorf("sanitize scoped support file %s: %w", dstPath, err)
		}
		if err := os.WriteFile(dstPath, filtered, 0o644); err != nil {
			return fmt.Errorf("write scoped support file %s: %w", dstPath, err)
		}
	}

	const localBackend = `terraform {
  backend "local" {}
}
`
	if err := os.WriteFile(filepath.Join(dstDir, scopedBackendOverrideFilename), []byte(localBackend), 0o644); err != nil {
		return fmt.Errorf("write scoped backend.tf: %w", err)
	}
	// Use the same local-module discovery strategy as target-scoped workspaces:
	// collect module sources across the full config tree. This prevents
	// --file plans from failing when a copied support file contains a module
	// block whose source path isn't declared in the selected file itself.
	modulePaths, err := collectLocalModulePaths(srcDir)
	if err != nil {
		return err
	}
	modulePaths = append(modulePaths, analyzed.LocalModulePaths...)
	modulePaths = dedupeSortedPaths(modulePaths)
	for _, localModulePath := range modulePaths {
		rel, err := filepath.Rel(srcDir, localModulePath)
		if err != nil {
			return fmt.Errorf("scoped plan: module path %s: %w", localModulePath, err)
		}
		if err := copyDir(localModulePath, filepath.Join(dstDir, rel)); err != nil {
			return err
		}
	}
	return nil
}

func buildTargetWorkspace(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("targeted plan: read dir %s: %w", srcDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "terraform.tfstate" || name == "terraform.tfstate.backup" {
			continue
		}
		srcPath := filepath.Join(srcDir, name)
		dstPath := filepath.Join(dstDir, name)
		if e.IsDir() {
			if name == ".terraform" {
				if err := copyDir(srcPath, dstPath); err != nil {
					return err
				}
			}
			continue
		}
		if name == ".terraform.lock.hcl" || filepath.Ext(name) == ".tfvars" {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if filepath.Ext(name) == ".tf" {
			if err := copyTerraformFileForScopedWorkspace(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	const localBackend = `terraform {
  backend "local" {}
}
`
	if err := os.WriteFile(filepath.Join(dstDir, scopedBackendOverrideFilename), []byte(localBackend), 0o644); err != nil {
		return fmt.Errorf("write targeted backend.tf: %w", err)
	}
	modulePaths, err := collectLocalModulePaths(srcDir)
	if err != nil {
		return err
	}
	for _, localModulePath := range modulePaths {
		rel, err := filepath.Rel(srcDir, localModulePath)
		if err != nil {
			return fmt.Errorf("targeted plan: module path %s: %w", localModulePath, err)
		}
		if err := copyDir(localModulePath, filepath.Join(dstDir, rel)); err != nil {
			return err
		}
	}
	return nil
}

func collectLocalModulePaths(configDir string) ([]string, error) {
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return nil, fmt.Errorf("targeted plan: read dir %s: %w", configDir, err)
	}
	seen := map[string]struct{}{}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		path := filepath.Join(configDir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("targeted plan: read file %s: %w", path, err)
		}
		blocks, err := scanRootBlocks(b)
		if err != nil {
			return nil, fmt.Errorf("targeted plan: parse file %s: %w", path, err)
		}
		for _, block := range blocks {
			if block.Type != "module" {
				continue
			}
			sm := moduleSourceRE.FindSubmatch(block.Bytes)
			if len(sm) != 2 {
				continue
			}
			src := string(sm[1])
			if !isLocalModuleSource(src) {
				continue
			}
			local := filepath.Clean(filepath.Join(configDir, filepath.FromSlash(src)))
			if _, ok := seen[local]; ok {
				continue
			}
			seen[local] = struct{}{}
			out = append(out, local)
		}
	}
	sort.Strings(out)
	return out, nil
}

func dedupeSortedPaths(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := make([]string, 0, len(in))
	var prev string
	for i, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i == 0 || p != prev {
			out = append(out, p)
		}
		prev = p
	}
	return out
}

func writeScopedState(dir string, st *slice.TrunkState) error {
	b, err := slice.MarshalTrunkState(st)
	if err != nil {
		return fmt.Errorf("scoped plan: marshal state slice: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfstate"), b, 0o644); err != nil {
		return fmt.Errorf("scoped plan: write terraform.tfstate: %w", err)
	}
	return nil
}

type rootBlock struct {
	Type  string
	Start int
	End   int
	Bytes []byte
}

var rootBlockStartRE = regexp.MustCompile(`(?m)^[ \t]*(terraform|locals|provider|variable|output|resource|data|module|moved|removed|import)\b[^\n{]*\{`)
var backendBlockStartRE = regexp.MustCompile(`(?m)^[ \t]*backend[ \t]+"[^"]+"[^\n{]*\{`)

func filterSupportBlocks(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read support file %s: %w", path, err)
	}
	blocks, err := scanRootBlocks(b)
	if err != nil {
		return nil, fmt.Errorf("parse support file %s: %w", path, err)
	}
	var out bytes.Buffer
	for _, block := range blocks {
		switch block.Type {
		case "terraform", "provider", "variable", "locals", "resource", "data", "module":
			out.Write(block.Bytes)
			if len(block.Bytes) == 0 || block.Bytes[len(block.Bytes)-1] != '\n' {
				out.WriteByte('\n')
			}
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), nil
}

func sanitizeTerraformForScopedWorkspace(src []byte) ([]byte, error) {
	blocks, err := scanRootBlocks(src)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return append([]byte(nil), src...), nil
	}
	var out bytes.Buffer
	cursor := 0
	for _, block := range blocks {
		if block.Start > cursor {
			out.Write(src[cursor:block.Start])
		}
		if block.Type != "terraform" {
			out.Write(block.Bytes)
			cursor = block.End
			continue
		}
		sanitized, err := stripBackendBlocksFromTerraformBlock(block.Bytes)
		if err != nil {
			return nil, err
		}
		out.Write(sanitized)
		cursor = block.End
	}
	if cursor < len(src) {
		out.Write(src[cursor:])
	}
	return out.Bytes(), nil
}

func stripBackendBlocksFromTerraformBlock(block []byte) ([]byte, error) {
	openRel := bytes.IndexByte(block, '{')
	if openRel < 0 || len(block) < 2 {
		return append([]byte(nil), block...), nil
	}
	closeIdx := len(block) - 1
	header := append([]byte(nil), block[:openRel+1]...)
	body := block[openRel+1 : closeIdx]
	footer := append([]byte(nil), block[closeIdx:]...)

	type cut struct{ start, end int }
	var cuts []cut
	pos := 0
	for pos < len(body) {
		loc := backendBlockStartRE.FindSubmatchIndex(body[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		headerSlice := body[start : pos+loc[1]]
		open := bytes.IndexByte(headerSlice, '{')
		if open < 0 {
			pos = start + 1
			continue
		}
		openIdx := start + open
		endIdx, err := findMatchingBrace(body, openIdx)
		if err != nil {
			return nil, err
		}
		cuts = append(cuts, cut{start: start, end: endIdx + 1})
		pos = endIdx + 1
	}
	if len(cuts) == 0 {
		return append([]byte(nil), block...), nil
	}
	var out bytes.Buffer
	out.Write(header)
	cursor := 0
	for _, c := range cuts {
		if c.start > cursor {
			out.Write(body[cursor:c.start])
		}
		cursor = c.end
	}
	if cursor < len(body) {
		out.Write(body[cursor:])
	}
	out.Write(footer)
	return out.Bytes(), nil
}

func scanRootBlocks(src []byte) ([]rootBlock, error) {
	var out []rootBlock
	pos := 0
	for pos < len(src) {
		loc := rootBlockStartRE.FindSubmatchIndex(src[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		typ := string(src[pos+loc[2] : pos+loc[3]])
		header := src[start : pos+loc[1]]
		openRel := bytes.IndexByte(header, '{')
		if openRel < 0 {
			return nil, fmt.Errorf("root block %q missing opening brace", typ)
		}
		openIdx := start + openRel
		endIdx, err := findMatchingBrace(src, openIdx)
		if err != nil {
			return nil, err
		}
		out = append(out, rootBlock{
			Type:  typ,
			Start: start,
			End:   endIdx + 1,
			Bytes: append([]byte(nil), src[start:endIdx+1]...),
		})
		pos = endIdx + 1
	}
	return out, nil
}

func findMatchingBrace(src []byte, openIdx int) (int, error) {
	depth := 0
	inString := false
	escape := false
	inLineComment := false
	inBlockComment := false

	for i := openIdx; i < len(src); i++ {
		c := src[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inString {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			continue
		}
		if c == '#' {
			inLineComment = true
			continue
		}
		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				inLineComment = true
				i++
				continue
			}
			if src[i+1] == '*' {
				inBlockComment = true
				i++
				continue
			}
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated block starting at byte %d", openIdx)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if err := os.WriteFile(dst, b, info.Mode().Perm()); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
}

func copyTerraformFileForScopedWorkspace(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	sanitized, err := sanitizeTerraformForScopedWorkspace(b)
	if err != nil {
		return fmt.Errorf("sanitize terraform file %s: %w", src, err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if err := os.WriteFile(dst, sanitized, info.Mode().Perm()); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", src, err)
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) > 0 && name[0] == '.' {
			continue
		}
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)
		if e.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func buildScopedStateSlice(trunk *slice.TrunkState, targets []string) *slice.TrunkState {
	if trunk == nil {
		return nil
	}
	targetSet := map[string]struct{}{}
	modulePrefixes := make([]string, 0)
	for _, t := range targets {
		targetSet[t] = struct{}{}
		if modulePrefixTarget(t) {
			modulePrefixes = append(modulePrefixes, t+".")
		}
	}
	out := &slice.TrunkState{
		Version:          trunk.Version,
		TerraformVersion: trunk.TerraformVersion,
		Serial:           trunk.Serial,
		Lineage:          trunk.Lineage,
		Outputs:          trunk.Outputs,
		CheckResults:     trunk.CheckResults,
	}
	for _, r := range trunk.Resources {
		addr := r.Address()
		if _, ok := targetSet[addr]; ok {
			out.Resources = append(out.Resources, r)
			continue
		}
		for _, prefix := range modulePrefixes {
			if len(addr) > len(prefix) && addr[:len(prefix)] == prefix {
				out.Resources = append(out.Resources, r)
				break
			}
		}
	}
	return out
}

func modulePrefixTarget(target string) bool {
	return len(target) > len("module.") && target[:len("module.")] == "module."
}
