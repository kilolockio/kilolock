//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/testdb"
)

func TestCreateControlOperatorTokenAndAuthorize(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	row, secret, err := s.CreateControlOperatorToken(ctx, "alice@example.com", "support_admin", "global", "", "itest")
	if err != nil {
		t.Fatalf("CreateControlOperatorToken: %v", err)
	}
	if row.Name == "" || secret == "" {
		t.Fatalf("expected operator token metadata and secret, got row=%+v secret=%q", row, secret)
	}

	p, ok, err := s.AuthorizeControlAPIToken(ctx, secret, "state.delete")
	if err != nil || !ok {
		t.Fatalf("AuthorizeControlAPIToken state.delete: ok=%v err=%v", ok, err)
	}
	if p.Source != "control-api-token" {
		t.Fatalf("principal source = %q, want control-api-token", p.Source)
	}
	if scoped, err := s.AuthorizeControlPrincipalScope(ctx, p, "state.delete", "*", ""); err != nil || !scoped {
		t.Fatalf("AuthorizeControlPrincipalScope global state.delete: scoped=%v err=%v", scoped, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, p, "rbac.manage", "*", ""); err != nil || ok {
		t.Fatalf("support_admin should not have rbac.manage: ok=%v err=%v", ok, err)
	}

	list, err := s.ListControlOperatorTokens(ctx, false)
	if err != nil {
		t.Fatalf("ListControlOperatorTokens: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected operator token listing to include created token")
	}
	if got := list[0].RoleKey; got == "" {
		t.Fatalf("expected role in operator token listing, got empty row=%+v", list[0])
	}
}
