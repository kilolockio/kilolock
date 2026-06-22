package workdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveScratchRoot_DefaultEmpty(t *testing.T) {
	t.Setenv("KL_DATA_DIR", "")
	t.Setenv("TF_DATA_DIR", "")

	got, err := ResolveScratchRoot("")
	if err != nil {
		t.Fatalf("ResolveScratchRoot: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestResolveScratchRoot_PrefersKLDataDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TF_DATA_DIR", filepath.Join(tmp, "tf"))
	t.Setenv("KL_DATA_DIR", filepath.Join(tmp, "kl"))

	got, err := ResolveScratchRoot(filepath.Join(tmp, "default"))
	if err != nil {
		t.Fatalf("ResolveScratchRoot: %v", err)
	}
	want := filepath.Join(tmp, "kl")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected %q to exist: %v", want, err)
	}
}

func TestResolveScratchRoot_FallsBackToTFDataDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KL_DATA_DIR", "")
	t.Setenv("TF_DATA_DIR", filepath.Join(tmp, "tf"))

	got, err := ResolveScratchRoot("")
	if err != nil {
		t.Fatalf("ResolveScratchRoot: %v", err)
	}
	want := filepath.Join(tmp, "tf")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveScratchRoot_UsesDefaultRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KL_DATA_DIR", "")
	t.Setenv("TF_DATA_DIR", "")

	want := filepath.Join(tmp, "default")
	got, err := ResolveScratchRoot(want)
	if err != nil {
		t.Fatalf("ResolveScratchRoot: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveScratchRoot_MakesRelativeAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Setenv("KL_DATA_DIR", "tmp/kl-work")
	t.Setenv("TF_DATA_DIR", "")

	got, err := ResolveScratchRoot("")
	if err != nil {
		t.Fatalf("ResolveScratchRoot: %v", err)
	}
	want := filepath.Join(wd, "tmp", "kl-work")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
