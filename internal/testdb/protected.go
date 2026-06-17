// Package testdb hosts helpers shared across the various
// _test.go files that need to clean up the integration database
// without nuking operator-managed fixtures.
//
// Background: integration tests share one Postgres instance with
// the operator's local dev work, including the big-state demo
// fixture (10k+ rows, takes minutes to re-bootstrap). The old
// pattern was `TRUNCATE TABLE ... CASCADE` — fast, simple, and
// destructive of anything in the path. This package replaces it
// with an allowlist: states whose names are in
// $KL_TEST_PROTECT_STATES (default: "big-state") survive
// every test sweep; everything else gets DELETEd, cascading
// through state_versions, resources, outputs, state_locks,
// apply_runs, resource_reservations, refresh_runs via the
// schema's ON DELETE CASCADE.
//
// The helpers here are intentionally trivial and side-effect-free
// so they're safe to call from any test setup. They are pure Go
// (no DB calls); the SQL lives next to each test package's
// cleanup function so the table list stays close to the data
// model each suite cares about.
package testdb

import (
	"os"
	"strings"
)

// ProtectedStates returns the list of state names that integration
// test cleanup must preserve. Reads $KL_TEST_PROTECT_STATES
// (comma-separated) and falls back to a single hardcoded default:
// "big-state". Returns at least one element — an explicit
// empty-string env var still falls back to the default rather than
// returning nil, because nil would silently disable the protection
// and the resulting "where did big-state go again" support ticket
// is the entire problem this package exists to prevent.
func ProtectedStates() []string {
	raw := os.Getenv("KL_TEST_PROTECT_STATES")
	if raw == "" {
		return defaultProtected()
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return defaultProtected()
	}
	return out
}

func defaultProtected() []string {
	return []string{"big-state"}
}
