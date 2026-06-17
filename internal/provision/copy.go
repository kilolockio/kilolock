package provision

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CopyDatabase clones all objects from srcDSN to dstDSN using pg_dump and pg_restore.
// Requires pg_dump and pg_restore on PATH (postgresql client tools).
func CopyDatabase(ctx context.Context, srcDSN, dstDSN string) error {
	srcDSN = strings.TrimSpace(srcDSN)
	dstDSN = strings.TrimSpace(dstDSN)
	if srcDSN == "" || dstDSN == "" {
		return fmt.Errorf("source and destination DSN are required")
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		return fmt.Errorf("pg_dump not found on PATH: %w", err)
	}
	if _, err := exec.LookPath("pg_restore"); err != nil {
		return fmt.Errorf("pg_restore not found on PATH: %w", err)
	}

	tmp, err := os.CreateTemp("", "kl-pgdump-*.dump")
	if err != nil {
		return err
	}
	dumpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(dumpPath)

	dump := exec.CommandContext(ctx, "pg_dump",
		"--format=custom",
		"--no-owner",
		"--no-acl",
		"--dbname", srcDSN,
		"--file", dumpPath,
	)
	if out, err := dump.CombinedOutput(); err != nil {
		return fmt.Errorf("pg_dump: %w: %s", err, strings.TrimSpace(string(out)))
	}

	restore := exec.CommandContext(ctx, "pg_restore",
		"--clean",
		"--if-exists",
		"--no-owner",
		"--no-acl",
		"--dbname", dstDSN,
		dumpPath,
	)
	if out, err := restore.CombinedOutput(); err != nil {
		// pg_restore may exit non-zero for benign warnings; require dump file used.
		if !strings.Contains(string(out), "pg_restore:") && err != nil {
			return fmt.Errorf("pg_restore: %w: %s", err, strings.TrimSpace(string(out)))
		}
		// If error is only warnings, still fail on hard errors — keep strict for v1.
		if err != nil {
			return fmt.Errorf("pg_restore: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
