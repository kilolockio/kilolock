//go:build integration

// This file contains apply-pattern benchmarks for the normalization
// path. It is built only with `-tags=integration`, the same gate the
// other store integration tests use.
//
// The benchmark is *not* go's testing.B because the workload is not
// repeatable in fixed-size chunks (each write depends on the previous
// state). Instead, it's a parametric test that writes N cumulatively
// growing state versions and prints clear before/after timing.
//
// Run, optionally with -v to see per-bucket detail:
//
//	docker exec kl-postgres psql -U kl -d postgres \
//	   -c 'DROP DATABASE IF EXISTS kl_bench'
//	docker exec kl-postgres psql -U kl -d postgres \
//	   -c 'CREATE DATABASE kl_bench'
//
//	KL_DATABASE_URL='postgres://kl:kl@localhost:5432/kl_bench?sslmode=disable' \
//	  KL_BENCH_N=2000 \
//	  go test -tags=integration -timeout=20m -v -run TestApplyPattern_Bench \
//	  ./pkg/store/
//
// KL_BENCH_N (default 500) is the maximum resource count; the
// benchmark performs N cumulative writes, growing from 1 resource to N
// resources, mirroring what `terraform apply` does to the http backend
// during a fresh apply of N resources.

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/db"
	"github.com/davesade/kilolock/internal/migrate"
	"github.com/davesade/kilolock/internal/testdb"
	"github.com/davesade/kilolock/pkg/store"
)

// TestApplyPattern_Bench simulates a terraform apply that creates N
// resources by issuing N cumulative state writes against the store,
// timing each, and reporting summary statistics. It is the
// reproducible measurement gate for normalization perf work.
//
// Not named Benchmark* because Go's testing package would then demand
// a *testing.B signature, and this workload is not repeatable in
// fixed-size chunks (each write depends on the previous version).
func TestApplyPattern_Bench(t *testing.T) {
	url := os.Getenv("KL_DATABASE_URL")
	if url == "" {
		t.Skip("KL_DATABASE_URL not set; this benchmark needs a dedicated Postgres database")
	}

	maxN := 500
	if v := os.Getenv("KL_BENCH_N"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("invalid KL_BENCH_N=%q", v)
		}
		maxN = n
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 20*time.Minute)
	defer cancel()

	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := migrate.Run(ctx, pool.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Always start the benchmark from a clean slate so numbers are
	// comparable run-to-run, while preserving any state listed in
	// internal/testdb.ProtectedStates (default: "big-state"). The
	// old TRUNCATE form silently wiped operator-managed fixtures
	// when the bench was accidentally pointed at a shared dev DB.
	protected := testdb.ProtectedStates()
	if _, err := pool.Exec(ctx, `
		DELETE FROM events
		WHERE state_id IS NULL
		   OR state_id NOT IN (SELECT id FROM states WHERE name = ANY($1))
	`, protected); err != nil {
		t.Fatalf("delete unprotected events: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM states WHERE NOT (name = ANY($1))
	`, protected); err != nil {
		t.Fatalf("delete unprotected states: %v", err)
	}

	st := store.New(pool.Pool)
	stateName := fmt.Sprintf("bench-%d", time.Now().UnixNano())

	// Sample timings at log-spaced points so we get useful detail at
	// both small and large N without printing N lines.
	sampleAt := logSpacedSamplePoints(maxN)
	samples := make(map[int]time.Duration, len(sampleAt))
	durations := make([]time.Duration, 0, maxN)

	t.Logf("benchmark: maxN=%d, sampling at %d points", maxN, len(sampleAt))
	t.Logf("%-8s %-12s %-12s", "version", "resources", "write_ms")

	wallStart := time.Now()
	for i := 1; i <= maxN; i++ {
		body := syntheticState(int64(i), i)
		writeStart := time.Now()
		err := st.WriteState(ctx, stateName, "", body, "bench", "bench")
		writeDur := time.Since(writeStart)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		durations = append(durations, writeDur)

		if _, ok := sampleAt[i]; ok {
			samples[i] = writeDur
			t.Logf("%-8d %-12d %-12.2f", i, i, float64(writeDur.Microseconds())/1000)
		}
	}
	wallTotal := time.Since(wallStart)

	stats := summarize(durations)
	t.Logf("")
	t.Logf("=== summary (N=%d) ===", maxN)
	t.Logf("total wall time   : %s", wallTotal.Round(time.Millisecond))
	t.Logf("writes            : %d", maxN)
	t.Logf("per-write avg     : %s", stats.avg.Round(time.Microsecond))
	t.Logf("per-write median  : %s", stats.p50.Round(time.Microsecond))
	t.Logf("per-write p90     : %s", stats.p90.Round(time.Microsecond))
	t.Logf("per-write p99     : %s", stats.p99.Round(time.Microsecond))
	t.Logf("per-write max     : %s", stats.max.Round(time.Microsecond))
	t.Logf("first write       : %s", durations[0].Round(time.Microsecond))
	t.Logf("last write        : %s", durations[len(durations)-1].Round(time.Microsecond))
	t.Logf("last/first ratio  : %.1fx", float64(durations[len(durations)-1])/float64(durations[0]))
}

// syntheticState produces a terraform v4 state body containing
// resourceCount instances of a simple resource shape (mirrors the
// big-state demo's `random_id` resources). serial is the state serial.
//
// We don't bother with dependency edges here -- the goal is to
// measure the row-insert cost, which dominates at large N. Edge
// resolution is one query regardless of N.
func syntheticState(serial int64, resourceCount int) []byte {
	resources := make([]any, 0, resourceCount)
	for i := 0; i < resourceCount; i++ {
		resources = append(resources, map[string]any{
			"mode":     "managed",
			"type":     "random_id",
			"name":     fmt.Sprintf("r%d", i),
			"provider": "provider[\"registry.terraform.io/hashicorp/random\"]",
			"instances": []any{
				map[string]any{
					"schema_version": 0,
					"attributes": map[string]any{
						"id":          fmt.Sprintf("id-%d", i),
						"b64_std":     fmt.Sprintf("AAAA%d==", i),
						"b64_url":     fmt.Sprintf("AAAA%d", i),
						"byte_length": 8,
						"dec":         fmt.Sprintf("%d", 12345678+i),
						"hex":         fmt.Sprintf("%08x", i),
						"keepers":     nil,
						"prefix":      "",
					},
					"sensitive_attributes": []any{},
				},
			},
		})
	}
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            serial,
		"lineage":           "9b39e2c0-bbbb-cccc-dddd-eeeeeeeeeeee",
		"outputs":           map[string]any{},
		"resources":         resources,
	}
	b, _ := json.Marshal(body)
	return b
}

type benchStats struct {
	avg, p50, p90, p99, max time.Duration
}

func summarize(d []time.Duration) benchStats {
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, v := range d {
		total += v
	}
	pct := func(p float64) time.Duration {
		idx := int(float64(len(sorted)-1) * p)
		return sorted[idx]
	}
	return benchStats{
		avg: total / time.Duration(len(d)),
		p50: pct(0.50),
		p90: pct(0.90),
		p99: pct(0.99),
		max: sorted[len(sorted)-1],
	}
}

// logSpacedSamplePoints returns a set of versions to print
// per-write timing for. We always include the first, the last,
// and roughly log-spaced points in between so output stays
// readable at any N.
func logSpacedSamplePoints(maxN int) map[int]struct{} {
	out := map[int]struct{}{1: {}, maxN: {}}
	for v := 10; v <= maxN; v *= 10 {
		out[v] = struct{}{}
		if v*2 <= maxN {
			out[v*2] = struct{}{}
		}
		if v*5 <= maxN {
			out[v*5] = struct{}{}
		}
	}
	return out
}
