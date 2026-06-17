package main

import (
	"flag"
	"testing"
)

func TestRegisterStringFlagAlias_ParsesShortForm(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var out string
	registerStringFlagAlias(fs, &out, "out", "o", "", "output path")

	if err := fs.Parse([]string{"-o", "plan.json"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out != "plan.json" {
		t.Fatalf("out = %q, want %q", out, "plan.json")
	}
}

func TestRegisterFlagValueAlias_ParsesShortForm(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	files := &fileFlag{}
	registerFlagValueAlias(fs, files, "file", "f", "file scope")

	if err := fs.Parse([]string{"-f", "slow_a.tf", "-f", "slow_b.tf"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := len(files.values), 2; got != want {
		t.Fatalf("len(files.values) = %d, want %d", got, want)
	}
	if files.values[0] != "slow_a.tf" || files.values[1] != "slow_b.tf" {
		t.Fatalf("files.values = %#v", files.values)
	}
}
