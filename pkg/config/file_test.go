package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// parseFileSettings
// ---------------------------------------------------------------------------

func TestParseFileSettings_TopLevelKeys(t *testing.T) {
	src := `
# comment line
database_url = "postgres://kl:kl@localhost:5432/kl?sslmode=disable"
backend_address = "http://localhost:8080/v1/states/big-state"
`
	got, err := parseFileSettings(strings.NewReader(src), "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := "postgres://kl:kl@localhost:5432/kl?sslmode=disable"; got.DatabaseURL != want {
		t.Errorf("DatabaseURL = %q, want %q", got.DatabaseURL, want)
	}
	if want := "http://localhost:8080/v1/states/big-state"; got.BackendAddress != want {
		t.Errorf("BackendAddress = %q, want %q", got.BackendAddress, want)
	}
}

func TestParseFileSettings_SectionScopedKeys(t *testing.T) {
	src := `
[database]
url = "postgres://example/db"

[backend]
address = "http://infra.example/v1/states/foo"
`
	got, err := parseFileSettings(strings.NewReader(src), "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.DatabaseURL != "postgres://example/db" {
		t.Errorf("DatabaseURL = %q", got.DatabaseURL)
	}
	if got.BackendAddress != "http://infra.example/v1/states/foo" {
		t.Errorf("BackendAddress = %q", got.BackendAddress)
	}
}

func TestParseFileSettings_UnknownKeysIgnored(t *testing.T) {
	src := `
database_url = "postgres://a"
some_future_key = "ignored"

[unknown]
also = "ignored"
`
	got, err := parseFileSettings(strings.NewReader(src), "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.DatabaseURL != "postgres://a" {
		t.Errorf("unrelated unknown keys must not affect known values; got %q", got.DatabaseURL)
	}
}

func TestParseFileSettings_InlineComments(t *testing.T) {
	src := `database_url = "postgres://a" # trailing comment
backend_address = "http://b"  #another
`
	got, err := parseFileSettings(strings.NewReader(src), "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.DatabaseURL != "postgres://a" {
		t.Errorf("trailing-comment value corrupted: %q", got.DatabaseURL)
	}
	if got.BackendAddress != "http://b" {
		t.Errorf("trailing-comment value corrupted: %q", got.BackendAddress)
	}
}

func TestParseFileSettings_MalformedLineReturnsLocatedError(t *testing.T) {
	src := `
database_url = "postgres://a"
this line is not a kv
`
	_, err := parseFileSettings(strings.NewReader(src), "myfile")
	if err == nil {
		t.Fatal("expected error on malformed line")
	}
	if !strings.Contains(err.Error(), "myfile:3") {
		t.Errorf("error should include source:line tag, got %v", err)
	}
}

func TestParseFileSettings_UnterminatedString(t *testing.T) {
	src := `database_url = "postgres://...`
	if _, err := parseFileSettings(strings.NewReader(src), "f"); err == nil {
		t.Error("expected error on unterminated string")
	}
}

func TestParseFileSettings_UnquotedValueRejected(t *testing.T) {
	src := `database_url = postgres://a`
	if _, err := parseFileSettings(strings.NewReader(src), "f"); err == nil {
		t.Error("expected error on unquoted value")
	}
}

// ---------------------------------------------------------------------------
// FindConfigFile
// ---------------------------------------------------------------------------

func TestFindConfigFile_WalksUpAndFinds(t *testing.T) {
	root := t.TempDir()
	mid := filepath.Join(root, "a", "b")
	leaf := filepath.Join(mid, "c")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := filepath.Join(mid, ConfigFileName)
	if err := os.WriteFile(cfg, []byte(`database_url = "postgres://x"`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := FindConfigFile(leaf)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != cfg {
		t.Errorf("found %q, want %q", got, cfg)
	}
}

func TestFindConfigFile_NotFoundIsErrNotExist(t *testing.T) {
	root := t.TempDir()
	_, err := FindConfigFile(root)
	if err == nil || !os.IsNotExist(err) {
		t.Errorf("got %v, want os.ErrNotExist", err)
	}
}

func TestFindConfigFile_NearestWins(t *testing.T) {
	root := t.TempDir()
	mid := filepath.Join(root, "a")
	leaf := filepath.Join(mid, "b")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	farPath := filepath.Join(root, ConfigFileName)
	nearPath := filepath.Join(mid, ConfigFileName)
	if err := os.WriteFile(farPath, []byte(`database_url = "FAR"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nearPath, []byte(`database_url = "NEAR"`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FindConfigFile(leaf)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nearPath {
		t.Errorf("got %q, want nearer file %q", got, nearPath)
	}
}

func TestFindConfigFile_RejectsDirectoryNamedLikeConfig(t *testing.T) {
	root := t.TempDir()
	weird := filepath.Join(root, ConfigFileName)
	if err := os.MkdirAll(weird, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := FindConfigFile(root); err == nil {
		t.Error("expected error when .kl.toml is a directory")
	}
}

// ---------------------------------------------------------------------------
// Load — integration: file + env precedence
// ---------------------------------------------------------------------------

func TestLoad_FileSetsDatabaseURLWhenEnvUnset(t *testing.T) {
	// Set CWD to a tempdir with a .kl.toml.
	dir := t.TempDir()
	cfg := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(cfg, []byte(`database_url = "postgres://from-file/db"`), 0o644); err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KL_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")

	c := Load()
	if c.DatabaseURL != "postgres://from-file/db" {
		t.Errorf("DatabaseURL = %q, want from-file value", c.DatabaseURL)
	}
	assertSameFile(t, c.LoadedConfigFile, cfg)
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(cfg, []byte(`database_url = "postgres://from-file/db"`), 0o644); err != nil {
		t.Fatal(err)
	}
	origWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KL_DATABASE_URL", "postgres://from-env/db")

	c := Load()
	if c.DatabaseURL != "postgres://from-env/db" {
		t.Errorf("DatabaseURL = %q, want env value to override file", c.DatabaseURL)
	}
	// File still recorded as loaded — env just overrides specific
	// fields, not the loaded-from-file fact.
	assertSameFile(t, c.LoadedConfigFile, cfg)
}

// assertSameFile compares two paths by stat'ing both and checking
// they resolve to the same inode. Works around macOS's
// /var/folders → /private/var/folders symlink that makes simple
// string comparison flaky for any path under TMPDIR.
func assertSameFile(t *testing.T, got, want string) {
	t.Helper()
	gotInfo, gotErr := os.Stat(got)
	wantInfo, wantErr := os.Stat(want)
	if gotErr != nil || wantErr != nil {
		t.Fatalf("stat got=%v err=%v / want=%v err=%v", got, gotErr, want, wantErr)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Errorf("paths refer to different files: got=%q want=%q", got, want)
	}
}

func TestLoad_NoFileNoEnvLeavesDatabaseURLEmpty(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KL_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	c := Load()
	if c.DatabaseURL != "" {
		t.Errorf("DatabaseURL = %q, want empty (will fail Validate)", c.DatabaseURL)
	}
	if c.LoadedConfigFile != "" {
		t.Errorf("LoadedConfigFile = %q, want empty", c.LoadedConfigFile)
	}
	if err := c.Validate(); err == nil {
		t.Error("Validate must reject empty DatabaseURL")
	} else if !strings.Contains(err.Error(), ".kl.toml") {
		t.Errorf("error message should mention .kl.toml as a source, got %v", err)
	}
}
