package main

// effectiveBackend resolves the backend key for an endpoint: its own if set,
// otherwise its product's default. Empty means unresolvable.
func effectiveBackend(spec Spec, e Endpoint) string {
	if e.Backend != "" {
		return e.Backend
	}
	return spec.Products[e.Product].Backend
}

// exposedPath returns the route the gateway actually serves: the endpoint's
// own path with its product's prefix prepended. An undeclared or missing
// product yields the zero-value Product, whose empty Prefix leaves the path
// untouched — so this is safe to call unconditionally, before or after
// Validate has run. It is the single source of truth for the exposed route:
// Validate keys collisions on it, and Normalize writes it into Endpoint.Path.
func exposedPath(spec Spec, e Endpoint) string {
	return spec.Products[e.Product].Prefix + e.Path
}

// Normalize applies defaults and resolves derived fields, returning the
// endpoints in input order ready for JSON emission.
func Normalize(spec Spec) []Endpoint {
	out := make([]Endpoint, len(spec.Endpoints))
	for i, e := range spec.Endpoints {
		if e.OutputEncoding == "" {
			e.OutputEncoding = spec.Defaults.OutputEncoding
		}
		if e.Encoding == "" {
			e.Encoding = spec.Defaults.Encoding
		}
		if e.Timeout == "" {
			e.Timeout = spec.Defaults.Timeout
		}
		// url_pattern must be derived from the UNPREFIXED path: the prefix is the
		// edge's namespace, and the backend is never told about it. Deriving it
		// after prefixing would send /vitxo/api/orders upstream.
		if e.URLPattern == "" {
			e.URLPattern = e.Path
		}
		e.Path = exposedPath(spec, e)
		if b, ok := spec.Backends[effectiveBackend(spec, e)]; ok {
			e.HostEnv = b.HostEnv
			e.HostDefault = b.HostDefault
		}
		if e.InputQueryStrings == nil {
			e.InputQueryStrings = []string{}
		}
		out[i] = e
	}
	return out
}
