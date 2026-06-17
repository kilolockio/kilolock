package store

import "testing"

func TestDatabaseNameFor(t *testing.T) {
	if got := databaseNameFor("Acme-Corp", "prod"); got != "kl_acme_corp_prod" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeInstanceKey(t *testing.T) {
	if got := normalizeInstanceKey(""); got != "shared" {
		t.Fatalf("empty => %q", got)
	}
	if got := normalizeInstanceKey("  PREMIUM "); got != "premium" {
		t.Fatalf("normalized => %q", got)
	}
}
