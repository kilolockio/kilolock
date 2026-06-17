package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	output := fs.String("o", "-", "output file path (\"-\" for stdout)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: kl export [flags] <state-name>

Writes the current version of a state to disk (or stdout). The output is
a byte-equivalent copy of whatever Terraform last POSTed; no
re-serialization is performed.

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
	stateName := fs.Arg(0)

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()

	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl export:", err)
		return 1
	}
	raw, err := client.getBytes(ctx, "/states/"+stateName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl export:", err)
		return 1
	}

	if *output == "-" {
		if _, err := os.Stdout.Write(raw); err != nil {
			fmt.Fprintln(os.Stderr, "kl export: write stdout:", err)
			return 1
		}
		return 0
	}
	if err := os.WriteFile(*output, raw, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "kl export: write file:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "kl export: wrote %d bytes to %s\n", len(raw), *output)
	return 0
}
