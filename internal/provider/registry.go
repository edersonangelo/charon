package provider

// Default is the registry this project ships with: the kinds of proof a
// provider can offer, and nothing about who offers them.
func Default() *Registry {
	registry := NewRegistry()
	registry.Register(HMAC, buildHMAC)
	registry.Register(SharedToken, buildSharedToken)
	registry.Register(BasicAuth, buildBasicAuth)
	return registry
}
