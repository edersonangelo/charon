// Package authz decides what an operator may do, once auth has decided who
// they are.
//
// Permissions are closed: each one names something the code actually checks,
// so one that nothing enforces cannot be invented. Roles are open: a role is a
// name for a set of permissions, and that is configuration.
package authz

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Least is what somebody holds in a tenant when their membership states no
// role: belonging without a role stated is belonging as a viewer.
const Least = "viewer"

type Permission string

const (
	EventsRead        Permission = "events.read"
	EventsReplay      Permission = "events.replay"
	EventsForce       Permission = "events.force"
	RoutesRead        Permission = "routes.read"
	RoutesWrite       Permission = "routes.write"
	VerificationRead  Permission = "verification.read"
	VerificationWrite Permission = "verification.write"
	OperatorsRead     Permission = "operators.read"
	OperatorsWrite    Permission = "operators.write"
	RolesWrite        Permission = "roles.write"
	TenantsWrite      Permission = "tenants.write"
)

// Described is every permission with what granting it allows, so a role can be
// built without reading the code.
func Described() map[Permission]string {
	return map[Permission]string{
		EventsRead:        "search events and read what arrived and where it went",
		EventsReplay:      "send a recorded event to its destinations again",
		EventsForce:       "deliver an event whose signature failed, saying why",
		RoutesRead:        "see where each provider's events are delivered",
		RoutesWrite:       "add, change and remove routes and destinations",
		VerificationRead:  "see how each provider's requests are checked",
		VerificationWrite: "change how requests are checked, and recheck refused ones",
		OperatorsRead:     "see who can sign in",
		OperatorsWrite:    "add and remove operators, and change their role",
		RolesWrite:        "define roles and what they may do",
		TenantsWrite:      "create and remove tenants",
	}
}

// Area is the part of Charon a permission is about, and Action is what it
// allows there. Splitting the name is how the panel groups them instead of
// listing ten strings in a row.
func (p Permission) Area() string {
	area, _, _ := strings.Cut(string(p), ".")
	return area
}

func (p Permission) Action() string {
	_, action, _ := strings.Cut(string(p), ".")
	return action
}

// Everything is what a system administrator holds: not a role stored anywhere,
// because a role belongs to a tenant and this deliberately does not.
func Everything() Role {
	return Role{
		Name:        "system administrator",
		Description: "not confined to a tenant",
		Grants:      All(),
	}
}

// Group is one area with everything it allows.
type Group struct {
	Area        string
	Permissions []Permission
}

// Grouped is the closed set arranged by area, in the order the areas first
// appear, so the panel can show it as something readable.
func Grouped() []Group {
	groups := make([]Group, 0, 6)
	for _, permission := range All() {
		area := permission.Area()
		at := slices.IndexFunc(groups, func(g Group) bool { return g.Area == area })
		if at < 0 {
			groups = append(groups, Group{Area: area})
			at = len(groups) - 1
		}
		groups[at].Permissions = append(groups[at].Permissions, permission)
	}
	return groups
}

func All() []Permission {
	permissions := slices.Collect(maps.Keys(Described()))
	slices.Sort(permissions)
	return permissions
}

func Known(permission Permission) bool {
	_, exists := Described()[permission]
	return exists
}

var (
	// ErrDenied is what a handler turns into a refusal.
	ErrDenied = errors.New("the role does not allow that")
	// ErrUnknownPermission guards against storing a permission nothing checks,
	// which would look granted and do nothing.
	ErrUnknownPermission = errors.New("no such permission")
)

// Role is a name and what it allows. It is data: a deployment defines its own.
type Role struct {
	Name        string
	Description string
	// BuiltIn marks the roles shipped with the project. They are editable like
	// any other; the flag only lets the panel say so.
	BuiltIn bool
	Grants  []Permission
}

func (r Role) Allows(permission Permission) bool {
	return slices.Contains(r.Grants, permission)
}

func (r Role) Authorize(permission Permission) error {
	if r.Allows(permission) {
		return nil
	}
	return fmt.Errorf("%w: %s needs %s", ErrDenied, r.Name, permission)
}

// Validate rejects a role that grants something nothing enforces.
func (r Role) Validate() error {
	if r.Name == "" {
		return errors.New("a role needs a name")
	}
	for _, granted := range r.Grants {
		if !Known(granted) {
			return fmt.Errorf("%w: %q", ErrUnknownPermission, granted)
		}
	}
	return nil
}

// The roles a new tenant starts with. They are seeded, not enforced: a
// deployment is free to change or remove any of them.
const (
	Viewer   = "viewer"
	Operator = "operator"
	Admin    = "admin"
	Owner    = "owner"
)

func BuiltIn() []Role {
	read := []Permission{EventsRead, RoutesRead, VerificationRead, OperatorsRead}

	viewer := Role{
		Name:        Viewer,
		Description: "read what arrived and where it went, and nothing else",
		BuiltIn:     true,
		Grants:      read,
	}

	operator := Role{
		Name:        Operator,
		Description: "everything a viewer can, and send an event again",
		BuiltIn:     true,
		Grants:      append(slices.Clone(read), EventsReplay),
	}

	admin := Role{
		Name:        Admin,
		Description: "everything an operator can, and configure this tenant",
		BuiltIn:     true,
		Grants: append(slices.Clone(operator.Grants),
			EventsForce, RoutesWrite, VerificationWrite, OperatorsWrite, RolesWrite),
	}

	owner := Role{
		Name:        Owner,
		Description: "everything an admin can, and manage tenants",
		BuiltIn:     true,
		Grants:      append(slices.Clone(admin.Grants), TenantsWrite),
	}

	return []Role{viewer, operator, admin, owner}
}

type tenantKey struct{}

// WithTenant marks a context as acting for one tenant. Everything done under
// it is confined to that tenant, and storage is expected to enforce it rather
// than trust the caller to filter.
func WithTenant(ctx context.Context, tenant uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenant)
}

func TenantFrom(ctx context.Context) (uuid.UUID, bool) {
	tenant, scoped := ctx.Value(tenantKey{}).(uuid.UUID)
	return tenant, scoped && tenant != uuid.Nil
}
