package provider

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Source is where the settings come from. Declared here, by the side that
// consumes it, so this package depends on no storage.
type Source interface {
	AllVerification(ctx context.Context) (map[uuid.UUID]map[string]Settings, error)
	ProviderChanges(ctx context.Context) <-chan struct{}
}

// Cache holds a built verifier per tenant and provider, so checking a request
// costs no query and no construction. A provider that is absent gets the
// verifier that checks nothing, which is why a cold or failed load can never
// keep a request from being recorded.
type Cache struct {
	source   Source
	registry *Registry
	logger   *slog.Logger
	refresh  time.Duration

	mu       sync.RWMutex
	built    map[uuid.UUID]map[string]Verifier
	fallback Verifier
}

func NewCache(source Source, registry *Registry, refresh time.Duration, logger *slog.Logger) *Cache {
	if registry == nil {
		registry = Default()
	}
	if refresh <= 0 {
		refresh = time.Minute
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Cache{
		source:   source,
		registry: registry,
		logger:   logger,
		refresh:  refresh,
		built:    map[uuid.UUID]map[string]Verifier{},
		fallback: Unverified{},
	}
}

func (c *Cache) Verifier(tenant uuid.UUID, name string) Verifier {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if verifier, configured := c.built[tenant][name]; configured {
		return verifier
	}
	return c.fallback
}

// Watch loads the settings and keeps them current: at once when a change is
// announced, and on the refresh interval regardless, so a missed announcement
// cannot leave verification stale.
func (c *Cache) Watch(ctx context.Context) {
	c.load(ctx)

	changes := c.source.ProviderChanges(ctx)
	ticker := time.NewTicker(c.refresh)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-changes:
			if !open {
				return
			}
			c.load(ctx)
		case <-ticker.C:
			c.load(ctx)
		}
	}
}

func (c *Cache) load(ctx context.Context) {
	byTenant, err := c.source.AllVerification(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.ErrorContext(ctx, "could not load verification settings", "error", err)
		}
		return
	}

	built := make(map[uuid.UUID]map[string]Verifier, len(byTenant))
	for tenant, settings := range byTenant {
		verifiers := make(map[string]Verifier, len(settings))
		for name, item := range settings {
			verifier, err := c.registry.Verifier(item)
			if err != nil {
				c.logger.ErrorContext(ctx, "verification settings are not usable",
					"tenant", tenant, "provider", name, "error", err)
				// Refuse rather than accept silently: settings that cannot be
				// built are a misconfiguration, not permission to skip checking.
				verifier = alwaysInvalid{reason: err}
			}
			verifiers[name] = verifier
		}
		built[tenant] = verifiers
	}

	c.mu.Lock()
	c.built = built
	c.mu.Unlock()
}
