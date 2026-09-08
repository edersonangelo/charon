package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

// SigningFor is what a destination signs with, and what the process that
// delivers last found when it tried to read each one.
func (s *Store) SigningFor(ctx context.Context, destination uuid.UUID) ([]console.SigningSecret, error) {
	rows, err := s.q.SigningFor(ctx, destination)
	if err != nil {
		return nil, fmt.Errorf("reading how %s is signed: %w", destination, err)
	}

	secrets := make([]console.SigningSecret, 0, len(rows))
	for _, row := range rows {
		secrets = append(secrets, console.SigningSecret{
			Reference: row.Reference,
			Added:     row.CreatedAt,
			Checked:   row.Readable.Valid,
			Readable:  row.Readable.Bool,
			Detail:    row.Detail,
			CheckedAt: row.CheckedAt.Time,
		})
	}
	return secrets, nil
}

// Unsigned is every destination that is delivered to and signs with nothing.
func (s *Store) Unsigned(ctx context.Context) ([]console.UnsignedDestination, error) {
	rows, err := s.q.DestinationsWithoutSigning(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading the destinations that sign with nothing: %w", err)
	}

	unsigned := make([]console.UnsignedDestination, 0, len(rows))
	for _, row := range rows {
		unsigned = append(unsigned, console.UnsignedDestination{Name: row.Name, Routes: row.Routes})
	}
	return unsigned, nil
}

// SigningSecret is one secret a destination signs with, as an operator sees
// it: where the secret is kept, never what it is.
type SigningSecret struct {
	Destination string
	Reference   string
	Added       time.Time
}

func (s *Store) AddSigningSecret(ctx context.Context, destination, reference string) error {
	target, err := s.q.DestinationByName(ctx, db.DestinationByNameParams{
		TenantID: s.tenantOf(ctx), Name: destination,
	})
	if err != nil {
		return ErrNoDestination
	}

	if err := s.q.AddSigningSecret(ctx, db.AddSigningSecretParams{
		TenantID:      s.tenantOf(ctx),
		DestinationID: target.ID,
		Reference:     reference,
	}); err != nil {
		return fmt.Errorf("adding a signing secret to %q: %w", destination, err)
	}
	s.announceSigning(ctx)
	return nil
}

var ErrNoSigningSecret = fmt.Errorf("that destination does not sign with that secret")

func (s *Store) RemoveSigningSecret(ctx context.Context, destination, reference string) error {
	target, err := s.q.DestinationByName(ctx, db.DestinationByNameParams{
		TenantID: s.tenantOf(ctx), Name: destination,
	})
	if err != nil {
		return ErrNoDestination
	}

	removed, err := s.q.RemoveSigningSecret(ctx, db.RemoveSigningSecretParams{
		TenantID:      s.tenantOf(ctx),
		DestinationID: target.ID,
		Reference:     reference,
	})
	if err != nil {
		return fmt.Errorf("removing a signing secret from %q: %w", destination, err)
	}
	if removed == 0 {
		return ErrNoSigningSecret
	}
	s.announceSigning(ctx)
	return nil
}

// A secret that was just added is unread until the process that delivers looks
// at it, and the panel says so. Announcing the change makes that a moment
// rather than however long the reporting interval had left to run.
func (s *Store) announceSigning(ctx context.Context) {
	_ = s.q.NotifySigning(ctx)
	s.announceToPanel(ctx)
}

// SigningChanges reports that a destination gained or lost a signing secret.
func (s *Store) SigningChanges(ctx context.Context) <-chan struct{} {
	return s.listenOn(ctx, "charon_signing")
}

func (s *Store) SigningSecrets(ctx context.Context) ([]SigningSecret, error) {
	rows, err := s.q.SigningSecrets(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading the signing secrets: %w", err)
	}

	secrets := make([]SigningSecret, 0, len(rows))
	for _, row := range rows {
		secrets = append(secrets, SigningSecret{
			Destination: row.Destination,
			Reference:   row.Reference,
			Added:       row.CreatedAt,
		})
	}
	return secrets, nil
}

// AReference is one signing secret as the process that delivers sees it:
// something to try to read, and somewhere to say what happened.
type AReference struct {
	ID        uuid.UUID
	Tenant    uuid.UUID
	Reference string
}

// EveryReference reads them across all tenants, one tenant at a time, so it
// returns the same thing whether or not the role it connects as is confined by
// row level security.
func (s *Store) EveryReference(ctx context.Context) ([]AReference, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return nil, err
	}

	var all []AReference
	for _, tenant := range tenants {
		scope := authz.WithTenant(ctx, tenant.ID)
		rows, err := s.q.SigningSecretsEverywhere(scope)
		if err != nil {
			return nil, fmt.Errorf("reading the signing secrets of %s: %w", tenant.Slug, err)
		}
		for _, row := range rows {
			all = append(all, AReference{
				ID: row.ID, Tenant: row.TenantID, Reference: row.Reference,
			})
		}
	}
	return all, nil
}

// RecordReading writes what the process that delivers found when it tried to
// read a secret, which is the only honest source for what the panel shows.
func (s *Store) RecordReading(ctx context.Context, ref AReference, err error) error {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}

	scope := authz.WithTenant(ctx, ref.Tenant)
	if writeErr := s.q.RecordSigningCheck(scope, db.RecordSigningCheckParams{
		SigningSecretID: ref.ID,
		TenantID:        ref.Tenant,
		Readable:        err == nil,
		Detail:          detail,
	}); writeErr != nil {
		return fmt.Errorf("recording the reading of %s: %w", ref.Reference, writeErr)
	}
	return nil
}

// SendTest puts a synthetic event through the same path a real one takes, to
// one destination only.
//
// It is not delivered from here: the process that serves the panel holds no
// signing secrets, so anything it sent would be unsigned and would prove the
// opposite of what was asked. Recording it and letting the dispatcher deliver
// it is what makes the test the real thing, signature and all.
func (s *Store) SendTest(ctx context.Context, routeID uuid.UUID) (uuid.UUID, error) {
	route, err := s.q.RouteByID(ctx, db.RouteByIDParams{
		ID: routeID, TenantID: s.tenantOf(ctx),
	})
	if err != nil {
		return uuid.Nil, ErrNoRoute
	}

	event, err := s.Record(ctx, inbound.Request{
		Provider:   route.Provider,
		Path:       "/webhooks/" + route.Provider + "/test",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"charon":"test"}`),
	})
	if err != nil {
		return uuid.Nil, err
	}

	if err := s.q.CreateDelivery(ctx, db.CreateDeliveryParams{
		TenantID:      s.tenantOf(ctx),
		EventID:       event,
		DestinationID: route.DestinationID,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("planning the test delivery: %w", err)
	}
	if err := s.q.MarkEventPlanned(ctx, event); err != nil {
		return uuid.Nil, fmt.Errorf("marking the test event planned: %w", err)
	}

	s.announceToPanel(ctx)
	return event, nil
}
