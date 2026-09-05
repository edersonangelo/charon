package postgres

import (
	"context"
	"fmt"
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
	return nil
}
