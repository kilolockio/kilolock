package testdb

import "os"

// DataPlaneAdminURL returns a superuser DSN for CREATE DATABASE tests.
func DataPlaneAdminURL() string {
	if v := os.Getenv("KL_DATA_PLANE_ADMIN_URL"); v != "" {
		return v
	}
	return os.Getenv("KL_DATABASE_URL")
}

// DataPlaneBaseURL returns the libpq URL used to derive per-environment DSNs.
func DataPlaneBaseURL() string {
	if v := os.Getenv("KL_DATA_PLANE_DATABASE_URL"); v != "" {
		return v
	}
	if v := os.Getenv("KL_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("DATABASE_URL")
}
