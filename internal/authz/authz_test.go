package authz_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/authz"
)

// A role may only grant something the code checks, or it would look granted
// and do nothing.
func TestARoleCannotGrantAPermissionNothingEnforces(t *testing.T) {
	t.Parallel()

	role := authz.Role{Name: "made-up", Grants: []authz.Permission{"events.delete"}}
	if err := role.Validate(); !errors.Is(err, authz.ErrUnknownPermission) {
		t.Errorf("error = %v, want ErrUnknownPermission", err)
	}
}

func TestEveryPermissionIsDescribed(t *testing.T) {
	t.Parallel()

	for _, permission := range authz.All() {
		if authz.Described()[permission] == "" {
			t.Errorf("%s has no description, so a role cannot be built without reading the code",
				permission)
		}
	}
}

// The shipped roles have to be valid and ordered by what they allow, because
// that is what makes them a useful starting point.
func TestTheShippedRolesAreValidAndNested(t *testing.T) {
	t.Parallel()

	byName := map[string]authz.Role{}
	for _, role := range authz.BuiltIn() {
		if err := role.Validate(); err != nil {
			t.Fatalf("shipped role %q is not valid: %v", role.Name, err)
		}
		if !role.BuiltIn {
			t.Errorf("shipped role %q is not marked built in", role.Name)
		}
		byName[role.Name] = role
	}

	nested := []string{authz.Viewer, authz.Operator, authz.Admin, authz.Owner}
	for i := 1; i < len(nested); i++ {
		wider, narrower := byName[nested[i]], byName[nested[i-1]]
		for _, granted := range narrower.Grants {
			if !wider.Allows(granted) {
				t.Errorf("%s does not allow %s, which %s does",
					wider.Name, granted, narrower.Name)
			}
		}
	}

	if byName[authz.Viewer].Allows(authz.EventsReplay) {
		t.Error("viewer can replay: the point of the role is that it cannot send anything again")
	}
	if !byName[authz.Owner].Allows(authz.TenantsWrite) {
		t.Error("owner cannot manage tenants")
	}
}

func TestAuthorizeSaysWhatWasMissing(t *testing.T) {
	t.Parallel()

	viewer := authz.BuiltIn()[0]
	err := viewer.Authorize(authz.EventsReplay)
	if !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if got := err.Error(); !strings.Contains(got, "events.replay") {
		t.Errorf("error = %q, want it to name the permission", got)
	}
}
