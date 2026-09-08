package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres/db"
	"github.com/edersonangelo/charon/internal/provider"
)

// Verification reads the settings, resolving each secret from the environment.
// The database never holds a secret, only the name of the variable that does.
func (s *Store) Verification(ctx context.Context) (map[string]provider.Settings, error) {
	rows, err := s.q.Providers(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading verification settings: %w", err)
	}

	settings := make(map[string]provider.Settings, len(rows))
	for _, row := range rows {
		settings[row.Name] = provider.Settings{
			Verifier:     row.Verifier,
			Secret:       os.Getenv(row.SecretEnv),
			VerifyToken:  os.Getenv(row.VerifyTokenEnv),
			Header:       row.SignatureHeader.String,
			Scheme:       row.Scheme,
			Algorithm:    row.Algorithm,
			Encoding:     row.Encoding,
			TimestampKey: row.TimestampKey,
			SignatureKey: row.SignatureKey,
			Tolerance:    time.Duration(row.ToleranceSeconds) * time.Second,
		}
	}
	return settings, nil
}

// AllVerification reads the settings of every tenant, because one process
// checks requests for all of them. It reads them one tenant at a time rather
// than in a single query, so it returns the same thing whether or not the role
// it connects as is confined by row level security.
func (s *Store) AllVerification(
	ctx context.Context,
) (map[uuid.UUID]map[string]provider.Settings, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return nil, err
	}

	byTenant := make(map[uuid.UUID]map[string]provider.Settings, len(tenants))
	for _, tenant := range tenants {
		settings, err := s.Verification(authz.WithTenant(ctx, tenant.ID))
		if err != nil {
			return nil, err
		}
		if len(settings) > 0 {
			byTenant[tenant.ID] = settings
		}
	}
	return byTenant, nil
}

// ProviderSettings is the configuration as stored, without secrets, for the
// panel and the command line to show.
type ProviderSettings struct {
	Name               string
	SecretEnv          string
	SecretPresent      bool
	VerifyTokenEnv     string
	VerifyTokenPresent bool
	provider.Settings
}

func (s *Store) ProviderSettings(ctx context.Context) ([]ProviderSettings, error) {
	rows, err := s.q.Providers(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading verification settings: %w", err)
	}

	settings := make([]ProviderSettings, 0, len(rows))
	for _, row := range rows {
		_, present := os.LookupEnv(row.SecretEnv)
		tokenPresent := false
		if row.VerifyTokenEnv != "" {
			_, tokenPresent = os.LookupEnv(row.VerifyTokenEnv)
		}
		settings = append(settings, ProviderSettings{
			Name:               row.Name,
			SecretEnv:          row.SecretEnv,
			SecretPresent:      present,
			VerifyTokenEnv:     row.VerifyTokenEnv,
			VerifyTokenPresent: tokenPresent,
			Settings: provider.Settings{
				Verifier:     row.Verifier,
				Header:       row.SignatureHeader.String,
				Scheme:       row.Scheme,
				Algorithm:    row.Algorithm,
				Encoding:     row.Encoding,
				TimestampKey: row.TimestampKey,
				SignatureKey: row.SignatureKey,
				Tolerance:    time.Duration(row.ToleranceSeconds) * time.Second,
			},
		})
	}
	return settings, nil
}

// Verifications is what the panel shows: the settings plus how many requests
// each provider has refused, which is the number that matters when a secret is
// wrong.
func (s *Store) Verifications(ctx context.Context) ([]console.Verification, error) {
	settings, err := s.ProviderSettings(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.q.RefusedByProvider(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("counting refused requests: %w", err)
	}
	refused := make(map[string]int64, len(rows))
	for _, row := range rows {
		refused[row.Provider] = row.Refused
	}

	out := make([]console.Verification, 0, len(settings))
	for _, item := range settings {
		out = append(out, console.Verification{
			Provider:           item.Name,
			Verifier:           item.Verifier,
			Scheme:             item.Scheme,
			Algorithm:          item.Algorithm,
			Encoding:           item.Encoding,
			Header:             item.Header,
			SecretEnv:          item.SecretEnv,
			SecretPresent:      item.SecretPresent,
			VerifyTokenEnv:     item.VerifyTokenEnv,
			VerifyTokenPresent: item.VerifyTokenPresent,
			Refused:            refused[item.Name],
		})
	}
	return out, nil
}

func (s *Store) SetProvider(ctx context.Context, settings ProviderSettings) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the provider transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	tolerance := int32(settings.Tolerance.Seconds()) //nolint:gosec // minutes at most
	if tolerance <= 0 {
		tolerance = 300
	}

	if err := q.SetProvider(ctx, db.SetProviderParams{
		TenantID:         s.tenantOf(ctx),
		Name:             settings.Name,
		Verifier:         settings.Verifier,
		SecretEnv:        settings.SecretEnv,
		SignatureHeader:  text(settings.Header),
		ToleranceSeconds: tolerance,
		Scheme:           orDefault(settings.Scheme, provider.Simple),
		Algorithm:        orDefault(settings.Algorithm, provider.SHA256),
		Encoding:         orDefault(settings.Encoding, provider.Hex),
		TimestampKey:     orDefault(settings.TimestampKey, "t"),
		SignatureKey:     orDefault(settings.SignatureKey, "v1"),
		VerifyTokenEnv:   settings.VerifyTokenEnv,
	}); err != nil {
		return fmt.Errorf("saving provider %q: %w", settings.Name, err)
	}
	if err := q.NotifyProviders(ctx); err != nil {
		return fmt.Errorf("announcing the provider change: %w", err)
	}
	if err := q.NotifyPanel(ctx); err != nil {
		return fmt.Errorf("announcing the provider change to the panel: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing provider %q: %w", settings.Name, err)
	}
	return nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (s *Store) DeleteProvider(ctx context.Context, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the provider transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	if err := q.DeleteProvider(ctx, db.DeleteProviderParams{
		TenantID: s.tenantOf(ctx), Name: name,
	}); err != nil {
		return fmt.Errorf("deleting provider %q: %w", name, err)
	}
	if err := q.NotifyProviders(ctx); err != nil {
		return fmt.Errorf("announcing the provider change: %w", err)
	}
	if err := q.NotifyPanel(ctx); err != nil {
		return fmt.Errorf("announcing the provider change to the panel: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the deletion of %q: %w", name, err)
	}
	return nil
}

// KnownProviders names every provider this tenant has already established, by
// configuring verification for it, by routing it, or by having received a
// request under it. A name is never declared up front, so this is the only
// thing a new one can be weighed against.
func (s *Store) KnownProviders(ctx context.Context) ([]string, error) {
	names, err := s.q.KnownProviders(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading the known providers: %w", err)
	}
	return names, nil
}

func (s *Store) ProviderChanges(ctx context.Context) <-chan struct{} {
	return s.listenOn(ctx, "charon_provider")
}

// Reconsider does that for every provider of every tenant that has settings,
// which is what a change to any of them calls for: whoever configured one just
// answered a question that was open for everything already recorded under it.
//
// Bounded per provider, because a deployment that configures verification a
// year in is asking about a year of requests, and the ones still waiting to be
// delivered are the ones the answer changes anything for.
func (s *Store) Reconsider(ctx context.Context, batch int32) (int, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return 0, err
	}

	var recovered int
	for _, tenant := range tenants {
		scope := authz.WithTenant(ctx, tenant.ID)

		configured, err := s.Verification(scope)
		if err != nil {
			return recovered, err
		}
		for name := range configured {
			count, err := s.Recheck(scope, name, batch)
			if err != nil {
				return recovered, err
			}
			recovered += count
		}
	}
	return recovered, nil
}

// Recheck answers, for the requests of a provider that have no answer yet or
// were refused, the question the settings in force can now answer, and reopens
// planning for the ones that pass.
//
// A request recorded before any verification existed is unchecked, which is
// not a verdict but the absence of one: nothing was configured, so nothing was
// asked. Configuring it later is what makes the question askable, which is why
// unchecked is reconsidered and not left as though it had been decided.
func (s *Store) Recheck(
	ctx context.Context, name string, batch int32,
) (recovered int, err error) {
	all, err := s.Verification(ctx)
	if err != nil {
		return 0, err
	}
	settings, configured := all[name]
	if !configured {
		return 0, fmt.Errorf("provider %q: %w", name, ErrNoProvider)
	}

	verifier, err := provider.Default().Verifier(settings)
	if err != nil {
		return 0, fmt.Errorf("verification settings of %q: %w", name, err)
	}

	events, err := s.q.UnverifiedEvents(ctx, db.UnverifiedEventsParams{
		TenantID: s.tenantOf(ctx),
		Provider: name, Limit: batch,
	})
	if err != nil {
		return 0, fmt.Errorf("reading unverified requests of %q: %w", name, err)
	}

	for _, event := range events {
		var headers map[string][]string
		if err := json.Unmarshal(event.Headers, &headers); err != nil {
			return recovered, fmt.Errorf("decoding the headers of %s: %w", event.ID, err)
		}

		state, _ := verifier.Verify(provider.Request{
			Headers: http.Header(headers), Body: event.Body, Now: time.Now(),
		})
		if state == provider.Valid {
			if err := s.q.MarkSignatureAndReopen(ctx, db.MarkSignatureAndReopenParams{
				ID: event.ID, Signature: string(state),
			}); err != nil {
				return recovered, fmt.Errorf("reopening %s: %w", event.ID, err)
			}
			recovered++
			continue
		}
		if err := s.q.MarkSignature(ctx, db.MarkSignatureParams{
			ID: event.ID, Signature: string(state),
		}); err != nil {
			return recovered, fmt.Errorf("marking %s: %w", event.ID, err)
		}
	}

	if recovered > 0 {
		if err := s.q.NotifyWork(ctx); err != nil {
			return recovered, fmt.Errorf("announcing the recovered events: %w", err)
		}
	}
	return recovered, nil
}

func (s *Store) SignatureTotals(ctx context.Context) (map[string]int64, error) {
	rows, err := s.q.SignatureTotals(ctx)
	if err != nil {
		return nil, fmt.Errorf("counting signatures: %w", err)
	}
	totals := make(map[string]int64, len(rows))
	for _, row := range rows {
		totals[row.Signature] = row.Total
	}
	return totals, nil
}
