package main

import (
	"strings"
	"testing"
)

func TestTerraformStateCommandEnv_PrefersExplicitStateURLAndToken(t *testing.T) {
	t.Setenv("KL_STATE_URL", "https://env.example/v1/states/ws_env/env_env/demo")
	t.Setenv("KL_TOKEN", "kl_env_token")

	env := terraformStateCommandEnv(stateTarget{
		StateName: "ws_cfg/env_cfg/demo",
		Username:  "backend-user",
		Password:  "backend-pass",
	}, adminClientFlags{
		stateURL: ptrString("https://explicit.example/v1/states/ws_exp/env_exp/demo"),
		token:    ptrString("kl_explicit_token"),
	})

	if got := envValue(env, "TF_HTTP_ADDRESS"); got != "https://explicit.example/v1/states/ws_exp/env_exp/demo" {
		t.Fatalf("TF_HTTP_ADDRESS=%q", got)
	}
	if got := envValue(env, "TF_HTTP_PASSWORD"); got != "kl_explicit_token" {
		t.Fatalf("TF_HTTP_PASSWORD=%q", got)
	}
	if got := envValue(env, "TF_HTTP_USERNAME"); got != "backend-user" {
		t.Fatalf("TF_HTTP_USERNAME=%q", got)
	}
}

func TestTerraformStateCommandEnv_UsesKLTokenBeforeBackendPassword(t *testing.T) {
	t.Setenv("KL_TOKEN", "kl_env_token")
	env := terraformStateCommandEnv(stateTarget{
		Password: "backend-pass",
	}, adminClientFlags{})
	if got := envValue(env, "TF_HTTP_PASSWORD"); got != "kl_env_token" {
		t.Fatalf("TF_HTTP_PASSWORD=%q", got)
	}
}

func ptrString(v string) *string { return &v }

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}
