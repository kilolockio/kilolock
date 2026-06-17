package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/davesade/kilolock/internal/bootstrapinit"
)

func runOperator(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "kl operator: missing subcommand (init|seal-status)")
		return 2
	}
	switch args[0] {
	case "init":
		return runOperatorInit(args[1:])
	case "seal-status":
		return runOperatorSealStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "kl operator: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runOperatorInit(args []string) int {
	fs := flag.NewFlagSet("operator init", flag.ContinueOnError)
	tenant := fs.String("tenant", "operator", "Initial operator tenant slug.")
	name := fs.String("tenant-name", "Operator", "Initial operator tenant name.")
	tokenName := fs.String("token-name", "operator-bootstrap", "Initial operator token name.")
	token := fs.String("token", "", "Optional explicit token secret (prints generated token when omitted).")
	outputFile := fs.String("output-file", "", "Optional path to write bootstrap JSON output.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client := newControlAPIClient()
	var status struct {
		Initialized   bool    `json:"initialized"`
		InitMode      string  `json:"init_mode"`
		InitializedBy string  `json:"initialized_by"`
		InitializedAt *string `json:"initialized_at"`
	}
	err := client.getJSON(ctx, "/bootstrap/status", &status)
	if err != nil {
		fmt.Fprintln(os.Stderr, "operator init:", err)
		return 1
	}
	if status.Initialized {
		at := "unknown"
		if status.InitializedAt != nil && *status.InitializedAt != "" {
			at = *status.InitializedAt
		}
		fmt.Fprintf(os.Stderr, "operator init: already initialized (%s by %s)\n",
			at, status.InitializedBy)
		return 1
	}
	var resp struct {
		Tenant      string `json:"tenant"`
		TokenName   string `json:"token_name"`
		TokenSecret string `json:"token_secret"`
	}
	if err := client.postJSON(ctx, "/bootstrap/init", map[string]any{
		"tenant":      *tenant,
		"tenant_name": *name,
		"token_name":  *tokenName,
		"token":       *token,
	}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "operator init:", err)
		return 1
	}
	if path := strings.TrimSpace(*outputFile); path != "" {
		if err := bootstrapinit.WriteFile(path, bootstrapinit.Output{
			Tenant:     resp.Tenant,
			TenantName: *name,
			TokenName:  resp.TokenName,
			Token:      resp.TokenSecret,
			CreatedAt:  time.Now().UTC(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "operator init: write output file: %v\n", err)
			return 1
		}
	}
	fmt.Println("kl operator init complete")
	fmt.Printf("tenant=%s token_name=%s\n", resp.Tenant, resp.TokenName)
	fmt.Printf("token=%s\n", resp.TokenSecret)
	fmt.Println("store this token in a secret manager; it is shown only now.")
	return 0
}

func runOperatorSealStatus(args []string) int {
	fs := flag.NewFlagSet("operator seal-status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client := newControlAPIClient()
	var status struct {
		Initialized   bool    `json:"initialized"`
		InitMode      string  `json:"init_mode"`
		InitializedBy string  `json:"initialized_by"`
		InitializedAt *string `json:"initialized_at"`
	}
	err := client.getJSON(ctx, "/bootstrap/status", &status)
	if err != nil {
		fmt.Fprintln(os.Stderr, "operator seal-status:", err)
		return 1
	}
	if !status.Initialized {
		fmt.Println("sealed=true initialized=false")
		return 0
	}
	at := "unknown"
	if status.InitializedAt != nil && *status.InitializedAt != "" {
		at = *status.InitializedAt
	}
	fmt.Println("sealed=false initialized=true")
	fmt.Printf("mode=%s initialized_by=%s initialized_at=%s\n",
		status.InitMode, status.InitializedBy, at)
	return 0
}
