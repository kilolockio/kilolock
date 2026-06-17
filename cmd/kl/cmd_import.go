package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	name := fs.String("name", "", "state name to import into (default: derived from filename)")
	// --source tags the resulting state_version with a non-default
	// provenance string. Useful when reconstructing history (an
	// archived state belongs in source='apply' even though it
	// arrives via import) and required by the drift demo, which
	// uses --source=refresh to simulate a refresh-discovered state
	// without needing a real provider conversation.
	src := fs.String("source", "import", "value to record in state_versions.source (one of: import, apply, refresh, unknown)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: kl import [flags] <file>

Loads a Terraform v4 .tfstate file into the configured Postgres database.
Reads from stdin when the file argument is "-".

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	source := fs.Arg(0)

	if !validImportSource(*src) {
		fmt.Fprintf(os.Stderr, "kl import: invalid --source %q (allowed: import, apply, refresh, unknown)\n", *src)
		return 2
	}

	raw, stateName, err := readImportSource(source, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl import:", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl import:", err)
		return 1
	}
	if err := client.postJSON(ctx, "/admin/state/import", stateName, map[string]any{
		"name":      stateName,
		"raw_state": string(raw),
		"source":    *src,
		"actor":     cliActor(),
	}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "kl import:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "import succeeded: state=%s source=%s\n", stateName, source)
	return 0
}

// validImportSource gates the --source flag against the small,
// closed set of provenance strings the rest of the system already
// understands. The state_versions.source column is free-form text,
// but downstream views (notably current_resource_drift) filter on
// exact values; accepting arbitrary input here would silently
// disable those views for the resulting rows.
func validImportSource(s string) bool {
	switch s {
	case "import", "apply", "refresh", "unknown":
		return true
	}
	return false
}

// readImportSource reads bytes from `source` ("-" means stdin) and
// derives a state name from the filename when no explicit name is given.
func readImportSource(source, explicitName string) (raw []byte, name string, err error) {
	if source == "-" {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", fmt.Errorf("read stdin: %w", err)
		}
		if explicitName == "" {
			return nil, "", errors.New("--name is required when reading from stdin")
		}
		return raw, explicitName, nil
	}

	raw, err = os.ReadFile(source)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", source, err)
	}
	if explicitName != "" {
		return raw, explicitName, nil
	}
	return raw, deriveStateName(source), nil
}

// deriveStateName produces a state name from a filename:
//
//	./terraform.tfstate            -> terraform
//	./environments/prod.tfstate    -> prod
//	./prod.tfstate.backup          -> prod.tfstate
//
// Anything left after stripping the .tfstate suffix is kept as-is.
func deriveStateName(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".tfstate")
	if base == "" {
		base = "default"
	}
	return base
}
