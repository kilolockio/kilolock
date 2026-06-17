package main

import (
	"strings"
	"testing"
)

func TestDecodeConfigJSON_Object(t *testing.T) {
	cfg, err := decodeConfigJSON([]byte(`{"region":"us-east-1","profile":"dev"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg["region"] != "us-east-1" {
		t.Errorf("region: got %v", cfg["region"])
	}
	if cfg["profile"] != "dev" {
		t.Errorf("profile: got %v", cfg["profile"])
	}
}

func TestDecodeConfigJSON_EmptyObject(t *testing.T) {
	cfg, err := decodeConfigJSON([]byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg) != 0 {
		t.Errorf("expected empty map, got %#v", cfg)
	}
}

func TestDecodeConfigJSON_Rejects(t *testing.T) {
	cases := map[string]struct {
		in       string
		errMatch string
	}{
		"empty":           {"", "empty config payload"},
		"whitespace only": {"   \n\t  ", "empty config payload"},
		"invalid json":    {`{not valid`, "decode JSON"},
		"array":           {`[1, 2, 3]`, "must be a JSON object"},
		"scalar":          {`"a string"`, "must be a JSON object"},
		"null":            {`null`, "must be a JSON object"},
		"number":          {`42`, "must be a JSON object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeConfigJSON([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error matching %q, got nil", tc.errMatch)
			}
			if !strings.Contains(err.Error(), tc.errMatch) {
				t.Errorf("error %q does not contain %q", err, tc.errMatch)
			}
		})
	}
}

func TestSummarizeAttrs(t *testing.T) {
	cases := map[string]struct {
		in   map[string]any
		want string
	}{
		"empty":  {map[string]any{}, "(none)"},
		"single": {map[string]any{"region": "us-east-1"}, "region"},
		// Keys must be sorted so output is stable across runs;
		// `provider list` would be useless if ordering varied.
		"multi sorted": {
			map[string]any{"region": "x", "profile": "y", "endpoints": "z"},
			"endpoints, profile, region",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := summarizeAttrs(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
