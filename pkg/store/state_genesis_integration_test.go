//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestEnsureCurrentStateInfo_BootstrapsGenesisVersion(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	info, err := s.EnsureCurrentStateInfo(ctx, "genesis-state")
	if err != nil {
		t.Fatalf("EnsureCurrentStateInfo: %v", err)
	}
	if info.Serial != 0 {
		t.Fatalf("serial=%d want 0", info.Serial)
	}
	if info.StateID == "" || info.VersionID == "" {
		t.Fatalf("missing ids: %+v", info)
	}

	st, err := tfstate.Parse(info.Raw)
	if err != nil {
		t.Fatalf("parse genesis raw state: %v", err)
	}
	if len(st.Resources) != 0 {
		t.Fatalf("resources=%d want 0", len(st.Resources))
	}

	again, err := s.GetCurrentStateInfo(ctx, "genesis-state")
	if err != nil {
		t.Fatalf("GetCurrentStateInfo: %v", err)
	}
	if again.VersionID != info.VersionID {
		t.Fatalf("version mismatch: got %q want %q", again.VersionID, info.VersionID)
	}
	if again.Serial != 0 {
		t.Fatalf("serial after reread=%d want 0", again.Serial)
	}
}
