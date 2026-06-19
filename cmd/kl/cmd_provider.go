package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/kilolockio/kilolock/internal/provider"
	"github.com/kilolockio/kilolock/pkg/store"
)

// runProvider dispatches `kl provider <action> ...`.
//
// Actions are kept as separate functions per command so each one
// can own its own flag.FlagSet — keeps the help output focused on
// the action the user actually invoked.
func runProvider(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, providerUsage)
		return 2
	}
	action := args[0]
	rest := args[1:]

	switch action {
	case "configure", "set":
		return runProviderConfigure(rest)
	case "get", "show":
		return runProviderGet(rest)
	case "list", "ls":
		return runProviderList(rest)
	case "remove", "rm", "delete":
		return runProviderRemove(rest)
	case "help", "--help", "-h":
		fmt.Fprint(os.Stdout, providerUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "kl provider: unknown action %q\n\n%s", action, providerUsage)
		return 2
	}
}

const providerUsage = `Usage:
  kl provider <action> [flags]

Actions:
  configure <source>   Store a configuration block for a provider.
                       Reads JSON from --from-json or stdin.
  get <source>         Print the stored configuration for a provider.
  list                 Show every stored provider configuration.
  remove <source>      Delete a stored configuration.

Source addresses follow Terraform's required_providers format:
  "null"                                 → hashicorp/null
  "hashicorp/aws"                        → registry.terraform.io/hashicorp/aws
  "registry.terraform.io/hashicorp/aws"  → as written

Flags common to most actions:
  --alias=NAME      Provider alias (empty = default unaliased config).
  --token=TOKEN     Bearer token for cloud/admin API auth. Overrides KL_TOKEN.
`

// --- configure -------------------------------------------------------------

func runProviderConfigure(args []string) int {
	fs := flag.NewFlagSet("provider configure", flag.ContinueOnError)
	var (
		alias    = fs.String("alias", "", "Provider alias (empty = default).")
		fromJSON = fs.String("from-json", "", "Path to a JSON file containing the config. Use '-' or omit for stdin.")
	)
	adminFlags := registerAdminClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "kl provider configure: missing required <source> argument")
		return 2
	}
	addr, err := provider.ParseSourceAddress(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider configure: %v\n", err)
		return 2
	}

	cfg, err := readConfigJSON(*fromJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider configure: %v\n", err)
		return 2
	}

	icfg := loadConfigOrExit("provider configure")
	logger := newLogger(icfg.LogFormat, icfg.LogLevel)
	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider configure: %v\n", err)
		return 1
	}
	var entry store.ProviderConfigEntry
	if err := client.postJSON(ctx, "/admin/provider-config/set", "", map[string]any{
		"source": addr.String(),
		"alias":  *alias,
		"config": cfg,
	}, &entry); err != nil {
		logger.Error("store provider config", "err", err, "source", addr.String(), "alias", *alias)
		return 1
	}
	fmt.Fprintf(os.Stdout, "stored config for %s (alias=%q)\n", entry.Source, entry.Alias)
	return 0
}

// readConfigJSON reads the config payload from a file path, or
// stdin when path is empty or "-", then dispatches to
// decodeConfigJSON for shape validation.
func readConfigJSON(path string) (map[string]any, error) {
	var data []byte
	var err error
	switch path {
	case "", "-":
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	default:
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	return decodeConfigJSON(data)
}

// decodeConfigJSON parses raw JSON bytes into a provider config
// attribute map, rejecting payloads that aren't a top-level object.
// Kept pure (no IO) so the validation rules have direct unit-test
// coverage.
//
// What's validated:
//
//   - non-empty input
//   - syntactically valid JSON
//   - top-level shape is an object (arrays, scalars, and JSON null
//     would silently break encoding at refresh time)
//
// What's NOT validated: the attribute keys against any provider
// schema. That requires a live Launch and is the refresh path's
// concern, not the configure command's. The (intentional)
// consequence is that you can store a config with typos; refresh
// will report them via provider diagnostics.
func decodeConfigJSON(data []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("empty config payload (expected a JSON object)")
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config payload must be a JSON object, got %T", raw)
	}
	return m, nil
}

// --- get -------------------------------------------------------------------

func runProviderGet(args []string) int {
	fs := flag.NewFlagSet("provider get", flag.ContinueOnError)
	alias := fs.String("alias", "", "Provider alias (empty = default).")
	adminFlags := registerAdminClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "kl provider get: missing required <source> argument")
		return 2
	}
	addr, err := provider.ParseSourceAddress(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider get: %v\n", err)
		return 2
	}

	icfg := loadConfigOrExit("provider get")
	logger := newLogger(icfg.LogFormat, icfg.LogLevel)
	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider get: %v\n", err)
		return 1
	}
	var entry store.ProviderConfigEntry
	err = client.getJSON(ctx, "/admin/provider-config?source="+queryEscape(addr.String())+"&alias="+queryEscape(*alias), &entry)
	if err != nil && strings.Contains(err.Error(), "404 Not Found") {
		fmt.Fprintf(os.Stderr, "no config for %s (alias=%q)\n", addr, *alias)
		return 1
	}
	if err != nil {
		logger.Error("load provider config", "err", err)
		return 1
	}

	// Pretty-print so the output is itself a valid input to
	// `kl provider configure --from-json`.
	pretty, err := json.MarshalIndent(entry.Config, "", "  ")
	if err != nil {
		logger.Error("encode config", "err", err)
		return 1
	}
	fmt.Println(string(pretty))
	return 0
}

// --- list ------------------------------------------------------------------

func runProviderList(args []string) int {
	fs := flag.NewFlagSet("provider list", flag.ContinueOnError)
	adminFlags := registerAdminClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	icfg := loadConfigOrExit("provider list")
	logger := newLogger(icfg.LogFormat, icfg.LogLevel)
	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider list: %v\n", err)
		return 1
	}
	var resp struct {
		Entries []store.ProviderConfigEntry `json:"entries"`
	}
	err = client.getJSON(ctx, "/admin/provider-configs", &resp)
	if err != nil {
		logger.Error("list provider configs", "err", err)
		return 1
	}
	entries := resp.Entries
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no provider configs stored")
		return 0
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tALIAS\tATTRIBUTES\tUPDATED")
	for _, e := range entries {
		alias := e.Alias
		if alias == "" {
			alias = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			e.Source, alias, summarizeAttrs(e.Config), e.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
	if err := tw.Flush(); err != nil {
		logger.Error("flush output", "err", err)
		return 1
	}
	return 0
}

// summarizeAttrs renders a short, deterministic preview of the
// config attribute names (not values — values can be secrets).
// The list view should never leak credentials to a shoulder-surfer.
// `kl provider get` (returning JSON) is the deliberate way
// to see actual values.
func summarizeAttrs(cfg map[string]any) string {
	if len(cfg) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// --- remove ----------------------------------------------------------------

func runProviderRemove(args []string) int {
	fs := flag.NewFlagSet("provider remove", flag.ContinueOnError)
	alias := fs.String("alias", "", "Provider alias (empty = default).")
	adminFlags := registerAdminClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "kl provider remove: missing required <source> argument")
		return 2
	}
	addr, err := provider.ParseSourceAddress(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider remove: %v\n", err)
		return 2
	}

	icfg := loadConfigOrExit("provider remove")
	logger := newLogger(icfg.LogFormat, icfg.LogLevel)
	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl provider remove: %v\n", err)
		return 1
	}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	err = client.postJSON(ctx, "/admin/provider-config/delete", "", map[string]any{
		"source": addr.String(),
		"alias":  *alias,
	}, &resp)
	if err != nil {
		logger.Error("remove provider config", "err", err)
		return 1
	}
	n := resp.Deleted
	if n == 0 {
		fmt.Fprintf(os.Stderr, "no config for %s (alias=%q)\n", addr, *alias)
		// Exit 0 anyway: Delete is idempotent, callers piping
		// `kl provider remove` in a cleanup script
		// shouldn't have to special-case the "already gone" case.
		return 0
	}
	fmt.Fprintf(os.Stdout, "removed config for %s (alias=%q)\n", addr, *alias)
	return 0
}
