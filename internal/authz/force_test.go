package authz_test

import (
	"slices"
	"testing"

	"github.com/edersonangelo/charon/internal/authz"
)

// Delivering something that failed verification is not a retry. It is an
// operator overruling a security check, so it belongs to whoever configures
// the tenant, not to whoever watches it.
func TestOnlyAnAdministratorCanDeliverPastAFailedSignature(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		authz.Viewer:   false,
		authz.Operator: false,
		authz.Admin:    true,
		authz.Owner:    true,
	}

	for _, role := range authz.BuiltIn() {
		want, known := allowed[role.Name]
		if !known {
			t.Fatalf("role %q is shipped and this test says nothing about it", role.Name)
		}
		if got := slices.Contains(role.Grants, authz.EventsForce); got != want {
			t.Errorf("%s can force: %v, want %v", role.Name, got, want)
		}
	}

	if !slices.Contains(authz.Everything().Grants, authz.EventsForce) {
		t.Error("a system administrator cannot force, and reaches everything by definition")
	}
}
