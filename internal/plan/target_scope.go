package plan

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateTargetedWriteSet ensures terraform's predicted writes stay inside the
// caller-approved target scope/closure. Module targets authorize writes to
// descendants (module.x.*); resource/data targets authorize exact-address only.
func ValidateTargetedWriteSet(writeSet []string, allowed []string) error {
	allowSet := make(map[string]struct{}, len(allowed))
	modulePrefixes := make([]string, 0, len(allowed))
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		allowSet[a] = struct{}{}
		if strings.HasPrefix(a, "module.") {
			modulePrefixes = append(modulePrefixes, a+".")
		}
	}

	var unexpected []string
	for _, w := range writeSet {
		if _, ok := allowSet[w]; ok {
			continue
		}
		ok := false
		for _, p := range modulePrefixes {
			if strings.HasPrefix(w, p) {
				ok = true
				break
			}
		}
		if !ok {
			unexpected = append(unexpected, w)
		}
	}
	if len(unexpected) == 0 {
		return nil
	}
	sort.Strings(unexpected)
	allowedPreview := allowed
	if len(allowedPreview) > 8 {
		allowedPreview = allowedPreview[:8]
	}
	msg := fmt.Sprintf("target scope violation: planned writes outside safe target closure: %s", strings.Join(unexpected, ", "))
	if len(allowed) > 0 {
		msg += fmt.Sprintf(" (allowed preview: %s", strings.Join(allowedPreview, ", "))
		if len(allowed) > len(allowedPreview) {
			msg += fmt.Sprintf(", ... +%d more", len(allowed)-len(allowedPreview))
		}
		msg += ")"
	}
	msg += "; add missing --target entries or run full plan"
	return fmt.Errorf("%s", msg)
}
