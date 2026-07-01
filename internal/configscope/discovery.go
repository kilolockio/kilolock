package configscope

import (
	"fmt"
	"os"
	"strings"

	"github.com/kilolockio/kilolock/internal/plan"
)

const (
	// EnvDiscoveryEngine selects the config-intent discovery backend.
	//
	// Supported values:
	//   heuristic  - current lightweight extractor
	//   opentofu   - OpenTofu-style HCL graph walk
	//   auto       - prefer opentofu and fall back to heuristic
	//
	// Empty behaves the same as "auto".
	EnvDiscoveryEngine = "KL_CONFIG_DISCOVERY"

	EngineHeuristic = "heuristic"
	EngineOpenTofu  = "opentofu"
	EngineAuto      = "auto"
)

// ErrUnsupportedEngine is returned when a requested discovery engine is not
// compiled into the current binary.
var ErrUnsupportedEngine = fmt.Errorf("unsupported config discovery engine")

// Selector is the backend-facing scope primitive derived from local config.
// The model is intentionally small and stable so the current heuristic
// extractor can later be replaced by an OpenTofu-backed implementation
// without changing caller contracts.
type Selector struct {
	Kind  string
	Value string
}

// ConfigNode is a lightweight config-declared graph node sent to the backend
// so it can reason about undeployed resources alongside realized state.
type ConfigNode struct {
	Address      string
	Dependencies []string
}

// Intent is the normalized local-config signal that kl sends to the
// state-engine backend for scope expansion.
type Intent struct {
	PlanningTargets            []string
	Selectors                  []Selector
	ExplicitWriteCandidates    []string
	ExplicitReadCandidates     []string
	UndeployedConfigCandidates []string
	RemovedConfigCandidates    []string
	ConfigNodes                []ConfigNode
	DiscoveryEngine            string
	DiscoveryNotes             []string
}

// Engine produces Intent from local Terraform/OpenTofu configuration.
// The rest of kl depends on this interface rather than on any specific parser.
type Engine interface {
	Name() string
	DiscoverForFiles(configDir string, scope *plan.FileScope) (*Intent, error)
	DiscoverForTargets(configDir string, targets []string) (*Intent, error)
}

// DiscoverForFiles derives backend expansion intent from --file selections.
func DiscoverForFiles(configDir string, scope *plan.FileScope) (*Intent, error) {
	engine, err := selectedEngine()
	if err != nil {
		return nil, err
	}
	return engine.DiscoverForFiles(configDir, scope)
}

// DiscoverForTargets derives backend expansion intent from --target addresses.
func DiscoverForTargets(configDir string, targets []string) (*Intent, error) {
	engine, err := selectedEngine()
	if err != nil {
		return nil, err
	}
	return engine.DiscoverForTargets(configDir, targets)
}

func selectedEngine() (Engine, error) {
	switch normalizedEngineName(os.Getenv(EnvDiscoveryEngine)) {
	case "", EngineAuto:
		return autoEngine{}, nil
	case EngineHeuristic:
		return heuristicEngine{}, nil
	case EngineOpenTofu:
		if !opentofuEngineAvailable() {
			return nil, fmt.Errorf("%w %q: OpenTofu-backed config discovery is not compiled into this build yet", ErrUnsupportedEngine, EngineOpenTofu)
		}
		return opentofuEngine{}, nil
	default:
		return nil, fmt.Errorf("%w %q (supported: %s, %s, %s)", ErrUnsupportedEngine, strings.TrimSpace(os.Getenv(EnvDiscoveryEngine)), EngineAuto, EngineHeuristic, EngineOpenTofu)
	}
}

func normalizedEngineName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

type heuristicEngine struct{}

func (heuristicEngine) Name() string { return EngineHeuristic }

type autoEngine struct{}

func (autoEngine) Name() string { return EngineAuto }

func (autoEngine) DiscoverForFiles(configDir string, scope *plan.FileScope) (*Intent, error) {
	return discoverWithAutoFallback(
		func() (*Intent, error) { return opentofuEngine{}.DiscoverForFiles(configDir, scope) },
		func() (*Intent, error) { return heuristicEngine{}.DiscoverForFiles(configDir, scope) },
	)
}

func (autoEngine) DiscoverForTargets(configDir string, targets []string) (*Intent, error) {
	return discoverWithAutoFallback(
		func() (*Intent, error) { return opentofuEngine{}.DiscoverForTargets(configDir, targets) },
		func() (*Intent, error) { return heuristicEngine{}.DiscoverForTargets(configDir, targets) },
	)
}

func (heuristicEngine) DiscoverForFiles(configDir string, scope *plan.FileScope) (*Intent, error) {
	selected, err := plan.AnalyzeSelectedFiles(configDir, scope.Relative)
	if err != nil {
		return nil, err
	}
	sliceCandidates, err := plan.ExpandTargetSliceAddresses(configDir, selected.Targets)
	if err != nil {
		return nil, err
	}
	graph, _ := loadOpenTofuGraph(configDir)
	intent := buildIntent(selected.Targets, selected.RootResources, selected.RemovedResources, sliceCandidates, graph)
	intent.DiscoveryEngine = EngineHeuristic
	return intent, nil
}

func (heuristicEngine) DiscoverForTargets(configDir string, targets []string) (*Intent, error) {
	sliceCandidates, err := plan.ExpandTargetSliceAddresses(configDir, targets)
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
	graph, _ := loadOpenTofuGraph(configDir)
	intent := buildIntent(targets, rootResources, nil, sliceCandidates, graph)
	intent.DiscoveryEngine = EngineHeuristic
	return intent, nil
}

func buildIntent(targets []string, rootResources map[string]struct{}, removedResources map[string]struct{}, sliceCandidates []string, graph map[string][]string) *Intent {
	intent := &Intent{
		PlanningTargets: targets,
	}
	writeSet := make(map[string]struct{}, len(rootResources))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if strings.HasPrefix(target, "module.") {
			intent.Selectors = append(intent.Selectors, Selector{Kind: "module_prefix", Value: target})
			continue
		}
		intent.Selectors = append(intent.Selectors, Selector{Kind: "resource_address", Value: target})
	}
	for addr := range rootResources {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			writeSet[addr] = struct{}{}
		}
	}
	for addr := range writeSet {
		intent.ExplicitWriteCandidates = append(intent.ExplicitWriteCandidates, addr)
	}
	for _, addr := range sliceCandidates {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		intent.UndeployedConfigCandidates = append(intent.UndeployedConfigCandidates, addr)
		if _, ok := writeSet[addr]; ok {
			continue
		}
		intent.ExplicitReadCandidates = append(intent.ExplicitReadCandidates, addr)
	}
	intent.Selectors = dedupeSelectors(intent.Selectors)
	intent.ExplicitWriteCandidates = dedupeSortedStrings(intent.ExplicitWriteCandidates)
	intent.ExplicitReadCandidates = dedupeSortedStrings(intent.ExplicitReadCandidates)
	intent.UndeployedConfigCandidates = dedupeSortedStrings(intent.UndeployedConfigCandidates)
	intent.RemovedConfigCandidates = buildRemovedCandidates(rootResources, removedResources, graph)
	// Include read candidates too, not only writes/undeployed nodes. Safe
	// native file-scoped applies often depend on config-only nodes that first
	// appear on the read side of the local graph. If we omit their config-node
	// metadata here, the backend has less graph context than the client already
	// discovered and may unnecessarily classify the scope as ambiguous/fallback.
	intent.ConfigNodes = buildConfigNodes(
		graph,
		append(
			append(
				append([]string{}, intent.ExplicitWriteCandidates...),
				intent.ExplicitReadCandidates...,
			),
			intent.UndeployedConfigCandidates...,
		),
	)
	return intent
}

func discoverWithAutoFallback(primary, fallback func() (*Intent, error)) (*Intent, error) {
	intent, err := primary()
	if err == nil {
		if intent != nil && strings.TrimSpace(intent.DiscoveryEngine) == "" {
			intent.DiscoveryEngine = EngineOpenTofu
		}
		return intent, nil
	}
	fallbackIntent, fallbackErr := fallback()
	if fallbackErr != nil {
		return nil, fmt.Errorf("auto config discovery failed: opentofu=%v; heuristic=%v", err, fallbackErr)
	}
	if fallbackIntent != nil {
		fallbackIntent.DiscoveryEngine = EngineHeuristic
		fallbackIntent.DiscoveryNotes = append(fallbackIntent.DiscoveryNotes,
			fmt.Sprintf("config discovery fell back from %s to %s: %v", EngineOpenTofu, EngineHeuristic, err))
	}
	return fallbackIntent, nil
}

func buildRemovedCandidates(rootResources map[string]struct{}, removedResources map[string]struct{}, graph map[string][]string) []string {
	if len(rootResources) == 0 {
		return nil
	}
	out := make([]string, 0)
	for addr := range rootResources {
		addr = strings.TrimSpace(addr)
		if addr == "" || strings.HasPrefix(addr, "module.") {
			continue
		}
		if _, ok := removedResources[addr]; ok {
			out = append(out, addr)
			continue
		}
		if _, ok := graph[addr]; ok {
			continue
		}
		out = append(out, addr)
	}
	return dedupeSortedStrings(out)
}

func buildConfigNodes(graph map[string][]string, addresses []string) []ConfigNode {
	if len(graph) == 0 || len(addresses) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]ConfigNode, 0, len(addresses))
	for _, addr := range dedupeSortedStrings(addresses) {
		deps, ok := graph[addr]
		if !ok {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, ConfigNode{
			Address:      addr,
			Dependencies: dedupeSortedStrings(deps),
		})
	}
	return out
}

func dedupeSelectors(in []Selector) []Selector {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]Selector, 0, len(in))
	for _, s := range in {
		kind := strings.TrimSpace(s.Kind)
		value := strings.TrimSpace(s.Value)
		if kind == "" || value == "" {
			continue
		}
		key := kind + "\x00" + value
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Selector{Kind: kind, Value: value})
	}
	return out
}

func dedupeSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) > 1 {
		for i := 0; i < len(out)-1; i++ {
			for j := i + 1; j < len(out); j++ {
				if out[j] < out[i] {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
	}
	return out
}
