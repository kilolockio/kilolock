//go:build integration

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestCreateTenantWithDefaultEnvironment_GeneratesWorkspaceSlugAndName(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	row, err := s.CreateTenantWithDefaultEnvironment(ctx, "", "", false)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if !strings.HasPrefix(row.Slug, "ws_") {
		t.Fatalf("slug=%q want ws_ prefix", row.Slug)
	}
	if row.Name != row.Slug {
		t.Fatalf("name=%q want generated slug %q", row.Name, row.Slug)
	}
}
