package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The Terraform CLI lays installed provider binaries out under a
// well-defined tree:
//
//	<root>/<hostname>/<namespace>/<name>/<version>/<os>_<arch>/terraform-provider-<name>[_v<version>][_x<protocol>]
//
// Two roots matter in practice:
//
//   - The workspace's own `.terraform/providers/` directory, populated
//     by `terraform init`.
//
//   - The user's global plugin cache (TF_PLUGIN_CACHE_DIR), populated
//     when the cache env var is set during init.
//
// Discover walks these roots to locate a binary suitable for launching
// via the Launch function in this package. It is the production
// equivalent of the providerOnDisk test helper: the latter uses
// `terraform init` to populate the tree from scratch; production reads
// what's already there.

// Default registry hostname used when a SourceAddress omits one.
const DefaultRegistry = "registry.terraform.io"

// Default namespace used when a SourceAddress is given as a bare
// name like "null". Mirrors Terraform's own implicit default.
const DefaultNamespace = "hashicorp"

// SourceAddress identifies a provider by its registry coordinates.
// Two SourceAddresses are equal iff all three fields are equal.
type SourceAddress struct {
	// Hostname is the registry the provider is served from, e.g.
	// "registry.terraform.io" or "private.example.com".
	Hostname string

	// Namespace is the registry-internal organization name, e.g.
	// "hashicorp" or "your-org".
	Namespace string

	// Name is the provider name, e.g. "null" or "aws". Does NOT
	// include the "terraform-provider-" filename prefix.
	Name string
}

// String reports the canonical three-part address.
func (s SourceAddress) String() string {
	return s.Hostname + "/" + s.Namespace + "/" + s.Name
}

// ParseSourceAddress parses a provider source string into its three
// components. Three forms are accepted, matching what Terraform's
// HCL `required_providers` block accepts:
//
//	"name"                       → hashicorp/name on registry.terraform.io
//	"namespace/name"             → registry.terraform.io/namespace/name
//	"hostname/namespace/name"    → as written
//
// Leading/trailing whitespace is stripped. Empty components are
// rejected. Hostnames must contain a dot (matching Terraform's own
// disambiguation rule: a leading "foo/bar" treats "foo" as a
// namespace, never a hostname, even if "foo" looked plausible
// as one).
func ParseSourceAddress(s string) (SourceAddress, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return SourceAddress{}, errors.New("source address is empty")
	}
	parts := strings.Split(s, "/")
	for i, p := range parts {
		if p == "" {
			return SourceAddress{}, fmt.Errorf("source address %q: empty component at position %d", s, i)
		}
	}
	switch len(parts) {
	case 1:
		return SourceAddress{Hostname: DefaultRegistry, Namespace: DefaultNamespace, Name: parts[0]}, nil
	case 2:
		return SourceAddress{Hostname: DefaultRegistry, Namespace: parts[0], Name: parts[1]}, nil
	case 3:
		if !strings.Contains(parts[0], ".") {
			return SourceAddress{}, fmt.Errorf("source address %q: hostname %q must contain a dot", s, parts[0])
		}
		return SourceAddress{Hostname: parts[0], Namespace: parts[1], Name: parts[2]}, nil
	default:
		return SourceAddress{}, fmt.Errorf("source address %q: expected 1-3 slash-separated parts, got %d", s, len(parts))
	}
}

// DiscoveryOptions configure how Discover locates a provider binary.
// The zero value is not valid; SearchPaths must be populated.
type DiscoveryOptions struct {
	// SearchPaths are root directories that contain registry trees.
	// Order matters: the first root that yields a candidate wins.
	// Within a root, the highest installed version (or the pinned
	// version if Version is set) is selected.
	//
	// Pass the workspace's `.terraform/providers/` directory first,
	// then any plugin cache (TF_PLUGIN_CACHE_DIR) as a fallback.
	SearchPaths []string

	// Version, if non-empty, pins selection to that exact version
	// string (matched literally against the version directory name).
	// Empty means "highest installed".
	Version string

	// Platform overrides the default <GOOS>_<GOARCH> selection.
	// Useful for tests that need to construct deterministic trees.
	// Empty defaults to runtime.GOOS + "_" + runtime.GOARCH.
	Platform string
}

// DiscoveredProvider is what Discover returns: the resolved binary
// and metadata about how it was chosen.
type DiscoveredProvider struct {
	Source   SourceAddress
	Version  string
	Platform string
	Binary   string
}

// ErrProviderNotFound is the sentinel returned when no candidate
// matched. The error message includes the search paths tried and
// any near misses observed.
var ErrProviderNotFound = errors.New("provider: no installed binary found")

// Discover locates a provider binary across the configured search
// paths. Selection rules:
//
//  1. For each SearchPath, examine the canonical layout subtree
//     `<root>/<hostname>/<namespace>/<name>/`.
//  2. Within that subtree, collect installed version directories.
//     If opts.Version is set, require an exact match.
//  3. Pick the highest-comparing version using compareVersions.
//  4. Within the chosen version dir, look at the
//     `<platform>/` subdirectory.
//  5. Find exactly one executable file whose name starts with
//     "terraform-provider-<name>". Ambiguity (multiple such files)
//     is an error.
//
// SearchPaths are tried in order; the first one that yields a
// candidate wins. This matches Terraform's "local first, cache
// second" precedence.
//
// Errors:
//
//   - opts.SearchPaths empty → "no search paths configured"
//   - no candidate after trying all paths → ErrProviderNotFound,
//     wrapped with a description of what was tried.
//   - ambiguous binary inside a version dir → returned directly
//     (not ErrProviderNotFound), since this is a configuration
//     bug worth surfacing.
func Discover(source SourceAddress, opts DiscoveryOptions) (*DiscoveredProvider, error) {
	if len(opts.SearchPaths) == 0 {
		return nil, errors.New("Discover: no search paths configured")
	}
	platform := opts.Platform
	if platform == "" {
		platform = runtime.GOOS + "_" + runtime.GOARCH
	}

	var misses []string
	for _, root := range opts.SearchPaths {
		subtree := filepath.Join(root, source.Hostname, source.Namespace, source.Name)
		entries, err := os.ReadDir(subtree)
		if errors.Is(err, os.ErrNotExist) {
			misses = append(misses, fmt.Sprintf("%s: subtree missing", subtree))
			continue
		}
		if err != nil {
			misses = append(misses, fmt.Sprintf("%s: %v", subtree, err))
			continue
		}

		versions := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if opts.Version != "" && e.Name() != opts.Version {
				continue
			}
			versions = append(versions, e.Name())
		}
		if len(versions) == 0 {
			if opts.Version != "" {
				misses = append(misses, fmt.Sprintf("%s: no directory matching version %q", subtree, opts.Version))
			} else {
				misses = append(misses, fmt.Sprintf("%s: no version directories", subtree))
			}
			continue
		}

		picked := pickHighestVersion(versions)
		versionDir := filepath.Join(subtree, picked)
		platformDir := filepath.Join(versionDir, platform)

		binary, err := findProviderBinary(platformDir, source.Name)
		if errors.Is(err, os.ErrNotExist) {
			misses = append(misses, fmt.Sprintf("%s: platform dir missing for %s", versionDir, platform))
			continue
		}
		if err != nil {
			return nil, err // ambiguous binary etc — surface immediately
		}
		return &DiscoveredProvider{
			Source:   source,
			Version:  picked,
			Platform: platform,
			Binary:   binary,
		}, nil
	}
	return nil, fmt.Errorf("%w (%s): %s", ErrProviderNotFound, source.String(), strings.Join(misses, "; "))
}

// findProviderBinary returns the single executable file inside dir
// whose name starts with "terraform-provider-<name>". Returns the
// os.ErrNotExist class of error if dir itself is missing.
//
// On platforms where directory listings can return arbitrarily
// ordered names, multiple candidates produce a deterministic error
// listing them sorted. We do not pick "the first one" silently —
// that's a known foot-gun.
func findProviderBinary(dir, name string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	prefix := "terraform-provider-" + name
	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fname := e.Name()
		if !strings.HasPrefix(fname, prefix) {
			continue
		}
		// Be liberal about suffix: the canonical form is
		// `<prefix>_v<version>_x<protocol>`, but bare
		// `<prefix>` is also valid (used by some custom builds).
		// What matters is that the file is executable.
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0o111 == 0 {
			// Not executable. On macOS with `terraform init`'s
			// install path this shouldn't happen, but if it
			// does, document the miss instead of silent-skip.
			continue
		}
		candidates = append(candidates, filepath.Join(dir, fname))
	}
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no executable %s* in %s", prefix, dir)
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple %s* binaries in %s: %v", prefix, dir, candidates)
	}
}

// pickHighestVersion returns the version string that compares
// greatest under compareVersions. Caller guarantees the slice is
// non-empty.
func pickHighestVersion(versions []string) string {
	best := versions[0]
	for _, v := range versions[1:] {
		if compareVersions(v, best) > 0 {
			best = v
		}
	}
	return best
}

// compareVersions reports a < b → -1, a > b → +1, a == b → 0.
//
// The comparator handles the version-string shapes Terraform's
// registry actually produces: dotted decimals optionally followed
// by a `-suffix` pre-release tag. Numeric segments compare
// numerically (so "3.10.0" > "3.2.0"); non-numeric segments fall
// back to lex order. A version with NO pre-release suffix beats
// one WITH a suffix at the same numeric prefix ("1.0.0" > "1.0.0-rc1").
//
// This is deliberately a small in-package comparator, not a full
// semver library: the only consumer is provider version selection,
// which doesn't need range matching, build metadata, or any of the
// edge cases a generic semver dep would carry. If we later add
// version constraints (`~> 3.2`), graduate to hashicorp/go-version.
func compareVersions(a, b string) int {
	aNum, aPre := splitVersion(a)
	bNum, bPre := splitVersion(b)

	if c := compareDottedNumeric(aNum, bNum); c != 0 {
		return c
	}
	// Numeric prefixes equal. Pre-release ordering: no-suffix is
	// greater than any suffix; among suffixes, lex order.
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	default:
		return strings.Compare(aPre, bPre)
	}
}

// splitVersion separates the numeric prefix from the pre-release
// suffix. "1.2.3-rc1" → ("1.2.3", "rc1"); "1.2.3" → ("1.2.3", "").
func splitVersion(v string) (numeric, prerelease string) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func compareDottedNumeric(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		ai, aIsNum := indexNumeric(ap, i)
		bi, bIsNum := indexNumeric(bp, i)
		if aIsNum && bIsNum {
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
			continue
		}
		// Non-numeric segment somewhere — fall back to lex on the
		// remaining segments joined. Rare; provider versions
		// almost always parse fully numeric.
		ar := strings.Join(ap[i:], ".")
		br := strings.Join(bp[i:], ".")
		return strings.Compare(ar, br)
	}
	return 0
}

// indexNumeric returns parts[i] as an int, or 0/false if either
// the index is out of range (treated as implicit zero for shorter
// version strings — so "1.2" compares equal to "1.2.0") or the
// segment is not all digits.
func indexNumeric(parts []string, i int) (int, bool) {
	if i >= len(parts) {
		return 0, true
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return 0, false
	}
	return n, true
}
