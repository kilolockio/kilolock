package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

func runList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	adminFlags := registerAdminClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl list:", err)
		return 1
	}
	var resp struct {
		States []struct {
			Name             string `json:"name"`
			Serial           int64  `json:"serial"`
			ResourceCount    int    `json:"resource_count"`
			TerraformVersion string `json:"terraform_version"`
			Locked           bool   `json:"locked"`
			UpdatedAt        string `json:"updated_at"`
		} `json:"states"`
	}
	err = client.getJSON(ctx, "/admin/states", &resp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl list:", err)
		return 1
	}

	if len(resp.States) == 0 {
		fmt.Fprintln(os.Stderr, "no states yet")
		return 0
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSERIAL\tRESOURCES\tTF VERSION\tLOCKED\tUPDATED")
	for _, r := range resp.States {
		locked := "no"
		if r.Locked {
			locked = "yes"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n",
			r.Name, r.Serial, r.ResourceCount,
			r.TerraformVersion, locked, r.UpdatedAt)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "kl list: flush output:", err)
		return 1
	}
	return 0
}
