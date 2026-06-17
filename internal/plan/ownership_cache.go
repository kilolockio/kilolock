package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const ownershipCacheFormatVersion = "1"

type ownershipCache struct {
	FormatVersion string                    `json:"format_version"`
	Addresses     map[string]ownershipEntry `json:"addresses"`
	UpdatedAt     time.Time                 `json:"updated_at"`
}

type ownershipEntry struct {
	OwnerRel string    `json:"owner_rel"`
	LastSeen time.Time `json:"last_seen"`
}

func ownershipCachePath(configDir string) string {
	return filepath.Join(configDir, ".kl", "ownership.json")
}

func loadOwnershipCache(configDir string) (*ownershipCache, error) {
	path := ownershipCachePath(configDir)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read ownership cache %s: %w", path, err)
	}
	var c ownershipCache
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode ownership cache %s: %w", path, err)
	}
	if c.FormatVersion != ownershipCacheFormatVersion {
		return nil, fmt.Errorf("ownership cache %s has unsupported format_version %q", path, c.FormatVersion)
	}
	if c.Addresses == nil {
		c.Addresses = map[string]ownershipEntry{}
	}
	return &c, nil
}

// UpdateOwnershipCache merges the current plan configuration's file ownership
// metadata into a stable cache under configDir.
//
// The cache is intentionally "sticky": addresses are not removed when they
// disappear from the current configuration, so a subsequent scoped plan can
// still attribute delete actions to the file that previously owned the address.
func UpdateOwnershipCache(configDir string, f *File) error {
	if configDir == "" || f == nil {
		return nil
	}

	now := time.Now().UTC()

	c, err := loadOwnershipCache(configDir)
	if err != nil {
		return err
	}
	if c == nil {
		c = &ownershipCache{
			FormatVersion: ownershipCacheFormatVersion,
			Addresses:     map[string]ownershipEntry{},
		}
	}

	owners := map[string]string{}
	walkConfigForFileOwnership(f.Configuration.RootModule, "", owners)
	for addr, filename := range owners {
		rel := normalizePlanFilename(configDir, filename)
		if rel == "" {
			continue
		}
		c.Addresses[addr] = ownershipEntry{
			OwnerRel: rel,
			LastSeen: now,
		}
	}
	c.UpdatedAt = now

	path := ownershipCachePath(configDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ownership cache: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write ownership cache %s: %w", path, err)
	}
	return nil
}

func ownershipAddressesForFiles(configDir string, relFiles []string) (map[string]struct{}, error) {
	c, err := loadOwnershipCache(configDir)
	if err != nil {
		return nil, err
	}
	if c == nil || len(c.Addresses) == 0 || len(relFiles) == 0 {
		return nil, nil
	}
	allowed := map[string]struct{}{}
	for _, f := range relFiles {
		if f == "" {
			continue
		}
		allowed[f] = struct{}{}
	}
	out := map[string]struct{}{}
	for addr, e := range c.Addresses {
		if _, ok := allowed[e.OwnerRel]; !ok {
			continue
		}
		out[addr] = struct{}{}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
