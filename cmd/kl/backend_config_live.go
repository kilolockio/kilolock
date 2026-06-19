package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/kilolockio/kilolock/internal/plan"
)

func discoverLiveBackend(cwd string) (*plan.BackendInfo, error) {
	bi, err := plan.DiscoverBackendConfig(cwd)
	if err != nil {
		return nil, err
	}

	if addr := strings.TrimSpace(os.Getenv("TF_HTTP_ADDRESS")); addr != "" {
		bi.Address = addr
		name, err := stateNameFromAddress(addr)
		if err != nil {
			return nil, fmt.Errorf("derive state name from TF_HTTP_ADDRESS %q: %w", addr, err)
		}
		bi.StateName = name
	}
	if user := strings.TrimSpace(os.Getenv("TF_HTTP_USERNAME")); user != "" {
		bi.Username = user
	} else if user := strings.TrimSpace(os.Getenv("TF_HTTP_USER")); user != "" {
		bi.Username = user
	}
	if pass := strings.TrimSpace(os.Getenv("TF_HTTP_PASSWORD")); pass != "" {
		bi.Password = pass
	}
	return bi, nil
}
