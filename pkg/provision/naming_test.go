package provision

import "testing"

func TestGCPInstanceName(t *testing.T) {
	if got := gcpInstanceName("Acme_Corp", "prod"); got != "ig-acme-corp-prod" {
		t.Fatalf("got %q", got)
	}
}
