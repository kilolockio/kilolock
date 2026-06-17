package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFileName is the per-project config file name. Lookup walks up
// from the starting directory and stops at the first match, the same
// pattern git uses for .git/ and cargo uses for Cargo.toml. The file
// is dot-prefixed so operators don't see it in casual `ls`.
const ConfigFileName = ".kl.toml"

// FileSettings is the subset of Config that can be supplied via the
// project-local config file. Fields are deliberately a strict subset:
// secrets sit naturally in env vars and the file should not be the
// only place ones lives — but a development DB URL or backend address
// is fine.
//
// Extending FileSettings is forward-compatible because the parser
// ignores unknown keys; adding fields later won't break existing
// files. Removing or renaming a field IS a breaking change for any
// committed file in the wild, so think before doing so.
type FileSettings struct {
	DatabaseURL    string
	BackendAddress string
}

// FindConfigFile walks up the directory tree from startDir looking
// for .kl.toml, stopping at the first match. Returns
// (path, nil) on hit; (path="", os.ErrNotExist) when no file is
// found before reaching the filesystem root; any other I/O error is
// surfaced as-is.
//
// Symbolic links in the parent chain are followed by virtue of
// filepath.Dir+Stat going through the OS layer; no extra logic
// needed. Hitting the filesystem root is detected by
// filepath.Dir(p)==p (POSIX) or matching "X:\\" on Windows; the
// stdlib's behavior is consistent across platforms.
func FindConfigFile(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("resolve config search root: %w", err)
	}
	for {
		candidate := filepath.Join(dir, ConfigFileName)
		info, err := os.Stat(candidate)
		switch {
		case err == nil && !info.IsDir():
			return candidate, nil
		case err == nil && info.IsDir():
			// .kl.toml exists as a directory — refuse rather
			// than silently skipping. Operators see a clear error
			// instead of "why is my config not loading?".
			return "", fmt.Errorf("%s is a directory, expected a file", candidate)
		case errors.Is(err, os.ErrNotExist):
			// keep walking
		default:
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// LoadFile reads and parses a .kl.toml file at the given
// path. The parser handles the strict subset of TOML we use:
//
//   - blank lines
//   - line comments starting with '#'
//   - section headers in the form '[name]'
//   - key/value lines in the form 'key = "value"'
//
// Anything beyond that (inline tables, arrays, numbers, multi-line
// strings, etc.) is rejected with a position-tagged error so
// operators don't end up with values silently dropping. We can grow
// the grammar later when there's a concrete need.
//
// Recognized keys (case-sensitive):
//
//	[database]
//	url = "postgres://..."   // → FileSettings.DatabaseURL
//
//	[backend]
//	address = "http://..."   // → FileSettings.BackendAddress
//	                          //   (advisory; not yet consumed by
//	                          //    any subcommand, reserved for the
//	                          //    apply CLI's eventual HTTP-target
//	                          //    discovery)
//
// For symmetry with the env-var path, top-level (section-less)
// 'database_url' and 'backend_address' are also accepted; this lets
// trivial single-line configs skip the section nesting entirely.
func LoadFile(path string) (FileSettings, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileSettings{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return parseFileSettings(f, path)
}

// parseFileSettings is the parser entrypoint exposed for tests. The
// public LoadFile wraps it with file I/O so this stays a pure
// function on io.Reader.
func parseFileSettings(r io.Reader, srcName string) (FileSettings, error) {
	out := FileSettings{}
	scanner := bufio.NewScanner(r)
	lineNum := 0
	currentSection := ""
	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if !strings.HasSuffix(trimmed, "]") {
				return FileSettings{}, fmt.Errorf("%s:%d: section header missing closing ']'", srcName, lineNum)
			}
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if name == "" {
				return FileSettings{}, fmt.Errorf("%s:%d: empty section header", srcName, lineNum)
			}
			currentSection = name
			continue
		}
		// Strip trailing inline comments (everything after '#' that
		// isn't inside the quoted value). Cheap because we only
		// accept quoted string values: the value substring ends at
		// the closing '"'.
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			return FileSettings{}, fmt.Errorf("%s:%d: expected KEY = VALUE, got %q", srcName, lineNum, raw)
		}
		key := strings.TrimSpace(trimmed[:eq])
		valuePart := strings.TrimSpace(trimmed[eq+1:])
		val, err := parseQuotedString(valuePart)
		if err != nil {
			return FileSettings{}, fmt.Errorf("%s:%d: %s = %s: %w", srcName, lineNum, key, valuePart, err)
		}
		if err := applyKey(&out, currentSection, key, val); err != nil {
			return FileSettings{}, fmt.Errorf("%s:%d: %w", srcName, lineNum, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return FileSettings{}, fmt.Errorf("scan %s: %w", srcName, err)
	}
	return out, nil
}

// parseQuotedString accepts a "double-quoted" literal possibly
// followed by an inline '# comment'. Returns the literal's contents
// verbatim (no escape processing — keep it simple; if someone needs
// embedded quotes we can add escapes later). Errors on missing
// quotes or trailing junk that isn't whitespace or a '#' comment.
func parseQuotedString(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		return "", fmt.Errorf("value must be a \"quoted string\"")
	}
	closing := strings.IndexByte(s[1:], '"')
	if closing < 0 {
		return "", fmt.Errorf("unterminated quoted string")
	}
	val := s[1 : 1+closing]
	rest := strings.TrimSpace(s[1+closing+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("trailing junk after value: %q", rest)
	}
	return val, nil
}

// applyKey routes a parsed (section, key, value) triple to the
// corresponding FileSettings field. Unknown keys are silently
// ignored so the file format can grow without breaking older
// binaries — operators get warnings only on truly malformed
// syntax, not on unrecognized fields.
func applyKey(out *FileSettings, section, key, val string) error {
	full := key
	if section != "" {
		full = section + "." + key
	}
	switch full {
	case "database_url", "database.url":
		out.DatabaseURL = val
	case "backend_address", "backend.address":
		out.BackendAddress = val
	default:
		// Unknown key: tolerate so a newer file (with future fields)
		// doesn't break an older binary. The Load path could log a
		// debug-level message about this if it ever becomes
		// noticeable.
	}
	return nil
}
