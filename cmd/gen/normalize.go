package main

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
		if p, ok := spec.Products[e.Product]; ok {
			e.Path = p.Prefix + e.Path
		}
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
