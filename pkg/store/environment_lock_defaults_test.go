package store

import (
	"testing"

	"github.com/kilolockio/kilolock/pkg/auth"
)

func TestInitialStatePolicyForPrincipal(t *testing.T) {
	t.Run("vanilla environment forces strict exclusive", func(t *testing.T) {
		exclusive, coexistence := initialStatePolicyForPrincipal(auth.Principal{
			EnvironmentStateLockDefaultMode: string(EnvironmentStateLockDefaultVanilla),
		}, initialStateCreatorKL)
		if !exclusive || coexistence != StateCoexistenceStrict {
			t.Fatalf("got exclusive=%v coexistence=%q", exclusive, coexistence)
		}
	})

	t.Run("kilolock environment forces optimistic warn", func(t *testing.T) {
		exclusive, coexistence := initialStatePolicyForPrincipal(auth.Principal{
			EnvironmentStateLockDefaultMode: string(EnvironmentStateLockDefaultKilolock),
		}, initialStateCreatorBackend)
		if exclusive || coexistence != StateCoexistenceWarn {
			t.Fatalf("got exclusive=%v coexistence=%q", exclusive, coexistence)
		}
	})

	t.Run("auto backend prefers vanilla behavior", func(t *testing.T) {
		exclusive, coexistence := initialStatePolicyForPrincipal(auth.Principal{
			EnvironmentStateLockDefaultMode: string(EnvironmentStateLockDefaultAuto),
		}, initialStateCreatorBackend)
		if !exclusive || coexistence != StateCoexistenceStrict {
			t.Fatalf("got exclusive=%v coexistence=%q", exclusive, coexistence)
		}
	})

	t.Run("auto kl prefers kilolock behavior", func(t *testing.T) {
		exclusive, coexistence := initialStatePolicyForPrincipal(auth.Principal{
			EnvironmentStateLockDefaultMode: string(EnvironmentStateLockDefaultAuto),
		}, initialStateCreatorKL)
		if exclusive || coexistence != StateCoexistenceWarn {
			t.Fatalf("got exclusive=%v coexistence=%q", exclusive, coexistence)
		}
	})
}
