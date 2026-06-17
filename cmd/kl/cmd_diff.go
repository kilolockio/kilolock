package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

// runDiff implements `kl diff [state] --from=<ref> --to=<ref>`.
//
// The address-level diff already exists in `kl rollback`'s
// preview, but operators have repeatedly asked "WHICH attribute
// changed on this resource?" — usually right before a rollback, to
// decide whether the rollback would touch a field they care about
// (an ASG min_size, an IAM policy, a tag).
//
// Design:
//
//   - Defaults to --from=@1 --to=current so `kl diff` with no
//     flags answers "what's new in this version vs the previous one".
//   - Three formats: table (per-address with sample leaves; the
//     default), unified (git-style full leaf list per address), and
//     json (machine-readable, sensitive paths redacted).
//   - --address=GLOB filters to a slice of the change-set. Glob, not
//     regex, because operators reading terraform state are used to
//     terraform address-spec wildcards.
//   - Sensitive paths from EITHER side are masked (a sensitive→non-
//     sensitive transition is itself a policy event and must not leak
//     the value).
//
// Exit codes:
//
//	0  diff rendered (possibly empty); no errors
//	1  database error
//	2  argv error / state-not-found / version-not-found
func runDiff(args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		from    = fs.String("from", "@1", "Source version reference (default @1 = previous). Accepts <serial>, @<N>, <uuid>, or 'current'.")
		to      = fs.String("to", "current", "Target version reference (default current).")
		format  = fs.String("format", "table", "Output format: table|unified|json.")
		glob    = fs.String("address", "", "Optional glob to filter resource addresses (e.g. 'aws_instance.*' or '*.module.foo.*').")
		limit   = fs.Int("limit", 0, "Cap the number of resources rendered (0 = no cap). Applied AFTER --address filtering.")
		summary = fs.Bool("summary", false, "Print only the address-level summary (added/removed/changed counts and names) without attribute leaves.")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl diff:", err)
		fmt.Fprint(os.Stderr, diffUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl diff: too many positional arguments: %v\n", fs.Args())
		fmt.Fprint(os.Stderr, diffUsage)
		return 2
	}
	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl diff:", err)
		fmt.Fprint(os.Stderr, diffUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl diff:", err)
		return 1
	}
	var resp struct {
		State string                    `json:"state"`
		From  store.StateVersionInfo    `json:"from"`
		To    store.StateVersionInfo    `json:"to"`
		Rows  []store.ResourceAttrDelta `json:"rows"`
	}
	path := fmt.Sprintf("/admin/states/%s/diff?from=%s&to=%s",
		stateName, url.QueryEscape(*from), url.QueryEscape(*to))
	if err := client.getJSON(ctx, path, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "kl diff:", err)
		return 1
	}
	fromInfo := &resp.From
	toInfo := &resp.To
	if fromInfo.ID == toInfo.ID {
		fmt.Fprintf(os.Stderr, "kl diff: --from and --to resolve to the same version (serial %d); nothing to diff\n", fromInfo.Serial)
		return 0
	}
	rows := resp.Rows
	rows = applyAddressFilter(rows, *glob)
	if *limit > 0 && len(rows) > *limit {
		rows = rows[:*limit]
	}

	header := diffHeader{
		StateName:     stateName,
		FromSerial:    fromInfo.Serial,
		FromID:        fromInfo.ID,
		FromActor:     fromInfo.CreatedBy,
		FromWrittenAt: fromInfo.CreatedAt,
		ToSerial:      toInfo.Serial,
		ToID:          toInfo.ID,
		ToActor:       toInfo.CreatedBy,
		ToWrittenAt:   toInfo.CreatedAt,
	}

	switch *format {
	case "json":
		return renderDiffJSON(os.Stdout, header, rows, *summary)
	case "unified":
		return renderDiffUnified(os.Stdout, header, rows, *summary)
	case "table", "":
		return renderDiffTable(os.Stdout, header, rows, *summary)
	default:
		fmt.Fprintf(os.Stderr, "kl diff: unknown --format %q\n", *format)
		return 2
	}
}

// diffHeader is the metadata block printed at the top of every
// diff render. Carried as a struct because all three renderers
// (table/unified/json) print it, just in different shapes.
type diffHeader struct {
	StateName     string
	FromSerial    int64
	FromID        string
	FromActor     string
	FromWrittenAt time.Time
	ToSerial      int64
	ToID          string
	ToActor       string
	ToWrittenAt   time.Time
}

// applyAddressFilter narrows the result set to addresses matching
// the supplied glob. Empty glob = no filter. Standard `path.Match`
// glob semantics: `*` matches any non-`/` segment; we treat `.` as a
// non-separator (Terraform addresses are not paths) so a glob like
// `aws_instance.*` does what an operator expects.
//
// We pre-translate the glob to a path-style pattern by replacing `.`
// with `/` only when it appears outside brackets. This is the
// simplest approach that handles the common cases without bringing
// in a real glob library.
func applyAddressFilter(rows []store.ResourceAttrDelta, glob string) []store.ResourceAttrDelta {
	if glob == "" {
		return rows
	}
	out := rows[:0]
	for _, r := range rows {
		if matchAddressGlob(glob, r.Address) {
			out = append(out, r)
		}
	}
	return out
}

// matchAddressGlob implements the lightweight glob used by --address.
// Rules:
//
//   - matches any run of characters except '.'
//     **  matches any run of characters including '.'
//     ?   matches exactly one non-'.' character
//     [   ]   ::  treated as literal (Terraform indices)
//
// We do not use filepath.Match: it treats '.' as part of a path
// component, but its rules around path separators and bracket
// character classes don't translate cleanly to Terraform-style
// addresses (which are NOT paths, even though they use '.' the way
// paths use '/'). A custom matcher is small enough that the
// extra code beats the cognitive cost of remembering filepath.Match's
// edge cases.
func matchAddressGlob(pattern, addr string) bool {
	return globMatch(pattern, addr)
}

// globMatch is the address-glob matcher. Classic two-cursor algorithm
// with a star-backtrack stack:
//
//   - Walk pattern and address left-to-right.
//   - On exact match advance both cursors; on '?' advance both
//     (but '?' refuses '.').
//   - On '*' (or '**') record a backtrack frame and skip the
//     star in the pattern. The frame remembers "if a later
//     mismatch happens, retry by letting this star eat one
//     more char of the address".
//   - On non-* mismatch, pop the most recent star frame, let it
//     eat one more char (refusing '.' unless it was '**'),
//     and resume.
//
// Complexity: O(len(pattern) * len(addr)) worst case.
func globMatch(pattern, addr string) bool {
	type frame struct {
		pi, ai int // resume positions if we backtrack to here
		recur  bool
	}
	var stars []frame

	pi, ai := 0, 0
	for ai < len(addr) {
		matched := false
		if pi < len(pattern) {
			switch c := pattern[pi]; c {
			case '*':
				recur := pi+1 < len(pattern) && pattern[pi+1] == '*'
				step := 1
				if recur {
					step = 2
				}
				stars = append(stars, frame{pi: pi + step, ai: ai, recur: recur})
				pi += step
				matched = true
			case '?':
				if addr[ai] != '.' {
					pi++
					ai++
					matched = true
				}
			default:
				if c == addr[ai] {
					pi++
					ai++
					matched = true
				}
			}
		}
		if matched {
			continue
		}
		// Backtrack: let the most recent star eat one more char.
		// Single-'*' refuses to step over a '.' (Terraform-address
		// segments don't cross dots); '**' eats anything.
		for {
			if len(stars) == 0 {
				return false
			}
			top := &stars[len(stars)-1]
			if top.ai >= len(addr) {
				stars = stars[:len(stars)-1]
				continue
			}
			if !top.recur && addr[top.ai] == '.' {
				stars = stars[:len(stars)-1]
				continue
			}
			top.ai++
			pi = top.pi
			ai = top.ai
			break
		}
	}
	// Pattern must now be at end (or only trailing stars).
	for pi < len(pattern) {
		if pattern[pi] != '*' {
			return false
		}
		pi++
	}
	return true
}

// renderDiffTable: per-address compact form. One header per
// resource, one line per changed leaf (sampled, up to maxLeaves).
// Goals: skimmable on a 100-column terminal; never overflows on a
// 5000-resource change set (use --address/--limit to control).
func renderDiffTable(w io.Writer, h diffHeader, rows []store.ResourceAttrDelta, summary bool) int {
	renderDiffHeader(w, h)
	if len(rows) == 0 {
		fmt.Fprintln(w, "\n(no attribute changes between these versions)")
		return 0
	}

	added, removed, changed := countByStatus(rows)
	fmt.Fprintf(w, "\nResources:  +%d added  -%d removed  ~%d changed  (%d total)\n",
		added, removed, changed, len(rows))

	if summary {
		fmt.Fprintln(w)
		for _, r := range rows {
			fmt.Fprintf(w, "  %s  %s\n", statusMarker(r.Status), r.Address)
		}
		return 0
	}

	const maxLeavesPerResource = 10
	for _, r := range rows {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s  %s\n", statusMarker(r.Status), r.Address)
		if r.Status == "added" || r.Status == "removed" {
			// For added/removed we want only a small flavor of the
			// attributes — the operator already knows the whole row
			// appears/disappears.
			leaves, err := diffJSON(r.FromAttrs, r.ToAttrs, mergedSensitive(r))
			if err != nil {
				fmt.Fprintf(w, "    (failed to parse attributes: %v)\n", err)
				continue
			}
			renderLeavesSample(w, leaves, maxLeavesPerResource)
			continue
		}
		leaves, err := diffJSON(r.FromAttrs, r.ToAttrs, mergedSensitive(r))
		if err != nil {
			fmt.Fprintf(w, "    (failed to parse attributes: %v)\n", err)
			continue
		}
		renderLeavesSample(w, leaves, maxLeavesPerResource)
	}
	return 0
}

// renderDiffUnified: git-style. One resource per section, all leaves
// listed. For added/removed resources, prints the entire attribute
// set as +/− lines. For changed, prints only the leaves that differ.
// Wider than the table format; meant for piping to `less -R` or a
// patch-like consumer.
func renderDiffUnified(w io.Writer, h diffHeader, rows []store.ResourceAttrDelta, summary bool) int {
	renderDiffHeader(w, h)
	if len(rows) == 0 {
		fmt.Fprintln(w, "\n(no attribute changes between these versions)")
		return 0
	}
	if summary {
		fmt.Fprintln(w)
		for _, r := range rows {
			fmt.Fprintf(w, "%s %s\n", statusMarker(r.Status), r.Address)
		}
		return 0
	}
	for _, r := range rows {
		fmt.Fprintf(w, "\n@@ %s  %s @@\n", statusMarker(r.Status), r.Address)
		leaves, err := diffJSON(r.FromAttrs, r.ToAttrs, mergedSensitive(r))
		if err != nil {
			fmt.Fprintf(w, "  (failed to parse attributes: %v)\n", err)
			continue
		}
		for _, lf := range leaves {
			switch lf.Status {
			case "added":
				fmt.Fprintf(w, "+ %s = %s\n", lf.Path, renderValue(lf.After, lf.Sensitive))
			case "removed":
				fmt.Fprintf(w, "- %s = %s\n", lf.Path, renderValue(lf.Before, lf.Sensitive))
			case "changed":
				if lf.Sensitive {
					fmt.Fprintf(w, "~ %s = <sensitive value changed>\n", lf.Path)
				} else {
					fmt.Fprintf(w, "- %s = %s\n", lf.Path, renderValue(lf.Before, false))
					fmt.Fprintf(w, "+ %s = %s\n", lf.Path, renderValue(lf.After, false))
				}
			}
		}
	}
	return 0
}

// renderDiffJSON: machine-readable form. Sensitive paths are STILL
// redacted on the wire — JSON output is the format most likely to
// land in a log aggregator or shared transcript, so leaking secrets
// because the operator picked --format=json instead of --format=table
// would be unacceptable. Callers that legitimately need the raw
// sensitive payload must go through `export` on the underlying
// version.
func renderDiffJSON(w io.Writer, h diffHeader, rows []store.ResourceAttrDelta, summary bool) int {
	type jsonLeaf struct {
		Path      string `json:"path"`
		Status    string `json:"status"`
		Before    any    `json:"before,omitempty"`
		After     any    `json:"after,omitempty"`
		Sensitive bool   `json:"sensitive,omitempty"`
	}
	type jsonResource struct {
		Address string     `json:"address"`
		Status  string     `json:"status"`
		Leaves  []jsonLeaf `json:"leaves,omitempty"`
	}
	type jsonOut struct {
		State     string         `json:"state"`
		From      map[string]any `json:"from"`
		To        map[string]any `json:"to"`
		Counts    map[string]int `json:"counts"`
		Resources []jsonResource `json:"resources,omitempty"`
	}

	added, removed, changed := countByStatus(rows)
	out := jsonOut{
		State: h.StateName,
		From: map[string]any{
			"serial": h.FromSerial,
			"id":     h.FromID,
			"actor":  h.FromActor,
		},
		To: map[string]any{
			"serial": h.ToSerial,
			"id":     h.ToID,
			"actor":  h.ToActor,
		},
		Counts: map[string]int{
			"added":   added,
			"removed": removed,
			"changed": changed,
			"total":   len(rows),
		},
	}
	if !summary {
		for _, r := range rows {
			jr := jsonResource{Address: r.Address, Status: r.Status}
			leaves, err := diffJSON(r.FromAttrs, r.ToAttrs, mergedSensitive(r))
			if err == nil {
				for _, lf := range leaves {
					jr.Leaves = append(jr.Leaves, jsonLeaf{
						Path: lf.Path, Status: lf.Status,
						Before:    redactedOrValue(lf.Before, lf.Sensitive),
						After:     redactedOrValue(lf.After, lf.Sensitive),
						Sensitive: lf.Sensitive,
					})
				}
			}
			out.Resources = append(out.Resources, jr)
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Turn off HTML escaping: the diff output is consumed by CLI
	// tools, not browsers, so `<sensitive>` should render literally
	// rather than as `\u003csensitive\u003e`. This matches the
	// convention `kl export` uses for the same reason.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "kl diff: encode json:", err)
		return 1
	}
	return 0
}

func renderDiffHeader(w io.Writer, h diffHeader) {
	fmt.Fprintf(w, "Diff for state %q  (serial %d → %d)\n", h.StateName, h.FromSerial, h.ToSerial)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  from:\tserial %d  (%s)\tactor=%s\twritten=%s UTC\n",
		h.FromSerial, shortUUID(h.FromID), emptyDash(h.FromActor),
		h.FromWrittenAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(tw, "  to:\tserial %d  (%s)\tactor=%s\twritten=%s UTC\n",
		h.ToSerial, shortUUID(h.ToID), emptyDash(h.ToActor),
		h.ToWrittenAt.Format("2006-01-02 15:04:05"))
	_ = tw.Flush()
}

// renderLeavesSample prints the first N leaves with status markers,
// then a "... and K more" line. Picks the leaves with bracket-free
// paths first when possible (i.e. prefer "tags.Environment" over
// "policy.statement[0].action") because top-level scalars are the
// most-likely-relevant signal at first glance.
func renderLeavesSample(w io.Writer, leaves []jsonPathLeaf, max int) {
	if len(leaves) == 0 {
		fmt.Fprintln(w, "  (no attribute leaves changed; metadata-only)")
		return
	}
	ordered := make([]jsonPathLeaf, len(leaves))
	copy(ordered, leaves)
	sort.SliceStable(ordered, func(i, j int) bool {
		di := strings.Count(ordered[i].Path, ".")
		dj := strings.Count(ordered[j].Path, ".")
		if di != dj {
			return di < dj
		}
		return ordered[i].Path < ordered[j].Path
	})
	shown := 0
	for _, lf := range ordered {
		if shown >= max {
			fmt.Fprintf(w, "  ... and %d more leaf(es)\n", len(ordered)-shown)
			return
		}
		switch lf.Status {
		case "added":
			fmt.Fprintf(w, "  + %s = %s\n", lf.Path, renderValue(lf.After, lf.Sensitive))
		case "removed":
			fmt.Fprintf(w, "  - %s = %s\n", lf.Path, renderValue(lf.Before, lf.Sensitive))
		case "changed":
			if lf.Sensitive {
				// Both before/after would render as <sensitive>,
				// which loses the signal that there IS a change.
				// Collapse to a single line that says so plainly.
				fmt.Fprintf(w, "  ~ %s : <sensitive value changed>\n", lf.Path)
			} else {
				fmt.Fprintf(w, "  ~ %s : %s -> %s\n",
					lf.Path,
					renderValue(lf.Before, false),
					renderValue(lf.After, false))
			}
		}
		shown++
	}
}

func statusMarker(status string) string {
	switch status {
	case "added":
		return "+"
	case "removed":
		return "-"
	case "changed":
		return "~"
	default:
		return "?"
	}
}

func countByStatus(rows []store.ResourceAttrDelta) (added, removed, changed int) {
	for _, r := range rows {
		switch r.Status {
		case "added":
			added++
		case "removed":
			removed++
		case "changed":
			changed++
		}
	}
	return
}

// mergedSensitive combines the sensitive-path lists from both sides.
// A path that is sensitive on ONE side must be treated as sensitive
// on the rendered diff — the cross-side transition is itself the
// sensitive event we want to mask.
func mergedSensitive(r store.ResourceAttrDelta) pathSet {
	a := newPathSet(r.FromSensitive)
	b := newPathSet(r.ToSensitive)
	for k := range b {
		a[k] = struct{}{}
	}
	return a
}

func redactedOrValue(v any, sensitive bool) any {
	if !sensitive {
		return v
	}
	if v == nil {
		return nil
	}
	return "<sensitive>"
}

const diffUsage = `Usage:
  kl diff [state] [flags]

Attribute-level diff between two versions of a state. Resolves both
versions to state_versions rows, fetches the per-address attribute
blobs, walks the JSON to produce path-level leaves, and renders the
result in one of three formats.

By default diffs the previous version (@1) against the current one.

Positional:
  state                 State name (default: auto-detected from the
                        http backend address of the CWD).

Flags:
  --from=REF            Source version. Accepts <serial>, @<N>, <uuid>,
                        or 'current'. Default: @1 (one back).
  --to=REF              Target version. Same shapes. Default: current.
  --format=FMT          table (default), unified, or json.
  --address=GLOB        Filter resources by glob (e.g. 'aws_instance.*'
                        or '*.module.foo.*').
  --limit=N             Cap the number of resources rendered (after
                        filtering). 0 = no cap.
  --summary             Skip attribute leaves, print only the
                        per-address +/-/~ list.

Sensitive paths are redacted on output in ALL formats including json.
`
