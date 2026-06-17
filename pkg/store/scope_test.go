package store

import (
	"context"
	"testing"

	"github.com/kilolockio/kilolock/pkg/auth"
)

func TestStateByNameWhere(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: auth.SelfHostedTenantID,
	})
	unified := &Store{isolated: false}
	clause, args := unified.stateByNameWhere(ctx, "prod")
	if clause != "s.name = $1 AND s.tenant_id = $2 AND s.lifecycle_status = 'active'" {
		t.Fatalf("unified clause: %q", clause)
	}
	if len(args) != 2 || args[0] != "prod" {
		t.Fatalf("unified args: %v", args)
	}

	iso := &Store{isolated: true}
	clause, args = iso.stateByNameWhere(ctx, "prod")
	if clause != "s.name = $1 AND s.lifecycle_status = 'active'" || len(args) != 1 {
		t.Fatalf("isolated: clause=%q args=%v", clause, args)
	}
}
