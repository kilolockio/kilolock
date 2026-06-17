// Command kl is the client CLI entry point for the Kilolock
// project. Runtime and deployment commands live in the separate
// `kld` binary. v0 ships these client subcommands:
//
//	kl import <file>      Load a .tfstate file into the database.
//	kl export <name>      Write a state's current version to disk.
//	kl list               List managed states.
//	kl query <sql>        Run a read-only SQL query (operator mode).
//	kl query resources    Query live state resources via backend auth.
//	kl query history      Query per-resource history via backend auth.
//	kl provider <action>  Manage stored provider configurations.
//	kl refresh <name>     Refresh a state by talking to providers directly.
//	kl plan <config-dir>  Generate a kl plan spec from a Terraform configuration.
//	kl apply              Apply a kl plan spec to a trunk state (v2 parallel apply).
//	kl history [state]    List the state_versions history of a state (newest first).
//	kl rollback [state]   Replay a past state_version as a new write (dry-run by default).
//	kl rollback resource  Replay one resource address from history into current state.
//	kl status [state]     Live operational status of a state (lock, applies, reservations).
//	kl diff [state]       Attribute-level diff between two state versions.
//	kl tag <sub>          Manage named pointers to state versions (set/unset/list).
//	kl operator <sub>     Control-plane bootstrap helpers (init, seal-status).
//	kl version            Print the binary version.
//
// See README.md and docs/protocol.md for details.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

// version is the binary version. Overridden at build time via -ldflags.
var version = "0.0.0-dev"

// These are optionally overridden at build time via -ldflags. When they are
// left empty, we fall back to Go's embedded VCS build info if available.
var (
	buildCommit = ""
	buildTime   = ""
	buildDirty  = ""
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "--help", "-h":
		printUsage(os.Stdout)
	case "serve", "migrate", "provision":
		fmt.Fprintf(os.Stderr, "kl: %q moved to %q\n", sub, "kld")
		fmt.Fprintf(os.Stderr, "Run: %s %s\n", "kld", strings.Join(append([]string{sub}, args...), " "))
		os.Exit(2)
	case "import":
		os.Exit(runImport(args))
	case "export":
		os.Exit(runExport(args))
	case "list", "ls":
		os.Exit(runList(args))
	case "query", "q":
		os.Exit(runQuery(args))
	case "provider":
		os.Exit(runProvider(args))
	case "refresh":
		os.Exit(runRefresh(args))
	case "plan":
		os.Exit(runPlan(args))
	case "apply":
		os.Exit(runApply(args))
	case "history":
		os.Exit(runHistory(args))
	case "rollback":
		os.Exit(runRollback(args))
	case "status":
		os.Exit(runStatus(args))
	case "diff":
		os.Exit(runDiff(args))
	case "tag":
		os.Exit(runTag(args))
	case "operator":
		os.Exit(runOperator(args))
	default:
		fmt.Fprintf(os.Stderr, "kl: unknown subcommand %q\n\n", sub)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `kl %s

Usage:
  kl <subcommand> [flags]

Subcommands:
  import    Load a .tfstate file into the configured database.
  export    Write a state's current version to disk (or stdout).
  list      List managed states with summary stats.
  query     Run a read-only SQL query, or backend-native state/resource queries.
  provider  Manage stored provider configurations (configure/get/list/remove).
  refresh   Refresh a state by talking to providers directly (no Terraform CLI).
  plan      Generate a kl plan spec from a Terraform configuration (v2).
  apply     Apply a kl plan spec to a trunk state (v2 parallel apply).
  history   List the state_versions history of a state (newest first).
  rollback  Replay a past state_version, or one historical resource address, as a new write.
  status    Live operational status (lock, in-flight applies, reservations).
  diff      Attribute-level diff between two state versions.
  tag       Manage named pointers (set/unset/list) — usable as version refs.
  operator  Control-plane bootstrap helpers (init, seal-status).
  version   Print the binary version.
  help      Show this message.

Runtime / deployment:
  Use %q for:
    serve
    migrate
    provision

Configuration:
  .kl.toml          Project-local config; kl walks up from
                            CWD to find it. See .kl.toml.example
                            at the repo root for the supported keys.

Environment (overrides .kl.toml):
  KL_DATABASE_URL   Postgres connection string (required by the backend server).
                            Falls back to DATABASE_URL if unset.
  KL_LISTEN_ADDR    Address for the HTTP server to bind (default :8080).
  KL_LOG_LEVEL      debug|info|warn|error (default info).
  KL_LOG_FORMAT     text|json (default text).
  KL_AUTH_MODE      open|static|database|auto (default auto).
  KL_AUTH_TOKEN     Shared secret for static/auto mode.
  KL_BOOTSTRAP_*    Seed tenant+token on serve (database mode).
  KL_INIT_MODE      dev|prod (default dev). prod expects metadata
                            init to be handled by klc.
  KL_DATA_PLANE_MAX_CONNS
                            Default max conns for routed environment pools.
  KL_DATA_PLANE_MAX_CONNS_<KEY>
                            Per-instance max conns override (e.g. _PREMIUM=40).
  KL_ROUTING_STATS_INTERVAL_SECONDS
                            Periodic routing cache stats log interval (default 60, 0 disables).
  KL_ROUTING_CIRCUIT_FAILURE_THRESHOLD
                            Consecutive connect failures before opening circuit (default 2).
  KL_ROUTING_CIRCUIT_COOLDOWN_SECONDS
                            Cooldown window for open circuit before retry (default 10).
  KL_ENV_MIGRATION_ENABLED
                            Enable background migration for provisioned environment DBs (default true).
  KL_ENV_MIGRATION_INTERVAL_SECONDS
                            Background environment migration interval in seconds (default 300).
`, versionString(), "kld")
}

func versionString() string {
	commit := strings.TrimSpace(buildCommit)
	buildAt := strings.TrimSpace(buildTime)
	dirty := strings.TrimSpace(buildDirty)

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" {
					commit = setting.Value
				}
			case "vcs.time":
				if buildAt == "" {
					buildAt = setting.Value
				}
			case "vcs.modified":
				if dirty == "" {
					if setting.Value == "true" {
						dirty = "dirty"
					} else if setting.Value == "false" {
						dirty = "clean"
					}
				}
			}
		}
	}

	parts := []string{version}
	if commit != "" {
		short := commit
		if len(short) > 12 {
			short = short[:12]
		}
		parts = append(parts, "commit="+short)
	}
	if dirty != "" && dirty != "clean" {
		parts = append(parts, dirty)
	}
	if buildAt != "" {
		parts = append(parts, "built="+buildAt)
	}
	return strings.Join(parts, " ")
}
