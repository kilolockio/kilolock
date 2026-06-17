package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

var version = "0.0.0-dev"

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
	case "serve":
		os.Exit(runServe(args))
	case "migrate":
		os.Exit(runMigrate(args))
	case "provision":
		os.Exit(runProvision(args))
	default:
		fmt.Fprintf(os.Stderr, "kld: unknown subcommand %q\n\n", sub)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `kld %s

Usage:
  kld <subcommand> [flags]

Subcommands:
  serve      Run the Kilolock backend/runtime server.
  migrate    Apply pending schema migrations.
  provision  Run infrastructure-side provisioning helpers.
  version    Print the binary version.
  help       Show this message.

Notes:
  kld is the infrastructure-side binary. It may connect directly
  to control-plane and data-plane databases.

  Use the separate %q client CLI for day-to-day operator and user workflows.
`, versionString(), "kl")
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
