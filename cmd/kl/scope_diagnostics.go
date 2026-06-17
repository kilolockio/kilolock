package main

import (
	"fmt"
	"sort"
	"strings"
)

const scopePreviewLimit = 8

func formatTargetScopeViolation(verr error, requestedTargets, writeSet []string) error {
	if verr == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString(verr.Error())
	if len(requestedTargets) > 0 {
		b.WriteString("\n  requested targets: ")
		b.WriteString(strings.Join(requestedTargets, ", "))
	}
	if len(writeSet) > 0 {
		b.WriteString("\n  planned writes (preview): ")
		b.WriteString(strings.Join(previewSlice(writeSet, scopePreviewLimit), ", "))
	}
	suggested := suggestTargetsFromWrites(writeSet, requestedTargets)
	if len(suggested) > 0 {
		b.WriteString("\n  suggested extra --target: ")
		b.WriteString(strings.Join(suggested, ", "))
	}
	b.WriteString("\n  hint: if this fanout is expected, run full plan/apply instead of scoped target mode")
	return fmt.Errorf("%s", b.String())
}

func previewSlice(in []string, max int) []string {
	if len(in) <= max {
		return append([]string(nil), in...)
	}
	out := append([]string(nil), in[:max]...)
	out = append(out, fmt.Sprintf("... +%d more", len(in)-max))
	return out
}

func suggestTargetsFromWrites(writeSet, requested []string) []string {
	if len(writeSet) == 0 {
		return nil
	}
	seenReq := map[string]struct{}{}
	for _, t := range requested {
		seenReq[strings.TrimSpace(t)] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, w := range writeSet {
		s := targetSuggestionForAddress(strings.TrimSpace(w))
		if s == "" {
			continue
		}
		if _, ok := seenReq[s]; ok {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > scopePreviewLimit {
		out = out[:scopePreviewLimit]
	}
	return out
}

func targetSuggestionForAddress(addr string) string {
	if addr == "" {
		return ""
	}
	parts := strings.Split(addr, ".")
	if len(parts) < 2 {
		return ""
	}
	if parts[0] == "module" && len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return parts[0] + "." + parts[1]
}
