package store

import "testing"

func TestPortalRoleAllowsControlPermission(t *testing.T) {
	cases := []struct {
		name       string
		role       string
		permission string
		want       bool
	}{
		{name: "owner env create", role: "owner", permission: "environment.create", want: true},
		{name: "owner tenant lifecycle", role: "owner", permission: "tenant.lifecycle.update", want: true},
		{name: "owner billing", role: "owner", permission: "tenant.billing.checkout", want: true},
		{name: "tenant admin env create", role: "tenant_admin", permission: "environment.create", want: true},
		{name: "tenant admin state config", role: "tenant_admin", permission: "state.config.update", want: true},
		{name: "tenant admin transfer denied", role: "tenant_admin", permission: "environment.transfer.update", want: false},
		{name: "tenant admin tenant lifecycle denied", role: "tenant_admin", permission: "tenant.lifecycle.update", want: false},
		{name: "billing admin billing", role: "billing_admin", permission: "tenant.billing.checkout", want: true},
		{name: "billing admin state denied", role: "billing_admin", permission: "state.delete", want: false},
		{name: "member denied", role: "member", permission: "environment.read", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PortalRoleAllowsControlPermission(tc.role, tc.permission); got != tc.want {
				t.Fatalf("PortalRoleAllowsControlPermission(%q, %q)=%v want %v", tc.role, tc.permission, got, tc.want)
			}
		})
	}
}
