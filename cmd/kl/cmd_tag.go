package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/kilolockio/kilolock/pkg/store"
)

// runTag dispatches `kl tag <subcommand>`. Subcommands:
//
//	set      attach a tag to a version (creates or moves)
//	unset    detach an existing tag
//	list     list tags on a state (or "ls" alias)
//
// Modeled as a sub-subcommand rather than mode flags because the
// argument shapes diverge enough that a single flag-set would either
// require optional positionals (Go's flag package handles those
// awkwardly) or repeat the state-name parsing three times. With
// subcommands the per-mode usage strings stay focused.
//
// Bonus: future `kl tag move` (rename), `kl tag show
// <name>` (one-tag detail) etc. extend naturally without changing
// the existing surface.
func runTag(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "kl tag: missing subcommand")
		fmt.Fprint(os.Stderr, tagUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return runTagSet(rest)
	case "unset", "rm", "remove", "delete":
		return runTagUnset(rest)
	case "list", "ls":
		return runTagList(rest)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, tagUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "kl tag: unknown subcommand %q\n", sub)
		fmt.Fprint(os.Stderr, tagUsage)
		return 2
	}
}

// runTagSet implements `kl tag set <state> <tag-name> <version-ref> [--description=...]`.
//
// The positional order is (state, tag-name, version-ref) rather
// than (state, version-ref, tag-name) because operators write
// "tag this version as X" mentally before they write the ref —
// putting the tag name next to the verb reads more naturally as
// `tag set prod prod-deploy current`.
func runTagSet(args []string) int {
	fs := flag.NewFlagSet("tag set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		desc  = fs.String("description", "", "Optional free-form note for the tag (max 1000 chars).")
		actor = fs.String("actor", "", "Override the actor recorded on the tag (default: $USER@cli).")
	)
	adminFlags := registerAdminClientFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl tag set:", err)
		fmt.Fprint(os.Stderr, tagSetUsage)
		return 2
	}
	if fs.NArg() != 3 {
		fmt.Fprintln(os.Stderr, "kl tag set: expected <state> <tag-name> <version-ref>")
		fmt.Fprint(os.Stderr, tagSetUsage)
		return 2
	}
	stateArg, tagName, versionRef := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	target, _, err := adminFlags.resolveStateTarget(stateArg, ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag set:", err)
		return 2
	}
	stateName := target.StateName
	if err := store.ValidateTagName(tagName); err != nil {
		fmt.Fprintln(os.Stderr, "kl tag set:", err)
		return 2
	}
	if len(*desc) > 1000 {
		fmt.Fprintln(os.Stderr, "kl tag set: --description too long (max 1000 chars)")
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag set:", err)
		return 1
	}

	resolvedActor := *actor
	if resolvedActor == "" {
		resolvedActor = cliActor()
	}

	var row store.VersionTag
	err = client.postJSON(ctx, "/admin/state/tags/set?name="+queryEscape(stateName), stateName, map[string]any{
		"tag":         tagName,
		"version_ref": versionRef,
		"description": *desc,
		"actor":       resolvedActor,
	}, &row)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag set:", err)
		return 1
	}

	fmt.Printf("Tagged state %q version serial=%d as %q\n",
		row.StateName, row.Serial, row.Tag)
	if row.Description != "" {
		fmt.Printf("  description: %s\n", row.Description)
	}
	fmt.Printf("  actor:       %s\n", row.Actor)
	fmt.Printf("  version id:  %s\n", row.VersionID)
	return 0
}

func runTagUnset(args []string) int {
	fs := flag.NewFlagSet("tag unset", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	actor := fs.String("actor", "", "Override the actor recorded on the unset event (default: $USER@cli).")
	adminFlags := registerAdminClientFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl tag unset:", err)
		fmt.Fprint(os.Stderr, tagUnsetUsage)
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "kl tag unset: expected <state> <tag-name>")
		fmt.Fprint(os.Stderr, tagUnsetUsage)
		return 2
	}
	stateArg, tagName := fs.Arg(0), fs.Arg(1)

	target, _, err := adminFlags.resolveStateTarget(stateArg, ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag unset:", err)
		return 2
	}
	stateName := target.StateName

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag unset:", err)
		return 1
	}

	resolvedActor := *actor
	if resolvedActor == "" {
		resolvedActor = cliActor()
	}

	if err := client.postJSON(ctx, "/admin/state/tags/unset?name="+queryEscape(stateName), stateName, map[string]any{
		"tag":   tagName,
		"actor": resolvedActor,
	}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "kl tag unset:", err)
		return 1
	}
	fmt.Printf("Removed tag %q from state %q\n", tagName, stateName)
	return 0
}

func runTagList(args []string) int {
	fs := flag.NewFlagSet("tag list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "table", "Output format: table|json.")
	adminFlags := registerAdminClientFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl tag list:", err)
		fmt.Fprint(os.Stderr, tagListUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "kl tag list: too many positional arguments")
		fmt.Fprint(os.Stderr, tagListUsage)
		return 2
	}

	target, _, err := adminFlags.resolveStateTarget(fs.Arg(0), ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag list:", err)
		return 2
	}
	stateName := target.StateName

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag list:", err)
		return 1
	}
	var resp struct {
		State string             `json:"state"`
		Tags  []store.VersionTag `json:"tags"`
	}
	err = client.getJSON(ctx, "/admin/state/tags?name="+queryEscape(stateName), &resp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl tag list:", err)
		return 1
	}
	tags := resp.Tags

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(tags); err != nil {
			fmt.Fprintln(os.Stderr, "kl tag list: encode json:", err)
			return 1
		}
		return 0
	case "table", "":
		if len(tags) == 0 {
			fmt.Printf("state %q has no tags.\n", stateName)
			return 0
		}
		fmt.Printf("state: %s\n\n", stateName)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "TAG\tSERIAL\tACTOR\tUPDATED\tDESCRIPTION")
		for _, t := range tags {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
				t.Tag, t.Serial, emptyDash(t.Actor),
				t.UpdatedAt.UTC().Format("2006-01-02 15:04:05"),
				emptyDash(t.Description))
		}
		_ = tw.Flush()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "kl tag list: unknown --format %q\n", *format)
		return 2
	}
}

const tagUsage = `Usage:
  kl tag <subcommand> [args...]

Subcommands:
  set      Attach a tag to a state_version (creates or moves).
  unset    Remove a tag (also: rm / remove / delete).
  list     List all tags on a state (also: ls).

A tag is a per-state named pointer to a state_version. The same tag
on two different states is independent. Tags accept any
version-reference shape that 'rollback --to=' accepts: serial, @N,
UUID, or 'current'. Reserved names: pure numbers, '@'-prefixed
strings, and the literal 'current' collide with the version-ref
resolver and are rejected.

Once set, the tag name is usable as a version reference anywhere
that takes one — e.g.:

  kl rollback prod --to=pre-mig
  kl diff prod --from=pre-mig --to=current
  kl export prod --version=pre-mig
`

const tagSetUsage = `Usage:
  kl tag set <state> <tag-name> <version-ref> [flags]

Creates a tag or moves an existing tag to a new version.

Positional:
  state                 State name.
  tag-name              Tag identifier. Allowed: any non-empty string
                        up to 64 chars that ISN'T a serial, an @N
                        ref, or the literal 'current'.
  version-ref           Target version. Accepts <serial>, @<N>,
                        <uuid>, or 'current'.

Flags:
  --description=NOTE    Optional free-form note (max 1000 chars).
  --actor=NAME          Override the actor recorded on the tag.
  --state-url=URL       Full state URL. Overrides KL_STATE_URL and
                        backend auto-discovery.
  --token=TOKEN         Bearer token for cloud/admin API auth.
                        Overrides KL_TOKEN.

Examples:
  kl tag set prod prod-2026-05 current
  kl tag set prod pre-mig @1 --description="before migration"
`

const tagUnsetUsage = `Usage:
  kl tag unset <state> <tag-name> [flags]

Removes a tag. The pointed-to state_version is NOT deleted.

Flags:
  --actor=NAME          Override the actor recorded on the unset
                        event (default $USER@cli).
  --state-url=URL       Full state URL. Overrides KL_STATE_URL and
                        backend auto-discovery.
  --token=TOKEN         Bearer token for cloud/admin API auth.
                        Overrides KL_TOKEN.
`

const tagListUsage = `Usage:
  kl tag list [state] [flags]

Lists tags on a state, newest first by last-modified.

Flags:
  --format=FMT          table (default) or json.
  --state-url=URL       Full state URL. Overrides KL_STATE_URL and
                        backend auto-discovery.
  --token=TOKEN         Bearer token for cloud/admin API auth.
                        Overrides KL_TOKEN.
`
