//go:build !cloud

package main

import (
	"context"
	"fmt"
	"net/http"
)

// OSS build: cloud-only routes and permissions are absent.

type cloudFeatures struct{}

func newCloudFeatures() *cloudFeatures { return nil }

func (s *server) registerCloudRoutes(_ *http.ServeMux) {}

func cloudPermissionForRoute(_, _ string) (string, bool) { return "", false }

func (s *server) handleCloudAPI(_ http.ResponseWriter, _ *http.Request, _ string, _ string) bool {
	return false
}

func (s *server) billingPlansPayload() map[string]any {
	return map[string]any{"plans": []any{}}
}

func (s *server) billingCheckoutSessionPayload(_ context.Context, _, _, _, _, _ string) (map[string]any, error) {
	return nil, fmt.Errorf("stripe billing is not enabled in this build")
}

func (s *server) billingPortalSessionPayload(_ context.Context, _ string) (map[string]any, error) {
	return nil, fmt.Errorf("stripe billing is not enabled in this build")
}
