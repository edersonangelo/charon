package postgres

import (
	"context"
	"fmt"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

// Kinds is what a build knows how to do: the transports it can deliver over,
// the proofs it can check, and the ways it accepts a sign-in. Each is a
// registry a deployment extends by writing Go, so the rows a key points at are
// written by the process that has them rather than frozen in a migration.
type Kinds struct {
	Transports []string
	Verifiers  []string
	Methods    []string
}

// The states a delivery and a signature can be in. Closed domains, held as
// rows so a key can point at them, and written from here for the same reason
// as everything else: a migration that seeded them once is not a guarantee
// they are still there.
func deliveryStates() map[string]string {
	return map[string]string{
		"pending":   "waiting to be handed over, or waiting to be tried again",
		"delivered": "the destination answered that it took it",
		"dead":      "the attempt limit was reached, and it was never delivered",
	}
}

func signatureStates() map[string]string {
	return map[string]string{
		"unchecked": "no verification is configured for this provider",
		"valid":     "the proof matched",
		"invalid":   "a proof was offered and did not match",
		"missing":   "a proof was required and none was offered",
	}
}

func (s *Store) Register(ctx context.Context, known Kinds) error {
	for _, name := range known.Transports {
		if err := s.q.RegisterTransport(ctx, name); err != nil {
			return fmt.Errorf("registering transport %q: %w", name, err)
		}
	}
	for _, name := range known.Verifiers {
		if err := s.q.RegisterVerifier(ctx, name); err != nil {
			return fmt.Errorf("registering verifier %q: %w", name, err)
		}
	}
	for _, name := range known.Methods {
		if err := s.q.RegisterAuthMethod(ctx, name); err != nil {
			return fmt.Errorf("registering the %q sign-in: %w", name, err)
		}
	}

	for name, description := range deliveryStates() {
		if err := s.q.RegisterDeliveryState(ctx, db.RegisterDeliveryStateParams{
			Name: name, Description: description,
		}); err != nil {
			return fmt.Errorf("registering delivery state %q: %w", name, err)
		}
	}
	for name, description := range signatureStates() {
		if err := s.q.RegisterSignatureState(ctx, db.RegisterSignatureStateParams{
			Name: name, Description: description,
		}); err != nil {
			return fmt.Errorf("registering signature state %q: %w", name, err)
		}
	}

	return s.registerAuthorization(ctx)
}

// What an operator may do is decided in Go, and the rows are how a key points
// at it and how a deployment changes one. Writing them here rather than only
// in a migration is what keeps a lost row from being a locked door: the next
// start puts back what this build knows, and nothing about the deployment's
// own roles is touched.
func (s *Store) registerAuthorization(ctx context.Context) error {
	described := authz.Described()
	for _, permission := range authz.All() {
		if err := s.q.RegisterPermission(ctx, db.RegisterPermissionParams{
			Name: string(permission), Description: described[permission],
		}); err != nil {
			return fmt.Errorf("registering permission %q: %w", permission, err)
		}
	}

	for _, role := range authz.BuiltIn() {
		id, err := s.q.RegisterShippedRole(ctx, db.RegisterShippedRoleParams{
			Name: role.Name, Description: role.Description,
		})
		if err != nil {
			return fmt.Errorf("registering role %q: %w", role.Name, err)
		}

		// A role that grants something already has been decided by whoever
		// runs this, and that decision outlives a restart.
		granted, err := s.q.RoleHasAnyGrant(ctx, id)
		if err != nil {
			return fmt.Errorf("reading what %q grants: %w", role.Name, err)
		}
		if granted {
			continue
		}

		for _, permission := range role.Grants {
			if err := s.q.RegisterShippedGrant(ctx, db.RegisterShippedGrantParams{
				RoleID: id, Name: string(permission),
			}); err != nil {
				return fmt.Errorf("granting %q to %q: %w", permission, role.Name, err)
			}
		}
	}
	return nil
}
