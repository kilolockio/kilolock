package main

import (
	"flag"
	"fmt"
	"strings"
)

type adminClientFlags struct {
	stateURL *string
	token    *string
}

func registerAdminClientFlags(fs *flag.FlagSet, includeStateURL bool) adminClientFlags {
	flags := adminClientFlags{
		token: fs.String("token", "", "Bearer token for cloud/admin API auth. Use kl_... for automation. klp_... PATs can authorize environment-scoped admin requests when a state/environment scope is provided; TF_HTTP_PASSWORD remains backend auth."),
	}
	if includeStateURL {
		flags.stateURL = fs.String("state-url", "", "Full state URL. Overrides KL_STATE_URL and backend discovery.")
	}
	return flags
}

func (f adminClientFlags) explicitStateURL() string {
	if f.stateURL == nil {
		return ""
	}
	return strings.TrimSpace(*f.stateURL)
}

func (f adminClientFlags) explicitToken() string {
	if f.token == nil {
		return ""
	}
	return strings.TrimSpace(*f.token)
}

func (f adminClientFlags) newClient(cwd string) (*apiClient, error) {
	if stateURL := f.explicitStateURL(); stateURL != "" {
		target, _, err := resolveStateTarget(stateURL, cwd)
		if err != nil {
			return nil, fmt.Errorf("parse --state-url: %w", err)
		}
		return newAPIClientForTarget(cwd, target, f.explicitToken())
	}
	return newAPIClientWithToken(cwd, f.explicitToken())
}

func (f adminClientFlags) resolveStateTarget(positional, cwd string) (stateTarget, bool, error) {
	stateRef := strings.TrimSpace(positional)
	if stateURL := f.explicitStateURL(); stateURL != "" {
		stateRef = stateURL
	}
	return resolveStateTarget(stateRef, cwd)
}
