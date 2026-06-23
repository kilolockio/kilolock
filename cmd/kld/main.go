package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kilolockio/kilolock/pkg/buildinfo"
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
		os.Exit(runVersion(args))
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
	return buildinfo.Current("kld", "Kilolock Backend").ShortString()
}

func runVersion(args []string) int {
	jsonOut := false
	for _, arg := range args {
		switch strings.TrimSpace(arg) {
		case "--json":
			jsonOut = true
		case "":
		default:
			fmt.Fprintf(os.Stderr, "kld version: unknown flag %q\n", arg)
			return 2
		}
	}
	info := buildinfo.Current("kld", "Kilolock Backend")
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			fmt.Fprintln(os.Stderr, "kld version:", err)
			return 1
		}
		return 0
	}
	fmt.Println(info.ShortString())
	return 0
}
