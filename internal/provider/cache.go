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
	tokens   map[uuid.UUID]map[string]handshakeToken
	fallback Verifier
}

// handshakeToken carries what a resolved value cannot say on its own: that a
// variable was named for it. A value implies one was, since nothing resolved a
// token nobody named.
type handshakeToken struct {
	value string
	named bool
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
		tokens:   map[uuid.UUID]map[string]handshakeToken{},
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

// VerifyToken is the token a provider must offer before its callback address
// is confirmed, and whether one was ever asked for. A provider that asked and
// whose variable is not set here answers ("", true): nothing can be compared,
// and nothing in this process can put it there either, which is a different
// answer from an address that confirms nothing.
func (c *Cache) VerifyToken(tenant uuid.UUID, name string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	token := c.tokens[tenant][name]
	return token.value, token.named
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
	tokens := make(map[uuid.UUID]map[string]handshakeToken, len(byTenant))
	for tenant, settings := range byTenant {
		verifiers := make(map[string]Verifier, len(settings))
		for name, item := range settings {
			if item.VerifyTokenNamed || item.VerifyToken != "" {
				if tokens[tenant] == nil {
					tokens[tenant] = map[string]handshakeToken{}
				}
				tokens[tenant][name] = handshakeToken{value: item.VerifyToken, named: true}
			}

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

	// Both under one lock: a reader must never see a verifier from this load
	// beside a token from the last one.
	c.mu.Lock()
	c.built = built
	c.tokens = tokens
	c.mu.Unlock()
}
