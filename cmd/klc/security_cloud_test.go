//go:build cloud

package main

import "testing"

func TestRequiredPermissionForRoute_Cloud(t *testing.T) {
	cases := []struct {
		method string
		path   string
		perm   string
	}{
		{"GET", "/billing/plans", "tenant.billing.checkout"},
		{"POST", "/billing/checkout-session", "tenant.billing.checkout"},
		{"POST", "/billing/portal-session", "tenant.billing.checkout"},
		{"GET", "/portal/users", "rbac.manage"},
		{"POST", "/portal/users/role", "rbac.manage"},
	}
	for _, tc := range cases {
		got, ok := requiredPermissionForRoute(tc.method, tc.path)
		if !ok {
			t.Fatalf("%s %s unexpectedly unguarded", tc.method, tc.path)
		}
		if got != tc.perm {
			t.Fatalf("%s %s permission=%q want=%q", tc.method, tc.path, got, tc.perm)
		}
	}
}
